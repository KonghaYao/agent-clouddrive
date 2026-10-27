package ocis

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

func provisionerConfig(url string) SpaceProvisionerConfig {
	return SpaceProvisionerConfig{
		URL: url, Instance: "ocis",
		TokenURL: url + "/token", ClientID: "provisioner-client", ClientSecret: "provisioner-secret",
		Audience: "ocis", Scopes: []string{"openid"}, Verified: true,
	}
}

func TestSpaceProvisionerCreatesProjectDrive(t *testing.T) {
	var tokenRequests, driveRequests atomic.Int64
	var formOK, driveOK bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenRequests.Add(1)
			_ = r.ParseForm()
			formOK = r.Form.Get("grant_type") == "client_credentials" &&
				r.Form.Get("client_id") == "provisioner-client" &&
				r.Form.Get("client_secret") == "provisioner-secret" &&
				r.Form.Get("audience") == "ocis" &&
				r.Form.Get("scope") == "openid"
			_, _ = w.Write([]byte(`{"access_token":"admin-token","token_type":"Bearer","expires_in":60}`))
		case "/graph/v1.0/drives":
			driveRequests.Add(1)
			var body struct {
				DriveType string `json:"driveType"`
				Name      string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			driveOK = r.Header.Get("Authorization") == "Bearer admin-token" &&
				strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") &&
				body.DriveType == "project" && body.Name == "drive-user-1234abcd"
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"drive-1","root":{"id":"root-1"},"driveAlias":"project/drive-user-1234abcd"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p, e := NewSpaceProvisioner(provisionerConfig(srv.URL))
	if e != nil {
		t.Fatal(e)
	}
	out, e := p.ProvisionSpace(context.Background(), ports.ProvisionSpaceRequest{OwnerIssuer: "fenix", OwnerSubject: "user-1234abcd", Name: "drive-user-1234abcd"})
	if e != nil {
		t.Fatal(e)
	}
	if out.NativeInstance != "ocis" || out.NativeSpaceID != "drive-1" || out.NativeRootResourceID != "root-1" {
		t.Fatalf("wrong provisioned space: %+v", out)
	}
	if !formOK || !driveOK || tokenRequests.Load() != 1 || driveRequests.Load() != 1 {
		t.Fatalf("provisioner requests wrong: form=%v drive=%v token=%d drive=%d", formOK, driveOK, tokenRequests.Load(), driveRequests.Load())
	}
}

func TestSpaceProvisionerRejectsIncompleteOrConflictingResponse(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"empty id", http.StatusCreated, `{"root":{"id":"root-1"}}`, ports.ErrIntegrity},
		{"empty root", http.StatusCreated, `{"id":"drive-1","root":{}}`, ports.ErrIntegrity},
		{"conflict", http.StatusConflict, `{"error":"exists"}`, ports.ErrConflict},
		{"unauthorized", http.StatusUnauthorized, `{"error":"denied"}`, ports.ErrUnauthorized},
		{"server error", http.StatusInternalServerError, `{"error":"boom"}`, ports.ErrOutcomeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					_, _ = w.Write([]byte(`{"access_token":"admin-token","token_type":"Bearer"}`))
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			p, e := NewSpaceProvisioner(provisionerConfig(srv.URL))
			if e != nil {
				t.Fatal(e)
			}
			_, e = p.ProvisionSpace(context.Background(), ports.ProvisionSpaceRequest{OwnerIssuer: "fenix", OwnerSubject: "user-1", Name: "drive-user-1"})
			if !errors.Is(e, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, e)
			}
			if strings.Contains(e.Error(), "admin-token") || strings.Contains(e.Error(), tc.body) {
				t.Fatalf("error leaked token or body: %v", e)
			}
		})
	}
}

func TestSpaceProvisionerTokenFailureAndNoLeak(t *testing.T) {
	var driveRequests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client","access_token":"leaked-token"}`))
			return
		}
		driveRequests.Add(1)
	}))
	defer srv.Close()
	p, e := NewSpaceProvisioner(provisionerConfig(srv.URL))
	if e != nil {
		t.Fatal(e)
	}
	_, e = p.ProvisionSpace(context.Background(), ports.ProvisionSpaceRequest{OwnerIssuer: "fenix", OwnerSubject: "user-1", Name: "drive-user-1"})
	if !errors.Is(e, ports.ErrUnauthorized) {
		t.Fatalf("expected token authorization error, got %v", e)
	}
	if driveRequests.Load() != 0 {
		t.Fatal("drive was requested after token failure")
	}
	if strings.Contains(e.Error(), "leaked-token") || strings.Contains(e.Error(), "provisioner-secret") {
		t.Fatalf("error leaked a secret: %v", e)
	}
}

func TestSpaceProvisionerRequiresVerificationAndValidInput(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	cfg := provisionerConfig(srv.URL)
	cfg.Verified = false
	p, e := NewSpaceProvisioner(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.ProvisionSpace(context.Background(), ports.ProvisionSpaceRequest{OwnerIssuer: "fenix", OwnerSubject: "user-1", Name: "drive-user-1"}); !errors.Is(e, ports.ErrUnverified) {
		t.Fatalf("unverified provisioner must fail closed: %v", e)
	}
	cfg.Verified = true
	p, e = NewSpaceProvisioner(cfg)
	if e != nil {
		t.Fatal(e)
	}
	for _, req := range []ports.ProvisionSpaceRequest{
		{Name: "drive"},
		{OwnerIssuer: "fenix", Name: "drive"},
		{OwnerIssuer: "fenix", OwnerSubject: "user", Name: ""},
		{OwnerIssuer: "fenix", OwnerSubject: "user", Name: "../escape"},
	} {
		if _, e = p.ProvisionSpace(context.Background(), req); !errors.Is(e, ports.ErrInvalid) {
			t.Fatalf("invalid request %+v accepted: %v", req, e)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("invalid requests reached HTTP: %d", hits.Load())
	}
}

func TestSpaceProvisionerConfigValidation(t *testing.T) {
	valid := provisionerConfig("http://ocis.test")
	cases := []SpaceProvisionerConfig{
		{},
		func() SpaceProvisionerConfig { c := valid; c.URL = "file:///tmp/ocis"; return c }(),
		func() SpaceProvisionerConfig { c := valid; c.TokenURL = "http://ocis.test/token?x=1"; return c }(),
		func() SpaceProvisionerConfig { c := valid; c.Instance = ""; return c }(),
		func() SpaceProvisionerConfig { c := valid; c.ClientID = ""; return c }(),
		func() SpaceProvisionerConfig { c := valid; c.ClientSecret = ""; return c }(),
		func() SpaceProvisionerConfig { c := valid; c.Audience = ""; return c }(),
		func() SpaceProvisionerConfig { c := valid; c.Scopes = []string{"openid", "profile"}; return c }(),
	}
	for _, cfg := range cases {
		if _, e := NewSpaceProvisioner(cfg); !errors.Is(e, ports.ErrInvalid) {
			t.Fatalf("unsafe provisioner config accepted: %+v %v", cfg, e)
		}
	}
}

func TestOcisStoreAcceptsProvisionerInstanceAlias(t *testing.T) {
	store := &Store{cfg: Config{Instance: "http://ocis.test", InstanceAliases: []string{"ocis"}}}
	if e := store.resource(ports.ResourceRef{Instance: "ocis", SpaceID: "space-1", ResourceID: "root-1"}); e != nil {
		t.Fatalf("provisioner instance alias rejected: %v", e)
	}
	if e := store.resource(ports.ResourceRef{Instance: "http://ocis.test", SpaceID: "space-1", ResourceID: "root-1"}); e != nil {
		t.Fatalf("primary instance rejected: %v", e)
	}
	if e := store.resource(ports.ResourceRef{Instance: "other", SpaceID: "space-1", ResourceID: "root-1"}); e == nil {
		t.Fatal("foreign instance accepted")
	}
}
