package ocis

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

func serviceCredentialConfig(url string) ServiceCredentialConfig {
	return ServiceCredentialConfig{TokenURL: url, ClientID: "service-client", ClientSecret: "service-secret", Audience: "ocis", Scopes: []string{"drive.read"}, TTL: 5 * time.Minute, Verified: true, Now: func() time.Time { return time.Unix(1000, 0) }}
}

func TestServiceCredentialFormAndExpiry(t *testing.T) {
	var formSeen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e := r.ParseForm(); e != nil {
			t.Errorf("parse form: %v", e)
		}
		formSeen = r.Form.Get("grant_type") == "client_credentials" &&
			r.Form.Get("client_id") == "service-client" &&
			r.Form.Get("client_secret") == "service-secret" &&
			r.Form.Get("audience") == "ocis" &&
			r.Form.Get("scope") == "drive.read"
		_, _ = w.Write([]byte(`{"access_token":"service-access","expires_in":30,"token_type":"Bearer"}`))
	}))
	defer srv.Close()
	p, e := NewServiceCredential(serviceCredentialConfig(srv.URL))
	if e != nil {
		t.Fatal(e)
	}
	cred, e := p.JobCredential(context.Background(), ports.JobCredentialRequest{Subject: ports.Subject{Issuer: "fenix", Subject: "user-1"}, Audience: "ocis", JobID: "job-1", Scopes: []string{"drive.read"}})
	if e != nil {
		t.Fatal(e)
	}
	if !formSeen {
		t.Fatal("client credentials form was not correct")
	}
	if cred.AccessToken != "service-access" || !cred.ExpiresAt.Equal(time.Unix(1030, 0)) {
		t.Fatalf("credential wrong: %+v", cred)
	}
}

func TestServiceCredentialReadScopesAllowOpenID(t *testing.T) {
	var scope string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e := r.ParseForm(); e != nil {
			t.Errorf("parse form: %v", e)
		}
		scope = r.Form.Get("scope")
		_, _ = w.Write([]byte(`{"access_token":"service-access","expires_in":30,"token_type":"Bearer"}`))
	}))
	defer srv.Close()
	cfg := serviceCredentialConfig(srv.URL)
	cfg.Scopes = []string{"openid", "drive.read"}
	p, e := NewServiceCredential(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.JobCredential(context.Background(), ports.JobCredentialRequest{Audience: "ocis", Scopes: []string{"drive.read", "openid"}}); e != nil {
		t.Fatal(e)
	}
	if scope != "openid drive.read" {
		t.Fatalf("read credential must request openid for userinfo: %q", scope)
	}
}

func TestServiceCredentialExpiryIsCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"service-access","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer srv.Close()
	cfg := serviceCredentialConfig(srv.URL)
	cfg.TTL = 5 * time.Minute
	p, e := NewServiceCredential(cfg)
	if e != nil {
		t.Fatal(e)
	}
	cred, e := p.JobCredential(context.Background(), ports.JobCredentialRequest{Audience: "ocis"})
	if e != nil {
		t.Fatal(e)
	}
	if !cred.ExpiresAt.Equal(time.Unix(1300, 0)) {
		t.Fatalf("TTL cap not applied: %v", cred.ExpiresAt)
	}
}

func TestServiceCredentialGatesAndFailClosed(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"service-access","expires_in":60,"token_type":"Bearer"}`))
	}))
	defer srv.Close()
	cfg := serviceCredentialConfig(srv.URL)
	cfg.Verified = false
	p, e := NewServiceCredential(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.JobCredential(context.Background(), ports.JobCredentialRequest{Audience: "ocis"}); !errors.Is(e, ports.ErrUnverified) {
		t.Fatalf("unverified provider must fail closed: %v", e)
	}
	if hits.Load() != 0 {
		t.Fatal("unverified provider contacted the token endpoint")
	}
	cfg.Verified = true
	p, e = NewServiceCredential(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.JobCredential(context.Background(), ports.JobCredentialRequest{Audience: "other"}); !errors.Is(e, ports.ErrInvalid) {
		t.Fatalf("wrong audience must be rejected: %v", e)
	}
	if _, e = p.JobCredential(context.Background(), ports.JobCredentialRequest{Audience: "ocis", Scopes: []string{"drive.write"}}); !errors.Is(e, ports.ErrUnauthorized) {
		t.Fatalf("write scope must be rejected: %v", e)
	}
	if hits.Load() != 0 {
		t.Fatal("invalid requests reached the token endpoint")
	}
}

func TestServiceCredentialRejectsTokenEndpointAndDoesNotLeak(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client","access_token":"leaked-token"}`))
	}))
	defer srv.Close()
	p, e := NewServiceCredential(serviceCredentialConfig(srv.URL))
	if e != nil {
		t.Fatal(e)
	}
	_, e = p.JobCredential(context.Background(), ports.JobCredentialRequest{Audience: "ocis"})
	if e == nil {
		t.Fatal("rejected token endpoint must fail")
	}
	var dep *ports.DependencyError
	if !errors.As(e, &dep) || dep.Status != http.StatusUnauthorized || dep.Code != "token_rejected" {
		t.Fatalf("wrong dependency error: %v", e)
	}
	if strings.Contains(e.Error(), "leaked-token") || strings.Contains(e.Error(), "service-secret") {
		t.Fatalf("error leaked a secret: %v", e)
	}
}

func TestServiceCredentialConfigValidation(t *testing.T) {
	cases := []ServiceCredentialConfig{
		{},
		{TokenURL: "file:///tmp/token", ClientID: "id", ClientSecret: "secret", Audience: "ocis"},
		{TokenURL: "http://ocis.test/token", ClientID: "", ClientSecret: "secret", Audience: "ocis"},
		{TokenURL: "http://ocis.test/token", ClientID: "id", ClientSecret: "", Audience: "ocis"},
		{TokenURL: "http://ocis.test/token", ClientID: "id", ClientSecret: "secret", Audience: ""},
		{TokenURL: "http://ocis.test/token", ClientID: "id", ClientSecret: "secret", Audience: "ocis", Scopes: []string{"drive.write"}},
		{TokenURL: "http://ocis.test/token", ClientID: "id", ClientSecret: "secret", Audience: "ocis", Scopes: []string{"openid"}},
	}
	for _, cfg := range cases {
		if _, e := NewServiceCredential(cfg); !errors.Is(e, ports.ErrInvalid) {
			t.Fatalf("unsafe config accepted: %+v %v", cfg, e)
		}
	}
}
