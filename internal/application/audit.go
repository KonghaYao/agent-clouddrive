package application

import (
	"context"
	"encoding/json"
	"github.com/agentclouddrive/agent-clouddrive/internal/application/access"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"strings"
	"time"
)

// Audit grants are provisioned by an authorized deployment operator, not by an
// ordinary admin role, Agent tool, share link, or received-share capability.
func (s *Service) auditFile(ctx context.Context, a Actor, fid string) (Object, ports.Subject, error) {
	if a.Principal.Type != domain.ActorUser || !a.Principal.Active || !a.Principal.StatusVerified {
		return nil, ports.Subject{}, denied()
	}
	f, e := one(ctx, s.DB, `SELECT to_jsonb(f)||jsonb_build_object('audit_expires_at',g.expires_at,'audit_grant_id',g.id,'audit_granted_by',g.granted_by,'owner_issuer',p.issuer,'owner_subject',p.subject) FROM audit_grants g JOIN files f ON f.id=g.file_id JOIN principals p ON p.id=f.owner_id JOIN principals auditor ON auditor.id=g.principal_id WHERE g.principal_id=$1 AND g.file_id=$2 AND auditor.status='active' AND g.auditor_epoch=auditor.identity_epoch AND (g.expires_at IS NULL OR g.expires_at>now())`, a.Principal.ID, fid)
	if e != nil {
		return nil, ports.Subject{}, denied()
	}
	if e = domain.Authorize(domain.AccessRequest{Principal: a.Principal, File: fileDomain(f), Action: domain.ActionAuditRead, ExplicitAudit: true}); e != nil {
		return nil, ports.Subject{}, e
	}
	return f, ports.Subject{Issuer: str(f, "owner_issuer"), Subject: str(f, "owner_subject")}, nil
}
func (s *Service) recordAuditRead(ctx context.Context, a Actor, f Object, vid, aid, kind string) error {
	_, e := s.DB.Exec(ctx, `INSERT INTO audit_reads(principal_id,file_id,version_id,artifact_set_id,granted_by,read_kind) VALUES($1,$2,$3,$4,$5,$6)`, a.Principal.ID, str(f, "id"), null(vid), null(aid), str(f, "audit_granted_by"), kind)
	return normalizeError(e)
}
func (s *Service) AuditVersions(ctx context.Context, a Actor, fid string) (any, error) {
	f, _, e := s.auditFile(ctx, a, fid)
	if e != nil {
		return nil, e
	}
	rows, e := many(ctx, s.DB, `SELECT to_jsonb(v)-'native_revision_ref'-'native_etag' FROM file_versions v WHERE file_id=$1 ORDER BY version_seq DESC`, fid)
	if e != nil {
		return nil, e
	}
	if e = s.recordAuditRead(ctx, a, f, "", "", "versions"); e != nil {
		return nil, e
	}
	return Object{"file_id": fid, "items": rows}, nil
}
func (s *Service) AuditContent(ctx context.Context, a Actor, fid, vid string, rng *ports.ByteRange) (ports.Content, error) {
	if vid == "" {
		return ports.Content{}, domain.NewError(domain.InvalidArgument, "audit requires an explicit immutable version_id")
	}
	f, owner, e := s.auditFile(ctx, a, fid)
	if e != nil {
		return ports.Content{}, e
	}
	// No admin impersonation: native reads still require the owner's lawful,
	// previously consented delegation. Revoked or disabled grants fail closed.
	expires, _ := time.Parse(time.RFC3339Nano, str(f, "audit_expires_at"))
	c, e := access.New(s.deps()).AuditOriginalVersion(ctx, owner, ports.AuditCapability{AuditorPrincipalID: a.Principal.ID, GrantID: str(f, "audit_grant_id"), FileID: fid, VersionID: vid, Purpose: "audit_original", ExpiresAt: expires}, rng)
	if e != nil {
		return ports.Content{}, normalizeError(e)
	}
	if e = s.recordAuditRead(ctx, a, f, vid, "", "original"); e != nil {
		c.Body.Close()
		return ports.Content{}, e
	}
	return c, nil
}
func (s *Service) AuditMarkdown(ctx context.Context, a Actor, fid, aid string) (ports.Content, error) {
	if aid == "" {
		return ports.Content{}, domain.NewError(domain.InvalidArgument, "audit requires an explicit immutable artifact_set_id")
	}
	f, _, e := s.auditFile(ctx, a, fid)
	if e != nil {
		return ports.Content{}, e
	}
	artifact, e := one(ctx, s.DB, `SELECT to_jsonb(a) FROM artifact_sets a JOIN file_versions v ON v.id=a.source_version_id WHERE a.id=$1 AND v.file_id=$2 AND a.published_at IS NOT NULL`, aid, fid)
	if e != nil {
		return ports.Content{}, e
	}
	c, e := s.Artifacts.Open(ctx, str(artifact, "markdown_key"), nil)
	if e != nil {
		return ports.Content{}, normalizeError(e)
	}
	if e = s.recordAuditRead(ctx, a, f, str(artifact, "source_version_id"), aid, "markdown"); e != nil {
		c.Body.Close()
		return ports.Content{}, e
	}
	c.ContentType = "text/plain; charset=utf-8"
	return c, nil
}

func (s *Service) AuditAsset(ctx context.Context, a Actor, fid, aid, asset string) (ports.Content, error) {
	if aid == "" || asset == "" || strings.ContainsAny(asset, "/\\") {
		return ports.Content{}, denied()
	}
	f, _, e := s.auditFile(ctx, a, fid)
	if e != nil {
		return ports.Content{}, e
	}
	ar, e := one(ctx, s.DB, `SELECT to_jsonb(a) FROM artifact_sets a JOIN file_versions v ON v.id=a.source_version_id WHERE a.id=$1 AND v.file_id=$2 AND a.published_at IS NOT NULL`, aid, fid)
	if e != nil {
		return ports.Content{}, e
	}
	manifest, e := s.Artifacts.Open(ctx, str(ar, "manifest_key"), nil)
	if e != nil {
		return ports.Content{}, normalizeError(e)
	}
	b, e := readBounded(manifest.Body, 4<<20)
	manifest.Body.Close()
	if e != nil {
		return ports.Content{}, normalizeError(e)
	}
	var m struct{ Assets []struct{ ID, Key string } }
	if e = json.Unmarshal(b, &m); e != nil {
		return ports.Content{}, dependency()
	}
	for _, entry := range m.Assets {
		if entry.ID == asset {
			c, e := s.Artifacts.Open(ctx, entry.Key, nil)
			if e != nil {
				return ports.Content{}, normalizeError(e)
			}
			if e = s.recordAuditRead(ctx, a, f, str(ar, "source_version_id"), aid, "asset"); e != nil {
				c.Body.Close()
				return ports.Content{}, e
			}
			return c, nil
		}
	}
	return ports.Content{}, denied()
}
