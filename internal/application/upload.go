package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/agentclouddrive/agent-clouddrive/internal/application/access"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/jackc/pgx/v5"
	"io"
	"strings"
	"time"
)

func (s *Service) BeginUpload(ctx context.Context, a Actor, in Object) (any, error) {
	if e := access.New(s.deps()).Scope(a, "drive.write"); e != nil {
		return nil, e
	}
	if _, ok := in["size_bytes"]; !ok {
		return nil, domain.NewError(domain.InvalidArgument, "size_bytes required")
	}
	for _, key := range []string{"file_id", "parent_id", "name", "content_type", "expected_version_id", "idempotency_key"} {
		if value, exists := in[key]; exists && value != nil {
			if _, ok := value.(string); !ok {
				return nil, domain.NewError(domain.InvalidArgument, key+" must be a string")
			}
		}
	}
	size := num(in, "size_bytes")
	if size < 0 || size > s.Config.UploadMax {
		return nil, domain.NewError(domain.UploadTooLarge, "upload exceeds configured maximum")
	}
	key := str(in, "idempotency_key")
	if !validIdempotencyKey(key) {
		return nil, domain.NewError(domain.InvalidArgument, "idempotency_key is required (maximum 200 characters)")
	}
	name, e := domain.ValidateName(str(in, "name"), s.Config.TrashDir)
	if e != nil {
		return nil, e
	}
	in["name"] = name
	fid := str(in, "file_id")
	kind := "create"
	if fid != "" {
		kind = "overwrite"
	}
	requestHash := hash(in)
	old, e := one(ctx, s.DB, `SELECT to_jsonb(u)||jsonb_build_object('request_hash',o.request_hash,'file_id',o.file_id) FROM file_operations o JOIN upload_sessions u ON u.operation_id=o.id WHERE o.actor_principal_id=$1 AND o.kind=$2 AND o.idempotency_key=$3`, a.Principal.ID, kind, key)
	if e == nil {
		if str(old, "request_hash") != requestHash {
			return nil, domain.NewError(domain.IdempotencyConflict, "same key used for a different request")
		}
		return s.uploadView(old), nil
	}
	p, pref, e := access.New(s.deps()).Parent(ctx, a, str(in, "parent_id"))
	if e != nil {
		return nil, normalizeError(e)
	}
	var f Object
	if fid != "" {
		f, e = access.New(s.deps()).Owned(ctx, a, fid, domain.ActionWrite)
		if e != nil {
			return nil, e
		}
		if str(f, "kind") != "file" {
			return nil, domain.NewError(domain.InvalidArgument, "cannot overwrite a directory")
		}
		if e = domain.CheckRevision(fileDomain(f), str(in, "expected_version_id"), num(in, "expected_metadata_revision")); e != nil {
			return nil, e
		}
		if str(f, "parent_id") != str(in, "parent_id") || str(f, "name") != name {
			return nil, domain.NewError(domain.InvalidArgument, "overwriting cannot move or rename the file")
		}
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, str(p, "space_id")); e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, a.Principal.ID+":"+kind+":"+key); e != nil {
		return nil, e
	}
	existing, existingErr := one(ctx, tx, `SELECT to_jsonb(u)||jsonb_build_object('request_hash',o.request_hash,'file_id',o.file_id) FROM file_operations o JOIN upload_sessions u ON u.operation_id=o.id WHERE o.actor_principal_id=$1 AND o.kind=$2 AND o.idempotency_key=$3`, a.Principal.ID, kind, key)
	if existingErr == nil {
		if str(existing, "request_hash") != requestHash {
			return nil, domain.NewError(domain.IdempotencyConflict, "same key used for a different request")
		}
		return s.uploadView(existing), nil
	}
	if str(in, "parent_id") != "" {
		p, e = one(ctx, tx, `SELECT to_jsonb(f) FROM files f WHERE id=$1 AND owner_id=$2`, str(in, "parent_id"), a.Principal.ID)
		if e != nil {
			return nil, e
		}
	}
	targetPath := strings.TrimRight(str(p, "path"), "/") + "/" + name
	if e = s.validateMutationTarget(ctx, tx, a, str(in, "parent_id"), str(p, "space_id"), targetPath, fid, num(in, "expected_parent_revision"), false); e != nil {
		if domain.ErrorCode(e) == domain.PathConflict {
			return nil, s.uploadPathConflict(ctx, tx, a, str(p, "space_id"), targetPath, fid)
		}
		return nil, e
	}
	if fid == "" {
		fid = id()
		f, e = one(ctx, tx, `INSERT INTO files(id,owner_id,space_id,parent_id,name,path,kind,native_instance,content_type) VALUES($1,$2,$3,$4,$5,$6,'file',$7,$8) RETURNING to_jsonb(files)`, fid, a.Principal.ID, str(p, "space_id"), null(str(in, "parent_id")), name, targetPath, pref.Instance, mediaType(in))
		if e != nil {
			return nil, e
		}
	}
	opid := id()
	op, e := one(ctx, tx, `SELECT to_jsonb(o) FROM begin_file_operation($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb) o`, opid, fid, a.Principal.ID, string(a.Principal.Type), kind, num(f, "metadata_revision"), null(str(f, "current_version_id")), key, requestHash, js(in))
	if e != nil {
		return nil, e
	}
	opid = str(op, "id")
	uid := id()
	up, e := one(ctx, tx, `INSERT INTO upload_sessions(id,operation_id,expected_size,expected_version_id,node_id,expires_at) VALUES($1,$2,$3,$4,$5,now()+interval '24 hours') RETURNING to_jsonb(upload_sessions)`, uid, opid, size, null(str(f, "current_version_id")), s.Config.NodeID)
	if e != nil {
		return nil, e
	}
	var unused []byte
	e = tx.QueryRow(ctx, `SELECT to_jsonb(reserve_upload_quota($1))`, uid).Scan(&unused)
	if e != nil {
		return nil, normalizeError(e)
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, e
	}
	cred, e := access.New(s.deps()).Credential(ctx, a, ports.RevisionRef{})
	if e != nil {
		s.failPrepared(ctx, opid, uid)
		return nil, normalizeError(e)
	}
	started, e := s.DB.Exec(ctx, `UPDATE file_operations SET state='executing',updated_at=now() WHERE id=$1 AND state='prepared'`, opid)
	if e != nil {
		return nil, e
	}
	if started.RowsAffected() != 1 {
		return nil, domain.NewError(domain.OperationPending, "admission expired before native dispatch; use a new idempotency key")
	}
	var etag string
	if str(f, "current_version_id") != "" {
		_ = s.DB.QueryRow(ctx, `SELECT native_etag FROM file_versions WHERE id=$1`, str(f, "current_version_id")).Scan(&etag)
	}
	remote, e := s.Original.CreateUpload(ctx, ports.UploadRequest{OperationID: opid, SpaceID: pref.SpaceID, ParentResourceID: pref.ResourceID, Name: name, Size: size, ExpectedETag: etag, ExistingResourceID: str(f, "native_resource_id"), Credential: cred})
	if e != nil {
		if !(errors.Is(e, ports.ErrConflict) || errors.Is(e, ports.ErrUnauthorized) || errors.Is(e, ports.ErrInvalid) || errors.Is(e, ports.ErrUnverified) || errors.Is(e, ports.ErrNotFound)) {
			_, _ = s.DB.Exec(ctx, `UPDATE file_operations SET state='outcome_unknown',error_code='native_upload_unknown' WHERE id=$1`, opid)
		} else {
			s.failPrepared(ctx, opid, uid)
		}
		return nil, normalizeError(e)
	}
	_, e = s.DB.Exec(ctx, `UPDATE upload_sessions SET native_upload_ref=$2,state='uploading',updated_at=now() WHERE id=$1`, uid, remote.NativeURL)
	if e != nil {
		return nil, e
	}
	up["file_id"] = fid
	up["state"] = "uploading"
	return s.uploadView(up), nil
}

// uploadPathConflict diagnoses an occupied target path without exposing another
// principal's data. Only an owner-scoped placeholder that never received bytes
// is actionable through AbandonUpload; every other blocker is a name conflict.
func (s *Service) uploadPathConflict(ctx context.Context, tx pgx.Tx, a Actor, spaceID, targetPath, fileID string) error {
	conflict := domain.NewError(domain.PathConflict, "target path is occupied")
	blocker, e := one(ctx, tx, `SELECT jsonb_build_object(
		'placeholder', f.native_resource_id IS NULL AND f.current_version_id IS NULL,
		'upload_id', u.id,
		'upload_state', u.state,
		'expected_size', u.expected_size,
		'expires_at', u.expires_at,
		'received_bytes', u.received_bytes,
		'transfer_completed', u.transfer_completed)
		FROM files f
		JOIN file_operations o ON o.id=f.active_operation_id
		JOIN upload_sessions u ON u.operation_id=o.id
		WHERE f.space_id=$1 AND f.path=$2 AND f.lifecycle IN ('active','deleting','restoring')
		  AND f.id IS DISTINCT FROM $3::uuid
		  AND f.owner_id=$4 AND o.actor_principal_id=$4
		ORDER BY f.created_at DESC LIMIT 1`, spaceID, targetPath, null(fileID), a.Principal.ID)
	if e == nil && blocker["placeholder"] == true && blocker["transfer_completed"] == false && num(blocker, "received_bytes") == 0 && (str(blocker, "upload_state") == "created" || str(blocker, "upload_state") == "uploading") {
		conflict.Details = map[string]any{"reason": "incomplete_upload", "upload_id": str(blocker, "upload_id"), "expected_size": num(blocker, "expected_size"), "expires_at": blocker["expires_at"]}
		return conflict
	}
	conflict.Details = map[string]any{"reason": "name_taken"}
	return conflict
}

// AbandonUpload retires an owner-scoped upload that never received bytes. The
// operation must become failed_confirmed before the quota function will release
// the reservation, and only an invisible placeholder file may leave the path.
func (s *Service) AbandonUpload(ctx context.Context, a Actor, uid string) (any, error) {
	if e := access.New(s.deps()).Scope(a, "drive.write"); e != nil {
		return nil, e
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return nil, normalizeError(e)
	}
	defer tx.Rollback(ctx)
	// 空间锁先于 operation/upload 行锁，保持与版本提交相同的锁顺序。
	if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(f.space_id::text,0)) FROM files f JOIN file_operations o ON o.file_id=f.id JOIN upload_sessions u ON u.operation_id=o.id WHERE u.id=$1 AND o.actor_principal_id=$2`, uid, a.Principal.ID); e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, `SELECT 1 FROM file_operations o JOIN upload_sessions u ON u.operation_id=o.id WHERE u.id=$1 AND o.actor_principal_id=$2 FOR UPDATE OF o`, uid, a.Principal.ID); e != nil {
		return nil, e
	}
	u, e := one(ctx, tx, `SELECT to_jsonb(u)||jsonb_build_object('file_id',o.file_id,'op_state',o.state,'leased',o.lease_until>now()) FROM upload_sessions u JOIN file_operations o ON o.id=u.operation_id WHERE u.id=$1 AND o.actor_principal_id=$2 FOR UPDATE OF u`, uid, a.Principal.ID)
	if e != nil {
		return nil, e
	}
	if str(u, "state") != "failed_confirmed" || str(u, "op_state") != "failed_confirmed" {
		if num(u, "in_flight_patches") > 0 || u["legacy_dispatch_unknown"] == true || (num(u, "patch_epoch") > 0 && u["native_termination_confirmed_at"] == nil) || u["leased"] == true {
			if _, e = tx.Exec(ctx, `UPDATE upload_sessions SET abandon_requested_at=COALESCE(abandon_requested_at,now()) WHERE id=$1`, uid); e != nil {
				return nil, e
			}
			if e = tx.Commit(ctx); e != nil {
				return nil, e
			}
			return nil, uploadPending("dispatched_write_unresolved")
		}
		if u["transfer_completed"] == true {
			return nil, abandonRefused("transfer_completed")
		}
		if num(u, "received_bytes") > 0 {
			return nil, abandonRefused("bytes_received")
		}
		if (str(u, "state") != "created" && str(u, "state") != "uploading") || (str(u, "op_state") != "prepared" && str(u, "op_state") != "executing") {
			return nil, abandonRefused("state_not_abandonable")
		}
		if _, e = tx.Exec(ctx, `UPDATE file_operations SET state='failed_confirmed',error_code='abandoned_by_user',updated_at=now() WHERE id=$1 AND state IN('prepared','executing')`, str(u, "operation_id")); e != nil {
			return nil, normalizeError(e)
		}
		if _, e = tx.Exec(ctx, `SELECT release_upload_quota($1)`, uid); e != nil {
			return nil, normalizeError(e)
		}
	}
	if _, e = tx.Exec(ctx, `UPDATE files SET lifecycle='trashed',updated_at=now() WHERE id=$1 AND lifecycle='active' AND kind='file' AND native_resource_id IS NULL AND current_version_id IS NULL`, str(u, "file_id")); e != nil {
		return nil, normalizeError(e)
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, normalizeError(e)
	}
	return Object{"upload_id": uid, "file_id": str(u, "file_id"), "status": "abandoned"}, nil
}

func abandonRefused(reason string) error {
	e := domain.NewError(domain.InvalidTransition, "upload cannot be abandoned after transfer data was accepted")
	e.Details = map[string]any{"reason": reason}
	return e
}

func mediaType(in Object) string {
	m := str(in, "content_type")
	if m == "" {
		return "application/octet-stream"
	}
	return m
}
func (s *Service) uploadView(u Object) Object {
	return Object{"id": str(u, "id"), "file_id": str(u, "file_id"), "operation_id": str(u, "operation_id"), "status": str(u, "state"), "upload_url": strings.TrimRight(s.Config.PublicURL, "/") + "/v1/tus/" + str(u, "id"), "expires_at": u["expires_at"]}
}
func (s *Service) failPrepared(ctx context.Context, opid, uid string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return
	}
	defer tx.Rollback(ctx)
	_, e = tx.Exec(ctx, `UPDATE file_operations SET state='failed_confirmed',error_code='native_rejected',updated_at=now() WHERE id=$1 AND state IN('prepared','executing')`, opid)
	if e != nil {
		return
	}
	_, _ = tx.Exec(ctx, `UPDATE files SET active_operation_id=NULL,lifecycle=CASE WHEN native_resource_id IS NULL THEN 'trashed' ELSE lifecycle END WHERE active_operation_id=$1`, opid)
	_, _ = tx.Exec(ctx, `UPDATE upload_sessions SET state='failed_confirmed' WHERE id=$1`, uid)
	_, _ = tx.Exec(ctx, `SELECT release_upload_quota($1)`, uid)
	_ = tx.Commit(ctx)
}
func (s *Service) UploadNative(ctx context.Context, a Actor, uid string) (Object, ports.Credential, error) {
	if e := access.New(s.deps()).Scope(a, "drive.write"); e != nil {
		return nil, ports.Credential{}, e
	}
	u, e := one(ctx, s.DB, `SELECT to_jsonb(u)||jsonb_build_object('file_id',o.file_id,'op_state',o.state) FROM upload_sessions u JOIN file_operations o ON o.id=u.operation_id WHERE u.id=$1 AND o.actor_principal_id=$2`, uid, a.Principal.ID)
	if e != nil {
		return nil, ports.Credential{}, e
	}
	if str(u, "state") != "uploading" || str(u, "op_state") != "executing" {
		return nil, ports.Credential{}, domain.NewError(domain.OperationPending, "upload is not writable; reconcile its operation")
	}
	if _, e = access.New(s.deps()).Owned(ctx, a, str(u, "file_id"), domain.ActionWrite); e != nil {
		return nil, ports.Credential{}, e
	}
	cred, e := access.New(s.deps()).Credential(ctx, a, ports.RevisionRef{})
	return u, cred, e
}

func (s *Service) FinishUpload(ctx context.Context, a Actor, uid string) (any, error) {
	if e := access.New(s.deps()).Scope(a, "drive.write"); e != nil {
		return nil, e
	}
	u, e := one(ctx, s.DB, `SELECT to_jsonb(o) FROM upload_sessions u JOIN file_operations o ON o.id=u.operation_id WHERE u.id=$1 AND o.actor_principal_id=$2`, uid, a.Principal.ID)
	if e != nil {
		return nil, e
	}
	if str(u, "state") == "committed" {
		return Object{"file_id": str(u, "file_id"), "version_id": str(u, "result_version_id"), "status": "saved"}, nil
	}
	token, e := s.claimUploadFinalize(ctx, a, uid)
	if e != nil {
		return nil, e
	}
	defer s.releaseUploadLease(ctx, uid, token, "finalize_retry")
	return s.finishUpload(ctx, a, uid, token)
}

func (s *Service) finishUpload(ctx context.Context, a Actor, uid string, token int64) (any, error) {
	ctx, stopLease := s.keepUploadLease(ctx, uid, token)
	defer stopLease()
	if e := access.New(s.deps()).Scope(a, "drive.write"); e != nil {
		return nil, e
	}
	u, e := one(ctx, s.DB, `SELECT to_jsonb(u)||jsonb_build_object('file_id',o.file_id,'op_state',o.state,'result_version_id',o.result_version_id,'remote_result',o.remote_result) FROM upload_sessions u JOIN file_operations o ON o.id=u.operation_id WHERE u.id=$1 AND o.actor_principal_id=$2`, uid, a.Principal.ID)
	if e != nil {
		return nil, e
	}
	if str(u, "op_state") == "committed" {
		return Object{"file_id": str(u, "file_id"), "version_id": str(u, "result_version_id"), "status": "saved"}, nil
	}
	f, e := access.New(s.deps()).Owned(ctx, a, str(u, "file_id"), domain.ActionWrite)
	if e != nil {
		return nil, e
	}
	cred, e := access.New(s.deps()).Credential(ctx, a, ports.RevisionRef{})
	if e != nil {
		return nil, normalizeError(e)
	}
	ref, e := access.New(s.deps()).Ref(ctx, f)
	if e != nil {
		return nil, e
	}
	if e = s.uploadTransferForFinalize(ctx, u, token, cred); e != nil {
		return nil, e
	}
	if nativeID := str(u, "native_resource_id"); nativeID != "" {
		ref.ResourceID = nativeID
	}
	_, pref, e := access.New(s.deps()).Parent(ctx, a, str(f, "parent_id"))
	if e != nil {
		return nil, e
	}
	completed := u["transfer_completed"] == true
	var confirmed struct {
		Metadata ports.OriginalMetadata `json:"metadata"`
		Revision ports.RevisionRef      `json:"revision"`
	}
	if str(u, "op_state") == "remote_confirmed" {
		if e = json.Unmarshal(js(u["remote_result"]), &confirmed); e != nil || confirmed.Metadata.ResourceID == "" || confirmed.Metadata.ETag == "" {
			return nil, uploadPending("remote_confirmation_invalid")
		}
		ref.ResourceID = confirmed.Metadata.ResourceID
		completed = true
	}
	rec, e := s.Original.Reconcile(ctx, ports.ReconcileRequest{Upload: ports.UploadInfo{NativeURL: str(u, "native_upload_ref"), OperationID: str(u, "operation_id"), Length: num(u, "expected_size")}, Target: ref, TargetParent: pref, TargetName: str(f, "name"), TransferCompleted: completed, ExpectedSize: num(u, "expected_size"), ExpectedETag: confirmed.Metadata.ETag, NativeRevisionID: confirmed.Revision.RevisionID, Credential: cred})
	if e != nil {
		return nil, normalizeError(e)
	}
	if rec.State != "remote_confirmed" {
		if rec.Reason == "transfer_incomplete" {
			return Object{"operation_id": str(u, "operation_id"), "status": "uploading", "reason": rec.Reason}, nil
		}
		_, _ = s.DB.Exec(ctx, `UPDATE file_operations SET state='outcome_unknown',updated_at=now() WHERE id=$1 AND state='executing' AND lease_token=$2 AND lease_until>now()`, str(u, "operation_id"), token)
		return Object{"operation_id": str(u, "operation_id"), "status": "outcome_unknown", "reason": rec.Reason}, nil
	}
	content, e := s.Original.OpenRevision(ctx, rec.Revision, nil, cred)
	if e != nil {
		return nil, normalizeError(e)
	}
	h := sha256.New()
	n, e := io.Copy(h, io.LimitReader(content.Body, s.Config.UploadMax+1))
	content.Body.Close()
	if e != nil || n != num(u, "expected_size") {
		return nil, domain.NewError(domain.DependencyUnavailable, "native content integrity verification did not complete")
	}
	sha := hex.EncodeToString(h.Sum(nil))
	remote := Object{"metadata": rec.Metadata, "revision": rec.Revision, "sha256": sha}
	v, e := s.commitUploadResult(ctx, u, f, remote, rec, token, n, sha)
	if e != nil {
		return nil, e
	}
	return Object{"file_id": str(f, "id"), "version_id": str(v, "id"), "status": "saved", "conversion_status": "queued"}, nil
}

type originalUploader interface {
	StreamUpload(context.Context, ports.UploadInfo, int64, io.Reader, int64, ports.Credential) (ports.UploadStatus, error)
}

func (s *Service) CreateText(ctx context.Context, a Actor, in Object) (any, error) {
	body := str(in, "content")
	if len(body) > 1<<20 {
		return nil, domain.NewError(domain.UploadTooLarge, "use drive_begin_upload for content larger than 1 MiB")
	}
	req := Object{}
	for k, v := range in {
		if k != "content" {
			req[k] = v
		}
	}
	req["size_bytes"] = int64(len(body))
	req["content_type"] = "text/markdown; charset=utf-8"
	sh := sha256.Sum256([]byte(body))
	req["content_sha256"] = hex.EncodeToString(sh[:])
	out, e := s.BeginUpload(ctx, a, req)
	if e != nil {
		return nil, e
	}
	u := out.(Object)
	if str(u, "status") == "committed" {
		return s.FinishUpload(ctx, a, str(u, "id"))
	}
	native, cred, e := s.UploadNative(ctx, a, str(u, "id"))
	if e != nil {
		return nil, e
	}
	uploader, ok := s.Original.(originalUploader)
	if !ok {
		return nil, dependency()
	}
	info := ports.UploadInfo{NativeURL: str(native, "native_upload_ref"), OperationID: str(native, "operation_id"), Length: int64(len(body))}
	status, e := s.Original.StatUpload(ctx, info, cred)
	if e != nil {
		return s.FinishUpload(ctx, a, str(u, "id"))
	}
	if status.Offset > int64(len(body)) {
		return nil, dependency()
	}
	if status.Offset < int64(len(body)) || len(body) == 0 {
		epoch, err := s.AdmitUploadPatch(ctx, a, str(u, "id"), status.Offset, int64(len(body))-status.Offset)
		if err != nil {
			return nil, err
		}
		status, e = uploader.StreamUpload(ctx, info, status.Offset, strings.NewReader(body[status.Offset:]), int64(len(body))-status.Offset, cred)
		if e = s.CompleteUploadPatch(ctx, str(u, "id"), epoch, status, e); e != nil {
			return nil, e
		}
	}
	return s.FinishUpload(ctx, a, str(u, "id"))
}
