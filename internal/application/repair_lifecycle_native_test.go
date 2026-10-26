package application

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/adapters/ocis"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

type repairLiveIdentity struct {
	mutationIdentity
	cred ports.Credential
}

func (i repairLiveIdentity) DelegatedCredential(context.Context, ports.DelegationRequest) (ports.Credential, error) {
	return i.cred, nil
}

type repairLiveOriginal struct {
	ports.OriginalStore
	lookup           childLookup
	pause            string
	entered, release chan struct{}
	loseRetention    bool
}

func (o *repairLiveOriginal) LookupChild(ctx context.Context, p ports.ResourceRef, n string, c ports.Credential) (ports.OriginalMetadata, error) {
	return o.lookup.LookupChild(ctx, p, n, c)
}
func (o *repairLiveOriginal) Move(ctx context.Context, r ports.MoveRequest) (ports.OriginalMetadata, error) {
	if r.Source.ResourceID == o.pause {
		close(o.entered)
		select {
		case <-o.release:
		case <-ctx.Done():
			return ports.OriginalMetadata{}, ctx.Err()
		}
	}
	return o.OriginalStore.Move(ctx, r)
}
func (o *repairLiveOriginal) CreateDirectory(ctx context.Context, r ports.DirectoryRequest) (ports.OriginalMetadata, error) {
	m, e := o.OriginalStore.CreateDirectory(ctx, r)
	if e == nil && o.loseRetention && r.Name == ".retained" {
		o.loseRetention = false
		return ports.OriginalMetadata{}, ports.ErrOutcomeUnknown
	}
	return m, e
}

// 必须显式提供私有凭据文件与可丢弃环境；不启动/修改既有 API 部署。
func TestRepairLifecycleNativeOCIS(t *testing.T) {
	if os.Getenv("REPAIR_NATIVE_WRITES") != "yes" {
		t.Skip("REPAIR_NATIVE_WRITES=yes and private delegated credential required")
	}
	data, e := os.ReadFile(os.Getenv("REPAIR_NATIVE_CREDENTIAL_FILE"))
	if e != nil {
		t.Fatal("private credential unavailable")
	}
	var c struct {
		AccessToken string `json:"access_token"`
		Issuer      string `json:"issuer"`
		Subject     string `json:"subject"`
	}
	if json.Unmarshal(data, &c) != nil || c.AccessToken == "" {
		t.Fatal("private credential invalid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	url := os.Getenv("REPAIR_NATIVE_OCIS_URL")
	native, e := ocis.New(ocis.Config{URL: url, Instance: url, VerifiedMoveHistory: true, VerifiedRevisionRead: true})
	if e != nil {
		t.Fatal("native config invalid")
	}
	cred := ports.Credential{AccessToken: c.AccessToken, ExpiresAt: time.Now().Add(time.Minute)}
	sp, e := native.ResolvePersonalSpace(ctx, cred)
	if e != nil {
		t.Fatalf("real personal-space resolution: %v", e)
	}
	namespace, e := native.CreateDirectory(ctx, ports.DirectoryRequest{Parent: ports.ResourceRef{Instance: url, SpaceID: sp.SpaceID, ResourceID: sp.RootResourceID}, Name: "agent-B-repair-" + id(), Credential: cred})
	if e != nil {
		t.Fatalf("real namespace MKCOL: %v", e)
	}
	s, a, _ := mutationFixture(t)
	repairPool(t, s, 1)
	_, e = s.DB.Exec(ctx, `UPDATE spaces SET native_instance=$1,native_space_id=$2,native_root_resource_id=$3`, url, sp.SpaceID, namespace.ResourceID)
	if e != nil {
		t.Fatal(e)
	}
	o := &repairLiveOriginal{OriginalStore: native, lookup: native}
	s.Original = o
	s.Identity = repairLiveIdentity{cred: cred}
	parent := repairMkdir(t, s, a, "target", nil)
	source := repairMkdir(t, s, a, "source", nil)
	_ = s.DB.QueryRow(ctx, `SELECT native_resource_id FROM files WHERE id=$1`, str(source, "id")).Scan(&o.pause)
	o.entered, o.release = make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, e := s.Mutate(ctx, a, "move", Object{"file_id": str(source, "id"), "target_parent_id": str(parent, "id"), "expected_metadata_revision": num(source, "metadata_revision"), "idempotency_key": "native-move"})
		done <- e
	}()
	select {
	case <-o.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	_, e = s.Mutate(ctx, a, "rename", Object{"file_id": str(parent, "id"), "name": "invalid-race", "expected_metadata_revision": num(parent, "metadata_revision"), "idempotency_key": "native-conflict"})
	close(o.release)
	if domain.ErrorCode(normalizeError(e)) != domain.OperationPending {
		t.Fatal("native race not rejected", e)
	}
	if e = <-done; e != nil {
		t.Fatal("real MOVE failed", e)
	}
	o.pause = ""
	var sourceRef, parentRef ports.ResourceRef
	sourceRef = ports.ResourceRef{Instance: url, SpaceID: sp.SpaceID}
	parentRef = sourceRef
	_ = s.DB.QueryRow(ctx, `SELECT native_resource_id FROM files WHERE id=$1`, str(source, "id")).Scan(&sourceRef.ResourceID)
	_ = s.DB.QueryRow(ctx, `SELECT native_resource_id FROM files WHERE id=$1`, str(parent, "id")).Scan(&parentRef.ResourceID)
	m, e := native.LookupChild(ctx, parentRef, "source", cred)
	if e != nil || m.ResourceID != sourceRef.ResourceID {
		t.Fatal("real tree mismatch", e)
	}
	current, e := one(ctx, s.DB, `SELECT to_jsonb(f) FROM files f WHERE id=$1`, str(source, "id"))
	if e != nil {
		t.Fatal(e)
	}
	o.loseRetention = true
	in := Object{"file_id": str(source, "id"), "expected_metadata_revision": num(current, "metadata_revision"), "idempotency_key": "native-delete"}
	if _, e = s.Mutate(ctx, a, "delete", in); domain.ErrorCode(e) != domain.OperationPending {
		t.Fatal("lost real MKCOL response not held", e)
	}
	_, e = s.DB.Exec(ctx, `UPDATE retention_roots SET retry_at=now()`)
	if e != nil {
		t.Fatal(e)
	}
	restarted := &Service{DB: s.DB, Config: s.Config, Original: s.Original, Identity: s.Identity}
	if e = restarted.ReconcileRetentionRoots(ctx); e != nil {
		t.Fatal("real retention recovery", e)
	}
	if e = restarted.ReconcileRetentionRoots(ctx); e != nil {
		t.Fatal(e)
	}
	r, e := restarted.Mutate(ctx, a, "delete", in)
	if e != nil {
		t.Fatal("real delete", e)
	}
	f := r.(map[string]any)["file"].(map[string]any)
	var trash string
	_ = s.DB.QueryRow(ctx, `SELECT id::text FROM trash_entries WHERE root_file_id=$1`, str(f, "id")).Scan(&trash)
	a.Principal.Type = domain.ActorAgent
	a.Principal.Scopes = map[string]bool{domain.ScopeRestore: true}
	r, e = restarted.Mutate(ctx, a, "restore", Object{"trash_id": trash, "target_parent_id": str(parent, "id"), "expected_parent_revision": num(parent, "metadata_revision"), "expected_metadata_revision": num(f, "metadata_revision"), "idempotency_key": "native-restore", "name": "restored"})
	if e != nil || str(r.(map[string]any), "state") != "committed" {
		t.Fatal("real restore-only", e)
	}
	m, e = native.LookupChild(ctx, parentRef, "restored", cred)
	if e != nil || m.ResourceID != sourceRef.ResourceID {
		t.Fatal("restore native identity mismatch", e)
	}
	a.Principal.Scopes = domain.AllScopes()
	r, e = restarted.Batch(ctx, a, Object{"idempotency_key": "native-batch", "items": []Object{{"item_id": "one", "command": "mkdir", "input": Object{"name": "batch"}}}})
	if e != nil || str(r.(map[string]any), "state") != "completed" {
		t.Fatal("real single-connection batch", e)
	}
	t.Log("真实 oCIS MKCOL/MOVE/LookupChild + PostgreSQL：并发门禁、保留根响应丢失/重启恢复、restore-only 和单连接 batch 全部通过；隔离目录保留供复核：", namespace.ResourceID)
}
