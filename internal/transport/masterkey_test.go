package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agentclouddrive/agent-clouddrive/internal/application"
	"github.com/agentclouddrive/agent-clouddrive/internal/config"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/cloudwego/hertz/pkg/app"
)

func masterKeyAPI() *API {
	return &API{Service: &application.Service{Config: config.Config{
		MasterKeys:         map[string]string{"k1": "secret1"},
		MasterKeyDefaultID: "k1",
		MasterKeyIssuer:    "fenix",
	}}}
}

func TestMasterKeyRequestRouting(t *testing.T) {
	a := masterKeyAPI()
	if _, ok := a.masterKeyRequest("oidc-token", "", "", "127.0.0.1:1", "/v1/files"); ok {
		t.Fatal("OIDC token must stay on the OIDC path")
	}
	in, ok := a.masterKeyRequest("secret1", "", "", "127.0.0.1:1", "/v1/files")
	if !ok || in.Bearer != "secret1" || in.OnBehalfOf != "" || in.Route != "/v1/files" {
		t.Fatalf("master key without assertion must route for a 400: %+v %v", in, ok)
	}
	in, ok = a.masterKeyRequest("secret1", "k1", "user-1", "127.0.0.1:1", "/v1/files")
	if !ok || in.KeyID != "k1" || in.OnBehalfOf != "user-1" || in.RemoteIP != "127.0.0.1:1" {
		t.Fatalf("assertion routing wrong: %+v %v", in, ok)
	}
	if _, ok = a.masterKeyRequest("oidc-token", "", "user-1", "127.0.0.1:1", "/v1/files"); !ok {
		t.Fatal("X-On-Behalf-Of must force the master-key path")
	}
	if _, ok = (&API{Service: &application.Service{}}).masterKeyRequest("secret1", "", "", "127.0.0.1:1", "/v1"); ok {
		t.Fatal("unconfigured master key routed to master-key auth")
	}
}

func TestAuthStatusMapping(t *testing.T) {
	if got := authStatus(application.ErrInvalidOnBehalfOf); got != http.StatusBadRequest {
		t.Fatalf("assertion error must map to 400, got %d", got)
	}
	if got := authStatus(domain.NewError(domain.Unauthenticated, "invalid")); got != http.StatusUnauthorized {
		t.Fatalf("invalid key must map to 401, got %d", got)
	}
	if got := authStatus(domain.NewError(domain.AccessDenied, "denied")); got != http.StatusForbidden {
		t.Fatalf("denied must map to 403, got %d", got)
	}
}

func TestHTTPActorMasterKeyRouting(t *testing.T) {
	a := masterKeyAPI()
	ctx := context.Background()
	request := func(headers map[string]string) *app.RequestContext {
		c := app.NewContext(0)
		c.Request.SetRequestURI("/v1/files")
		for k, v := range headers {
			c.Request.Header.Set(k, v)
		}
		return c
	}
	if _, e := a.actor(ctx, request(map[string]string{"Authorization": "Bearer secret1"})); !errors.Is(e, application.ErrInvalidOnBehalfOf) {
		t.Fatalf("master key without X-On-Behalf-Of must be a 400, got %v", e)
	}
	if _, e := a.actor(ctx, request(map[string]string{"Authorization": "Bearer secret1", "X-On-Behalf-Of": "user-1"})); domain.ErrorCode(e) != domain.AccessDenied {
		t.Fatalf("empty CIDR allowlist must deny with 403, got %v", e)
	}
	if _, e := a.actor(ctx, request(map[string]string{"Authorization": "Bearer oidc-token"})); domain.ErrorCode(e) != domain.Unauthenticated {
		t.Fatalf("OIDC path changed: %v", e)
	}
}

func TestMCPMasterKeyStatuses(t *testing.T) {
	cases := []struct {
		name     string
		keyID    string
		onBehalf string
		want     int
	}{
		{"missing assertion", "", "", http.StatusBadRequest},
		{"unsafe assertion", "", "user/../admin", http.StatusBadRequest},
		{"wrong key id", "k2", "user-1", http.StatusUnauthorized},
		{"unknown key", "", "user-1", http.StatusUnauthorized}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := masterKeyAPI()
			request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
			bearer := "secret1"
			if tc.name == "unknown key" {
				bearer = "other"
			}
			request.Header.Set("Authorization", "Bearer "+bearer)
			request.Header.Set("Content-Type", "application/json")
			if tc.keyID != "" {
				request.Header.Set("X-Key-Id", tc.keyID)
			}
			if tc.onBehalf != "" {
				request.Header.Set("X-On-Behalf-Of", tc.onBehalf)
			}
			recorder := httptest.NewRecorder()
			a.MCPHandler().ServeHTTP(recorder, request)
			if recorder.Code != tc.want {
				t.Fatalf("expected %d, got %d: %s", tc.want, recorder.Code, recorder.Body.String())
			}
		})
	}
}
