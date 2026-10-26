package application

import (
	"context"
	"encoding/json"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"strings"
	"time"
)

func (s *Service) RequestShare(ctx context.Context, a Actor, in Object) (any, error) {
	f, e := s.owned(ctx, a, str(in, "file_id"), domain.ActionRequestShare)
	if e != nil {
		return nil, e
	}
	r := domain.ShareRequest{}
	if e = json.Unmarshal(js(in), &r); e != nil {
		return nil, domain.NewError(domain.InvalidArgument, "invalid share")
	}
	r.OwnerID = a.Principal.ID
	if r.VersionPolicy == "" || r.VersionPolicy == "current" {
		r.VersionPolicy = domain.VersionLatest
	}
	if len(r.Rights) == 0 {
		if r.AudienceType == domain.AudienceLink {
			r.Rights = []domain.ShareRight{domain.RightPreview}
		} else {
			r.Rights = []domain.ShareRight{domain.RightRead, domain.RightDownload}
		}
	}
	if e = domain.ValidateShareRequest(r, fileDomain(f), time.Now()); e != nil {
		return nil, e
	}
	if r.VersionID != "" {
		var ok bool
		_ = s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM file_versions WHERE id=$1 AND file_id=$2)`, r.VersionID, r.FileID).Scan(&ok)
		if !ok {
			return nil, denied()
		}
	}
	out, e := one(ctx, s.DB, `INSERT INTO share_approvals(requester_principal,file_id,request_hash,request_body,expires_at) VALUES($1,$2,$3,$4::jsonb,now()+interval '15 minutes') RETURNING to_jsonb(share_approvals)`, a.Principal.ID, r.FileID, hash(r), js(r))
	if e != nil {
		return nil, e
	}
	out["confirmation_url"] = strings.TrimRight(s.Config.PublicURL, "/") + "/?confirm_share=" + str(out, "id")
	return out, nil
}
func (s *Service) ApproveShare(ctx context.Context, a Actor, rid string) (any, error) {
	if a.Principal.Type != domain.ActorUser || !a.Interactive {
		return nil, domain.NewError(domain.ConfirmationRequired, "sharing requires the user's interactive confirmation")
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	if e = s.scope(a, "drive.share.request"); e != nil {
		return nil, e
	}
	_, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(f.space_id::text,0)) FROM share_approvals r JOIN files f ON f.id=r.file_id WHERE r.id=$1 AND r.requester_principal=$2`, rid, a.Principal.ID)
	if e != nil {
		return nil, e
	}
	var enabledOwner string
	if e = tx.QueryRow(ctx, `SELECT id::text FROM principals WHERE id=$1 AND status='active' FOR SHARE`, a.Principal.ID).Scan(&enabledOwner); e != nil {
		return nil, denied()
	}
	row, e := one(ctx, tx, `SELECT to_jsonb(r) FROM share_approvals r WHERE id=$1 AND requester_principal=$2 FOR UPDATE`, rid, a.Principal.ID)
	if e != nil {
		return nil, e
	}
	if str(row, "state") != "pending" {
		return nil, domain.NewError(domain.ConfirmationExpired, "confirmation is no longer pending")
	}
	expiry, _ := time.Parse(time.RFC3339Nano, str(row, "expires_at"))
	if !expiry.After(time.Now()) {
		return nil, domain.NewError(domain.ConfirmationExpired, "confirmation expired")
	}
	var r domain.ShareRequest
	if e = json.Unmarshal(js(row["request_body"]), &r); e != nil {
		return nil, e
	}
	if hash(r) != str(row, "request_hash") {
		return nil, domain.NewError(domain.IdempotencyConflict, "confirmation details changed")
	}
	f, e := one(ctx, tx, `SELECT to_jsonb(f) FROM files f WHERE f.id=$1 AND f.owner_id=$2 AND file_is_active(f.id) FOR UPDATE`, r.FileID, a.Principal.ID)
	if e != nil {
		return nil, e
	}
	if e = domain.ValidateShareRequest(r, fileDomain(f), time.Now()); e != nil {
		return nil, e
	}
	var raw string
	var th []byte
	if r.AudienceType == domain.AudienceLink {
		raw, th, e = token()
		if e != nil {
			return nil, e
		}
	}
	rights := []string{}
	for _, x := range r.Rights {
		rights = append(rights, string(x))
	}
	var expires any
	if !r.ExpiresAt.IsZero() {
		expires = r.ExpiresAt
	}
	_, e = tx.Exec(ctx, `UPDATE share_approvals SET state='approved',approved_by=$2,decided_at=now() WHERE id=$1`, rid, a.Principal.ID)
	if e != nil {
		return nil, e
	}
	grant, e := one(ctx, tx, `INSERT INTO share_grants(approval_id,file_id,creator_principal_id,audience_type,audience_id,token_hash,rights,version_policy,version_id,approved_by,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$3,$10) RETURNING to_jsonb(share_grants)-'token_hash'`, rid, r.FileID, a.Principal.ID, string(r.AudienceType), null(r.AudienceID), th, rights, string(r.VersionPolicy), null(r.VersionID), expires)
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, e
	}
	if raw != "" {
		grant["url"] = strings.TrimRight(s.Config.PublicURL, "/") + "/s/" + raw
	}
	return grant, nil
}
func (s *Service) RevokeShare(ctx context.Context, a Actor, sid string) (any, error) {
	if e := s.scope(a, "drive.share.request"); e != nil {
		return nil, e
	}
	r, e := s.DB.Exec(ctx, `UPDATE share_grants SET state='revoked',revoked_at=now() WHERE id=$1 AND creator_principal_id=$2 AND state='active'`, sid, a.Principal.ID)
	if e != nil {
		return nil, e
	}
	if r.RowsAffected() == 0 {
		return nil, denied()
	}
	return Object{"revoked": true}, nil
}
func (s *Service) Shared(ctx context.Context, a Actor) (any, error) {
	if a.Principal.Type != domain.ActorUser {
		return nil, domain.NewError(domain.AccessDenied, "Agent access excludes received shares")
	}
	if e := s.scope(a, "drive.read"); e != nil {
		return nil, e
	}
	groups, e := s.Identity.Groups(ctx, a.Subject.Issuer, a.Subject.Subject)
	if e != nil {
		return nil, dependency()
	}
	rows, e := many(ctx, s.DB, `SELECT to_jsonb(g)-'token_hash'||jsonb_build_object('file',to_jsonb(f)-'native_resource_id'-'native_instance','issuer',p.issuer,'subject',p.subject) FROM share_grants g JOIN files f ON f.id=g.file_id JOIN principals p ON p.id=f.owner_id WHERE g.state='active' AND (g.expires_at IS NULL OR g.expires_at>now()) AND f.lifecycle='active' AND file_is_active(f.id) AND p.status='active' AND ((g.audience_type='user' AND g.audience_id=$1) OR (g.audience_type='group' AND g.audience_id=ANY($2::text[])))`, a.Principal.ID, groups)
	if e != nil {
		return nil, e
	}
	out := []Object{}
	for _, r := range rows {
		status, e := s.Identity.SubjectStatus(ctx, str(r, "issuer"), str(r, "subject"))
		if e != nil {
			return nil, dependency()
		}
		if !status.Enabled {
			s.observeDisabled(ctx, ports.Subject{Issuer: str(r, "issuer"), Subject: str(r, "subject")})
		}
		if status.Enabled {
			fm, _ := r["file"].(map[string]any)
			vid := str(fm, "current_version_id")
			if str(r, "version_policy") == string(domain.VersionFixed) {
				vid = str(r, "version_id")
			}
			projected, pe := s.sharedMetadata(ctx, fm, vid)
			if pe != nil {
				continue
			}
			r["file"] = projected
			delete(r, "issuer")
			delete(r, "subject")
			out = append(out, r)
		}
	}
	return Object{"items": out}, nil
}
func (s *Service) SharedFile(ctx context.Context, a Actor, fid, vid string, action domain.Action) (Object, ports.Subject, error) {
	if a.Principal.Type != domain.ActorUser {
		return nil, ports.Subject{}, denied()
	}
	if e := s.scope(a, "drive.read"); e != nil {
		return nil, ports.Subject{}, e
	}
	groups, e := s.Identity.Groups(ctx, a.Subject.Issuer, a.Subject.Subject)
	if e != nil {
		return nil, ports.Subject{}, dependency()
	}
	rows, e := many(ctx, s.DB, `SELECT to_jsonb(g)-'token_hash'||jsonb_build_object('file',to_jsonb(f),'owner_issuer',p.issuer,'owner_subject',p.subject) FROM share_grants g JOIN files f ON f.id=g.file_id JOIN principals p ON p.id=f.owner_id WHERE g.file_id=$1 AND g.state='active' AND (g.expires_at IS NULL OR g.expires_at>now()) AND file_is_active(f.id) AND p.status='active' AND ((g.audience_type='user' AND g.audience_id=$2) OR (g.audience_type='group' AND g.audience_id=ANY($3::text[])))`, fid, a.Principal.ID, groups)
	if e != nil {
		return nil, ports.Subject{}, e
	}
	for _, row := range rows {
		var grant domain.ShareGrant
		if e = json.Unmarshal(js(row), &grant); e != nil {
			continue
		}
		fm, _ := row["file"].(map[string]any)
		status, e := s.Identity.SubjectStatus(ctx, str(row, "owner_issuer"), str(row, "owner_subject"))
		if e != nil {
			return nil, ports.Subject{}, dependency()
		}
		if !status.Enabled {
			s.observeDisabled(ctx, ports.Subject{Issuer: str(row, "owner_issuer"), Subject: str(row, "owner_subject")})
		}
		a.Principal.Groups = groups
		requestedVersion := vid
		if requestedVersion == "" && grant.VersionPolicy == domain.VersionFixed {
			requestedVersion = grant.VersionID
		}
		er := domain.Authorize(domain.AccessRequest{Principal: a.Principal, File: fileDomain(fm), Action: action, VersionID: requestedVersion, Share: &grant, OwnerActive: status.Enabled, OwnerStatusVerified: true, Now: time.Now()})
		if er == nil {
			fm["preview_version_id"] = requestedVersion
			if requestedVersion == "" {
				fm["preview_version_id"] = str(fm, "current_version_id")
			}
			return fm, ports.Subject{Issuer: str(row, "owner_issuer"), Subject: str(row, "owner_subject")}, nil
		}
	}
	return nil, ports.Subject{}, denied()
}

// Shared metadata is projected from the exact authorized version, never from a
// later unshared source or its search artifact pointer.
func (s *Service) sharedMetadata(ctx context.Context, f Object, vid string) (Object, error) {
	v, e := one(ctx, s.DB, `SELECT to_jsonb(v)-'native_revision_ref'-'native_etag' FROM file_versions v WHERE id=$1 AND file_id=$2`, vid, str(f, "id"))
	if e != nil {
		return nil, e
	}
	out := Object{}
	for _, k := range []string{"id", "owner_id", "space_id", "parent_id", "name", "path", "kind", "lifecycle"} {
		out[k] = f[k]
	}
	out["current_version_id"] = vid
	out["preview_version_id"] = vid
	out["size"] = v["size"]
	out["size_bytes"] = v["size"]
	out["content_type"] = v["content_type"]
	return out, nil
}
