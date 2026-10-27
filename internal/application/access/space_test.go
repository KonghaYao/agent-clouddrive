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

func configForSpaceIssuer() config.Config {
	return config.Config{OIDCIssuer: "https://issuer.test", MasterKeyIssuer: "fenix", ExternalSpaceIssuers: []string{"fenix", "partner"}}
}

type fakeSpaceProvisioner struct {
	calls int
	req   ports.ProvisionSpaceRequest
	out   ports.ProvisionedSpace
	err   error
}

func (f *fakeSpaceProvisioner) ProvisionSpace(_ context.Context, req ports.ProvisionSpaceRequest) (ports.ProvisionedSpace, error) {
	f.calls++
	f.req = req
	return f.out, f.err
}

type fakePersonalSpaceOriginal struct {
	ports.OriginalStore
	resolved int
}

func (f *fakePersonalSpaceOriginal) ResolvePersonalSpace(context.Context, ports.Credential) (ports.PersonalSpace, error) {
	f.resolved++
	return ports.PersonalSpace{SpaceID: "personal-drive", RootResourceID: "personal-root", Name: "personal"}, nil
}

func externalSpaceActor(pid, issuer, subject string) core.Actor {
	return core.Actor{
		Principal: domain.Principal{ID: pid, Active: true, StatusVerified: true, Scopes: domain.AllScopes()},
		Subject:   ports.Subject{Issuer: issuer, Subject: subject},
	}
}

func TestExternalSpaceProvisionedAndReused(t *testing.T) {
	pool, deps := masterKeyDB(t)
	ctx := context.Background()
	deps.Config.ExternalSpaceIssuers = []string{"fenix"}
	deps.Config.SpaceProvisionerEnabled = true
	deps.Config.SpaceProvisionerNativeInstance = "ocis"
	provider := &fakeSpaceProvisioner{out: ports.ProvisionedSpace{NativeInstance: "ocis", NativeSpaceID: "drive-1", NativeRootResourceID: "root-1"}}
	deps.SpaceProvisioner = provider
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(previous)
	s := New(deps)
	pid := core.ID()
	if _, e := pool.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES($1,'fenix','ext-space')`, pid); e != nil {
		t.Fatal(e)
	}
	actor := externalSpaceActor(pid, "fenix", "ext-space")
	sp, e := s.Space(ctx, actor)
	if e != nil {
		t.Fatal(e)
	}
	if core.Str(sp, "native_instance") != "ocis" || core.Str(sp, "native_space_id") != "drive-1" || core.Str(sp, "native_root_resource_id") != "root-1" {
		t.Fatalf("wrong space row: %+v", sp)
	}
	if provider.calls != 1 || provider.req.OwnerIssuer != "fenix" || provider.req.OwnerSubject != "ext-space" {
		t.Fatalf("provisioner request wrong: %+v calls=%d", provider.req, provider.calls)
	}
	if !strings.HasPrefix(provider.req.Name, "drive-ext-space-") || len(provider.req.Name) > 63 {
		t.Fatalf("deterministic name wrong: %q", provider.req.Name)
	}
	again, e := s.Space(ctx, actor)
	if e != nil || core.Str(again, "id") != core.Str(sp, "id") || provider.calls != 1 {
		t.Fatalf("existing mapping was not reused: %+v %v calls=%d", again, e, provider.calls)
	}
	var count int
	if e = pool.QueryRow(ctx, `SELECT count(*) FROM spaces WHERE owner_id=$1`, pid).Scan(&count); e != nil || count != 1 {
		t.Fatalf("space row count: %d %v", count, e)
	}
	line := buf.String()
	for _, want := range []string{"operation=space_provision", "issuer=fenix", "subject=ext-space", "drive_id=drive-1", "status=created", "name=drive-ext-space-"} {
		if !strings.Contains(line, want) {
			t.Fatalf("space audit missing %q: %s", want, line)
		}
	}
}

func TestExternalSpaceRequiresProvisioner(t *testing.T) {
	pool, deps := masterKeyDB(t)
	ctx := context.Background()
	deps.Config.ExternalSpaceIssuers = []string{"fenix"}
	deps.Config.SpaceProvisionerEnabled = false
	original := &fakePersonalSpaceOriginal{}
	deps.Original = original
	s := New(deps)
	pid := core.ID()
	if _, e := pool.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES($1,'fenix','no-provisioner')`, pid); e != nil {
		t.Fatal(e)
	}
	_, e := s.Space(ctx, externalSpaceActor(pid, "fenix", "no-provisioner"))
	if domain.ErrorCode(e) != domain.SpaceProvisionerUnavailable || domain.HTTPStatus(e) != 503 {
		t.Fatalf("expected SPACE_PROVISIONER_UNAVAILABLE/503, got %v", e)
	}
	if original.resolved != 0 {
		t.Fatalf("external issuer fell back to /me/drives: %d", original.resolved)
	}
	var count int
	if e = pool.QueryRow(ctx, `SELECT count(*) FROM spaces WHERE owner_id=$1`, pid).Scan(&count); e != nil || count != 0 {
		t.Fatalf("space must not be written: %d %v", count, e)
	}
}

func TestExternalSpaceProvisionFailureDoesNotWriteMapping(t *testing.T) {
	pool, deps := masterKeyDB(t)
	ctx := context.Background()
	deps.Config.ExternalSpaceIssuers = []string{"fenix"}
	deps.Config.SpaceProvisionerEnabled = true
	deps.Config.SpaceProvisionerNativeInstance = "ocis"
	pid := core.ID()
	if _, e := pool.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES($1,'fenix','provision-fail')`, pid); e != nil {
		t.Fatal(e)
	}
	actor := externalSpaceActor(pid, "fenix", "provision-fail")
	cases := []struct {
		name string
		out  ports.ProvisionedSpace
		err  error
		want domain.Code
	}{
		{"unavailable", ports.ProvisionedSpace{}, &ports.DependencyError{Dependency: "ocis", Operation: "provision_space", Status: 503, Code: "transport_failure", Cause: ports.ErrUnavailable}, domain.SpaceProvisionerUnavailable},
		{"unverified", ports.ProvisionedSpace{}, ports.ErrUnverified, domain.SpaceProvisionerUnavailable},
		{"empty ids", ports.ProvisionedSpace{NativeInstance: "ocis"}, nil, domain.SpaceProvisionerUnavailable},
		{"conflict without mapping", ports.ProvisionedSpace{}, ports.ErrConflict, domain.PathConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps.SpaceProvisioner = &fakeSpaceProvisioner{out: tc.out, err: tc.err}
			s := New(deps)
			_, e := s.Space(ctx, actor)
			if domain.ErrorCode(e) != tc.want {
				t.Fatalf("expected %s, got %s: %v", tc.want, domain.ErrorCode(e), e)
			}
			var count int
			if e = pool.QueryRow(ctx, `SELECT count(*) FROM spaces WHERE owner_id=$1`, pid).Scan(&count); e != nil || count != 0 {
				t.Fatalf("failed provisioning wrote a mapping: %d %v", count, e)
			}
		})
	}
}

func TestExternalSpaceConflictReusesExistingMapping(t *testing.T) {
	pool, deps := masterKeyDB(t)
	ctx := context.Background()
	deps.Config.ExternalSpaceIssuers = []string{"fenix"}
	deps.Config.SpaceProvisionerEnabled = true
	deps.Config.SpaceProvisionerNativeInstance = "ocis"
	pid := core.ID()
	if _, e := pool.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES($1,'fenix','race-user')`, pid); e != nil {
		t.Fatal(e)
	}
	if _, e := pool.Exec(ctx, `INSERT INTO spaces(owner_id,native_instance,native_space_id,native_root_resource_id) VALUES($1,'ocis','existing-drive','existing-root')`, pid); e != nil {
		t.Fatal(e)
	}
	provider := &fakeSpaceProvisioner{err: ports.ErrConflict}
	deps.SpaceProvisioner = provider
	sp, e := New(deps).provisionExternalSpace(ctx, externalSpaceActor(pid, "fenix", "race-user"))
	if e != nil || core.Str(sp, "native_space_id") != "existing-drive" || provider.calls != 1 {
		t.Fatalf("conflict was not reconciled: %+v %v calls=%d", sp, e, provider.calls)
	}
}

func TestInsertSpaceIdempotenceAndNativeConflict(t *testing.T) {
	pool, deps := masterKeyDB(t)
	ctx := context.Background()
	first, second := core.ID(), core.ID()
	for _, pid := range []string{first, second} {
		if _, e := pool.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES($1,'fenix',$2)`, pid, "subject-"+pid); e != nil {
			t.Fatal(e)
		}
	}
	s := New(deps)
	if _, e := s.insertSpace(ctx, first, "ocis", "drive-a", "root-a"); e != nil {
		t.Fatal(e)
	}
	sp, e := s.insertSpace(ctx, first, "ocis", "drive-b", "root-b")
	if e != nil || core.Str(sp, "native_space_id") != "drive-a" {
		t.Fatalf("existing owner mapping was overwritten: %+v %v", sp, e)
	}
	if _, e = s.insertSpace(ctx, second, "ocis", "drive-a", "root-a"); domain.ErrorCode(e) != domain.PathConflict {
		t.Fatalf("native unique conflict must be a conflict, got %v", e)
	}
	var count int
	if e = pool.QueryRow(ctx, `SELECT count(*) FROM spaces WHERE native_space_id='drive-a'`).Scan(&count); e != nil || count != 1 {
		t.Fatalf("native space duplicated: %d %v", count, e)
	}
}

func TestKeycloakIssuerKeepsPersonalSpacePath(t *testing.T) {
	pool, deps := masterKeyDB(t)
	ctx := context.Background()
	deps.Config.OIDCIssuer = "https://issuer.test"
	deps.Config.ExternalSpaceIssuers = []string{"fenix"}
	deps.Config.SpaceProvisionerEnabled = true
	deps.Config.SpaceProvisionerNativeInstance = "ocis"
	deps.Config.OCISURL = "http://ocis.test"
	original := &fakePersonalSpaceOriginal{}
	provider := &fakeSpaceProvisioner{out: ports.ProvisionedSpace{NativeInstance: "ocis", NativeSpaceID: "must-not-use", NativeRootResourceID: "must-not-use"}}
	deps.Original = original
	deps.Identity = &writeIdentity{cred: ports.Credential{AccessToken: "user-token"}}
	deps.SpaceProvisioner = provider
	s := New(deps)
	pid := core.ID()
	if _, e := pool.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES($1,'https://issuer.test','oidc-user')`, pid); e != nil {
		t.Fatal(e)
	}
	sp, e := s.Space(ctx, externalSpaceActor(pid, "https://issuer.test", "oidc-user"))
	if e != nil {
		t.Fatal(e)
	}
	if core.Str(sp, "native_instance") != "http://ocis.test" || core.Str(sp, "native_space_id") != "personal-drive" {
		t.Fatalf("Keycloak path changed: %+v", sp)
	}
	if original.resolved != 1 || provider.calls != 0 {
		t.Fatalf("wrong path: personal=%d provisioner=%d", original.resolved, provider.calls)
	}
}

func TestExternalSpaceIssuerAndName(t *testing.T) {
	s := New(core.Deps{Config: configForSpaceIssuer()})
	if !s.externalSpaceIssuer("fenix") || !s.externalSpaceIssuer("partner") || !s.externalSpaceIssuer("unknown-external") || s.externalSpaceIssuer("https://issuer.test") || s.externalSpaceIssuer("") {
		t.Fatal("external issuer classification wrong")
	}
	name := externalSpaceName("fenix", "user-1234")
	if name != externalSpaceName("fenix", "user-1234") || len(name) > 63 || !strings.HasPrefix(name, "drive-user-1234-") {
		t.Fatalf("deterministic name wrong: %q", name)
	}
	if externalSpaceName("fenix", "user-1234") == externalSpaceName("partner", "user-1234") {
		t.Fatal("issuer was not included in the stable name")
	}
	if got := externalSpaceName("fenix", "!!!///"); !strings.HasPrefix(got, "drive-user-") {
		t.Fatalf("unsafe subject was not sanitized: %q", got)
	}
}
