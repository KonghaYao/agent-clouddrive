package access

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/application/core"
	"github.com/agentclouddrive/agent-clouddrive/internal/config"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type oidcFixture struct{ calls atomic.Int64 }

func (f *oidcFixture) ValidateToken(context.Context, string, ports.TokenRequirements) (ports.Subject, error) {
	f.calls.Add(1)
	return ports.Subject{Issuer: "https://issuer.test", Subject: "oidc-user", ClientID: "web", Scopes: []string{"drive.read"}}, nil
}
func (f *oidcFixture) SubjectStatus(context.Context, string, string) (ports.SubjectStatus, error) {
	return ports.SubjectStatus{Enabled: true}, nil
}
func (f *oidcFixture) Groups(context.Context, string, string) ([]string, error) { return nil, nil }
func (f *oidcFixture) DelegatedCredential(context.Context, ports.DelegationRequest) (ports.Credential, error) {
	return ports.Credential{}, ports.ErrUnverified
}

func masterKeyTestConfig() config.Config {
	return config.Config{
		WebClientID:                  "web",
		MasterKeys:                   map[string]string{"k1": "secret1", "k2": "secret2"},
		MasterKeyDefaultID:           "k1",
		MasterKeyIssuer:              "fenix",
		MasterKeyInteractiveClientID: "web",
		MasterKeyAllowedCIDRs:        []string{"127.0.0.1/32", "::1/128"},
		MasterKeyScopes:              []string{"drive.read", "drive.write", "drive.delete", "drive.restore", "drive.convert", "drive.share.request", "drive.feedback"},
		MasterKeyStatusTTL:           time.Minute,
		DefaultQuota:                 1 << 30,
	}
}

func TestMasterKeySelection(t *testing.T) {
	s := New(core.Deps{Config: masterKeyTestConfig()})
	cases := []struct {
		requested, presented, want string
		ok                         bool
	}{
		{"k2", "secret2", "k2", true},
		{"", "secret1", "k1", true},
		{"k2", "secret1", "", false},
		{"", "unknown", "", false},
		{"unknown", "secret1", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		got, ok := s.selectMasterKey(tc.requested, tc.presented)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("select(%q,%q)=(%q,%v), want (%q,%v)", tc.requested, tc.presented, got, ok, tc.want, tc.ok)
		}
	}
	if !s.IsMasterKey("secret2") || s.IsMasterKey("secret3") || s.IsMasterKey("") {
		t.Fatal("master key routing comparison is wrong")
	}
	if New(core.Deps{}).IsMasterKey("secret1") {
		t.Fatal("unconfigured master keys must never match")
	}
}

func TestMasterKeyWrongSecretIsUnauthenticated(t *testing.T) {
	s := New(core.Deps{Config: masterKeyTestConfig()})
	_, e := s.AuthenticateMasterKey(context.Background(), MasterKeyAuth{Bearer: "wrong", OnBehalfOf: "user-1", RemoteIP: "127.0.0.1", Route: "/v1/files"})
	if domain.ErrorCode(e) != domain.Unauthenticated {
		t.Fatalf("wrong key must be unauthenticated, got %v", e)
	}
	_, e = s.AuthenticateMasterKey(context.Background(), MasterKeyAuth{Bearer: "secret1", KeyID: "k2", OnBehalfOf: "user-1", RemoteIP: "127.0.0.1", Route: "/v1/files"})
	if domain.ErrorCode(e) != domain.Unauthenticated {
		t.Fatalf("key id mismatch must be unauthenticated, got %v", e)
	}
}

func TestMasterKeyOnBehalfOfIsRequiredAndSafe(t *testing.T) {
	s := New(core.Deps{Config: masterKeyTestConfig()})
	for _, obo := range []string{"", "../../etc/passwd", "user name", strings.Repeat("a", 129), "-leading"} {
		_, e := s.AuthenticateMasterKey(context.Background(), MasterKeyAuth{Bearer: "secret1", OnBehalfOf: obo, RemoteIP: "127.0.0.1", Route: "/v1/files"})
		if !errors.Is(e, ErrInvalidOnBehalfOf) {
			t.Fatalf("on-behalf-of %q must be rejected with the 400 sentinel, got %v", obo, e)
		}
	}
	for _, obo := range []string{"9f8c4e3a-1b2c-4d5e-8f90-1234567890ab", "user-1", "user@example.test", "tenant:user"} {
		if !validOnBehalfOf(obo) {
			t.Fatalf("safe identifier %q rejected", obo)
		}
	}
}

func TestMasterKeyCIDRDenial(t *testing.T) {
	cfg := masterKeyTestConfig()
	cases := []struct {
		name string
		cfg  func(*config.Config)
		ip   string
	}{
		{"outside", func(*config.Config) {}, "10.1.2.3"},
		{"empty allowlist", func(c *config.Config) { c.MasterKeyAllowedCIDRs = nil }, "127.0.0.1"},
		{"invalid remote", func(*config.Config) {}, "not-an-ip"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			local := cfg
			tc.cfg(&local)
			s := New(core.Deps{Config: local})
			_, e := s.AuthenticateMasterKey(context.Background(), MasterKeyAuth{Bearer: "secret1", OnBehalfOf: "user-1", RemoteIP: tc.ip, Route: "/v1/files"})
			if domain.ErrorCode(e) != domain.AccessDenied {
				t.Fatalf("expected CIDR denial, got %v", e)
			}
		})
	}
}

func TestMasterKeyStatusFailClosedAndCached(t *testing.T) {
	var hits atomic.Int64
	var status atomic.Value
	status.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/users/user-1/status" {
			t.Errorf("unexpected status path %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer status-token" {
			t.Errorf("status bearer not sent")
		}
		if !status.Load().(bool) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"enabled":false}`))
	}))
	defer srv.Close()
	cfg := masterKeyTestConfig()
	cfg.MasterKeyStatusURL = srv.URL + "/users"
	cfg.MasterKeyStatusToken = "status-token"
	cfg.MasterKeyStatusTTL = 50 * time.Millisecond
	deps := core.Deps{Config: cfg, MasterKeyStatus: core.NewMasterKeyStatusCache()}
	s := New(deps)
	call := func() error {
		_, e := s.AuthenticateMasterKey(context.Background(), MasterKeyAuth{Bearer: "secret1", OnBehalfOf: "user-1", RemoteIP: "127.0.0.1", Route: "/v1/files", ReadOnly: true})
		return e
	}
	if e := call(); domain.ErrorCode(e) != domain.AccessDenied {
		t.Fatalf("disabled status must be denied, got %v", e)
	}
	if e := call(); domain.ErrorCode(e) != domain.AccessDenied {
		t.Fatalf("cached disabled status must be denied, got %v", e)
	}
	if hits.Load() != 1 {
		t.Fatalf("status cache did not suppress a duplicate request: %d", hits.Load())
	}
	time.Sleep(70 * time.Millisecond)
	if e := call(); domain.ErrorCode(e) != domain.AccessDenied {
		t.Fatalf("expired cache must re-check and deny, got %v", e)
	}
	if hits.Load() != 2 {
		t.Fatalf("status TTL did not expire: %d", hits.Load())
	}
	status.Store(false)
	deps.Config.MasterKeyStatusTTL = time.Nanosecond
	deps.MasterKeyStatus = core.NewMasterKeyStatusCache()
	if _, e := New(deps).AuthenticateMasterKey(context.Background(), MasterKeyAuth{Bearer: "secret1", OnBehalfOf: "user-1", RemoteIP: "127.0.0.1", Route: "/v1/files", ReadOnly: true}); domain.ErrorCode(e) != domain.DependencyUnavailable {
		t.Fatalf("unavailable status must fail closed, got %v", e)
	}
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed.Close()
	deps.Config.MasterKeyStatusURL = closed.URL
	deps.MasterKeyStatus = core.NewMasterKeyStatusCache()
	if _, e := New(deps).AuthenticateMasterKey(context.Background(), MasterKeyAuth{Bearer: "secret1", OnBehalfOf: "user-1", RemoteIP: "127.0.0.1", Route: "/v1/files", ReadOnly: true}); domain.ErrorCode(e) != domain.DependencyUnavailable {
		t.Fatalf("unreachable status endpoint must fail closed, got %v", e)
	}
}

func TestMasterKeyAuditNeverContainsSecret(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(previous)
	s := New(core.Deps{Config: masterKeyTestConfig()})
	_, _ = s.AuthenticateMasterKey(context.Background(), MasterKeyAuth{Bearer: "wrong", KeyID: "k1", OnBehalfOf: "user-1", RemoteIP: "127.0.0.1", Route: "/v1/files"})
	line := buf.String()
	for _, want := range []string{"credential_source=master_key", "key_id=k1", "on_behalf_of=user-1", "route=/v1/files", "status=401"} {
		if !strings.Contains(line, want) {
			t.Fatalf("audit log missing %q: %s", want, line)
		}
	}
	if strings.Contains(line, "secret1") {
		t.Fatalf("audit log leaked the master key: %s", line)
	}
}

func TestMasterKeyAuditSuccessFields(t *testing.T) {
	_, deps := masterKeyDB(t)
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(previous)
	s := New(deps)
	if _, e := s.AuthenticateMasterKey(context.Background(), MasterKeyAuth{Bearer: "secret1", OnBehalfOf: "audit-user", RemoteIP: "127.0.0.1", Route: "/v1/files"}); e != nil {
		t.Fatal(e)
	}
	line := buf.String()
	for _, want := range []string{"credential_source=master_key", "key_id=k1", "on_behalf_of=audit-user", "route=/v1/files", "status=200"} {
		if !strings.Contains(line, want) {
			t.Fatalf("audit log missing %q: %s", want, line)
		}
	}
	if strings.Contains(line, "secret1") {
		t.Fatalf("audit log leaked the master key: %s", line)
	}
}

func masterKeyDB(t *testing.T) (*pgxpool.Pool, core.Deps) {
	t.Helper()
	dsn := os.Getenv("DRIVE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set DRIVE_TEST_DATABASE_URL for real PostgreSQL master-key tests")
	}
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := "access_" + strings.ReplaceAll(core.ID(), "-", "")
	if _, e = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); e != nil {
		t.Fatal(e)
	}
	dbConfig, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	dbConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, e := pgxpool.NewWithConfig(ctx, dbConfig)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	migrations, _ := filepath.Glob("../../../db/migrations/*.up.sql")
	sort.Strings(migrations)
	for _, p := range migrations {
		sql, er := os.ReadFile(p)
		if er != nil {
			t.Fatal(er)
		}
		if _, er = pool.Exec(ctx, string(sql)); er != nil {
			t.Fatalf("migration %s: %v", p, er)
		}
	}
	return pool, core.Deps{DB: pool, Config: masterKeyTestConfig(), MasterKeyStatus: core.NewMasterKeyStatusCache()}
}

func TestMasterKeyPrincipalQuotaActorAndSession(t *testing.T) {
	pool, deps := masterKeyDB(t)
	s := New(deps)
	ctx := context.Background()
	actor, e := s.AuthenticateMasterKey(ctx, MasterKeyAuth{Bearer: "secret1", OnBehalfOf: "ext-user-1", RemoteIP: "127.0.0.1", Route: "/v1/files"})
	if e != nil {
		t.Fatal(e)
	}
	if actor.Principal.Type != domain.ActorUser || actor.Subject.ClientID != "web" || actor.Subject.Issuer != "fenix" {
		t.Fatalf("interactive projection wrong: %+v", actor)
	}
	if actor.Token != "" {
		t.Fatal("master key must never become the actor token")
	}
	if !actor.Principal.Scopes["drive.write"] {
		t.Fatal("configured master key scopes not projected")
	}
	var pid string
	if e = pool.QueryRow(ctx, `SELECT id::text FROM principals WHERE issuer='fenix' AND subject='ext-user-1'`).Scan(&pid); e != nil || pid != actor.Principal.ID {
		t.Fatalf("principal not created/reused: %q %v", pid, e)
	}
	var quotas int
	if e = pool.QueryRow(ctx, `SELECT count(*) FROM quota_accounts WHERE owner_id=$1`, pid).Scan(&quotas); e != nil || quotas != 1 {
		t.Fatalf("quota account missing: %d %v", quotas, e)
	}
	again, e := s.AuthenticateMasterKey(ctx, MasterKeyAuth{Bearer: "secret2", KeyID: "k2", OnBehalfOf: "ext-user-1", RemoteIP: "127.0.0.1", Route: "/v1/files"})
	if e != nil || again.Principal.ID != pid {
		t.Fatalf("second authentication must reuse principal: %v %v", again.Principal.ID, e)
	}
	if e = pool.QueryRow(ctx, `SELECT count(*) FROM quota_accounts WHERE owner_id=$1`, pid).Scan(&quotas); e != nil || quotas != 1 {
		t.Fatalf("quota account duplicated: %d %v", quotas, e)
	}
	mcpActor, e := s.AuthenticateMasterKey(ctx, MasterKeyAuth{Bearer: "secret1", OnBehalfOf: "ext-user-1", RemoteIP: "127.0.0.1", Route: "mcp", Agent: true})
	if e != nil || mcpActor.Principal.Type != domain.ActorAgent || mcpActor.Principal.ID != pid {
		t.Fatalf("MCP projection wrong: %+v %v", mcpActor, e)
	}
	session, e := s.CreateSession(ctx, actor)
	if e != nil {
		t.Fatalf("interactive session must work for master key: %v", e)
	}
	token, _ := session["session_token"].(string)
	csrf, _ := session["csrf_token"].(string)
	if token == "" || csrf == "" || !s.VerifySession(ctx, actor, token, csrf) {
		t.Fatal("master key session was not verifiable")
	}
	readOnly, e := s.AuthenticateMasterKey(ctx, MasterKeyAuth{Bearer: "secret1", OnBehalfOf: "feedback-user", RemoteIP: "127.0.0.1", Route: "/v1/feedback", ReadOnly: true})
	if domain.ErrorCode(e) != domain.Unauthenticated {
		t.Fatalf("read-only must not create a principal, got %+v %v", readOnly, e)
	}
	var created int
	if e = pool.QueryRow(ctx, `SELECT count(*) FROM principals WHERE subject='feedback-user'`).Scan(&created); e != nil || created != 0 {
		t.Fatalf("read-only created a principal: %d %v", created, e)
	}
}

func TestMasterKeyStatusEnabledUsesCache(t *testing.T) {
	_, deps := masterKeyDB(t)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"enabled":true}`))
	}))
	defer srv.Close()
	deps.Config.MasterKeyStatusURL = srv.URL
	deps.Config.MasterKeyStatusTTL = time.Minute
	s := New(deps)
	for i := 0; i < 2; i++ {
		if _, e := s.AuthenticateMasterKey(context.Background(), MasterKeyAuth{Bearer: "secret1", OnBehalfOf: "status-user", RemoteIP: "127.0.0.1", Route: "/v1/files"}); e != nil {
			t.Fatal(e)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("enabled status was not cached: %d", hits.Load())
	}
}

func TestMasterKeyDisabledStatusDisablesPrincipal(t *testing.T) {
	pool, deps := masterKeyDB(t)
	ctx := context.Background()
	if _, e := pool.Exec(ctx, `INSERT INTO principals(issuer,subject) VALUES('fenix','disabled-user')`); e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"enabled":false}`))
	}))
	defer srv.Close()
	deps.Config.MasterKeyStatusURL = srv.URL
	deps.Config.MasterKeyStatusTTL = time.Nanosecond
	s := New(deps)
	_, e := s.AuthenticateMasterKey(ctx, MasterKeyAuth{Bearer: "secret1", OnBehalfOf: "disabled-user", RemoteIP: "127.0.0.1", Route: "/v1/files"})
	if domain.ErrorCode(e) != domain.AccessDenied {
		t.Fatalf("disabled status must deny, got %v", e)
	}
	var status string
	if e = pool.QueryRow(ctx, `SELECT status FROM principals WHERE issuer='fenix' AND subject='disabled-user'`).Scan(&status); e != nil || status != "disabled" {
		t.Fatalf("local principal not disabled: %q %v", status, e)
	}
}

func TestOIDCUnchangedWhenMasterKeysUnset(t *testing.T) {
	pool, deps := masterKeyDB(t)
	deps.Config.MasterKeys = nil
	ident := &oidcFixture{}
	deps.Identity = ident
	s := New(deps)
	actor, e := s.Authenticate(context.Background(), "oidc-token", false)
	if e != nil {
		t.Fatal(e)
	}
	if actor.Token != "oidc-token" || actor.Subject.Subject != "oidc-user" || actor.Principal.Type != domain.ActorUser {
		t.Fatalf("OIDC behavior changed: %+v", actor)
	}
	var count int
	if e = pool.QueryRow(context.Background(), `SELECT count(*) FROM principals WHERE subject='oidc-user'`).Scan(&count); e != nil || count != 1 {
		t.Fatalf("OIDC principal not created: %d %v", count, e)
	}
	_, e = s.AuthenticateMasterKey(context.Background(), MasterKeyAuth{Bearer: "secret1", OnBehalfOf: "user-1", RemoteIP: "127.0.0.1", Route: "/v1/files"})
	if domain.ErrorCode(e) != domain.Unauthenticated {
		t.Fatalf("master key path must stay disabled when unconfigured, got %v", e)
	}
	if ident.calls.Load() != 1 {
		t.Fatalf("master key path reached the OIDC adapter: %d", ident.calls.Load())
	}
}
