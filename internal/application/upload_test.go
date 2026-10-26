package application

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

// uploadStore is an explicit contract boundary, not an oCIS implementation.
// These tests prove PostgreSQL admission/exclusion/finalization, not native tus.
type uploadStore struct {
	*mutationStore
	uploadMu  sync.Mutex
	requests  map[string]ports.UploadRequest
	resources map[string]string
	bodies    map[string]string
	createErr error
	started   chan struct{}
	release   chan struct{}
	calls     int
}

func (u *uploadStore) CreateUpload(ctx context.Context, r ports.UploadRequest) (ports.UploadInfo, error) {
	u.uploadMu.Lock()
	u.calls++
	u.requests[r.OperationID] = r
	resource := r.ExistingResourceID
	if resource == "" {
		resource = "native-" + id()
	}
	u.resources[r.OperationID] = resource
	err, start, release := u.createErr, u.started, u.release
	u.uploadMu.Unlock()
	if start != nil {
		select {
		case start <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-ctx.Done():
			return ports.UploadInfo{}, ctx.Err()
		case <-release:
		}
	}
	if err != nil {
		return ports.UploadInfo{}, err
	}
	return ports.UploadInfo{NativeURL: "http://native.test/tus/" + r.OperationID, OperationID: r.OperationID, Length: r.Size}, nil
}
func (u *uploadStore) Reconcile(_ context.Context, r ports.ReconcileRequest) (ports.ReconcileResult, error) {
	u.uploadMu.Lock()
	defer u.uploadMu.Unlock()
	request, ok := u.requests[r.Upload.OperationID]
	if !ok {
		return ports.ReconcileResult{}, ports.ErrNotFound
	}
	ref := ports.ResourceRef{Instance: "http://ocis.test", SpaceID: request.SpaceID, ResourceID: u.resources[r.Upload.OperationID]}
	revision := ports.RevisionRef{ResourceRef: ref, RevisionID: "etag:" + request.OperationID, ETag: request.OperationID}
	u.bodies[revision.RevisionID] = strings.Repeat("x", int(request.Size))
	meta := ports.OriginalMetadata{ResourceRef: ref, Name: request.Name, Size: request.Size, ETag: request.OperationID}
	u.mutationStore.mu.Lock()
	u.mutationStore.objects[ref.ResourceID] = meta
	u.mutationStore.mu.Unlock()
	return ports.ReconcileResult{State: "remote_confirmed", Metadata: meta, Revision: revision}, nil
}
func (u *uploadStore) OpenRevision(_ context.Context, r ports.RevisionRef, _ *ports.ByteRange, _ ports.Credential) (ports.Content, error) {
	u.uploadMu.Lock()
	defer u.uploadMu.Unlock()
	body, ok := u.bodies[r.RevisionID]
	if !ok {
		return ports.Content{}, ports.ErrNotFound
	}
	return ports.Content{Body: io.NopCloser(strings.NewReader(body)), Size: int64(len(body)), TotalSize: int64(len(body)), ETag: r.ETag}, nil
}
func uploadFixture(t *testing.T) (*Service, Actor, *uploadStore) {
	t.Helper()
	s, a, m := mutationFixture(t)
	u := &uploadStore{mutationStore: m, requests: map[string]ports.UploadRequest{}, resources: map[string]string{}, bodies: map[string]string{}}
	s.Original = u
	s.Config.UploadMax = 1 << 20
	s.Config.ConvertPages = 100
	s.Config.NodeID = "test-node"
	return s, a, u
}
func seedUpload(t *testing.T, s *Service, a Actor, name string, size int64) Object {
	t.Helper()
	ctx := context.Background()
	result, err := s.BeginUpload(ctx, a, Object{"name": name, "size_bytes": size, "idempotency_key": "seed-" + name})
	if err != nil {
		t.Fatal(err)
	}
	up := result.(map[string]any)
	if _, err = s.FinishUpload(ctx, a, str(up, "id")); err != nil {
		t.Fatal(err)
	}
	f, err := s.owned(ctx, a, str(up, "file_id"), domain.ActionWrite)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestUploadConcurrentOverwriteExcludesEntireTransfer(t *testing.T) {
	s, a, u := uploadFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f := seedUpload(t, s, a, "document.md", 3)
	u.started = make(chan struct{}, 2)
	u.release = make(chan struct{})
	start := make(chan struct{})
	type outcome struct {
		value any
		err   error
	}
	out := make(chan outcome, 2)
	for _, key := range []string{"overwrite-a", "overwrite-b"} {
		go func(key string) {
			<-start
			value, err := s.BeginUpload(ctx, a, Object{"file_id": str(f, "id"), "name": "document.md", "size_bytes": int64(10), "expected_version_id": str(f, "current_version_id"), "expected_metadata_revision": num(f, "metadata_revision"), "idempotency_key": key})
			out <- outcome{value, err}
		}(key)
	}
	close(start)
	select {
	case <-u.started:
	case <-ctx.Done():
		t.Fatal("no admitted upload")
	}
	var rejected outcome
	select {
	case rejected = <-out:
	case <-ctx.Done():
		t.Fatal("concurrent overwrite was not rejected")
	}
	if domain.ErrorCode(normalizeError(rejected.err)) != domain.OperationPending {
		t.Fatal("wrong conflict", rejected.value, rejected.err)
	}
	var held, used int64
	var versions int
	if err := s.DB.QueryRow(ctx, `SELECT reserved_bytes,used_bytes FROM quota_accounts WHERE owner_id=$1`, a.Principal.ID).Scan(&held, &used); err != nil {
		t.Fatal(err)
	}
	_ = s.DB.QueryRow(ctx, `SELECT count(*) FROM file_versions WHERE file_id=$1`, str(f, "id")).Scan(&versions)
	if held != 7 || used != 3 || versions != 1 {
		t.Fatal("quota/version changed before transfer commit", held, used, versions)
	}
	close(u.release)
	admitted := <-out
	if admitted.err != nil {
		t.Fatal(admitted.err)
	}
	up := admitted.value.(map[string]any)
	// Even after tus creation returned, another overwrite cannot enter while the
	// first content operation is unresolved.
	_, err := s.BeginUpload(ctx, a, Object{"file_id": str(f, "id"), "name": "document.md", "size_bytes": int64(10), "expected_version_id": str(f, "current_version_id"), "expected_metadata_revision": num(f, "metadata_revision"), "idempotency_key": "after-tus-created"})
	if domain.ErrorCode(err) != domain.OperationPending {
		t.Fatal(err)
	}
	first, err := s.FinishUpload(ctx, a, str(up, "id"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.FinishUpload(ctx, a, str(up, "id"))
	if err != nil {
		t.Fatal(err)
	}
	if first.(map[string]any)["version_id"] != again.(map[string]any)["version_id"] {
		t.Fatal("duplicate finalization created another version")
	}
	_ = s.DB.QueryRow(ctx, `SELECT count(*) FROM file_versions WHERE file_id=$1`, str(f, "id")).Scan(&versions)
	_ = s.DB.QueryRow(ctx, `SELECT reserved_bytes,used_bytes FROM quota_accounts WHERE owner_id=$1`, a.Principal.ID).Scan(&held, &used)
	if versions != 2 || held != 0 || used != 10 || u.calls != 2 {
		t.Fatal(versions, held, used, u.calls)
	}
}
func TestUploadIdempotencyConflictAndDuplicateCommit(t *testing.T) {
	s, a, u := uploadFixture(t)
	ctx := context.Background()
	in := Object{"name": "once.txt", "size_bytes": int64(4), "idempotency_key": "once"}
	first, err := s.BeginUpload(ctx, a, in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.BeginUpload(ctx, a, in)
	if err != nil {
		t.Fatal(err)
	}
	if first.(map[string]any)["id"] != again.(map[string]any)["id"] || u.calls != 1 {
		t.Fatal("same key repeated native admission")
	}
	changed := Object{"name": "once.txt", "size_bytes": int64(5), "idempotency_key": "once"}
	_, err = s.BeginUpload(ctx, a, changed)
	if domain.ErrorCode(err) != domain.IdempotencyConflict {
		t.Fatal(err)
	}
	uid := str(first.(map[string]any), "id")
	v, err := s.FinishUpload(ctx, a, uid)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := s.FinishUpload(ctx, a, uid)
	if err != nil || v.(map[string]any)["version_id"] != v2.(map[string]any)["version_id"] {
		t.Fatal(v, v2, err)
	}
}
func TestUploadUnknownOutcomeKeepsReservation(t *testing.T) {
	for _, remoteError := range []error{ports.ErrOutcomeUnknown, errors.New("transport connection reset after request dispatch")} {
		t.Run(remoteError.Error(), func(t *testing.T) {
			s, a, u := uploadFixture(t)
			u.createErr = remoteError
			ctx := context.Background()
			_, err := s.BeginUpload(ctx, a, Object{"name": "unknown.bin", "size_bytes": int64(20), "idempotency_key": "unknown"})
			if err == nil {
				t.Fatal("expected dependency error")
			}
			var held int64
			var state, uploadID string
			var gated bool
			if err = s.DB.QueryRow(ctx, `SELECT q.reserved_bytes,o.state,u.id::text,f.active_operation_id IS NOT NULL FROM quota_accounts q JOIN files f ON f.owner_id=q.owner_id JOIN file_operations o ON o.file_id=f.id JOIN upload_sessions u ON u.operation_id=o.id WHERE q.owner_id=$1`, a.Principal.ID).Scan(&held, &state, &uploadID, &gated); err != nil {
				t.Fatal(err)
			}
			if held != 20 || state != "outcome_unknown" || !gated {
				t.Fatal("unknown outcome prematurely released quota/gate", held, state, gated)
			}
			if _, err = s.DB.Exec(ctx, `SELECT release_upload_quota($1)`, uploadID); err == nil {
				t.Fatal("database allowed reservation release with unknown remote result")
			}
		})
	}
}
func TestUploadDefinitiveFailureFreesProvisionalPath(t *testing.T) {
	s, a, u := uploadFixture(t)
	ctx := context.Background()
	u.createErr = ports.ErrConflict
	_, err := s.BeginUpload(ctx, a, Object{"name": "retry.bin", "size_bytes": int64(20), "idempotency_key": "failed"})
	if err == nil {
		t.Fatal("expected native conflict")
	}
	var held int64
	_ = s.DB.QueryRow(ctx, `SELECT reserved_bytes FROM quota_accounts WHERE owner_id=$1`, a.Principal.ID).Scan(&held)
	if held != 0 {
		t.Fatal("confirmed failed upload retained quota", held)
	}
	u.createErr = nil
	_, err = s.BeginUpload(ctx, a, Object{"name": "retry.bin", "size_bytes": int64(20), "idempotency_key": "retry"})
	if err != nil {
		t.Fatal("failed provisional file still occupies path", err)
	}
}

func TestUploadConcurrentSameKeyAdmitsOnce(t *testing.T) {
	s, a, u := uploadFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u.started = make(chan struct{}, 2)
	u.release = make(chan struct{})
	start := make(chan struct{})
	type outcome struct {
		value any
		err   error
	}
	out := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			v, e := s.BeginUpload(ctx, a, Object{"name": "same.txt", "size_bytes": int64(4), "idempotency_key": "same-key"})
			out <- outcome{v, e}
		}()
	}
	close(start)
	select {
	case <-u.started:
	case <-ctx.Done():
		t.Fatal("native admission never started")
	}
	var replay outcome
	select {
	case replay = <-out:
	case <-ctx.Done():
		t.Fatal("same-key replay did not observe persistent admission")
	}
	if replay.err != nil {
		t.Fatal(replay.err)
	}
	close(u.release)
	first := <-out
	if first.err != nil {
		t.Fatal(first.err)
	}
	if str(replay.value.(map[string]any), "id") != str(first.value.(map[string]any), "id") || u.calls != 1 {
		t.Fatal(replay.value, first.value, u.calls)
	}
	var files int
	_ = s.DB.QueryRow(ctx, `SELECT count(*) FROM files WHERE owner_id=$1`, a.Principal.ID).Scan(&files)
	if files != 1 {
		t.Fatal("replay inserted duplicate provisional file", files)
	}
}

func TestUploadedTrashRetainsQuotaVersionsAndFeedback(t *testing.T) {
	s, a, _ := uploadFixture(t)
	ctx := context.Background()
	f := seedUpload(t, s, a, "retained.txt", 10)
	if _, err := s.DB.Exec(ctx, `INSERT INTO feedback(file_id,version_id,category,body) VALUES($1,$2,'other','anonymous report')`, str(f, "id"), str(f, "current_version_id")); err != nil {
		t.Fatal(err)
	}
	result, err := s.Mutate(ctx, a, "delete", Object{"file_id": str(f, "id"), "expected_metadata_revision": num(f, "metadata_revision"), "idempotency_key": "trash-upload"})
	if err != nil {
		t.Fatal(err)
	}
	trashed := result.(map[string]any)["file"].(map[string]any)
	var used, reserved int64
	var versions, feedback int
	if err = s.DB.QueryRow(ctx, `SELECT used_bytes,reserved_bytes FROM quota_accounts WHERE owner_id=$1`, a.Principal.ID).Scan(&used, &reserved); err != nil {
		t.Fatal(err)
	}
	_ = s.DB.QueryRow(ctx, `SELECT count(*) FROM file_versions WHERE file_id=$1`, str(f, "id")).Scan(&versions)
	_ = s.DB.QueryRow(ctx, `SELECT count(*) FROM feedback WHERE file_id=$1`, str(f, "id")).Scan(&feedback)
	if used != 10 || reserved != 0 || versions != 1 || feedback != 1 {
		t.Fatal("trash changed retained facts", used, reserved, versions, feedback)
	}
	var trashID string
	if err = s.DB.QueryRow(ctx, `SELECT id::text FROM trash_entries WHERE root_file_id=$1 AND state='trashed'`, str(f, "id")).Scan(&trashID); err != nil {
		t.Fatal(err)
	}
	_, err = s.Mutate(ctx, a, "restore", Object{"trash_id": trashID, "name": "retained.txt", "expected_metadata_revision": num(trashed, "metadata_revision"), "idempotency_key": "restore-upload"})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.DB.QueryRow(ctx, `SELECT used_bytes,reserved_bytes FROM quota_accounts WHERE owner_id=$1`, a.Principal.ID).Scan(&used, &reserved)
	_ = s.DB.QueryRow(ctx, `SELECT count(*) FROM file_versions WHERE file_id=$1`, str(f, "id")).Scan(&versions)
	if used != 10 || reserved != 0 || versions != 1 {
		t.Fatal("restore double charged quota or created content version", used, reserved, versions)
	}
}
