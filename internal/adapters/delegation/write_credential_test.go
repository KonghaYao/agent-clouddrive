package delegation

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/jackc/pgx/v5/pgxpool"
)

func writeVault(t *testing.T, pool *pgxpool.Pool, base ports.IdentityAdapter, tokenURL string, enabled bool, provider ports.JobCredentialProvider, verified bool) *Wrapper {
	t.Helper()
	v, e := New(Config{
		Base: base, DB: pool, Issuer: "https://issuer.test/realm", TokenURL: tokenURL,
		ClientID: "offline", ClientSecret: "test", Key: bytes.Repeat([]byte{11}, 32),
		Verified: verified, ExternalWriteEnabled: enabled,
		ExternalWriteIssuers: map[string]bool{"external-idp": true},
		WriteCredential:      provider,
	})
	if e != nil {
		t.Fatal(e)
	}
	return v
}

func TestVaultWriteCredential(t *testing.T) {
	pool := vaultServicePool(t)
	ctx := context.Background()
	externalPin, _ := insertVaultSubject(t, pool, "external-idp", "ext-writer")
	ownerPin, _ := insertVaultSubject(t, pool, "https://issuer.test/realm", "owner")
	refresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer refresh.Close()
	base := &identityFixture{}
	base.enabled.Store(true)

	writeReq := func(issuer, subject string, source ports.RevisionRef, extra func(*ports.DelegationRequest)) ports.DelegationRequest {
		in := ports.DelegationRequest{Subject: ports.Subject{Issuer: issuer, Subject: subject}, Audience: "ocis", Source: source, Scopes: []string{"drive.read", "drive.write"}}
		if extra != nil {
			extra(&in)
		}
		return in
	}

	provider := &recordingProvider{cred: ports.Credential{AccessToken: "write-service", ExpiresAt: time.Now().Add(time.Minute)}}
	// Writes are independent of the read/delegation verification gate.
	vault := writeVault(t, pool, base, refresh.URL, true, provider, false)
	cred, e := vault.DelegatedCredential(ctx, writeReq("external-idp", "ext-writer", externalPin, nil))
	if e != nil || cred.AccessToken != "write-service" || provider.calls != 1 {
		t.Fatalf("external write credential failed: %+v %v %d", cred, e, provider.calls)
	}
	got := provider.req
	if got.Subject.Issuer != "external-idp" || got.Subject.Subject != "ext-writer" || got.Audience != "ocis" || got.JobID != "" || got.Source.ResourceID != externalPin.ResourceID {
		t.Fatalf("write provider request wrong: %+v", got)
	}
	if len(got.Scopes) != 2 || got.Scopes[0] != "drive.read" || got.Scopes[1] != "drive.write" {
		t.Fatalf("write provider scopes wrong: %+v", got.Scopes)
	}

	// The primary OIDC issuer is not an external write trust domain.
	if _, e = vault.DelegatedCredential(ctx, writeReq("https://issuer.test/realm", "owner", ownerPin, nil)); !errors.Is(e, ports.ErrUnauthorized) {
		t.Fatalf("OIDC issuer must not receive a write service credential: %v", e)
	}
	// Background jobs are never write-capable.
	if _, e = vault.DelegatedCredential(ctx, writeReq("external-idp", "ext-writer", externalPin, func(in *ports.DelegationRequest) { in.JobID = "job-1" })); !errors.Is(e, ports.ErrUnauthorized) {
		t.Fatalf("job-scoped write request must be denied: %v", e)
	}
	// Audit reads are never write-capable.
	if _, e = vault.DelegatedCredential(ctx, writeReq("external-idp", "ext-writer", externalPin, func(in *ports.DelegationRequest) { in.Audit = &ports.AuditCapability{} })); !errors.Is(e, ports.ErrUnauthorized) {
		t.Fatalf("audit write request must be denied: %v", e)
	}
	if provider.calls != 1 {
		t.Fatalf("denied write requests reached the provider: %d", provider.calls)
	}

	// A persisted grant cannot satisfy a write request.
	grantVault := writeVault(t, pool, base, refresh.URL, true, provider, true)
	if e = grantVault.StoreGrant(ctx, ports.Subject{Issuer: "https://issuer.test/realm", Subject: "owner"}, "refresh-0", []string{"offline_access", "drive.read"}); e != nil {
		t.Fatal(e)
	}
	if _, e = grantVault.DelegatedCredential(ctx, writeReq("https://issuer.test/realm", "owner", ownerPin, nil)); !errors.Is(e, ports.ErrUnauthorized) {
		t.Fatalf("grant must not satisfy drive.write: %v", e)
	}
	if provider.calls != 1 {
		t.Fatalf("grant fallback reached the write provider: %d", provider.calls)
	}

	// Configuration gates fail closed without fallback.
	disabled := writeVault(t, pool, base, refresh.URL, false, provider, false)
	if _, e = disabled.DelegatedCredential(ctx, writeReq("external-idp", "ext-writer", externalPin, nil)); !errors.Is(e, ports.ErrUnauthorized) {
		t.Fatalf("disabled external writes must be denied: %v", e)
	}
	externalGrant := writeVault(t, pool, base, refresh.URL, false, provider, true)
	if e = externalGrant.StoreGrant(ctx, ports.Subject{Issuer: "external-idp", Subject: "ext-writer"}, "refresh-0", []string{"offline_access", "drive.read"}); e != nil {
		t.Fatal(e)
	}
	if _, e = externalGrant.DelegatedCredential(ctx, writeReq("external-idp", "ext-writer", externalPin, nil)); !errors.Is(e, ports.ErrUnauthorized) {
		t.Fatalf("external grant must not satisfy drive.write: %v", e)
	}
	noProvider := writeVault(t, pool, base, refresh.URL, true, nil, false)
	if _, e = noProvider.DelegatedCredential(ctx, writeReq("external-idp", "ext-writer", externalPin, nil)); !errors.Is(e, ports.ErrUnauthorized) {
		t.Fatalf("missing write provider must be denied: %v", e)
	}
	unverified := writeVault(t, pool, base, refresh.URL, true, &recordingProvider{err: ports.ErrUnverified}, false)
	if _, e = unverified.DelegatedCredential(ctx, writeReq("external-idp", "ext-writer", externalPin, nil)); !errors.Is(e, ports.ErrUnauthorized) {
		t.Fatalf("unverified write provider must map to unauthorized: %v", e)
	}
	// Unknown scopes are rejected before provider invocation.
	if _, e = vault.DelegatedCredential(ctx, writeReq("external-idp", "ext-writer", externalPin, func(in *ports.DelegationRequest) {
		in.Scopes = []string{"drive.read", "drive.write", "drive.delete"}
	})); !errors.Is(e, ports.ErrUnauthorized) {
		t.Fatalf("unknown write scope must be denied: %v", e)
	}
	if provider.calls != 1 {
		t.Fatalf("denied write requests reached the provider: %d", provider.calls)
	}
}
