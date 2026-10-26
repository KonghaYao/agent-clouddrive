package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agentclouddrive/agent-clouddrive/internal/config"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Actor struct {
	Principal   domain.Principal
	Subject     ports.Subject
	Token       string
	Interactive bool
}
type Service struct {
	DB           *pgxpool.Pool
	Config       config.Config
	Original     ports.OriginalStore
	Artifacts    ports.ArtifactStore
	Identity     ports.IdentityAdapter
	Search       ports.SearchIndex
	Converter    ports.Converter
	Preview      ports.Converter
	Delegation   *DelegationConfig
	feedbackRate anonymousRate
}
type Object = map[string]any

func id() string {
	v, e := domain.NewID()
	if e != nil {
		panic(e)
	}
	return v
}
func token() (string, []byte, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", nil, e
	}
	s := base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(s))
	return s, h[:], nil
}
func digest(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

// validIdempotencyKey mirrors the file_operations CHECK length(): PostgreSQL
// length() counts code points, so keys are bounded by runes, not bytes.
func validIdempotencyKey(key string) bool {
	return key != "" && utf8.RuneCountInString(key) <= 200
}
func hash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func js(v any) []byte               { b, _ := json.Marshal(v); return b }
func str(m Object, k string) string { v, _ := m[k].(string); return v }
func num(m Object, k string) int64 {
	switch v := m[k].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, e := v.Int64()
		if e != nil {
			return -1
		}
		return n
	}
	return 0
}
func null(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func denied() error { return domain.NewError(domain.NotFound, "resource not found") }
func dependency() error {
	return domain.NewError(domain.DependencyUnavailable, "required integration is unavailable or has not passed its compatibility gate")
}
func normalizeError(err error) error {
	if err == nil {
		return nil
	}
	var de *domain.Error
	if errors.As(err, &de) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, ports.ErrNotFound) {
		return denied()
	}
	if errors.Is(err, ports.ErrUnauthorized) {
		return domain.NewError(domain.AccessDenied, "access denied")
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		for _, c := range []domain.Code{domain.VersionConflict, domain.PathConflict, domain.OperationPending, domain.IdempotencyConflict, domain.QuotaExceeded, domain.LeaseLost, domain.AccessDenied, domain.InvalidArgument} {
			if strings.Contains(pe.Message, string(c)) {
				return domain.NewError(c, string(c))
			}
		}
		if pe.Code == "23505" {
			return domain.NewError(domain.PathConflict, "target already exists")
		}
		if pe.Code == "22P02" || pe.Code == "23514" {
			return domain.NewError(domain.InvalidArgument, "invalid request")
		}
	}
	return dependency()
}
func Error(err error) error { return normalizeError(err) }

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func one(ctx context.Context, q querier, sql string, args ...any) (Object, error) {
	var b []byte
	e := q.QueryRow(ctx, sql, args...).Scan(&b)
	if e != nil {
		return nil, normalizeError(e)
	}
	var m Object
	e = json.Unmarshal(b, &m)
	return m, e
}
func many(ctx context.Context, q querier, sql string, args ...any) ([]Object, error) {
	rows, e := q.Query(ctx, sql, args...)
	if e != nil {
		return nil, normalizeError(e)
	}
	defer rows.Close()
	out := []Object{}
	for rows.Next() {
		var b []byte
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		var m Object
		if e = json.Unmarshal(b, &m); e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Service) Authenticate(ctx context.Context, bearer string, agent bool) (Actor, error) {
	return s.authenticate(ctx, bearer, agent, false)
}
func (s *Service) AuthenticateReadOnly(ctx context.Context, bearer string, agent bool) (Actor, error) {
	return s.authenticate(ctx, bearer, agent, true)
}
func (s *Service) authenticate(ctx context.Context, bearer string, agent, readOnly bool) (Actor, error) {
	var a Actor
	if s.Identity == nil || bearer == "" {
		return a, domain.NewError(domain.Unauthenticated, "valid bearer token required")
	}
	sub, e := s.Identity.ValidateToken(ctx, bearer, ports.TokenRequirements{Resource: s.Config.MCPURL})
	if e != nil {
		if errors.Is(e, ports.ErrDisabled) {
			if !readOnly {
				s.observeDisabled(ctx, sub)
			}
			return a, domain.NewError(domain.AccessDenied, "account disabled")
		}
		if errors.Is(e, ports.ErrUnavailable) || errors.Is(e, ports.ErrUnverified) {
			return a, dependency()
		}
		return a, domain.NewError(domain.Unauthenticated, "invalid token")
	}
	status, e := s.Identity.SubjectStatus(ctx, sub.Issuer, sub.Subject)
	if e != nil {
		return a, dependency()
	}
	if !status.Enabled {
		if !readOnly {
			_, _ = s.DB.Exec(ctx, `UPDATE principals SET status='disabled',disabled_reason='identity',updated_at=now() WHERE issuer=$1 AND subject=$2`, sub.Issuer, sub.Subject)
			_, _ = s.DB.Exec(ctx, `UPDATE share_grants SET state='disabled',disabled_reason='account_disabled' WHERE creator_principal_id IN(SELECT id FROM principals WHERE issuer=$1 AND subject=$2) AND state='active'`, sub.Issuer, sub.Subject)
		}
		return a, domain.NewError(domain.AccessDenied, "account disabled")
	}
	var pid, local string
	e = s.DB.QueryRow(ctx, `SELECT id::text,status FROM principals WHERE issuer=$1 AND subject=$2`, sub.Issuer, sub.Subject).Scan(&pid, &local)
	if e != nil {
		if !errors.Is(e, pgx.ErrNoRows) || readOnly {
			return a, domain.NewError(domain.Unauthenticated, "initialize your personal space before submitting feedback")
		}
		e = s.DB.QueryRow(ctx, `INSERT INTO principals(issuer,subject) VALUES($1,$2) ON CONFLICT(issuer,subject) DO UPDATE SET issuer=excluded.issuer RETURNING id::text,status`, sub.Issuer, sub.Subject).Scan(&pid, &local)
		if e != nil {
			return a, normalizeError(e)
		}
	}
	if !readOnly && local == "disabled" {
		var reenabled string
		e = s.DB.QueryRow(ctx, `UPDATE principals SET status='active',disabled_reason=NULL,updated_at=now() WHERE id=$1 AND disabled_reason='identity' RETURNING status`, pid).Scan(&reenabled)
		if e == nil {
			local = reenabled
		}
	}
	if local != "active" {
		return a, domain.NewError(domain.AccessDenied, "account disabled")
	}
	if !readOnly {
		_, e = s.DB.Exec(ctx, `INSERT INTO quota_accounts(owner_id,limit_bytes) VALUES($1,$2) ON CONFLICT DO NOTHING`, pid, s.Config.DefaultQuota)
		if e != nil {
			return a, e
		}
	}

	typ := domain.ActorUser
	if agent || sub.ClientID != s.Config.WebClientID {
		typ = domain.ActorAgent
	}
	a = Actor{Principal: domain.Principal{ID: pid, Type: typ, Active: true, StatusVerified: true, Scopes: domain.ParseScopes(strings.Join(sub.Scopes, " "))}, Subject: sub, Token: bearer}
	return a, nil
}
func (s *Service) credential(ctx context.Context, a Actor, source ports.RevisionRef) (ports.Credential, error) {
	if s.Identity == nil {
		return ports.Credential{}, dependency()
	}
	return s.Identity.DelegatedCredential(ctx, ports.DelegationRequest{Subject: a.Subject, SubjectToken: a.Token, Audience: s.Config.OCISAudience, Source: source})
}
func (s *Service) scope(a Actor, scope string) error {
	if !a.Principal.Active || !a.Principal.StatusVerified {
		return domain.NewError(domain.AccessDenied, "account disabled")
	}
	if !a.Principal.Scopes[scope] {
		return domain.NewError(domain.InsufficientScope, "requires "+scope)
	}
	return nil
}
func fileDomain(f Object) domain.File {
	return domain.File{ID: str(f, "id"), OwnerID: str(f, "owner_id"), SpaceID: str(f, "space_id"), ParentID: str(f, "parent_id"), Name: str(f, "name"), Kind: domain.Kind(str(f, "kind")), Lifecycle: domain.Lifecycle(str(f, "lifecycle")), MetadataRevision: num(f, "metadata_revision"), CurrentVersionID: str(f, "current_version_id"), SearchArtifactSetID: str(f, "search_artifact_set_id"), ActiveOperationID: str(f, "active_operation_id")}
}
func (s *Service) owned(ctx context.Context, a Actor, fid string, action domain.Action) (Object, error) {
	f, e := one(ctx, s.DB, `SELECT to_jsonb(f) FROM files f WHERE id=$1 AND owner_id=$2`, fid, a.Principal.ID)
	if e != nil {
		return nil, e
	}
	rows, e := s.DB.Query(ctx, `WITH RECURSIVE parents AS(SELECT p.id,p.parent_id,p.lifecycle FROM files p WHERE p.id=$1 UNION ALL SELECT p.id,p.parent_id,p.lifecycle FROM files p JOIN parents c ON p.id=c.parent_id) SELECT lifecycle FROM parents`, null(str(f, "parent_id")))
	if e != nil {
		return nil, e
	}
	anc := []domain.Lifecycle{}
	for rows.Next() {
		var l string
		_ = rows.Scan(&l)
		anc = append(anc, domain.Lifecycle(l))
	}
	rows.Close()
	e = domain.Authorize(domain.AccessRequest{Principal: a.Principal, File: fileDomain(f), Action: action, Ancestors: anc, OwnerActive: true, OwnerStatusVerified: true, Now: time.Now()})
	if e != nil {
		return nil, e
	}
	return f, nil
}
func (s *Service) space(ctx context.Context, a Actor) (Object, error) {
	sp, e := one(ctx, s.DB, `SELECT to_jsonb(s) FROM spaces s WHERE owner_id=$1`, a.Principal.ID)
	if e == nil {
		return sp, nil
	}
	resolver, ok := s.Original.(ports.PersonalSpaceResolver)
	if !ok {
		return nil, dependency()
	}
	cred, e := s.credential(ctx, a, ports.RevisionRef{})
	if e != nil {
		return nil, e
	}
	native, e := resolver.ResolvePersonalSpace(ctx, cred)
	if e != nil {
		return nil, e
	}
	return one(ctx, s.DB, `INSERT INTO spaces(owner_id,native_instance,native_space_id,native_root_resource_id) VALUES($1,$2,$3,$4) ON CONFLICT(owner_id) DO UPDATE SET owner_id=excluded.owner_id RETURNING to_jsonb(spaces)`, a.Principal.ID, s.Config.OCISURL, native.SpaceID, native.RootResourceID)
}
func (s *Service) ref(ctx context.Context, f Object) (ports.ResourceRef, error) {
	var sid, instance string
	e := s.DB.QueryRow(ctx, `SELECT native_space_id,native_instance FROM spaces WHERE id=$1`, str(f, "space_id")).Scan(&sid, &instance)
	return ports.ResourceRef{Instance: instance, SpaceID: sid, ResourceID: str(f, "native_resource_id")}, e
}
func (s *Service) parent(ctx context.Context, a Actor, pid string) (Object, ports.ResourceRef, error) {
	sp, e := s.space(ctx, a)
	if e != nil {
		return nil, ports.ResourceRef{}, e
	}
	if pid == "" {
		ref := ports.ResourceRef{Instance: str(sp, "native_instance"), SpaceID: str(sp, "native_space_id"), ResourceID: str(sp, "native_root_resource_id")}
		return Object{"id": "", "path": "/", "space_id": str(sp, "id")}, ref, nil
	}
	p, e := s.owned(ctx, a, pid, domain.ActionWrite)
	if e != nil {
		return nil, ports.ResourceRef{}, e
	}
	if str(p, "native_resource_id") == "" {
		return nil, ports.ResourceRef{}, domain.NewError(domain.OperationPending, "directory creation is not confirmed")
	}
	if str(p, "kind") != "directory" {
		return nil, ports.ResourceRef{}, domain.NewError(domain.InvalidPath, "parent is not a directory")
	}
	r, e := s.ref(ctx, p)
	return p, r, e
}
func (s *Service) Me(ctx context.Context, a Actor) (Object, error) {
	q, e := one(ctx, s.DB, `SELECT to_jsonb(q) FROM quota_accounts q WHERE owner_id=$1`, a.Principal.ID)
	if e != nil {
		return nil, e
	}
	var admin bool
	_ = s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reviewer_grants WHERE principal_id=$1 AND(can_view_jobs OR can_triage_feedback))`, a.Principal.ID).Scan(&admin)
	return Object{"id": a.Principal.ID, "display_name": a.Subject.Subject, "scopes": a.Subject.Scopes, "quota": q, "permissions": Object{"admin": admin}, "actor_type": a.Principal.Type}, nil
}
func (s *Service) CreateSession(ctx context.Context, a Actor) (Object, error) {
	if a.Principal.Type != domain.ActorUser || a.Subject.ClientID != s.Config.WebClientID {
		return nil, domain.NewError(domain.AccessDenied, "interactive web client required")
	}
	t, th, e := token()
	if e != nil {
		return nil, e
	}
	csrf, ch, e := token()
	if e != nil {
		return nil, e
	}
	_, e = s.DB.Exec(ctx, `INSERT INTO web_sessions(token_hash,csrf_hash,principal_id,expires_at) VALUES($1,$2,$3,now()+interval '30 minutes')`, th, ch, a.Principal.ID)
	return Object{"session_token": t, "csrf_token": csrf}, e
}
func (s *Service) VerifySession(ctx context.Context, a Actor, t, csrf string) bool {
	if a.Principal.Type != domain.ActorUser || t == "" || csrf == "" {
		return false
	}
	var ok bool
	_ = s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM web_sessions WHERE token_hash=$1 AND csrf_hash=$2 AND principal_id=$3 AND expires_at>now())`, digest(t), digest(csrf), a.Principal.ID).Scan(&ok)
	return ok
}
func (s *Service) ReadContent(ctx context.Context, a Actor, fid, vid string, rng *ports.ByteRange) (ports.Content, error) {
	action := domain.ActionDownload
	if vid != "" {
		action = domain.ActionReadHistory
	}
	f, e := s.owned(ctx, a, fid, action)
	owner := a.Subject
	subjectToken := a.Token
	if e != nil {
		f, owner, e = s.SharedFile(ctx, a, fid, vid, domain.ActionDownload)
		subjectToken = ""
	}
	if e != nil {
		return ports.Content{}, e
	}
	if vid == "" {
		vid = str(f, "preview_version_id")
		if vid == "" {
			vid = str(f, "current_version_id")
		}
	}
	return s.originalVersion(ctx, owner, subjectToken, fid, vid, rng)
}
func (s *Service) originalVersion(ctx context.Context, owner ports.Subject, subjectToken, fid, vid string, rng *ports.ByteRange) (ports.Content, error) {
	var ref ports.RevisionRef
	var b []byte
	e := s.DB.QueryRow(ctx, `SELECT native_revision_ref FROM file_versions WHERE id=$1 AND file_id=$2`, vid, fid).Scan(&b)
	if e != nil {
		return ports.Content{}, normalizeError(e)
	}
	if e = json.Unmarshal(b, &ref); e != nil {
		return ports.Content{}, e
	}
	cred, e := s.Identity.DelegatedCredential(ctx, ports.DelegationRequest{Subject: owner, SubjectToken: subjectToken, Audience: s.Config.OCISAudience, Source: ref})
	if e != nil {
		return ports.Content{}, e
	}
	return s.Original.OpenRevision(ctx, ref, rng, cred)
}

func readBounded(r io.Reader, n int64) ([]byte, error) {
	b, e := io.ReadAll(io.LimitReader(r, n+1))
	if int64(len(b)) > n {
		return nil, ports.ErrLimit
	}
	return b, e
}

var _ = fmt.Sprintf
