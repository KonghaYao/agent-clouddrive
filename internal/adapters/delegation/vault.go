// Package delegation stores only explicitly consented, encrypted offline grants.
// Access tokens are request-local. PostgreSQL serializes refresh-token rotation.
package delegation

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Config struct {
	Base                                     ports.IdentityAdapter
	DB                                       *pgxpool.Pool
	Issuer, TokenURL, ClientID, ClientSecret string
	Key                                      []byte
	Client                                   *http.Client
	Verified                                 bool
	JobCredentialMode                        string
	JobCredentialProvider                    ports.JobCredentialProvider
	WriteCredential                          ports.JobCredentialProvider
	ExternalWriteIssuers                     map[string]bool
	ExternalWriteEnabled                     bool
}
type Wrapper struct {
	cfg    Config
	aead   cipher.AEAD
	client *http.Client
}

func New(cfg Config) (*Wrapper, error) {
	u, e := url.Parse(cfg.TokenURL)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || cfg.Base == nil || cfg.DB == nil || cfg.Issuer == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || len(cfg.Key) != 32 {
		return nil, ports.ErrInvalid
	}
	if cfg.JobCredentialMode == "" {
		cfg.JobCredentialMode = "auto"
	}
	switch cfg.JobCredentialMode {
	case "auto", "delegation", "service", "disabled":
	default:
		return nil, ports.ErrInvalid
	}
	block, e := aes.NewCipher(cfg.Key)
	if e != nil {
		return nil, e
	}
	aead, e := cipher.NewGCM(block)
	if e != nil {
		return nil, e
	}
	cl := &http.Client{Timeout: 30 * time.Second}
	if cfg.Client != nil {
		*cl = *cfg.Client
		if cl.Timeout == 0 {
			cl.Timeout = 30 * time.Second
		}
	}
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg.Key = nil
	return &Wrapper{cfg: cfg, aead: aead, client: cl}, nil
}
func (w *Wrapper) seal(plain, principalID string) ([]byte, error) {
	nonce := make([]byte, w.aead.NonceSize())
	if _, e := rand.Read(nonce); e != nil {
		return nil, e
	}
	out := []byte{1}
	out = append(out, nonce...)
	return w.aead.Seal(out, nonce, []byte(plain), []byte(principalID)), nil
}
func (w *Wrapper) open(data []byte, principalID string) (string, error) {
	n := w.aead.NonceSize()
	if len(data) < 1+n+w.aead.Overhead() || data[0] != 1 {
		return "", ports.ErrIntegrity
	}
	plain, e := w.aead.Open(nil, data[1:1+n], data[1+n:], []byte(principalID))
	if e != nil {
		return "", ports.ErrIntegrity
	}
	return string(plain), nil
}
func (w *Wrapper) ValidateToken(ctx context.Context, raw string, req ports.TokenRequirements) (ports.Subject, error) {
	return w.cfg.Base.ValidateToken(ctx, raw, req)
}
func (w *Wrapper) SubjectStatus(ctx context.Context, issuer, subject string) (ports.SubjectStatus, error) {
	if issuer != w.cfg.Issuer {
		// An externally asserted trust domain has no live IdP here. The local
		// principal registry is the only authoritative state this deployment can
		// verify without consulting the asserting caller again.
		return w.localSubjectStatus(ctx, issuer, subject)
	}
	return w.cfg.Base.SubjectStatus(ctx, issuer, subject)
}
func (w *Wrapper) localSubjectStatus(ctx context.Context, issuer, subject string) (ports.SubjectStatus, error) {
	if issuer == "" || len(issuer) > 512 || subject == "" || len(subject) > 256 || strings.ContainsAny(issuer+subject, "\x00\r\n") {
		return ports.SubjectStatus{}, ports.ErrUnauthorized
	}
	var status string
	e := w.cfg.DB.QueryRow(ctx, `SELECT status FROM principals WHERE issuer=$1 AND subject=$2`, issuer, subject).Scan(&status)
	if errors.Is(e, pgx.ErrNoRows) {
		return ports.SubjectStatus{Enabled: false}, nil
	}
	if e != nil {
		return ports.SubjectStatus{}, e
	}
	return ports.SubjectStatus{Enabled: status == "active"}, nil
}
func (w *Wrapper) IdentityEvents(ctx context.Context, issuer, anchor string) (ports.IdentityEventBatch, error) {
	if issuer != w.cfg.Issuer {
		// This deployment has no authoritative admin-event feed for an external
		// trust domain; the caller must skip it without inventing a gap.
		return ports.IdentityEventBatch{}, ports.ErrUnsupportedIssuer
	}
	source, ok := w.cfg.Base.(ports.IdentityEventSource)
	if !ok {
		return ports.IdentityEventBatch{}, ports.ErrUnverified
	}
	return source.IdentityEvents(ctx, issuer, anchor)
}
func (w *Wrapper) Groups(ctx context.Context, issuer, subject string) ([]string, error) {
	if issuer != w.cfg.Issuer {
		// No group directory exists for an externally asserted trust domain.
		// Returning an empty set denies group-audience shares safely.
		status, e := w.localSubjectStatus(ctx, issuer, subject)
		if e != nil {
			return nil, e
		}
		if !status.Enabled {
			return nil, ports.ErrUnauthorized
		}
		return []string{}, nil
	}
	return w.cfg.Base.Groups(ctx, issuer, subject)
}
func (w *Wrapper) active(ctx context.Context, subject ports.Subject) error {
	if subject.Issuer == "" || subject.Subject == "" {
		return ports.ErrUnauthorized
	}
	if subject.Issuer != w.cfg.Issuer {
		status, e := w.localSubjectStatus(ctx, subject.Issuer, subject.Subject)
		if e != nil {
			return e
		}
		if !status.Enabled {
			return ports.ErrUnauthorized
		}
		return nil
	}
	status, e := w.cfg.Base.SubjectStatus(ctx, subject.Issuer, subject.Subject)
	if e != nil {
		return e
	}
	if !status.Enabled {
		return ports.ErrUnauthorized
	}
	return nil
}
func (w *Wrapper) StoreGrant(ctx context.Context, subject ports.Subject, refresh string, scopes []string) error {
	if !w.cfg.Verified {
		return ports.ErrUnverified
	}
	if e := w.active(ctx, subject); e != nil {
		return e
	}
	if refresh == "" || len(refresh) > 128<<10 || len(scopes) > 128 || !has(scopes, "offline_access") || !has(scopes, "drive.read") {
		return ports.ErrInvalid
	}
	var pid string
	e := w.cfg.DB.QueryRow(ctx, `SELECT id::text FROM principals WHERE issuer=$1 AND subject=$2 AND status='active'`, subject.Issuer, subject.Subject).Scan(&pid)
	if errors.Is(e, pgx.ErrNoRows) {
		return ports.ErrUnauthorized
	}
	if e != nil {
		return e
	}
	encrypted, e := w.seal(refresh, pid)
	if e != nil {
		return e
	}
	_, e = w.cfg.DB.Exec(ctx, `INSERT INTO delegation_grants(principal_id,encrypted_refresh,scopes,state) VALUES($1,$2,$3,'active') ON CONFLICT(principal_id) DO UPDATE SET encrypted_refresh=excluded.encrypted_refresh,scopes=excluded.scopes,state='active',updated_at=now()`, pid, encrypted, scopes)
	return e
}
func (w *Wrapper) HasGrant(ctx context.Context, subject ports.Subject) (bool, error) {
	if !w.cfg.Verified {
		return false, nil
	}
	if e := w.active(ctx, subject); e != nil {
		return false, e
	}
	var exists bool
	e := w.cfg.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM delegation_grants g JOIN principals p ON p.id=g.principal_id WHERE p.issuer=$1 AND p.subject=$2 AND p.status='active' AND g.state='active')`, subject.Issuer, subject.Subject).Scan(&exists)
	return exists, e
}
func (w *Wrapper) Revoke(ctx context.Context, subject ports.Subject) error {
	if subject.Issuer != w.cfg.Issuer || subject.Subject == "" {
		return ports.ErrUnauthorized
	}
	_, e := w.cfg.DB.Exec(ctx, `UPDATE delegation_grants g SET state='revoked',encrypted_refresh=''::bytea,updated_at=now() FROM principals p WHERE p.id=g.principal_id AND p.issuer=$1 AND p.subject=$2`, subject.Issuer, subject.Subject)
	return e
}
func has(scopes []string, v string) bool {
	for _, s := range scopes {
		if s == v {
			return true
		}
	}
	return false
}

// authorizeSource checks database ownership and fixed source identity, not a
// caller-supplied subject assertion alone. It admits only read credentials.
func (w *Wrapper) authorizeSource(ctx context.Context, tx pgx.Tx, in ports.DelegationRequest) error {
	if in.Source.ResourceID == "" || in.Source.SpaceID == "" {
		return ports.ErrUnauthorized
	}
	pin, e := json.Marshal(in.Source)
	if e != nil {
		return ports.ErrInvalid
	}
	var allowed bool
	if in.Audit != nil {
		a := in.Audit
		if in.JobID != "" || a.Purpose != "audit_original" || a.GrantID == "" || a.AuditorPrincipalID == "" || a.FileID == "" || a.VersionID == "" || in.Source.RevisionID == "" || in.Source.ETag == "" {
			return ports.ErrUnauthorized
		}
		var auditor, grantor ports.Subject
		var expiry any
		if !a.ExpiresAt.IsZero() {
			expiry = a.ExpiresAt
		}
		e = tx.QueryRow(ctx, `SELECT auditor.issuer,auditor.subject,issuer.issuer,issuer.subject FROM audit_grants g JOIN files f ON f.id=g.file_id JOIN file_versions v ON v.file_id=f.id JOIN principals owner ON owner.id=f.owner_id JOIN principals auditor ON auditor.id=g.principal_id JOIN principals issuer ON issuer.id=g.granted_by WHERE g.id::text=$1 AND g.principal_id::text=$2 AND g.file_id::text=$3 AND v.id::text=$4 AND v.native_revision_ref=$5::jsonb AND owner.issuer=$6 AND owner.subject=$7 AND owner.status='active' AND auditor.status='active' AND issuer.status='active' AND g.identity_epoch=owner.identity_epoch AND g.auditor_epoch=auditor.identity_epoch AND g.expires_at IS NOT DISTINCT FROM $8::timestamptz AND (g.expires_at IS NULL OR g.expires_at>clock_timestamp()) AND NOT EXISTS(WITH RECURSIVE ancestry AS(SELECT id,parent_id,lifecycle FROM files WHERE id=f.id UNION SELECT parent.id,parent.parent_id,parent.lifecycle FROM files parent JOIN ancestry child ON parent.id=child.parent_id) SELECT 1 FROM ancestry WHERE lifecycle NOT IN('active','trashed')) FOR SHARE OF g,owner,auditor,issuer`, a.GrantID, a.AuditorPrincipalID, a.FileID, a.VersionID, pin, in.Subject.Issuer, in.Subject.Subject, expiry).Scan(&auditor.Issuer, &auditor.Subject, &grantor.Issuer, &grantor.Subject)
		if errors.Is(e, pgx.ErrNoRows) {
			return ports.ErrUnauthorized
		}
		if e != nil {
			return e
		}
		if e = w.active(ctx, grantor); e != nil {
			return e
		}
		return w.active(ctx, auditor)
	} else if in.JobID != "" {
		e = tx.QueryRow(ctx, `WITH RECURSIVE target AS(SELECT f.id FROM conversion_jobs j JOIN file_versions v ON v.id=j.source_version_id JOIN files f ON f.id=v.file_id JOIN principals p ON p.id=f.owner_id WHERE j.id::text=$1 AND j.state='processing' AND j.lease_until>now() AND p.issuer=$2 AND p.subject=$3 AND p.status='active' AND v.native_revision_ref=$4::jsonb), ancestors AS(SELECT f.id,f.parent_id,f.lifecycle FROM files f JOIN target t ON t.id=f.id UNION SELECT f.id,f.parent_id,f.lifecycle FROM files f JOIN ancestors a ON f.id=a.parent_id) SELECT EXISTS(SELECT 1 FROM target) AND NOT EXISTS(SELECT 1 FROM ancestors WHERE lifecycle<>'active')`, in.JobID, in.Subject.Issuer, in.Subject.Subject, pin).Scan(&allowed)
		if e != nil {
			return e
		}
		if allowed {
			return nil
		}
		// Maintenance may inspect an unresolved authorized MOVE using its live
		// operation lease. This exception grants no mutation or arbitrary history.
		e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM file_operations o JOIN files f ON f.id=o.file_id JOIN spaces s ON s.id=f.space_id JOIN principals p ON p.id=f.owner_id WHERE o.id::text=$1 AND o.kind IN('move','rename','delete','restore') AND o.state IN('executing','outcome_unknown','remote_confirmed') AND o.lease_until>now() AND o.actor_principal_id=p.id AND p.issuer=$2 AND p.subject=$3 AND p.status='active' AND f.native_resource_id=$4 AND s.native_space_id=$5 AND f.native_instance=$6)`, in.JobID, in.Subject.Issuer, in.Subject.Subject, in.Source.ResourceID, in.Source.SpaceID, in.Source.Instance).Scan(&allowed)
		if e == nil && !allowed {
			// 仅允许带有效恢复租约的内部保留根任务读取固定空间根；不授权任意文件。
			e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM retention_roots r JOIN spaces s ON s.id=r.space_id JOIN principals p ON p.id=s.owner_id WHERE r.operation_id::text=$1 AND r.state IN('prepared','executing','outcome_unknown') AND r.lease_until>now() AND p.issuer=$2 AND p.subject=$3 AND p.status='active' AND s.native_root_resource_id=$4 AND s.native_space_id=$5 AND s.native_instance=$6)`, in.JobID, in.Subject.Issuer, in.Subject.Subject, in.Source.ResourceID, in.Source.SpaceID, in.Source.Instance).Scan(&allowed)
		}
	} else {
		if in.Source.RevisionID == "" || in.Source.ETag == "" {
			return ports.ErrUnauthorized
		}
		e = tx.QueryRow(ctx, `WITH RECURSIVE target AS(SELECT f.id FROM file_versions v JOIN files f ON f.id=v.file_id JOIN principals p ON p.id=f.owner_id WHERE p.issuer=$1 AND p.subject=$2 AND p.status='active' AND v.native_revision_ref=$3::jsonb), ancestors AS(SELECT f.id,f.parent_id,f.lifecycle FROM files f JOIN target t ON t.id=f.id UNION SELECT f.id,f.parent_id,f.lifecycle FROM files f JOIN ancestors a ON f.id=a.parent_id) SELECT EXISTS(SELECT 1 FROM target) AND NOT EXISTS(SELECT 1 FROM ancestors WHERE lifecycle<>'active')`, in.Subject.Issuer, in.Subject.Subject, pin).Scan(&allowed)
	}
	if e != nil {
		return e
	}
	if !allowed {
		return ports.ErrUnauthorized
	}
	return nil
}
func (w *Wrapper) refresh(ctx context.Context, token string) (string, string, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {w.cfg.ClientID}, "client_secret": {w.cfg.ClientSecret}, "refresh_token": {token}}
	r, e := http.NewRequestWithContext(ctx, "POST", w.cfg.TokenURL, strings.NewReader(form.Encode()))
	if e != nil {
		return "", "", ports.ErrInvalid
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, e := w.client.Do(r)
	if e != nil {
		return "", "", ports.ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		cause := ports.ErrUnavailable
		if resp.StatusCode == 400 || resp.StatusCode == 401 {
			cause = ports.ErrUnauthorized
		}
		return "", "", &ports.DependencyError{Dependency: "identity", Operation: "offline_refresh", Status: resp.StatusCode, Code: "refresh_rejected", Cause: cause}
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if e != nil || len(b) > 1<<20 {
		return "", "", ports.ErrIntegrity
	}
	var out struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Type    string `json:"token_type"`
	}
	if json.Unmarshal(b, &out) != nil || out.Access == "" || !strings.EqualFold(out.Type, "Bearer") {
		return "", "", ports.ErrIntegrity
	}
	if out.Refresh == "" {
		out.Refresh = token
	}
	return out.Access, out.Refresh, nil
}
func (w *Wrapper) DelegatedCredential(ctx context.Context, in ports.DelegationRequest) (ports.Credential, error) {
	if in.SubjectToken != "" {
		if in.Audit != nil {
			return ports.Credential{}, ports.ErrUnauthorized
		}
		return w.cfg.Base.DelegatedCredential(ctx, in)
	}
	if has(in.Scopes, "drive.write") {
		// A write credential is never derived from a grant or the read-only
		// service provider. Any unmet condition fails closed.
		return w.writeServiceCredential(ctx, in)
	}
	mode := w.cfg.JobCredentialMode
	if mode == "" {
		mode = "auto"
	}
	if !w.cfg.Verified || mode == "disabled" {
		return ports.Credential{}, ports.ErrUnverified
	}
	if e := w.active(ctx, in.Subject); e != nil {
		return ports.Credential{}, e
	}
	for _, scope := range in.Scopes {
		if scope != "drive.read" && scope != "openid" {
			return ports.Credential{}, ports.ErrUnauthorized
		}
	}
	in.Scopes = []string{"drive.read"}
	if mode == "auto" || mode == "delegation" {
		cred, e := w.grantCredential(ctx, in)
		// A missing grant is the only condition that may fall through to a
		// service credential. Revoked, disabled or failed grants never do.
		if !errors.Is(e, ports.ErrUnverified) || mode == "delegation" {
			return cred, e
		}
	}
	if mode == "auto" || mode == "service" {
		// Audit reads are a separate capability and must keep the persisted
		// grant requirement; a background service token is not an auditor token.
		if in.Audit != nil {
			return ports.Credential{}, ports.ErrUnverified
		}
		return w.serviceCredential(ctx, in)
	}
	return ports.Credential{}, ports.ErrUnverified
}

// writeServiceCredential issues a write-capable service credential only for an
// allowed external trust domain. Business ACL/ownership checks are performed by
// the caller before reaching this point; this method adds defense-in-depth.
func (w *Wrapper) writeServiceCredential(ctx context.Context, in ports.DelegationRequest) (ports.Credential, error) {
	if in.Audit != nil || in.JobID != "" {
		// Audit reads and background jobs are never write-capable.
		return ports.Credential{}, ports.ErrUnauthorized
	}
	if !w.cfg.ExternalWriteEnabled || !w.cfg.ExternalWriteIssuers[in.Subject.Issuer] || w.cfg.WriteCredential == nil {
		return ports.Credential{}, ports.ErrUnauthorized
	}
	if verifier, ok := w.cfg.WriteCredential.(interface{ Verified() bool }); ok && !verifier.Verified() {
		return ports.Credential{}, ports.ErrUnauthorized
	}
	for _, scope := range in.Scopes {
		if scope != "drive.read" && scope != "drive.write" && scope != "openid" {
			return ports.Credential{}, ports.ErrUnauthorized
		}
	}
	if e := w.active(ctx, in.Subject); e != nil {
		if errors.Is(e, ports.ErrUnauthorized) {
			return ports.Credential{}, ports.ErrUnauthorized
		}
		return ports.Credential{}, e
	}
	cred, e := w.cfg.WriteCredential.JobCredential(ctx, ports.JobCredentialRequest{
		Subject:  in.Subject,
		Audience: in.Audience,
		Source:   in.Source,
		JobID:    "",
		Scopes:   []string{"drive.read", "drive.write"},
	})
	if errors.Is(e, ports.ErrUnverified) {
		return ports.Credential{}, ports.ErrUnauthorized
	}
	return cred, e
}

// grantCredential uses the persisted, consent-based offline refresh grant.
func (w *Wrapper) grantCredential(ctx context.Context, in ports.DelegationRequest) (ports.Credential, error) {
	tx, e := w.cfg.DB.Begin(ctx)
	if e != nil {
		return ports.Credential{}, e
	}
	defer tx.Rollback(ctx)
	var pid, state string
	var encrypted []byte
	var granted []string
	e = tx.QueryRow(ctx, `SELECT p.id::text,g.encrypted_refresh,g.scopes,g.state FROM delegation_grants g JOIN principals p ON p.id=g.principal_id WHERE p.issuer=$1 AND p.subject=$2 AND p.status='active' FOR UPDATE OF g`, in.Subject.Issuer, in.Subject.Subject).Scan(&pid, &encrypted, &granted, &state)
	if errors.Is(e, pgx.ErrNoRows) {
		return ports.Credential{}, ports.ErrUnverified
	}
	if e != nil {
		return ports.Credential{}, e
	}
	if state != "active" || !has(granted, "drive.read") {
		return ports.Credential{}, ports.ErrUnauthorized
	}
	if e = w.authorizeSource(ctx, tx, in); e != nil {
		return ports.Credential{}, e
	}
	refresh, e := w.open(encrypted, pid)
	if e != nil {
		return ports.Credential{}, e
	}
	access, rotated, e := w.refresh(ctx, refresh)
	if e != nil {
		if errors.Is(e, ports.ErrUnauthorized) {
			_, _ = tx.Exec(ctx, `UPDATE delegation_grants SET state='revoked',encrypted_refresh=''::bytea,updated_at=now() WHERE principal_id=$1`, pid)
			_ = tx.Commit(ctx)
		}
		return ports.Credential{}, e
	}
	subject, e := w.cfg.Base.ValidateToken(ctx, access, ports.TokenRequirements{Scopes: []string{"drive.read"}})
	if e != nil || subject.Issuer != in.Subject.Issuer || subject.Subject != in.Subject.Subject {
		_, _ = tx.Exec(ctx, `UPDATE delegation_grants SET state='revoked',encrypted_refresh=''::bytea,updated_at=now() WHERE principal_id=$1`, pid)
		_ = tx.Commit(ctx)
		return ports.Credential{}, ports.ErrUnauthorized
	}
	if in.Audit != nil {
		// refresh/校验期间授权可能到期；发放前再次检查真实时钟。
		if e = w.authorizeSource(ctx, tx, in); e != nil {
			return ports.Credential{}, e
		}
	}
	sealed, e := w.seal(rotated, pid)
	if e != nil {
		return ports.Credential{}, e
	}
	_, e = tx.Exec(ctx, `UPDATE delegation_grants SET encrypted_refresh=$2,updated_at=now() WHERE principal_id=$1 AND state='active'`, pid, sealed)
	if e != nil {
		return ports.Credential{}, e
	}
	if e = tx.Commit(ctx); e != nil {
		return ports.Credential{}, e
	}
	in.Subject = subject
	in.Audit = nil // 能力已在 vault 验证，不向原生 token exchange 传播。
	in.SubjectToken = access
	return w.cfg.Base.DelegatedCredential(ctx, in)
}

// serviceCredential issues a job-scoped service credential only after the same
// source authorization used by the grant path succeeds.
func (w *Wrapper) serviceCredential(ctx context.Context, in ports.DelegationRequest) (ports.Credential, error) {
	if w.cfg.JobCredentialProvider == nil {
		return ports.Credential{}, ports.ErrUnverified
	}
	tx, e := w.cfg.DB.Begin(ctx)
	if e != nil {
		return ports.Credential{}, e
	}
	if e = w.authorizeSource(ctx, tx, in); e != nil {
		_ = tx.Rollback(ctx)
		return ports.Credential{}, e
	}
	_ = tx.Rollback(ctx)
	return w.cfg.JobCredentialProvider.JobCredential(ctx, ports.JobCredentialRequest{
		Subject:  in.Subject,
		Audience: in.Audience,
		Source:   in.Source,
		JobID:    in.JobID,
		Scopes:   in.Scopes,
	})
}

var _ ports.IdentityAdapter = (*Wrapper)(nil)
