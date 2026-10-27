package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/application/access"
	"github.com/agentclouddrive/agent-clouddrive/internal/application/batch"
	"github.com/agentclouddrive/agent-clouddrive/internal/application/core"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

func (s *Service) Execute(ctx context.Context, a Actor, name string, in Object) (any, error) {
	var result any
	var err error
	switch name {
	case "me":
		return s.Me(ctx, a)
	case "delegation_status":
		return s.DelegationStatus(ctx, a)
	case "admin_jobs":
		return s.Admin(ctx, a, "jobs")
	case "admin_feedback":
		return s.Admin(ctx, a, "feedback")
	case "list":
		return s.list(ctx, a, in)
	case "stat":
		var f Object
		f, err = access.New(s.deps()).Owned(ctx, a, str(in, "file_id"), domain.ActionStat)
		shared := false
		// Agents are confined to their own space. Probing share grants would
		// turn a missing file into a dependency error depending on unrelated
		// identity-event configuration, so keep the owner lookup authoritative.
		if err != nil && a.Principal.Type == domain.ActorUser {
			f, _, err = s.SharedFile(ctx, a, str(in, "file_id"), str(in, "version_id"), domain.ActionStat)
			shared = err == nil
		}
		if err == nil {
			if str(f, "native_resource_id") == "" {
				err = denied()
				break
			}
			if shared {
				result, err = s.sharedMetadata(ctx, f, str(f, "preview_version_id"))
			} else {
				result, err = one(ctx, s.DB, `SELECT to_jsonb(f)-'native_instance'-'native_resource_id'-'active_operation_id'||jsonb_build_object('size_bytes',f.size,'conversion',(SELECT jsonb_build_object('status',j.state,'error_code',j.error_code,'completeness',a.completeness) FROM conversion_jobs j LEFT JOIN artifact_sets a ON a.id=j.artifact_set_id WHERE j.source_version_id=f.current_version_id ORDER BY j.generation DESC LIMIT 1)) FROM files f WHERE id=$1`, str(f, "id"))
			}
		}
	case "versions":
		if _, err = access.New(s.deps()).Owned(ctx, a, str(in, "file_id"), domain.ActionReadHistory); err == nil {
			var rows []Object
			rows, err = many(ctx, s.DB, `SELECT (to_jsonb(v)-'native_revision_ref'-'native_etag')||jsonb_build_object('conversion_status',(SELECT j.state FROM conversion_jobs j WHERE j.source_version_id=v.id ORDER BY j.generation DESC LIMIT 1)) FROM file_versions v WHERE file_id=$1 ORDER BY version_seq DESC`, str(in, "file_id"))
			result = Object{"items": rows}
		} else {
			var f Object
			f, _, err = s.SharedFile(ctx, a, str(in, "file_id"), str(in, "version_id"), domain.ActionReadHistory)
			if err == nil {
				vid := str(f, "preview_version_id")
				if vid == "" {
					vid = str(f, "current_version_id")
				}
				var rows []Object
				rows, err = many(ctx, s.DB, `SELECT (to_jsonb(v)-'native_revision_ref'-'native_etag')||jsonb_build_object('conversion_status',(SELECT j.state FROM conversion_jobs j WHERE j.source_version_id=v.id ORDER BY j.generation DESC LIMIT 1)) FROM file_versions v WHERE file_id=$1 AND id=$2`, str(f, "id"), vid)
				result = Object{"items": rows}
			}
		}
	case "search":
		return s.search(ctx, a, in)
	case "markdown":
		return s.markdown(ctx, a, in)
	case "trash":
		if err = access.New(s.deps()).Scope(a, "drive.read"); err == nil {
			var rows []Object
			rows, err = many(ctx, s.DB, `SELECT to_jsonb(t)||jsonb_build_object('file',to_jsonb(f)-'native_resource_id'-'native_instance') FROM trash_entries t JOIN files f ON f.id=t.root_file_id WHERE f.owner_id=$1 AND t.state<>'restored' ORDER BY t.deleted_at DESC`, a.Principal.ID)
			result = Object{"items": rows}
		}
	case "operation":
		var op Object
		op, err = one(ctx, s.DB, `SELECT to_jsonb(o)-'remote_result' FROM file_operations o WHERE id=$1 AND actor_principal_id=$2`, str(in, "operation_id"), a.Principal.ID)
		if err == nil {
			scope := "drive.write"
			if str(op, "kind") == "delete" {
				scope = "drive.delete"
			}
			if str(op, "kind") == "restore" {
				scope = "drive.restore"
			}
			err = access.New(s.deps()).Scope(a, scope)
			result = op
		}
	case "begin_upload":
		return s.BeginUpload(ctx, a, in)
	case "abandon_upload":
		return s.AbandonUpload(ctx, a, str(in, "upload_id"))
	case "finish_upload":
		return s.FinishUpload(ctx, a, str(in, "upload_id"))
	case "mkdir":
		if err = validateParentRevision(in); err != nil {
			return nil, err
		}
		return s.Mutate(ctx, a, name, in)
	case "move", "rename", "delete", "restore":
		return s.Mutate(ctx, a, name, in)
	case "reconvert":
		return s.reconvert(ctx, a, in)
	case "request_share":
		return s.RequestShare(ctx, a, in)
	case "approve_share":
		return s.ApproveShare(ctx, a, str(in, "request_id"))
	case "revoke_share":
		return s.RevokeShare(ctx, a, str(in, "share_id"))
	case "share_requests":
		if err = access.New(s.deps()).Scope(a, "drive.read"); err == nil {
			var rows []Object
			rows, err = many(ctx, s.DB, `SELECT to_jsonb(x) FROM share_approvals x WHERE requester_principal=$1 ORDER BY created_at DESC`, a.Principal.ID)
			result = Object{"items": rows}
		}
	case "share_request":
		if err = access.New(s.deps()).Scope(a, "drive.read"); err != nil {
			break
		}
		result, err = one(ctx, s.DB, `SELECT to_jsonb(x) FROM share_approvals x WHERE id=$1 AND requester_principal=$2`, str(in, "request_id"), a.Principal.ID)
	case "shares":
		if err = access.New(s.deps()).Scope(a, "drive.read"); err == nil {
			var rows []Object
			rows, err = many(ctx, s.DB, `SELECT to_jsonb(x)-'token_hash' FROM share_grants x WHERE creator_principal_id=$1 ORDER BY created_at DESC`, a.Principal.ID)
			result = Object{"items": rows}
		}
	case "shared":
		return s.Shared(ctx, a)
	case "feedback":
		return s.feedback(ctx, a, in)
	case "batch":
		return batch.New(s.deps()).Batch(ctx, a, in, s.Execute)
	case "get_download":
		return s.DownloadTicket(ctx, a, in)
	case "create_text":
		return s.CreateText(ctx, a, in)
	default:
		err = domain.NewError(domain.InvalidArgument, "unknown operation")
	}
	return result, normalizeError(err)
}

// validateParentRevision makes the mandatory non-root mkdir concurrency input
// explicit on every entry point (REST, MCP and batch) instead of letting a
// mistyped value degrade to zero and surface as an unrelated "required" error.
func validateParentRevision(in Object) error {
	raw, ok := in["expected_parent_revision"]
	if !ok {
		return nil
	}
	var revision int64
	switch v := raw.(type) {
	case json.Number:
		n, e := v.Int64()
		if e != nil {
			return invalidParentRevision()
		}
		revision = n
	case int:
		revision = int64(v)
	case int64:
		revision = v
	case float64:
		if v < 1 || v > float64(1<<62) || v != float64(int64(v)) {
			return invalidParentRevision()
		}
		revision = int64(v)
	default:
		return invalidParentRevision()
	}
	if revision < 1 {
		return invalidParentRevision()
	}
	return nil
}

func invalidParentRevision() error {
	return domain.NewError(domain.InvalidArgument, "expected_parent_revision must be an integer >= 1")
}

func (s *Service) search(ctx context.Context, a Actor, in Object) (any, error) {
	if e := access.New(s.deps()).Scope(a, "drive.read"); e != nil {
		return nil, e
	}
	if s.Search == nil {
		return nil, dependency()
	}
	limit := int(num(in, "limit"))
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	offset := 0
	if c := str(in, "cursor"); c != "" {
		b, e := base64.RawURLEncoding.DecodeString(c)
		if e != nil {
			return nil, domain.NewError(domain.InvalidArgument, "invalid cursor")
		}
		offset, _ = strconv.Atoi(string(b))
		if offset < 0 || offset > 1000000 {
			return nil, domain.NewError(domain.InvalidArgument, "invalid cursor")
		}
	}
	out := []Object{}
	more := false
	for scan := 0; scan < 5 && len(out) < limit; scan++ {
		cand, e := s.Search.Query(ctx, ports.SearchQuery{OwnerID: a.Principal.ID, Query: str(in, "query"), Offset: offset, Limit: limit})
		if e != nil {
			return nil, normalizeError(e)
		}
		offset = cand.NextOffset
		more = cand.MayHaveMore
		startOffset := offset - len(cand.Hits)
		for hitIndex, hit := range cand.Hits {
			f, e := access.New(s.deps()).Owned(ctx, a, hit.FileID, domain.ActionSearch)
			if e != nil || str(f, "search_artifact_set_id") != hit.ArtifactSetID {
				continue
			}
			var state string
			_ = s.DB.QueryRow(ctx, `SELECT state FROM conversion_jobs WHERE source_version_id=$1 ORDER BY generation DESC LIMIT 1`, str(f, "current_version_id")).Scan(&state)
			out = append(out, Object{"file_id": hit.FileID, "path": str(f, "path"), "returned_version_id": hit.VersionID, "current_version_id": str(f, "current_version_id"), "artifact_set_id": hit.ArtifactSetID, "stale": hit.VersionID != str(f, "current_version_id"), "newer_version_status": state, "snippet": hit.Content, "completeness": Object{"status": hit.Completeness, "missing_text_pages": hit.MissingPages}, "location": Object{"page": hit.Page, "block": hit.BlockID}})
			if len(out) == limit {
				offset = startOffset + hitIndex + 1
				more = more || hitIndex+1 < len(cand.Hits)
				break
			}
		}
		if !more {
			break
		}
	}
	var cursor any
	if more {
		cursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
	}
	return Object{"items": out, "next_cursor": cursor}, nil
}
func (s *Service) markdown(ctx context.Context, a Actor, in Object) (any, error) {
	fid := str(in, "file_id")
	if fid == "" {
		return nil, domain.NewError(domain.InvalidArgument, "file_id is required")
	}
	explicitArtifact := str(in, "artifact_set_id")
	explicitVersion := str(in, "version_id")

	// A continuation is never allowed to fall back to the current visible
	// pointer. It resumes the exact immutable artifact recorded in the cursor,
	// and any caller-supplied binding must agree with it.
	var bound readCursor
	hasCursor := str(in, "cursor") != ""
	if hasCursor {
		var e error
		bound, e = s.loadReadCursor(ctx, str(in, "cursor"))
		if e != nil {
			return nil, e
		}
		if bound.PrincipalID != a.Principal.ID || bound.FileID != fid {
			return nil, invalidReadCursor()
		}
		if explicitArtifact != "" && explicitArtifact != bound.ArtifactSetID {
			return nil, domain.NewError(domain.InvalidArgument, "cursor does not match artifact_set_id")
		}
		if explicitVersion != "" && explicitVersion != bound.SourceVersionID {
			return nil, domain.NewError(domain.InvalidArgument, "cursor does not match version_id")
		}
		if _, ok := in["offset"]; ok && num(in, "offset") != bound.Offset {
			return nil, domain.NewError(domain.InvalidArgument, "cursor does not match offset")
		}
	}

	// Every page is authorized from scratch. A cursor only selects a pinned
	// artifact; it never carries authority, so scope loss, account disablement
	// and lifecycle changes are re-evaluated here.
	scope := "current"
	if hasCursor {
		scope = bound.Scope
	} else if explicitArtifact != "" || explicitVersion != "" {
		scope = "history"
	}
	action := domain.ActionReadMarkdown
	if scope == "history" {
		action = domain.ActionReadHistory
	}
	f, e := access.New(s.deps()).Owned(ctx, a, fid, action)
	if e != nil {
		return nil, e
	}

	var artifact Object
	if hasCursor {
		artifact, e = one(ctx, s.DB, `SELECT to_jsonb(a) FROM artifact_sets a JOIN file_versions v ON v.id=a.source_version_id WHERE a.id=$1 AND a.source_version_id=$2 AND v.file_id=$3 AND a.published_at IS NOT NULL`, bound.ArtifactSetID, bound.SourceVersionID, fid)
	} else {
		aid := explicitArtifact
		if aid == "" {
			if explicitVersion != "" {
				_ = s.DB.QueryRow(ctx, `SELECT a.id::text FROM artifact_sets a JOIN file_versions v ON v.id=a.source_version_id WHERE v.file_id=$1 AND v.id=$2 AND a.published_at IS NOT NULL ORDER BY a.generation DESC LIMIT 1`, fid, explicitVersion).Scan(&aid)
				if aid == "" {
					return nil, domain.NewError(domain.NotFound, "no published artifact for the requested version")
				}
			} else {
				aid = str(f, "search_artifact_set_id")
				if aid == "" {
					return nil, domain.NewError(domain.NotFound, "no published artifact for this file")
				}
			}
		}
		artifact, e = one(ctx, s.DB, `SELECT to_jsonb(a) FROM artifact_sets a JOIN file_versions v ON v.id=a.source_version_id WHERE a.id=$1 AND v.file_id=$2 AND a.published_at IS NOT NULL`, aid, fid)
		if e == nil && explicitVersion != "" && str(artifact, "source_version_id") != explicitVersion {
			return nil, domain.NewError(domain.InvalidArgument, "artifact_set_id does not belong to version_id")
		}
	}
	if e != nil {
		return nil, e
	}
	versionID := str(artifact, "source_version_id")
	artifactID := str(artifact, "id")
	offset := num(in, "offset")
	if hasCursor {
		offset = bound.Offset
	}
	page, end, total, e := s.readMarkdownPage(ctx, str(artifact, "markdown_key"), offset)
	if e != nil {
		return nil, e
	}
	result := Object{
		"file_id":         str(f, "id"),
		"version_id":      versionID,
		"artifact_set_id": artifactID,
		"completeness":    artifact["completeness"],
		"markdown":        string(page),
		"next_cursor":     nil,
	}
	if end < total {
		cursor, e := s.createReadCursor(ctx, a, fid, versionID, artifactID, scope, end)
		if e != nil {
			return nil, e
		}
		result["next_cursor"] = cursor
	}
	return result, nil
}
func (s *Service) reconvert(ctx context.Context, a Actor, in Object) (any, error) {
	f, e := access.New(s.deps()).Owned(ctx, a, str(in, "file_id"), domain.ActionConvert)
	if e != nil {
		return nil, e
	}
	key := str(in, "idempotency_key")
	if key == "" {
		return nil, domain.NewError(domain.InvalidArgument, "idempotency_key required")
	}
	vid := str(in, "version_id")
	if vid == "" {
		vid = str(f, "current_version_id")
	}
	var belongs bool
	_ = s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM file_versions WHERE id=$1 AND file_id=$2)`, vid, str(f, "id")).Scan(&belongs)
	if !belongs {
		return nil, denied()
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	_, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, a.Principal.ID+":reconvert:"+key)
	if e != nil {
		return nil, e
	}
	var previousHash, jid string
	e = tx.QueryRow(ctx, `SELECT request_hash,job_id::text FROM conversion_requests WHERE principal_id=$1 AND idempotency_key=$2`, a.Principal.ID, key).Scan(&previousHash, &jid)
	requestHash := hash(in)
	if e == nil {
		if previousHash != requestHash {
			return nil, domain.NewError(domain.IdempotencyConflict, "same key with different request")
		}
		return one(ctx, tx, `SELECT to_jsonb(j) FROM conversion_jobs j WHERE id=$1`, jid)
	}
	recipe := s.Recipe()
	r, e := one(ctx, tx, `SELECT to_jsonb(j) FROM request_conversion($1,$2,$3::jsonb) j`, vid, recipe.Fingerprint, js(recipe))
	if e != nil {
		return nil, e
	}
	_, e = tx.Exec(ctx, `INSERT INTO conversion_requests(principal_id,idempotency_key,request_hash,job_id) VALUES($1,$2,$3,$4)`, a.Principal.ID, key, requestHash, str(r, "id"))
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, e
	}
	return r, nil
}

func (s *Service) Recipe() ports.Recipe {
	return core.Recipe(s.Config)
}
func (s *Service) feedback(ctx context.Context, a Actor, in Object) (any, error) {
	if _, e := access.New(s.deps()).Owned(ctx, a, str(in, "file_id"), domain.ActionFeedback); e != nil {
		return nil, e
	}
	body := str(in, "body")
	if len(body) == 0 || len(body) > 10000 {
		return nil, domain.NewError(domain.InvalidArgument, "feedback body must be 1–10000 bytes")
	}
	e := s.submitFeedback(ctx, in)
	return Object{"accepted": e == nil}, normalizeError(e)
}
func (s *Service) DownloadTicket(ctx context.Context, a Actor, in Object) (any, error) {
	f, e := access.New(s.deps()).Owned(ctx, a, str(in, "file_id"), domain.ActionDownload)
	if e != nil {
		return nil, e
	}
	vid := str(in, "version_id")
	if vid == "" {
		vid = str(f, "current_version_id")
	}
	t, h, e := token()
	if e != nil {
		return nil, e
	}
	_, e = s.DB.Exec(ctx, `INSERT INTO access_tickets(token_hash,principal_id,file_id,version_id,scope,expires_at) VALUES($1,$2,$3,$4,'original.read',now()+interval '5 minutes')`, h, a.Principal.ID, str(f, "id"), vid)
	return Object{"url": strings.TrimRight(s.Config.PublicURL, "/") + "/v1/download/" + t, "file_id": str(f, "id"), "version_id": vid, "expires_at": time.Now().UTC().Add(5 * time.Minute)}, normalizeError(e)
}

var _ = json.Marshal
