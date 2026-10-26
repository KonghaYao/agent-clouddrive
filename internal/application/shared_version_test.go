package application

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

// Real PostgreSQL authorization and version selection; uploadStore is a native
// storage contract fixture, not evidence of oCIS compatibility.
func TestSharedContentUsesAuthorizedFixedVersion(t *testing.T) {
	s, owner, store := uploadFixture(t)
	owner.Interactive = true
	ctx := context.Background()
	first := seedUpload(t, s, owner, "fixed.txt", 3)
	fid, v1 := str(first, "id"), str(first, "current_version_id")
	result, e := s.BeginUpload(ctx, owner, Object{"file_id": fid, "name": "fixed.txt", "size_bytes": int64(5), "expected_version_id": v1, "expected_metadata_revision": num(first, "metadata_revision"), "idempotency_key": "second-version"})
	if e != nil {
		t.Fatal(e)
	}
	committed, e := s.FinishUpload(ctx, owner, str(result.(Object), "id"))
	if e != nil {
		t.Fatal(e)
	}
	v2 := str(committed.(Object), "version_id")
	if v1 == v2 {
		t.Fatal("overwrite did not produce a distinct source version")
	}
	recipient := Actor{Principal: domain.Principal{ID: id(), Type: domain.ActorUser, Active: true, StatusVerified: true, Scopes: domain.AllScopes()}, Subject: ports.Subject{Issuer: "test", Subject: "recipient"}}
	if _, e = s.DB.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES($1,'test','recipient')`, recipient.Principal.ID); e != nil {
		t.Fatal(e)
	}
	grant := func(policy, vid string) string {
		t.Helper()
		request, e := s.RequestShare(ctx, owner, Object{"file_id": fid, "audience_type": "user", "audience_id": recipient.Principal.ID, "version_policy": policy, "version_id": vid, "rights": []string{"read", "download"}})
		if e != nil {
			t.Fatal(e)
		}
		approved, e := s.ApproveShare(ctx, owner, str(request.(Object), "id"))
		if e != nil {
			t.Fatal(e)
		}
		return str(approved.(Object), "id")
	}
	fixed := grant("fixed", v1)
	read := func(actor Actor, vid string, want string) {
		t.Helper()
		content, e := s.ReadContent(ctx, actor, fid, vid, nil)
		if e != nil {
			t.Fatal(e)
		}
		defer content.Body.Close()
		b, e := io.ReadAll(content.Body)
		if e != nil || string(b) != want {
			t.Fatalf("read %q: got %q want %q: %v", vid, b, want, e)
		}
	}
	read(recipient, "", "xxx")
	read(recipient, v1, "xxx")
	if _, e = s.ReadContent(ctx, recipient, fid, v2, nil); e == nil {
		t.Fatal("fixed v1 grant leaked v2")
	}
	agent := recipient
	agent.Principal.Type = domain.ActorAgent
	if _, e = s.ReadContent(ctx, agent, fid, "", nil); e == nil {
		t.Fatal("agent could read shared content")
	}
	if _, e = s.DB.Exec(ctx, `UPDATE share_grants SET state='revoked' WHERE id=$1`, fixed); e != nil {
		t.Fatal(e)
	}
	grant("latest", "")
	read(recipient, "", "xxxxx")
	if _, e = s.ReadContent(ctx, recipient, fid, v1, nil); e == nil {
		t.Fatal("latest-only grant leaked historical content")
	}
	// Ensure the fixture retained both independent immutable source bodies.
	store.uploadMu.Lock()
	defer store.uploadMu.Unlock()
	var sizes []int
	for _, body := range store.bodies {
		sizes = append(sizes, len(body))
	}
	if len(sizes) != 2 {
		t.Fatal("historical fixture was lost", sizes)
	}
}

// A request observing live IdP disable must durably invalidate the grant even
// when the account is re-enabled before the periodic identity scan runs.
func TestObservedDisableNeverRevivesPublicShare(t *testing.T) {
	s, owner, fid := shareLifecycleFixture(t)
	s.Identity = syncIdentity{enabled: true}
	request := pendingLink(t, s, owner, fid)
	approved, e := s.ApproveShare(context.Background(), owner, request)
	if e != nil {
		t.Fatal(e)
	}
	grant := approved.(Object)
	rawURL := str(grant, "url")
	split := strings.Split(rawURL, "/")
	raw := split[len(split)-1]
	if raw == "" {
		t.Fatal("missing share token")
	}
	if _, _, e = s.linkFile(context.Background(), raw, ""); e != nil {
		t.Fatal(e)
	}
	s.Identity = syncIdentity{enabled: false}
	if _, _, e = s.linkFile(context.Background(), raw, ""); e == nil {
		t.Fatal("disabled owner share remained available")
	}
	s.Identity = syncIdentity{enabled: true}
	if _, _, e = s.linkFile(context.Background(), raw, ""); e == nil {
		t.Fatal("observed disable was forgotten after re-enable")
	}
	var state string
	if e = s.DB.QueryRow(context.Background(), `SELECT state FROM share_grants WHERE id=$1`, str(grant, "id")).Scan(&state); e != nil || state != "disabled" {
		t.Fatal(state, e)
	}
}
