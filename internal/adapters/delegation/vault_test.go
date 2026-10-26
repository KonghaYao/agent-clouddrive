package delegation

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func testID() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func TestCipherAuthenticatesOwnerAndTampering(t *testing.T) {
	block, _ := aes.NewCipher(bytes.Repeat([]byte{7}, 32))
	aead, _ := cipher.NewGCM(block)
	w := &Wrapper{aead: aead}
	sealed, e := w.seal("sensitive-offline-grant", "principal-a")
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(sealed, []byte("sensitive-offline-grant")) {
		t.Fatal("plaintext persisted")
	}
	again, _ := w.seal("sensitive-offline-grant", "principal-a")
	if bytes.Equal(sealed, again) {
		t.Fatal("nonce reused")
	}
	v, e := w.open(sealed, "principal-a")
	if e != nil || v != "sensitive-offline-grant" {
		t.Fatal(e)
	}
	if _, e = w.open(sealed, "principal-b"); !errors.Is(e, ports.ErrIntegrity) {
		t.Fatal("row substitution permitted", e)
	}
	sealed[len(sealed)-1] ^= 1
	if _, e = w.open(sealed, "principal-a"); !errors.Is(e, ports.ErrIntegrity) {
		t.Fatal("tamper accepted", e)
	}
}

type identityFixture struct{ enabled atomic.Bool }

func (f *identityFixture) ValidateToken(_ context.Context, token string, _ ports.TokenRequirements) (ports.Subject, error) {
	if !f.enabled.Load() || !strings.HasPrefix(token, "access-") {
		return ports.Subject{}, ports.ErrUnauthorized
	}
	return ports.Subject{Issuer: "https://issuer.test/realm", Subject: "owner", Scopes: []string{"drive.read"}}, nil
}
func (f *identityFixture) SubjectStatus(context.Context, string, string) (ports.SubjectStatus, error) {
	return ports.SubjectStatus{Enabled: f.enabled.Load()}, nil
}
func (f *identityFixture) Groups(context.Context, string, string) ([]string, error) { return nil, nil }
func (f *identityFixture) DelegatedCredential(_ context.Context, in ports.DelegationRequest) (ports.Credential, error) {
	if !strings.HasPrefix(in.SubjectToken, "access-") || len(in.Scopes) != 1 || in.Scopes[0] != "drive.read" {
		return ports.Credential{}, ports.ErrUnauthorized
	}
	return ports.Credential{AccessToken: "delegated-read-only"}, nil
}
func TestVaultPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("DRIVE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set DRIVE_TEST_DATABASE_URL for real PostgreSQL rotation test (IdP is httptest)")
	}
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := "delegation_" + strings.ReplaceAll(testID(), "-", "")
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
		sql, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = pool.Exec(ctx, string(sql)); e != nil {
			t.Fatalf("migration %s: %v", p, e)
		}
	}
	owner, space, file, version := testID(), testID(), testID(), testID()
	subject := ports.Subject{Issuer: "https://issuer.test/realm", Subject: "owner"}
	pin := ports.RevisionRef{ResourceRef: ports.ResourceRef{Instance: "native", SpaceID: "space", ResourceID: "resource"}, RevisionID: `etag:"etag"`, ETag: `"etag"`}
	raw, _ := json.Marshal(pin)
	for _, stmt := range []struct {
		sql  string
		args []any
	}{{`INSERT INTO principals(id,issuer,subject) VALUES($1,$2,$3)`, []any{owner, subject.Issuer, subject.Subject}}, {`INSERT INTO spaces(id,owner_id,native_instance,native_space_id,native_root_resource_id) VALUES($1,$2,'native','space','root')`, []any{space, owner}}, {`INSERT INTO files(id,owner_id,space_id,name,path,kind,native_instance,native_resource_id) VALUES($1,$2,$3,'file','/file','file','native','resource')`, []any{file, owner, space}}, {`INSERT INTO file_versions(id,file_id,version_seq,size,sha256,native_etag,native_revision_ref) VALUES($1,$2,1,3,$3,$4,$5)`, []any{version, file, strings.Repeat("a", 64), pin.ETag, raw}}} {
		if _, e = pool.Exec(ctx, stmt.sql, stmt.args...); e != nil {
			t.Fatal(e)
		}
	}
	var mu sync.Mutex
	next := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		defer mu.Unlock()
		expected := fmt.Sprintf("refresh-%d", next)
		if r.Form.Get("refresh_token") != expected {
			t.Errorf("lost refresh rotation: got %q expected %q", r.Form.Get("refresh_token"), expected)
			w.WriteHeader(400)
			return
		}
		next++
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("access-%d", next), "refresh_token": fmt.Sprintf("refresh-%d", next), "token_type": "Bearer"})
	}))
	defer srv.Close()
	base := &identityFixture{}
	base.enabled.Store(true)
	vault, e := New(Config{Base: base, DB: pool, Issuer: subject.Issuer, TokenURL: srv.URL, ClientID: "offline", ClientSecret: "test", Key: bytes.Repeat([]byte{1}, 32), Verified: true})
	if e != nil {
		t.Fatal(e)
	}
	if e = vault.StoreGrant(ctx, subject, "refresh-0", []string{"openid", "offline_access", "drive.read"}); e != nil {
		t.Fatal(e)
	}
	var stored []byte
	if e = pool.QueryRow(ctx, `SELECT encrypted_refresh FROM delegation_grants WHERE principal_id=$1`, owner).Scan(&stored); e != nil || bytes.Contains(stored, []byte("refresh-0")) {
		t.Fatal("unsealed database grant", e)
	}
	in := ports.DelegationRequest{Subject: subject, Source: pin, Audience: "ocis"}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := vault.DelegatedCredential(ctx, in); errs <- e }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if next != 2 {
		t.Fatal("refresh count", next)
	}
	wrong := in
	wrong.Source.ResourceID = "other-user-resource"
	if _, e = vault.DelegatedCredential(ctx, wrong); !errors.Is(e, ports.ErrUnauthorized) {
		t.Fatal("unowned pin permitted", e)
	}
	base.enabled.Store(false)
	if _, e = vault.DelegatedCredential(ctx, in); !errors.Is(e, ports.ErrUnauthorized) {
		t.Fatal("disabled subject permitted", e)
	}
	base.enabled.Store(true)
	if _, e = pool.Exec(ctx, `UPDATE files SET lifecycle='trashed' WHERE id=$1`, file); e != nil {
		t.Fatal(e)
	}
	if _, e = vault.DelegatedCredential(ctx, in); !errors.Is(e, ports.ErrUnauthorized) {
		t.Fatal("deleted source permitted", e)
	}
	if e = vault.Revoke(ctx, subject); e != nil {
		t.Fatal(e)
	}
	ok, e := vault.HasGrant(ctx, subject)
	if e != nil || ok {
		t.Fatal("revoked grant active", e)
	}
	t.Log("real PostgreSQL + mock IdP: encrypted persistence, concurrent refresh rotation, owner/source checks, live disable, lifecycle rejection and revoke passed")
}
