package ocis

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

func writeServiceConfig(url string) ServiceCredentialConfig {
	return ServiceCredentialConfig{TokenURL: url, ClientID: "write-client", ClientSecret: "write-secret", Audience: "ocis", Scopes: []string{"drive.write"}, TTL: 5 * time.Minute, Verified: true, AllowWrite: true, Now: func() time.Time { return time.Unix(1000, 0) }}
}

func TestWriteServiceCredentialFormAndRequest(t *testing.T) {
	var scope string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		scope = r.Form.Get("scope")
		_, _ = w.Write([]byte(`{"access_token":"write-access","expires_in":60,"token_type":"Bearer"}`))
	}))
	defer srv.Close()
	p, e := NewServiceCredential(writeServiceConfig(srv.URL))
	if e != nil {
		t.Fatal(e)
	}
	cred, e := p.JobCredential(context.Background(), ports.JobCredentialRequest{Subject: ports.Subject{Issuer: "fenix", Subject: "user-1"}, Audience: "ocis", Scopes: []string{"drive.write"}})
	if e != nil || cred.AccessToken != "write-access" || !cred.ExpiresAt.Equal(time.Unix(1060, 0)) {
		t.Fatalf("write credential failed: %+v %v", cred, e)
	}
	if !strings.Contains(scope, "drive.read") || !strings.Contains(scope, "drive.write") {
		t.Fatalf("write token must request both read and write: %q", scope)
	}
}

func TestWriteServiceCredentialRejectsReadOnlyAndUnknownScopes(t *testing.T) {
	p, e := NewServiceCredential(writeServiceConfig("http://ocis.test/token"))
	if e != nil {
		t.Fatal(e)
	}
	for _, scopes := range [][]string{nil, {"drive.read"}, {"drive.read", "drive.write", "drive.delete"}} {
		if _, e = p.JobCredential(context.Background(), ports.JobCredentialRequest{Audience: "ocis", Scopes: scopes}); !errors.Is(e, ports.ErrUnauthorized) {
			t.Fatalf("write provider accepted %v: %v", scopes, e)
		}
	}
}

func TestWriteServiceCredentialRequiresVerification(t *testing.T) {
	cfg := writeServiceConfig("http://ocis.test/token")
	cfg.Verified = false
	p, e := NewServiceCredential(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.JobCredential(context.Background(), ports.JobCredentialRequest{Audience: "ocis", Scopes: []string{"drive.write"}}); !errors.Is(e, ports.ErrUnverified) {
		t.Fatalf("unverified write provider must fail closed: %v", e)
	}
}

func TestWriteServiceCredentialConfigValidation(t *testing.T) {
	cases := []ServiceCredentialConfig{
		{TokenURL: "http://ocis.test/token", ClientID: "id", ClientSecret: "secret", Audience: "ocis", AllowWrite: true},
		{TokenURL: "http://ocis.test/token", ClientID: "id", ClientSecret: "secret", Audience: "ocis", AllowWrite: true, Scopes: []string{"drive.read"}},
		{TokenURL: "http://ocis.test/token", ClientID: "id", ClientSecret: "secret", Audience: "ocis", AllowWrite: true, Scopes: []string{"drive.write", "drive.delete"}},
	}
	for _, cfg := range cases {
		if _, e := NewServiceCredential(cfg); !errors.Is(e, ports.ErrInvalid) {
			t.Fatalf("unsafe write provider accepted: %+v %v", cfg, e)
		}
	}
	// The read-only instance must keep rejecting write scopes.
	if _, e := NewServiceCredential(ServiceCredentialConfig{TokenURL: "http://ocis.test/token", ClientID: "id", ClientSecret: "secret", Audience: "ocis", Scopes: []string{"drive.read", "drive.write"}}); !errors.Is(e, ports.ErrInvalid) {
		t.Fatalf("read provider accepted write scopes: %v", e)
	}
}
