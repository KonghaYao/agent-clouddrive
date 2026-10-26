package application

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/application/access"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/jackc/pgx/v5"
)

// Mutate persists intent and the subtree gate before an external mutation. A
// timeout is never treated as failure or permission to repeat a remote request.
func (s *Service) Mutate(ctx context.Context, a Actor, kind string, in Object) (any, error) {
	action := domain.ActionWrite
	switch kind {
	case "mkdir", "move", "rename":
	case "delete":
		action = domain.ActionDelete
	case "restore":
		action = domain.ActionRestore
	default:
		return nil, domain.NewError(domain.InvalidArgument, "unsupported mutation")
	}
	if e := access.New(s.deps()).Scope(a, domain.RequiredScope(action)); e != nil {
		return nil, e
	}
	key := str(in, "idempotency_key")
	if !validIdempotencyKey(key) {
		return nil, domain.NewError(domain.InvalidArgument, "idempotency_key is required (maximum 200 characters)")
	}
	requestHash := hash(in)
	existing, e := one(ctx, s.DB, `SELECT to_jsonb(o) FROM file_operations o WHERE actor_principal_id=$1 AND kind=$2 AND idempotency_key=$3`, a.Principal.ID, kind, key)
	if e == nil {
		if str(existing, "request_hash") != requestHash {
			return nil, domain.NewError(domain.IdempotencyConflict, "idempotency key has a different request")
		}
		return s.mutationResult(ctx, existing)
	}
	if domain.ErrorCode(e) != domain.NotFound {
		return nil, e
	}
	if s.Original == nil {
		return nil, dependency()
	}
	var f, parent Object
	var destination, source ports.ResourceRef
	var trash Object
	fid := str(in, "file_id")
	if kind == "restore" {
		trash, e = one(ctx, s.DB, `SELECT to_jsonb(t) FROM trash_entries t JOIN files f ON f.id=t.root_file_id WHERE t.id=$1 AND f.owner_id=$2 AND t.state='trashed'`, str(in, "trash_id"), a.Principal.ID)
		if e != nil {
			return nil, e
		}
		fid = str(trash, "root_file_id")
	}
	if kind != "mkdir" {
		f, e = access.New(s.deps()).Owned(ctx, a, fid, action)
		if e != nil {
			return nil, e
		}
		if num(in, "expected_metadata_revision") < 1 {
			return nil, domain.NewError(domain.InvalidArgument, "expected_metadata_revision is required")
		}
		if e = domain.CheckRevision(fileDomain(f), str(f, "current_version_id"), num(in, "expected_metadata_revision")); e != nil {
			return nil, e
		}
		source, e = access.New(s.deps()).Ref(ctx, f)
		if e != nil {
			return nil, e
		}
	}
	parentID := str(in, "target_parent_id")
	if _, ok := in["target_parent_id"]; !ok {
		parentID = str(in, "parent_id")
	}
	name := str(in, "name")
	if name == "" {
		name = str(in, "target_name")
	}
	if kind == "rename" {
		parentID = str(f, "parent_id")
	}
	if kind == "move" && name == "" {
		name = str(f, "name")
	}
	if kind == "restore" {
		if _, ok := in["target_parent_id"]; !ok {
			if _, ok = in["parent_id"]; !ok {
				parentID = str(trash, "original_parent_id")
			}
		}
		if name == "" {
			name = str(trash, "original_name")
		}
	}
	if kind != "delete" {
		name, e = domain.ValidateName(name, s.Config.TrashDir)
		if e != nil {
			return nil, e
		}
		parent, destination, e = access.New(s.deps()).Parent(ctx, a, parentID)
		if e != nil {
			return nil, e
		}
		if f != nil && str(parent, "space_id") != str(f, "space_id") {
			return nil, domain.NewError(domain.AccessDenied, "cross-space moves are not supported")
		}
	}
	cred, e := access.New(s.deps()).Credential(ctx, a, ports.RevisionRef{ResourceRef: source})
	if e != nil {
		return nil, normalizeError(e)
	}
	var retention ports.ResourceRef
	if kind == "delete" {
		retention, e = s.retentionRoot(ctx, a, str(f, "space_id"), cred)
		if e != nil {
			return nil, e
		}
		destination = retention
		name = id()
	}
	// A current native ETag is recorded before mutation, including for directories.
	expectedETag := ""
	if kind != "mkdir" {
		meta, err := s.Original.Stat(ctx, source, cred)
		if err != nil {
			return nil, normalizeError(err)
		}
		if meta.ResourceID != source.ResourceID || meta.SpaceID != source.SpaceID {
			return nil, domain.NewError(domain.DependencyUnavailable, "native identity verification failed")
		}
		expectedETag = meta.ETag
	}
	opID := id()
	newPath := ""
	if kind == "delete" {
		newPath = "/" + strings.Trim(s.Config.TrashDir, "/") + "/" + name
	} else {
		newPath = path.Join(str(parent, "path"), name)
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	spaceID := str(parent, "space_id")
	if f != nil {
		spaceID = str(f, "space_id")
	}
	if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, spaceID); e != nil {
		return nil, e
	}
	// Repeat idempotency after lock, before reserving path or issuing native calls.
	again, err := one(ctx, tx, `SELECT to_jsonb(o) FROM file_operations o WHERE actor_principal_id=$1 AND kind=$2 AND idempotency_key=$3`, a.Principal.ID, kind, key)
	if err == nil {
		if str(again, "request_hash") != requestHash {
			return nil, domain.NewError(domain.IdempotencyConflict, "idempotency key has a different request")
		}
		_ = tx.Rollback(ctx)
		return s.mutationResult(ctx, again)
	}
	if domain.ErrorCode(err) != domain.NotFound {
		return nil, err
	}
	if kind != "delete" {
		if e = s.validateMutationTarget(ctx, tx, a, parentID, spaceID, newPath, fid, num(in, "expected_parent_revision"), kind == "mkdir"); e != nil {
			return nil, e
		}
	}
	if kind == "mkdir" {
		fid = id()
		f, e = one(ctx, tx, `INSERT INTO files(id,owner_id,space_id,parent_id,name,path,kind,native_instance) VALUES($1,$2,$3,$4,$5,$6,'directory',$7) RETURNING to_jsonb(files)`, fid, a.Principal.ID, spaceID, null(parentID), name, newPath, destination.Instance)
		if e != nil {
			return nil, e
		}
	}
	intent := Object{"source": source, "destination_parent": destination, "target_parent_id": parentID, "target_name": name, "target_path": newPath, "old_path": str(f, "path"), "old_name": str(f, "name"), "old_parent_id": str(f, "parent_id"), "expected_etag": expectedETag}
	if kind == "restore" {
		intent["trash_id"] = str(trash, "id")
	}
	if kind == "delete" {
		intent["trash_id"] = name
	}
	expectedRevision := num(in, "expected_metadata_revision")
	if kind == "mkdir" {
		expectedRevision = 1
	}
	op, e := one(ctx, tx, `SELECT to_jsonb(o) FROM begin_file_operation($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb) o`, opID, fid, a.Principal.ID, string(a.Principal.Type), kind, expectedRevision, null(str(f, "current_version_id")), key, requestHash, js(intent))
	if e != nil {
		return nil, e
	}
	if kind == "delete" {
		_, e = tx.Exec(ctx, `INSERT INTO trash_entries(id,root_file_id,operation_id,original_parent_id,original_name,original_path,reserved_location) VALUES($1,$2,$3,$4,$5,$6,$7)`, name, fid, opID, null(str(f, "parent_id")), str(f, "name"), str(f, "path"), newPath)
		if e == nil {
			_, e = tx.Exec(ctx, `UPDATE files SET lifecycle='deleting',updated_at=now() WHERE id=$1`, fid)
		}
	} else if kind == "restore" {
		_, e = tx.Exec(ctx, `UPDATE trash_entries SET state='restoring' WHERE id=$1`, str(trash, "id"))
		if e == nil {
			_, e = tx.Exec(ctx, `UPDATE files SET lifecycle='restoring',updated_at=now() WHERE id=$1`, fid)
		}
	}
	if e != nil {
		return nil, normalizeError(e)
	}
	_, e = tx.Exec(ctx, `UPDATE file_operations SET state='executing',updated_at=now() WHERE id=$1`, opID)
	if e != nil {
		return nil, normalizeError(e)
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, normalizeError(e)
	}
	var meta ports.OriginalMetadata
	if kind == "mkdir" {
		meta, e = s.Original.CreateDirectory(ctx, ports.DirectoryRequest{Parent: destination, Name: name, Credential: cred})
	} else {
		meta, e = s.Original.Move(ctx, ports.MoveRequest{Source: source, DestinationParent: destination, Name: name, ExpectedETag: expectedETag, Credential: cred})
	}
	if e != nil {
		return s.recordMutationFailure(ctx, op, e)
	}
	// Official responses must prove both preserved identity and destination before
	// committing the projection. An ambiguous success is kept for reconciliation.
	if meta.ResourceID == "" || meta.SpaceID != destination.SpaceID || meta.Name != name || (kind != "mkdir" && meta.ResourceID != source.ResourceID) {
		return s.recordMutationFailure(ctx, op, ports.ErrOutcomeUnknown)
	}
	if e = s.verifyRetainedRevisions(ctx, fid, cred); e != nil {
		return s.recordMutationFailure(ctx, op, ports.ErrOutcomeUnknown)
	}
	result := Object{"metadata": meta, "confirmed_at": time.Now().UTC()}
	_, e = s.DB.Exec(ctx, `UPDATE file_operations SET state='remote_confirmed',remote_result=$2::jsonb,updated_at=now() WHERE id=$1 AND state='executing'`, opID, js(result))
	if e != nil {
		return Object{"operation_id": opID, "state": "outcome_unknown"}, normalizeError(e)
	}
	return s.commitMetadataMutation(ctx, opID)
}

func (s *Service) validateMutationTarget(ctx context.Context, tx pgx.Tx, a Actor, parentID, spaceID, targetPath, fileID string, expectedParent int64, requireParentRevision bool) error {
	if parentID != "" {
		p, e := one(ctx, tx, `SELECT to_jsonb(f) FROM files f WHERE id=$1 AND owner_id=$2 AND space_id=$3 FOR UPDATE`, parentID, a.Principal.ID, spaceID)
		if e != nil {
			return e
		}
		if str(p, "kind") != "directory" || str(p, "native_resource_id") == "" {
			return domain.NewError(domain.InvalidPath, "parent is not an available directory")
		}
		if requireParentRevision && expectedParent < 1 {
			return domain.NewError(domain.InvalidArgument, "expected_parent_revision is required")
		}
		if expectedParent > 0 && num(p, "metadata_revision") != expectedParent {
			return domain.NewError(domain.VersionConflict, "parent directory changed")
		}
		var blocked bool
		e = tx.QueryRow(ctx, `WITH RECURSIVE a AS(SELECT id,parent_id,lifecycle,active_operation_id FROM files WHERE id=$1 UNION ALL SELECT f.id,f.parent_id,f.lifecycle,f.active_operation_id FROM files f JOIN a ON f.id=a.parent_id) SELECT EXISTS(SELECT 1 FROM a WHERE lifecycle<>'active' OR active_operation_id IS NOT NULL OR id=$2::uuid)`, parentID, null(fileID)).Scan(&blocked)
		if e != nil {
			return e
		}
		if blocked {
			return domain.NewError(domain.OperationPending, "target ancestry is unavailable, busy, or cyclic")
		}
	}
	var exists bool
	e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM files WHERE space_id=$1 AND path=$2 AND lifecycle IN('active','deleting','restoring') AND id IS DISTINCT FROM $3::uuid UNION ALL SELECT 1 FROM file_operations o JOIN files f ON f.id=o.file_id WHERE f.space_id=$1 AND o.intent->>'target_path'=$2 AND o.state NOT IN('committed','failed_confirmed') AND f.id IS DISTINCT FROM $3::uuid)`, spaceID, targetPath, null(fileID)).Scan(&exists)
	if e != nil {
		return e
	}
	if exists {
		return domain.NewError(domain.PathConflict, "target path is occupied")
	}
	return nil
}

func (s *Service) mutationResult(ctx context.Context, op Object) (any, error) {
	result := Object{"operation_id": str(op, "id"), "file_id": str(op, "file_id"), "state": str(op, "state")}
	if str(op, "state") == "committed" {
		f, e := one(ctx, s.DB, `SELECT to_jsonb(f)-'native_instance'-'native_resource_id'-'active_operation_id' FROM files f WHERE id=$1`, str(op, "file_id"))
		if e == nil {
			result["file"] = f
		}
	}
	if str(op, "error_code") != "" {
		result["error_code"] = str(op, "error_code")
	}
	return result, nil
}

func (s *Service) recordMutationFailure(ctx context.Context, op Object, cause error) (any, error) {
	// Only errors proving non-execution are terminal. All network, decoder, 5xx,
	// cancellation and ambiguous adapter failures retain the active operation gate.
	definite := errors.Is(cause, ports.ErrConflict) || errors.Is(cause, ports.ErrUnauthorized) || errors.Is(cause, ports.ErrInvalid) || errors.Is(cause, ports.ErrUnverified) || errors.Is(cause, ports.ErrNotFound)
	state := "outcome_unknown"
	if definite {
		state = "failed_confirmed"
	}
	code := domain.DependencyUnavailable
	if errors.Is(cause, ports.ErrConflict) {
		code = domain.PathConflict
	}
	if errors.Is(cause, ports.ErrUnauthorized) {
		code = domain.AccessDenied
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	tx, e := s.DB.Begin(persistCtx)
	if e != nil {
		return Object{"operation_id": str(op, "id"), "state": "outcome_unknown"}, normalizeError(e)
	}
	defer tx.Rollback(persistCtx)
	_, e = tx.Exec(persistCtx, `SELECT pg_advisory_xact_lock(hashtextextended(f.space_id::text,0)) FROM files f JOIN file_operations o ON o.file_id=f.id WHERE o.id=$1`, str(op, "id"))
	if e != nil {
		return nil, e
	}
	_, e = tx.Exec(persistCtx, `UPDATE file_operations SET state=$2,error_code=$3,remote_result=$4::jsonb,updated_at=now() WHERE id=$1 AND state='executing'`, str(op, "id"), state, string(code), js(Object{"confirmed_not_executed": definite}))
	if e != nil {
		return nil, normalizeError(e)
	}
	if definite {
		lifecycle := "active"
		if str(op, "kind") == "restore" || str(op, "kind") == "mkdir" {
			lifecycle = "trashed"
		}
		_, e = tx.Exec(persistCtx, `UPDATE files SET lifecycle=$2,active_operation_id=NULL,updated_at=now() WHERE id=$1`, str(op, "file_id"), lifecycle)
		if e != nil {
			return nil, normalizeError(e)
		}
		if str(op, "kind") == "delete" {
			_, e = tx.Exec(persistCtx, `UPDATE trash_entries SET state='failed_confirmed' WHERE operation_id=$1`, str(op, "id"))
		} else if str(op, "kind") == "restore" {
			_, e = tx.Exec(persistCtx, `UPDATE trash_entries SET state='trashed' WHERE root_file_id=$1 AND state='restoring'`, str(op, "file_id"))
		}
		if e != nil {
			return nil, normalizeError(e)
		}
	}
	if e = tx.Commit(persistCtx); e != nil {
		return nil, normalizeError(e)
	}
	if definite {
		return Object{"operation_id": str(op, "id"), "state": state}, domain.NewError(code, "native operation was not completed")
	}
	return Object{"operation_id": str(op, "id"), "state": state, "message": "Remote result is unknown; reconciliation is required before another mutation."}, nil
}

// commitMetadataMutation can be reused by maintenance after it obtains explicit
// native evidence. It never reissues the remote mutation.
func (s *Service) commitMetadataMutation(ctx context.Context, operationID string) (any, error) {
	return s.commitMetadataMutationFenced(ctx, operationID, 0)
}

func (s *Service) commitMetadataMutationFenced(ctx context.Context, operationID string, leaseToken int64) (any, error) {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	_, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(f.space_id::text,0)) FROM files f JOIN file_operations o ON o.file_id=f.id WHERE o.id=$1`, operationID)
	if e != nil {
		return nil, e
	}
	op, e := one(ctx, tx, `SELECT to_jsonb(o) FROM file_operations o WHERE id=$1 FOR UPDATE`, operationID)
	if e != nil {
		return nil, e
	}
	if str(op, "state") == "committed" {
		_ = tx.Rollback(ctx)
		return s.mutationResult(ctx, op)
	}
	if leaseToken > 0 {
		until, parseErr := time.Parse(time.RFC3339Nano, str(op, "lease_until"))
		if parseErr != nil || num(op, "lease_token") != leaseToken || !time.Now().Before(until) {
			return nil, domain.NewError(domain.LeaseLost, "reconciliation lease expired")
		}
	}
	if str(op, "state") != "remote_confirmed" {
		return nil, domain.NewError(domain.OperationPending, "native operation must be confirmed first")
	}
	f, e := one(ctx, tx, `SELECT to_jsonb(f) FROM files f WHERE id=$1 FOR UPDATE`, str(op, "file_id"))
	if e != nil {
		return nil, e
	}
	if str(f, "active_operation_id") != operationID || num(f, "metadata_revision") != num(op, "expected_revision") {
		return nil, domain.NewError(domain.VersionConflict, "final revision comparison failed")
	}
	intent, _ := op["intent"].(map[string]any)
	remote, _ := op["remote_result"].(map[string]any)
	var meta ports.OriginalMetadata
	if e = json.Unmarshal(js(remote["metadata"]), &meta); e != nil || meta.ResourceID == "" {
		return nil, dependency()
	}
	targetPath := str(intent, "target_path")
	targetParent := str(intent, "target_parent_id")
	targetName := str(intent, "target_name")
	lifecycle := "active"
	if str(op, "kind") == "delete" {
		lifecycle = "trashed"
		targetParent = ""
	}
	_, e = tx.Exec(ctx, `UPDATE file_operations SET state='committed',updated_at=now() WHERE id=$1`, operationID)
	if e != nil {
		return nil, normalizeError(e)
	}
	_, e = tx.Exec(ctx, `UPDATE files SET parent_id=$2,name=$3,path=$4,lifecycle=$5,native_resource_id=$6,metadata_revision=metadata_revision+1,active_operation_id=NULL,updated_at=now() WHERE id=$1`, str(f, "id"), null(targetParent), targetName, targetPath, lifecycle, meta.ResourceID)
	if e != nil {
		return nil, normalizeError(e)
	}
	oldPath := str(f, "path")
	// Parent-child identity stays stable. Descendant paths are just projections;
	// replacing a prefix does not re-OCR or create a new source version.
	if str(f, "kind") == "directory" && oldPath != targetPath {
		_, e = tx.Exec(ctx, `WITH RECURSIVE children AS(SELECT id FROM files WHERE parent_id=$1 UNION ALL SELECT f.id FROM files f JOIN children c ON f.parent_id=c.id) UPDATE files SET path=$2||substring(path FROM char_length($3)+1),metadata_revision=metadata_revision+1,updated_at=now() WHERE id IN(SELECT id FROM children)`, str(f, "id"), targetPath, oldPath)
		if e != nil {
			return nil, normalizeError(e)
		}
	}
	switch str(op, "kind") {
	case "delete":
		_, e = tx.Exec(ctx, `UPDATE trash_entries SET state='trashed' WHERE id=$1`, str(intent, "trash_id"))
	case "restore":
		_, e = tx.Exec(ctx, `UPDATE trash_entries SET state='restored',restored_at=now() WHERE id=$1`, str(intent, "trash_id"))
	}
	if e != nil {
		return nil, normalizeError(e)
	}
	_, e = tx.Exec(ctx, `INSERT INTO outbox(event_type,aggregate_id,payload,deduplication_key) VALUES($1,$2,$3::jsonb,$4) ON CONFLICT(deduplication_key) DO NOTHING`, "file."+str(op, "kind"), str(f, "id"), js(Object{"file_id": str(f, "id"), "operation_id": operationID, "path": targetPath}), "operation:"+operationID)
	if e != nil {
		return nil, normalizeError(e)
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, normalizeError(e)
	}
	op["state"] = "committed"
	return s.mutationResult(ctx, op)
}

func (s *Service) retentionRoot(ctx context.Context, a Actor, spaceID string, cred ports.Credential) (ports.ResourceRef, error) {
	var root ports.ResourceRef
	var nativeRoot string
	e := s.DB.QueryRow(ctx, `SELECT native_instance,native_space_id,native_root_resource_id FROM spaces WHERE id=$1 AND owner_id=$2`, spaceID, a.Principal.ID).Scan(&root.Instance, &root.SpaceID, &nativeRoot)
	if e != nil || nativeRoot == "" {
		return root, dependency()
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return root, e
	}
	defer tx.Rollback(ctx)
	_, e = tx.Exec(ctx, `INSERT INTO retention_roots(space_id) VALUES($1) ON CONFLICT DO NOTHING`, spaceID)
	if e != nil {
		return root, e
	}
	record, e := one(ctx, tx, `SELECT to_jsonb(r) FROM retention_roots r WHERE space_id=$1 FOR UPDATE`, spaceID)
	if e != nil {
		return root, e
	}
	switch str(record, "state") {
	case "ready":
		root.ResourceID = str(record, "native_resource_id")
		return root, nil
	case "executing", "outcome_unknown":
		return root, domain.NewError(domain.OperationPending, "retention directory creation needs reconciliation")
	case "prepared":
	default:
		return root, dependency()
	}
	_, e = tx.Exec(ctx, `UPDATE retention_roots SET state='executing',updated_at=now() WHERE space_id=$1`, spaceID)
	if e != nil {
		return root, e
	}
	if e = tx.Commit(ctx); e != nil {
		return root, e
	}
	parent := root
	parent.ResourceID = nativeRoot
	meta, remoteErr := s.Original.CreateDirectory(ctx, ports.DirectoryRequest{Parent: parent, Name: s.Config.TrashDir, Credential: cred})
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if remoteErr != nil || meta.ResourceID == "" || meta.SpaceID != root.SpaceID || meta.Name != s.Config.TrashDir {
		// A configured adapter capability rejection happened before HTTP dispatch and
		// is safe to retry after configuration; conflicts and transport errors are not.
		state := "outcome_unknown"
		if errors.Is(remoteErr, ports.ErrUnverified) || errors.Is(remoteErr, ports.ErrInvalid) || errors.Is(remoteErr, ports.ErrUnauthorized) {
			state = "prepared"
		}
		_, _ = s.DB.Exec(persistCtx, `UPDATE retention_roots SET state=$2,updated_at=now() WHERE space_id=$1 AND state='executing'`, spaceID, state)
		if remoteErr != nil {
			return root, normalizeError(remoteErr)
		}
		return root, domain.NewError(domain.OperationPending, "retention directory identity could not be confirmed")
	}
	_, e = s.DB.Exec(persistCtx, `UPDATE retention_roots SET state='ready',native_resource_id=$2,updated_at=now() WHERE space_id=$1 AND state='executing'`, spaceID, meta.ResourceID)
	if e != nil {
		return root, normalizeError(e)
	}
	root.ResourceID = meta.ResourceID
	return root, nil
}

// childLookup is an optional read-only extension implemented by the official
// oCIS adapter. An absent capability leaves the durable unknown result intact.
type childLookup interface {
	LookupChild(context.Context, ports.ResourceRef, string, ports.Credential) (ports.OriginalMetadata, error)
}

func (s *Service) verifyRetainedRevisions(ctx context.Context, fileID string, cred ports.Credential) error {
	rows, err := s.DB.Query(ctx, `WITH RECURSIVE tree AS(SELECT id FROM files WHERE id=$1 UNION ALL SELECT f.id FROM files f JOIN tree t ON f.parent_id=t.id) SELECT native_revision_ref,size FROM file_versions WHERE file_id IN(SELECT id FROM tree)`, fileID)
	if err != nil {
		return err
	}
	type pin struct {
		ref  ports.RevisionRef
		size int64
	}
	pins := []pin{}
	for rows.Next() {
		var data []byte
		var size int64
		if err = rows.Scan(&data, &size); err != nil {
			rows.Close()
			return err
		}
		var ref ports.RevisionRef
		if err = json.Unmarshal(data, &ref); err != nil {
			rows.Close()
			return err
		}
		if ref.RevisionID == "" || ref.ResourceID == "" {
			rows.Close()
			return ports.ErrIntegrity
		}
		pins = append(pins, pin{ref, size})
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	// The oCIS adapter also supports immutable ETag pins for the current native
	// version, which does not appear in the historical revisions endpoint until
	// overwritten. OpenRevision verifies the returned ETag before exposing bytes.
	for _, p := range pins {
		var byteRange *ports.ByteRange
		if p.size > 0 {
			byteRange = &ports.ByteRange{Start: 0, End: 0}
		}
		content, err := s.Original.OpenRevision(ctx, p.ref, byteRange, cred)
		if err != nil {
			return err
		}
		if content.Body == nil {
			return ports.ErrIntegrity
		}
		if err = content.Body.Close(); err != nil {
			return err
		}
	}
	return nil
}

// ReconcileMutations is maintenance's bounded recovery pass. It never repeats
// MOVE or MKCOL. Fully confirmed database commits are repaired, and uncertain
// MOVE outcomes are adopted only after native identity and historical mappings
// have been verified at the exact stored destination.
func (s *Service) ReconcileMutations(ctx context.Context) error {
	rows, e := many(ctx, s.DB, `WITH candidates AS(SELECT id FROM file_operations WHERE kind IN('mkdir','move','rename','delete','restore') AND (state IN('outcome_unknown','remote_confirmed') OR (state='executing' AND updated_at<now()-interval '5 minutes')) AND (lease_until IS NULL OR lease_until<now()) AND retry_at<=now() ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 20) UPDATE file_operations o SET lease_owner=$1,lease_token=o.lease_token+1,lease_until=now()+interval '2 minutes',retry_at=now()+interval '1 minute' FROM candidates c WHERE o.id=c.id RETURNING to_jsonb(o)`, s.Config.NodeID)
	if e != nil {
		return e
	}
	var firstError error
	for _, op := range rows {
		operationID := str(op, "id")
		fence := num(op, "lease_token")
		if str(op, "state") == "remote_confirmed" {
			if _, err := s.commitMetadataMutationFenced(ctx, operationID, fence); err != nil && firstError == nil {
				firstError = err
			}
			continue
		}
		if str(op, "kind") == "mkdir" {
			continue
		} // Native identity was never learned. Do not adopt an unrelated child.
		lookup, ok := s.Original.(childLookup)
		if !ok {
			continue
		}
		principal, err := one(ctx, s.DB, `SELECT to_jsonb(p) FROM principals p WHERE id=$1 AND status='active'`, str(op, "actor_principal_id"))
		if err != nil {
			continue
		}
		if s.Identity == nil {
			continue
		}
		status, err := s.Identity.SubjectStatus(ctx, str(principal, "issuer"), str(principal, "subject"))
		if err != nil || !status.Enabled {
			continue
		}
		intent, _ := op["intent"].(map[string]any)
		var source, parent ports.ResourceRef
		if json.Unmarshal(js(intent["source"]), &source) != nil || json.Unmarshal(js(intent["destination_parent"]), &parent) != nil || source.ResourceID == "" {
			continue
		}
		cred, err := s.Identity.DelegatedCredential(ctx, ports.DelegationRequest{Subject: ports.Subject{Issuer: str(principal, "issuer"), Subject: str(principal, "subject")}, Audience: s.Config.OCISAudience, Source: ports.RevisionRef{ResourceRef: source}, JobID: operationID})
		if err != nil {
			continue
		}
		meta, err := lookup.LookupChild(ctx, parent, str(intent, "target_name"), cred)
		if err != nil {
			continue
		}
		if meta.ResourceID != source.ResourceID || meta.SpaceID != source.SpaceID || meta.Name != str(intent, "target_name") {
			continue
		}
		if err = s.verifyRetainedRevisions(ctx, str(op, "file_id"), cred); err != nil {
			continue
		}
		result := Object{"metadata": meta, "confirmed_at": time.Now().UTC(), "reconciled": true}
		tag, err := s.DB.Exec(ctx, `UPDATE file_operations SET state='remote_confirmed',remote_result=$3::jsonb,updated_at=now() WHERE id=$1 AND lease_token=$2 AND lease_until>now() AND state IN('executing','outcome_unknown')`, operationID, fence, js(result))
		if err != nil {
			if firstError == nil {
				firstError = normalizeError(err)
			}
			continue
		}
		if tag.RowsAffected() != 1 {
			continue
		}
		if _, err = s.commitMetadataMutationFenced(ctx, operationID, fence); err != nil && firstError == nil {
			firstError = err
		}
	}
	return firstError
}
