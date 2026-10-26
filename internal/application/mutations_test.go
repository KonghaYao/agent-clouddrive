package application

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/config"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type mutationStore struct {
	mu               sync.Mutex
	objects          map[string]ports.OriginalMetadata
	moveApplyUnknown bool
	moveErr          error
	mkdirErr         error
	moves, mkdirs    int
}

func (m *mutationStore) Stat(_ context.Context, r ports.ResourceRef, _ ports.Credential) (ports.OriginalMetadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.objects[r.ResourceID]
	if !ok {
		return v, ports.ErrNotFound
	}
	return v, nil
}
func (m *mutationStore) Move(_ context.Context, r ports.MoveRequest) (ports.OriginalMetadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.moves++
	if m.moveErr != nil && !m.moveApplyUnknown {
		return ports.OriginalMetadata{}, m.moveErr
	}
	v, ok := m.objects[r.Source.ResourceID]
	if !ok {
		return v, ports.ErrNotFound
	}
	if r.ExpectedETag != v.ETag {
		return v, ports.ErrConflict
	}
	v.Name = r.Name
	v.RelativePath = r.Name
	v.ETag = "changed"
	m.objects[r.Source.ResourceID] = v
	if m.moveApplyUnknown {
		return ports.OriginalMetadata{}, ports.ErrOutcomeUnknown
	}
	return v, nil
}
func (m *mutationStore) CreateDirectory(_ context.Context, r ports.DirectoryRequest) (ports.OriginalMetadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mkdirs++
	if m.mkdirErr != nil {
		return ports.OriginalMetadata{}, m.mkdirErr
	}
	v := ports.OriginalMetadata{ResourceRef: ports.ResourceRef{Instance: r.Parent.Instance, SpaceID: r.Parent.SpaceID, ResourceID: id()}, Name: r.Name, ETag: "etag", IsDirectory: true}
	m.objects[v.ResourceID] = v
	return v, nil
}
func (m *mutationStore) CreateUpload(context.Context, ports.UploadRequest) (ports.UploadInfo, error) {
	return ports.UploadInfo{}, ports.ErrUnverified
}
func (m *mutationStore) StatUpload(context.Context, ports.UploadInfo, ports.Credential) (ports.UploadStatus, error) {
	return ports.UploadStatus{}, ports.ErrUnverified
}
func (m *mutationStore) OpenRevision(context.Context, ports.RevisionRef, *ports.ByteRange, ports.Credential) (ports.Content, error) {
	return ports.Content{Body: io.NopCloser(strings.NewReader("x"))}, nil
}
func (m *mutationStore) Revisions(context.Context, ports.ResourceRef, ports.Credential) ([]ports.NativeRevision, error) {
	return nil, nil
}
func (m *mutationStore) Reconcile(context.Context, ports.ReconcileRequest) (ports.ReconcileResult, error) {
	return ports.ReconcileResult{}, ports.ErrUnverified
}

func (m *mutationStore) LookupChild(_ context.Context, parent ports.ResourceRef, name string, _ ports.Credential) (ports.OriginalMetadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.objects {
		if v.SpaceID == parent.SpaceID && v.Name == name {
			return v, nil
		}
	}
	return ports.OriginalMetadata{}, ports.ErrNotFound
}

type mutationIdentity struct{}

func (mutationIdentity) IdentityEvents(context.Context, string, string) (ports.IdentityEventBatch, error) {
	return ports.IdentityEventBatch{Anchor: "fixture-empty", Continuous: true}, nil
}

func (mutationIdentity) ValidateToken(context.Context, string, ports.TokenRequirements) (ports.Subject, error) {
	return ports.Subject{}, nil
}
func (mutationIdentity) SubjectStatus(context.Context, string, string) (ports.SubjectStatus, error) {
	return ports.SubjectStatus{Enabled: true}, nil
}
func (mutationIdentity) Groups(context.Context, string, string) ([]string, error) { return nil, nil }
func (mutationIdentity) DelegatedCredential(context.Context, ports.DelegationRequest) (ports.Credential, error) {
	return ports.Credential{AccessToken: "test-only", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func mutationFixture(t *testing.T) (*Service, Actor, *mutationStore) {
	t.Helper()
	dsn := os.Getenv("DRIVE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("DRIVE_TEST_DATABASE_URL not set; PostgreSQL integration test")
	}
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := "mutation_" + strings.ReplaceAll(id(), "-", "")
	if _, e = admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); e != nil {
		t.Fatal(e)
	}
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 10
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		admin.Close()
	})
	files, e := filepath.Glob("../../db/migrations/*.up.sql")
	if e != nil {
		t.Fatal(e)
	}
	sort.Strings(files)
	for _, file := range files {
		sql, e := os.ReadFile(file)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = pool.Exec(ctx, string(sql)); e != nil {
			t.Fatalf("migration %s: %v", file, e)
		}
	}
	owner, space := id(), id()
	_, e = pool.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES($1,'test','owner')`, owner)
	if e != nil {
		t.Fatal(e)
	}
	_, e = pool.Exec(ctx, `INSERT INTO spaces(id,owner_id,native_instance,native_space_id,native_root_resource_id) VALUES($1,$2,'http://ocis.test','native-space','root')`, space, owner)
	if e != nil {
		t.Fatal(e)
	}
	_, e = pool.Exec(ctx, `INSERT INTO quota_accounts(owner_id,limit_bytes,used_bytes) VALUES($1,100000,0)`, owner)
	if e != nil {
		t.Fatal(e)
	}
	store := &mutationStore{objects: map[string]ports.OriginalMetadata{}}
	service := &Service{DB: pool, Config: config.Config{OCISAudience: "ocis", TrashDir: ".retained"}, Original: store, Identity: mutationIdentity{}}
	actor := Actor{Principal: domain.Principal{ID: owner, Type: domain.ActorUser, Active: true, StatusVerified: true, Scopes: domain.AllScopes()}, Subject: ports.Subject{Issuer: "test", Subject: "owner"}}
	return service, actor, store
}
func mutationCreate(t *testing.T, s *Service, a Actor, name string) Object {
	t.Helper()
	result, e := s.Mutate(context.Background(), a, "mkdir", Object{"name": name, "idempotency_key": "mkdir-" + name})
	if e != nil {
		t.Fatal(e)
	}
	r := result.(map[string]any)
	if str(r, "state") != "committed" {
		t.Fatal(r)
	}
	return r["file"].(map[string]any)
}

func TestMutationMkdirRenameAndIdempotence(t *testing.T) {
	s, a, store := mutationFixture(t)
	ctx := context.Background()
	f := mutationCreate(t, s, a, "original")
	in := Object{"file_id": str(f, "id"), "name": "renamed", "expected_metadata_revision": num(f, "metadata_revision"), "idempotency_key": "rename-key"}
	first, e := s.Mutate(ctx, a, "rename", in)
	if e != nil {
		t.Fatal(e)
	}
	second, e := s.Mutate(ctx, a, "rename", in)
	if e != nil {
		t.Fatal(e)
	}
	if first.(map[string]any)["operation_id"] != second.(map[string]any)["operation_id"] || store.moves != 1 {
		t.Fatal("idempotent retry repeated remote mutation", first, second, store.moves)
	}
	var fid, path string
	var revision, versions int64
	if e = s.DB.QueryRow(ctx, `SELECT id::text,path,metadata_revision,(SELECT count(*) FROM file_versions WHERE file_id=files.id) FROM files WHERE id=$1`, str(f, "id")).Scan(&fid, &path, &revision, &versions); e != nil {
		t.Fatal(e)
	}
	if fid != str(f, "id") || path != "/renamed" || revision != 3 || versions != 0 {
		t.Fatal(fid, path, revision, versions)
	}
	in["name"] = "different"
	_, e = s.Mutate(ctx, a, "rename", in)
	if domain.ErrorCode(e) != domain.IdempotencyConflict {
		t.Fatal(e)
	}
}
func TestMutationUnknownOutcomeBlocksAndRetains(t *testing.T) {
	s, a, store := mutationFixture(t)
	f := mutationCreate(t, s, a, "unknown")
	store.moveErr = ports.ErrOutcomeUnknown
	in := Object{"file_id": str(f, "id"), "name": "target", "expected_metadata_revision": num(f, "metadata_revision"), "idempotency_key": "unknown-key"}
	result, e := s.Mutate(context.Background(), a, "rename", in)
	if e != nil {
		t.Fatal(e)
	}
	if str(result.(map[string]any), "state") != "outcome_unknown" {
		t.Fatal(result)
	}
	_, e = s.Mutate(context.Background(), a, "rename", in)
	if e != nil || store.moves != 1 {
		t.Fatal("unknown operation repeated", e, store.moves)
	}
	in["idempotency_key"] = "second-key"
	_, e = s.Mutate(context.Background(), a, "rename", in)
	if domain.ErrorCode(e) != domain.OperationPending {
		t.Fatal(e)
	}
	var active bool
	_ = s.DB.QueryRow(context.Background(), `SELECT active_operation_id IS NOT NULL FROM files WHERE id=$1`, str(f, "id")).Scan(&active)
	if !active {
		t.Fatal("unknown operation gate released")
	}
}
func TestMutationDeleteRestoreAndQuota(t *testing.T) {
	s, a, _ := mutationFixture(t)
	ctx := context.Background()
	f := mutationCreate(t, s, a, "folder")
	childResult, e := s.Mutate(ctx, a, "mkdir", Object{"name": "child", "parent_id": str(f, "id"), "expected_parent_revision": num(f, "metadata_revision"), "idempotency_key": "child"})
	if e != nil {
		t.Fatal(e)
	}
	child := childResult.(map[string]any)["file"].(map[string]any)
	deleted, e := s.Mutate(ctx, a, "delete", Object{"file_id": str(f, "id"), "expected_metadata_revision": num(f, "metadata_revision"), "idempotency_key": "delete"})
	if e != nil {
		t.Fatal(e)
	}
	deletedFile := deleted.(map[string]any)["file"].(map[string]any)
	if str(deletedFile, "lifecycle") != "trashed" {
		t.Fatal(deleted)
	}
	if _, e = s.owned(ctx, a, str(child, "id"), domain.ActionRead); domain.ErrorCode(e) != domain.NotFound {
		t.Fatal("descendant gate leaked", e)
	}
	var trashID string
	var used int64
	if e = s.DB.QueryRow(ctx, `SELECT id::text FROM trash_entries WHERE root_file_id=$1 AND state='trashed'`, str(f, "id")).Scan(&trashID); e != nil {
		t.Fatal(e)
	}
	_ = s.DB.QueryRow(ctx, `SELECT used_bytes FROM quota_accounts WHERE owner_id=$1`, a.Principal.ID).Scan(&used)
	if used != 0 {
		t.Fatal("directory trash changed quota")
	}
	if _, e = s.Mutate(ctx, a, "mkdir", Object{"name": "folder", "idempotency_key": "replacement-folder"}); e != nil {
		t.Fatal(e)
	}
	_, e = s.Mutate(ctx, a, "restore", Object{"trash_id": trashID, "name": "folder", "expected_metadata_revision": num(deletedFile, "metadata_revision"), "idempotency_key": "restore-conflict"})
	if domain.ErrorCode(e) != domain.PathConflict {
		t.Fatal(e)
	}
	restored, e := s.Mutate(ctx, a, "restore", Object{"trash_id": trashID, "name": "restored", "expected_metadata_revision": num(deletedFile, "metadata_revision"), "idempotency_key": "restore-new"})
	if e != nil {
		t.Fatal(e)
	}
	if str(restored.(map[string]any)["file"].(map[string]any), "id") != str(f, "id") {
		t.Fatal("restore changed file identity")
	}
	childNow, e := s.owned(ctx, a, str(child, "id"), domain.ActionRead)
	if e != nil || str(childNow, "path") != "/restored/child" {
		t.Fatal(childNow, e)
	}
}
func TestBatchIndependentAndDependencyBlocked(t *testing.T) {
	s, a, _ := mutationFixture(t)
	ctx := context.Background()
	mutationCreate(t, s, a, "exists")
	in := Object{"idempotency_key": "batch-one", "items": []Object{{"item_id": "failure", "command": "mkdir", "input": Object{"name": "exists"}}, {"item_id": "dependent", "command": "mkdir", "depends_on": []string{"failure"}, "input": Object{"name": "blocked"}}, {"item_id": "success", "command": "mkdir", "input": Object{"name": "independent"}}}}
	result, e := s.Batch(ctx, a, in)
	if e != nil {
		t.Fatal(e)
	}
	r := result.(map[string]any)
	if str(r, "state") != "completed" || len(r["succeeded"].([]Object)) != 1 || len(r["failed"].([]Object)) != 2 {
		t.Fatal(r)
	}
	var n int
	_ = s.DB.QueryRow(ctx, `SELECT count(*) FROM files WHERE name='independent' AND lifecycle='active'`).Scan(&n)
	if n != 1 {
		t.Fatal(n)
	}
	_, e = s.Batch(ctx, a, in)
	if e != nil {
		t.Fatal(e)
	}
	_ = s.DB.QueryRow(ctx, `SELECT count(*) FROM files WHERE name='independent' AND lifecycle='active'`).Scan(&n)
	if n != 1 {
		t.Fatal("batch replay duplicated", n)
	}
}
func TestMkdirDefinitiveFailureReleasesPath(t *testing.T) {
	s, a, store := mutationFixture(t)
	store.mkdirErr = ports.ErrConflict
	_, e := s.Mutate(context.Background(), a, "mkdir", Object{"name": "retry", "idempotency_key": "first"})
	if !errors.Is(e, &domain.Error{Code: domain.PathConflict}) {
		t.Fatal(e)
	}
	store.mkdirErr = nil
	_, e = s.Mutate(context.Background(), a, "mkdir", Object{"name": "retry", "idempotency_key": "second"})
	if e != nil {
		t.Fatal("definite failed provisional directory retained live namespace", e)
	}
}

func TestMutationReconcilesLostMoveResponse(t *testing.T) {
	s, a, store := mutationFixture(t)
	ctx := context.Background()
	f := mutationCreate(t, s, a, "before")
	store.moveApplyUnknown = true
	in := Object{"file_id": str(f, "id"), "name": "after", "expected_metadata_revision": num(f, "metadata_revision"), "idempotency_key": "lost-response"}
	result, err := s.Mutate(ctx, a, "rename", in)
	if err != nil {
		t.Fatal(err)
	}
	if str(result.(map[string]any), "state") != "outcome_unknown" {
		t.Fatal(result)
	}
	if err = s.ReconcileMutations(ctx); err != nil {
		t.Fatal(err)
	}
	result, err = s.Mutate(ctx, a, "rename", in)
	if err != nil {
		t.Fatal(err)
	}
	if str(result.(map[string]any), "state") != "committed" || store.moves != 1 {
		t.Fatal("recovery replayed native mutation or failed", result, store.moves)
	}
	if str(result.(map[string]any)["file"].(map[string]any), "path") != "/after" {
		t.Fatal(result)
	}
}
