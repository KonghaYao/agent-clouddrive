package delegation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type recordingProvider struct {
	calls int
	req   ports.JobCredentialRequest
	cred  ports.Credential
	err   error
}

func (p *recordingProvider) JobCredential(_ context.Context, req ports.JobCredentialRequest) (ports.Credential, error) {
	p.calls++
	p.req = req
	return p.cred, p.err
}

func vaultServicePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DRIVE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set DRIVE_TEST_DATABASE_URL for real PostgreSQL service credential tests")
	}
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := "jobcred_" + strings.ReplaceAll(testID(), "-", "")
	if _, e = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); e != nil {
		t.Fatal(e)
	}
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
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
	return pool
}

func insertVaultSubject(t *testing.T, pool *pgxpool.Pool, issuer, subject string) (ports.RevisionRef, string) {
	t.Helper()
	owner, space, file, version := testID(), testID(), testID(), testID()
	pin := ports.RevisionRef{
		ResourceRef: ports.ResourceRef{Instance: "native", SpaceID: "space-" + space, ResourceID: "resource-" + file},
		RevisionID:  `etag:"etag"`,
		ETag:        `"etag"`,
	}
	raw, e := json.Marshal(pin)
	if e != nil {
		t.Fatal(e)
	}
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO principals(id,issuer,subject) VALUES($1,$2,$3)`, []any{owner, issuer, subject}},
		{`INSERT INTO spaces(id,owner_id,native_instance,native_space_id,native_root_resource_id) VALUES($1,$2,'native',$3,'root')`, []any{space, owner, pin.SpaceID}},
		{`INSERT INTO files(id,owner_id,space_id,name,path,kind,native_instance,native_resource_id) VALUES($1,$2,$3,'file','/file','file','native',$4)`, []any{file, owner, space, pin.ResourceID}},
		{`INSERT INTO file_versions(id,file_id,version_seq,size,sha256,native_etag,native_revision_ref) VALUES($1,$2,1,3,$3,$4,$5)`, []any{version, file, strings.Repeat("a", 64), pin.ETag, raw}},
	}
	for _, stmt := range statements {
		if _, e = pool.Exec(context.Background(), stmt.sql, stmt.args...); e != nil {
			t.Fatal(e)
		}
	}
	return pin, version
}

func insertVaultJob(t *testing.T, pool *pgxpool.Pool, version string) string {
	t.Helper()
	job := testID()
	if _, e := pool.Exec(context.Background(), `INSERT INTO conversion_jobs(id,source_version_id,generation,recipe_fingerprint,recipe_snapshot,state,lease_owner,lease_until) VALUES($1,$2,1,'fp','{}','processing','test',now()+interval '1 minute')`, job, version); e != nil {
		t.Fatal(e)
	}
	return job
}

func newVaultForTest(t *testing.T, pool *pgxpool.Pool, base ports.IdentityAdapter, tokenURL, mode string, verified bool, provider ports.JobCredentialProvider) *Wrapper {
	t.Helper()
	w, e := New(Config{Base: base, DB: pool, Issuer: "https://issuer.test/realm", TokenURL: tokenURL, ClientID: "offline", ClientSecret: "test", Key: bytes.Repeat([]byte{9}, 32), Verified: verified, JobCredentialMode: mode, JobCredentialProvider: provider})
	if e != nil {
		t.Fatal(e)
	}
	return w
}

func TestVaultJobCredentialSelection(t *testing.T) {
	pool := vaultServicePool(t)
	ctx := context.Background()
	var refreshes atomic.Int64
	refresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := refreshes.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("access-%d", n), "refresh_token": fmt.Sprintf("refresh-%d", n), "token_type": "Bearer"})
	}))
	defer refresh.Close()

	ownerPin, _ := insertVaultSubject(t, pool, "https://issuer.test/realm", "owner")
	noGrantPin, noGrantVersion := insertVaultSubject(t, pool, "https://issuer.test/realm", "no-grant")
	disabledPin, _ := insertVaultSubject(t, pool, "https://issuer.test/realm", "disabled-grant")
	externalPin, _ := insertVaultSubject(t, pool, "external-idp", "ext-user")
	jobID := insertVaultJob(t, pool, noGrantVersion)

	base := &identityFixture{}
	base.enabled.Store(true)

	// Auto mode without a grant falls back to the service provider.
	provider := &recordingProvider{cred: ports.Credential{AccessToken: "service-access", ExpiresAt: time.Now().Add(time.Minute)}}
	vault := newVaultForTest(t, pool, base, refresh.URL, "auto", true, provider)
	cred, e := vault.DelegatedCredential(ctx, ports.DelegationRequest{Subject: ports.Subject{Issuer: "https://issuer.test/realm", Subject: "no-grant"}, Audience: "ocis", Source: noGrantPin, JobID: jobID})
	if e != nil || cred.AccessToken != "service-access" || provider.calls != 1 {
		t.Fatalf("service fallback failed: %+v %v %d", cred, e, provider.calls)
	}
	if provider.req.Audience != "ocis" || len(provider.req.Scopes) != 1 || provider.req.Scopes[0] != "drive.read" || provider.req.JobID != jobID || provider.req.Subject.Subject != "no-grant" {
		t.Fatalf("provider request wrong: %+v", provider.req)
	}

	// An active grant takes priority and never calls the service provider.
	if e = vault.StoreGrant(ctx, ports.Subject{Issuer: "https://issuer.test/realm", Subject: "owner"}, "refresh-0", []string{"offline_access", "drive.read"}); e != nil {
		t.Fatal(e)
	}
	provider = &recordingProvider{cred: ports.Credential{AccessToken: "service-access"}}
	vault = newVaultForTest(t, pool, base, refresh.URL, "auto", true, provider)
	cred, e = vault.DelegatedCredential(ctx, ports.DelegationRequest{Subject: ports.Subject{Issuer: "https://issuer.test/realm", Subject: "owner"}, Audience: "ocis", Source: ownerPin})
	if e != nil || cred.AccessToken != "delegated-read-only" || provider.calls != 0 {
		t.Fatalf("grant must win: %+v %v %d", cred, e, provider.calls)
	}

	// Delegation-only mode has no service fallback.
	provider = &recordingProvider{cred: ports.Credential{AccessToken: "service-access"}}
	vault = newVaultForTest(t, pool, base, refresh.URL, "delegation", true, provider)
	if _, e = vault.DelegatedCredential(ctx, ports.DelegationRequest{Subject: ports.Subject{Issuer: "https://issuer.test/realm", Subject: "no-grant"}, Audience: "ocis", Source: noGrantPin}); !errors.Is(e, ports.ErrUnverified) || provider.calls != 0 {
		t.Fatalf("delegation mode must not use the provider: %v %d", e, provider.calls)
	}

	// Service-only mode skips grants.
	provider = &recordingProvider{cred: ports.Credential{AccessToken: "service-access"}}
	vault = newVaultForTest(t, pool, base, refresh.URL, "service", true, provider)
	if _, e = vault.DelegatedCredential(ctx, ports.DelegationRequest{Subject: ports.Subject{Issuer: "https://issuer.test/realm", Subject: "owner"}, Audience: "ocis", Source: ownerPin}); e != nil || provider.calls != 1 {
		t.Fatalf("service mode failed: %v %d", e, provider.calls)
	}

	// Disabled mode ignores both grants and the provider.
	if e = vault.StoreGrant(ctx, ports.Subject{Issuer: "https://issuer.test/realm", Subject: "disabled-grant"}, "refresh-0", []string{"offline_access", "drive.read"}); e != nil {
		t.Fatal(e)
	}
	provider = &recordingProvider{cred: ports.Credential{AccessToken: "service-access"}}
	vault = newVaultForTest(t, pool, base, refresh.URL, "disabled", true, provider)
	if _, e = vault.DelegatedCredential(ctx, ports.DelegationRequest{Subject: ports.Subject{Issuer: "https://issuer.test/realm", Subject: "disabled-grant"}, Audience: "ocis", Source: disabledPin}); !errors.Is(e, ports.ErrUnverified) || provider.calls != 0 {
		t.Fatalf("disabled mode must fail closed: %v %d", e, provider.calls)
	}

	// The verification gate precedes every job credential path.
	provider = &recordingProvider{cred: ports.Credential{AccessToken: "service-access"}}
	vault = newVaultForTest(t, pool, base, refresh.URL, "auto", false, provider)
	if _, e = vault.DelegatedCredential(ctx, ports.DelegationRequest{Subject: ports.Subject{Issuer: "https://issuer.test/realm", Subject: "no-grant"}, Audience: "ocis", Source: noGrantPin}); !errors.Is(e, ports.ErrUnverified) || provider.calls != 0 {
		t.Fatalf("unverified integration must fail closed: %v %d", e, provider.calls)
	}

	// Service mode without a provider is fail-closed, not a nil dereference.
	vault = newVaultForTest(t, pool, base, refresh.URL, "service", true, nil)
	if _, e = vault.DelegatedCredential(ctx, ports.DelegationRequest{Subject: ports.Subject{Issuer: "https://issuer.test/realm", Subject: "no-grant"}, Audience: "ocis", Source: noGrantPin}); !errors.Is(e, ports.ErrUnverified) {
		t.Fatalf("service mode without a provider must fail closed: %v", e)
	}

	// Provider errors are propagated unchanged.
	provider = &recordingProvider{err: ports.ErrUnverified}
	vault = newVaultForTest(t, pool, base, refresh.URL, "auto", true, provider)
	if _, e = vault.DelegatedCredential(ctx, ports.DelegationRequest{Subject: ports.Subject{Issuer: "https://issuer.test/realm", Subject: "no-grant"}, Audience: "ocis", Source: noGrantPin}); !errors.Is(e, ports.ErrUnverified) {
		t.Fatalf("provider error not propagated: %v", e)
	}

	// Source authorization failures never reach the provider.
	provider = &recordingProvider{cred: ports.Credential{AccessToken: "service-access"}}
	vault = newVaultForTest(t, pool, base, refresh.URL, "auto", true, provider)
	wrong := ownerPin
	wrong.ResourceID = "not-owned"
	if _, e = vault.DelegatedCredential(ctx, ports.DelegationRequest{Subject: ports.Subject{Issuer: "https://issuer.test/realm", Subject: "no-grant"}, Audience: "ocis", Source: wrong}); !errors.Is(e, ports.ErrUnauthorized) || provider.calls != 0 {
		t.Fatalf("unowned source must not receive a service credential: %v %d", e, provider.calls)
	}
	if _, e = vault.DelegatedCredential(ctx, ports.DelegationRequest{Subject: ports.Subject{Issuer: "https://issuer.test/realm", Subject: "no-grant"}, Audience: "ocis", Source: noGrantPin, JobID: "missing-job"}); !errors.Is(e, ports.ErrUnauthorized) || provider.calls != 0 {
		t.Fatalf("unauthorized job must not receive a service credential: %v %d", e, provider.calls)
	}

	// Audit reads never receive a background service credential.
	provider = &recordingProvider{cred: ports.Credential{AccessToken: "service-access"}}
	vault = newVaultForTest(t, pool, base, refresh.URL, "auto", true, provider)
	if _, e = vault.DelegatedCredential(ctx, ports.DelegationRequest{Subject: ports.Subject{Issuer: "https://issuer.test/realm", Subject: "no-grant"}, Audience: "ocis", Source: noGrantPin, Audit: &ports.AuditCapability{}}); !errors.Is(e, ports.ErrUnverified) || provider.calls != 0 {
		t.Fatalf("audit read must not use a service credential: %v %d", e, provider.calls)
	}

	// Externally asserted issuers use the local principal registry and can use
	// the same job-scoped service credential.
	provider = &recordingProvider{cred: ports.Credential{AccessToken: "service-access"}}
	vault = newVaultForTest(t, pool, base, refresh.URL, "auto", true, provider)
	cred, e = vault.DelegatedCredential(ctx, ports.DelegationRequest{Subject: ports.Subject{Issuer: "external-idp", Subject: "ext-user"}, Audience: "ocis", Source: externalPin})
	if e != nil || cred.AccessToken != "service-access" || provider.calls != 1 {
		t.Fatalf("external issuer service credential failed: %v %d", e, provider.calls)
	}
	status, e := vault.SubjectStatus(ctx, "external-idp", "ext-user")
	if e != nil || !status.Enabled {
		t.Fatalf("external principal status wrong: %+v %v", status, e)
	}
	status, e = vault.SubjectStatus(ctx, "external-idp", "unknown")
	if e != nil || status.Enabled {
		t.Fatalf("unknown external principal must be disabled: %+v %v", status, e)
	}
	groups, e := vault.Groups(ctx, "external-idp", "ext-user")
	if e != nil || len(groups) != 0 {
		t.Fatalf("external subject must have no IdP groups: %v %v", groups, e)
	}
	if _, e = vault.Groups(ctx, "external-idp", "unknown"); !errors.Is(e, ports.ErrUnauthorized) {
		t.Fatalf("unknown external group lookup must be unauthorized: %v", e)
	}
	if _, e = vault.IdentityEvents(ctx, "external-idp", ""); !errors.Is(e, ports.ErrUnsupportedIssuer) {
		t.Fatalf("external issuer must report an unsupported event feed: %v", e)
	}
}
