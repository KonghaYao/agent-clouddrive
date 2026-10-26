package identity

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fixture struct {
	k       *Keycloak
	key     *rsa.PrivateKey
	server  *httptest.Server
	enabled atomic.Bool
	outage  atomic.Bool
	group   atomic.Bool
	now     time.Time
}

func setup(t *testing.T) *fixture {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	f := &fixture{key: key, now: time.Unix(1800000000, 0)}
	f.enabled.Store(true)
	f.group.Store(true)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.outage.Load() {
			w.WriteHeader(503)
			return
		}
		switch r.URL.Path {
		case "/realm/certs":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "kid": "key", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
		case "/realm/token":
			if e := r.ParseForm(); e != nil {
				t.Error(e)
			}
			if r.Form.Get("grant_type") == "urn:ietf:params:oauth:grant-type:token-exchange" {
				claims := f.claims()
				claims["aud"] = "ocis"
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": f.sign(claims), "token_type": "Bearer", "expires_in": 60})
			} else {
				if r.Form.Get("client_id") != "status" || r.Form.Get("client_secret") != "secret" {
					t.Error("wrong client")
				}
				fmt.Fprint(w, `{"access_token":"service","token_type":"Bearer","expires_in":60}`)
			}
		case "/admin/users/user":
			if r.Header.Get("Authorization") != "Bearer service" {
				t.Error("wrong status auth")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "user", "enabled": f.enabled.Load()})
		case "/admin/users/user/groups":
			if f.group.Load() {
				fmt.Fprint(w, `[{"id":"group-1"}]`)
			} else {
				fmt.Fprint(w, `[]`)
			}
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.server.Close)
	f.k, e = NewKeycloak(Config{Issuer: f.server.URL + "/realm", Audience: "drive", Resource: "https://drive.example/mcp", JWKSURL: f.server.URL + "/realm/certs", TokenURL: f.server.URL + "/realm/token", AdminURL: f.server.URL + "/admin", ServiceClientID: "status", ServiceClientSecret: "secret", ExchangeClientID: "exchange", ExchangeClientSecret: "secret", OCISAudience: "ocis", Now: func() time.Time { return f.now }})
	if e != nil {
		t.Fatal(e)
	}
	return f
}
func (f *fixture) claims() map[string]any {
	return map[string]any{"iss": f.server.URL + "/realm", "sub": "user", "aud": []string{"drive", "https://drive.example/mcp"}, "scope": "drive.read drive.write", "azp": "peri", "exp": f.now.Unix() + 60, "iat": f.now.Unix()}
}
func (f *fixture) sign(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "key"})
	body, _ := json.Marshal(claims)
	s := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	sum := sha256.Sum256([]byte(s))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	return s + "." + base64.RawURLEncoding.EncodeToString(sig)
}
func TestStrictJWTValidationAndLiveDisable(t *testing.T) {
	f := setup(t)
	raw := f.sign(f.claims())
	ctx := context.Background()
	subject, e := f.k.ValidateToken(ctx, raw, ports.TokenRequirements{Scopes: []string{"drive.read"}})
	if e != nil || subject.Subject != "user" || subject.ClientID != "peri" {
		t.Fatalf("%+v %v", subject, e)
	}
	cases := map[string]any{"iss": "wrong", "aud": []string{"drive"}, "exp": f.now.Unix() - 1, "nbf": f.now.Unix() + 100, "scope": "drive.write", "sub": "../user"}
	for field, value := range cases {
		claims := f.claims()
		claims[field] = value
		_, e = f.k.ValidateToken(ctx, f.sign(claims), ports.TokenRequirements{Scopes: []string{"drive.read"}})
		if !errors.Is(e, ports.ErrUnauthorized) {
			t.Errorf("accepted invalid %s: %v", field, e)
		}
	}
	bad := strings.Split(raw, ".")
	bad[2] = "AAAAAAAA"
	if _, e = f.k.ValidateToken(ctx, strings.Join(bad, "."), ports.TokenRequirements{}); !errors.Is(e, ports.ErrUnauthorized) {
		t.Fatal("bad signature", e)
	}
	f.enabled.Store(false)
	if subject, e = f.k.ValidateToken(ctx, raw, ports.TokenRequirements{}); !errors.Is(e, ports.ErrUnauthorized) || !errors.Is(e, ports.ErrDisabled) || subject.Subject != "user" || subject.Issuer != f.k.cfg.Issuer {
		t.Fatal("disabled account did not return verified identity and denial", subject, e)
	}
	if subject, e = f.k.ValidateToken(ctx, strings.Join(bad, "."), ports.TokenRequirements{}); !errors.Is(e, ports.ErrUnauthorized) || errors.Is(e, ports.ErrDisabled) || subject.Subject != "" {
		t.Fatal("invalid signature returned a revocable identity", subject, e)
	}
	f.enabled.Store(true)
	f.outage.Store(true)
	if _, e = f.k.ValidateToken(ctx, raw, ports.TokenRequirements{}); !errors.Is(e, ports.ErrUnavailable) {
		t.Fatal("IdP outage failed open", e)
	}
}
func TestGroupsAndExchangeFailClosed(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	groups, e := f.k.Groups(ctx, f.k.cfg.Issuer, "user")
	if e != nil || len(groups) != 1 {
		t.Fatal(groups, e)
	}
	f.group.Store(false)
	groups, e = f.k.Groups(ctx, f.k.cfg.Issuer, "user")
	if e != nil || len(groups) != 0 {
		t.Fatal("membership cached", groups, e)
	}
	raw := f.sign(f.claims())
	in := ports.DelegationRequest{Subject: ports.Subject{Issuer: f.k.cfg.Issuer, Subject: "user"}, SubjectToken: raw, Audience: "ocis", Scopes: []string{"drive.read"}}
	if _, e = f.k.DelegatedCredential(ctx, in); !errors.Is(e, ports.ErrUnverified) {
		t.Fatal("unverified exchange enabled", e)
	}
	f.k.cfg.DelegationVerified = true
	credential, e := f.k.DelegatedCredential(ctx, in)
	if e != nil || credential.AccessToken == "" {
		t.Fatal(e)
	}
	in.SubjectToken = ""
	if _, e = f.k.DelegatedCredential(ctx, in); !errors.Is(e, ports.ErrUnverified) {
		t.Fatal("background impersonation allowed", e)
	}
}
