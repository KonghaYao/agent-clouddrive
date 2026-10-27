package access

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/agentclouddrive/agent-clouddrive/internal/application/core"
	"github.com/agentclouddrive/agent-clouddrive/internal/config"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

type writeIdentity struct {
	requests []ports.DelegationRequest
	cred     ports.Credential
	err      error
}

func (i *writeIdentity) ValidateToken(context.Context, string, ports.TokenRequirements) (ports.Subject, error) {
	return ports.Subject{}, ports.ErrUnauthorized
}
func (i *writeIdentity) SubjectStatus(context.Context, string, string) (ports.SubjectStatus, error) {
	return ports.SubjectStatus{}, ports.ErrUnavailable
}
func (i *writeIdentity) Groups(context.Context, string, string) ([]string, error) { return nil, nil }
func (i *writeIdentity) DelegatedCredential(_ context.Context, in ports.DelegationRequest) (ports.Credential, error) {
	i.requests = append(i.requests, in)
	return i.cred, i.err
}

func writeActor() core.Actor {
	return core.Actor{
		Principal: domain.Principal{ID: "principal-1", Active: true, StatusVerified: true, Scopes: domain.ParseScopes("drive.write")},
		Subject:   ports.Subject{Issuer: "fenix", Subject: "user-1"},
	}
}

func writeConfig() config.Config {
	return config.Config{ExternalWriteEnabled: true, ExternalWriteIssuers: []string{"fenix"}, OCISAudience: "ocis"}
}

func TestCredentialForWriteExternalUsesWriteService(t *testing.T) {
	ident := &writeIdentity{cred: ports.Credential{AccessToken: "write-token"}}
	s := New(core.Deps{Config: writeConfig(), Identity: ident})
	source := ports.RevisionRef{ResourceRef: ports.ResourceRef{Instance: "native", SpaceID: "space-1", ResourceID: "resource-1"}, RevisionID: "rev", ETag: "etag"}
	cred, e := s.CredentialForWrite(context.Background(), writeActor(), source)
	if e != nil || cred.AccessToken != "write-token" {
		t.Fatalf("write credential failed: %+v %v", cred, e)
	}
	if len(ident.requests) != 1 {
		t.Fatalf("unexpected identity calls: %d", len(ident.requests))
	}
	req := ident.requests[0]
	if req.SubjectToken != "" || req.Audience != "ocis" || req.Source.ResourceID != "resource-1" {
		t.Fatalf("wrong write request: %+v", req)
	}
	if len(req.Scopes) != 2 || req.Scopes[0] != "drive.read" || req.Scopes[1] != "drive.write" {
		t.Fatalf("write request scopes wrong: %+v", req.Scopes)
	}
}

func TestCredentialForWriteUsesUserToken(t *testing.T) {
	ident := &writeIdentity{cred: ports.Credential{AccessToken: "user-write"}}
	cfg := writeConfig()
	cfg.ExternalWriteEnabled = false
	s := New(core.Deps{Config: cfg, Identity: ident})
	actor := writeActor()
	actor.Token = "user-token"
	if _, e := s.CredentialForWrite(context.Background(), actor, ports.RevisionRef{}); e != nil {
		t.Fatal(e)
	}
	if len(ident.requests) != 1 {
		t.Fatalf("unexpected identity calls: %d", len(ident.requests))
	}
	req := ident.requests[0]
	if req.SubjectToken != "user-token" || len(req.Scopes) != 0 {
		t.Fatalf("OIDC write must use the user token: %+v", req)
	}
}

func TestCredentialForWriteFailClosed(t *testing.T) {
	source := ports.RevisionRef{ResourceRef: ports.ResourceRef{ResourceID: "resource-1"}}
	cases := []struct {
		name  string
		cfg   config.Config
		actor core.Actor
		want  domain.Code
	}{
		{"missing scope", writeConfig(), func() core.Actor { a := writeActor(); a.Principal.Scopes = domain.ParseScopes("drive.read"); return a }(), domain.InsufficientScope},
		{"external writes disabled", func() config.Config { c := writeConfig(); c.ExternalWriteEnabled = false; return c }(), writeActor(), domain.AccessDenied},
		{"issuer not allowed", func() config.Config { c := writeConfig(); c.ExternalWriteIssuers = []string{"other-idp"}; return c }(), writeActor(), domain.AccessDenied},
		{"identity unavailable", writeConfig(), writeActor(), domain.DependencyUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ident *writeIdentity
			var deps core.Deps
			if tc.name == "identity unavailable" {
				deps = core.Deps{Config: tc.cfg}
			} else {
				ident = &writeIdentity{cred: ports.Credential{AccessToken: "should-not-issue"}}
				deps = core.Deps{Config: tc.cfg, Identity: ident}
			}
			_, e := New(deps).CredentialForWrite(context.Background(), tc.actor, source)
			if domain.ErrorCode(e) != tc.want {
				t.Fatalf("expected %s, got %s: %v", tc.want, domain.ErrorCode(e), e)
			}
			if ident != nil && len(ident.requests) != 0 {
				t.Fatal("denied write reached the identity adapter")
			}
		})
	}
}

func TestCredentialForWriteAudit(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(previous)
	ident := &writeIdentity{cred: ports.Credential{AccessToken: "write-token-secret"}}
	s := New(core.Deps{Config: writeConfig(), Identity: ident})
	ctx := WithWriteOperation(context.Background(), "mkdir")
	if _, e := s.CredentialForWrite(ctx, writeActor(), ports.RevisionRef{ResourceRef: ports.ResourceRef{ResourceID: "resource-1"}}); e != nil {
		t.Fatal(e)
	}
	line := buf.String()
	for _, want := range []string{"credential_kind=write_service", "subject=user-1", "issuer=fenix", "source=resource-1", "operation=mkdir"} {
		if !strings.Contains(line, want) {
			t.Fatalf("write audit missing %q: %s", want, line)
		}
	}
	if strings.Contains(line, "write-token-secret") {
		t.Fatalf("write audit leaked the token: %s", line)
	}
	buf.Reset()
	actor := writeActor()
	actor.Token = "user-token"
	if _, e := s.CredentialForWrite(context.Background(), actor, ports.RevisionRef{}); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(buf.String(), "credential_kind=write_service") {
		t.Fatalf("OIDC user token must not be audited as a write service credential: %s", buf.String())
	}
}
