package application

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

// 受控存储显式模拟原生边界，DB/行锁/事务为真实 PostgreSQL。
type repairUploadStore struct {
	*uploadStore
	mu          sync.Mutex
	statuses    map[string]ports.UploadStatus
	missing     map[string]bool
	headCalls   map[string]int
	entered     chan struct{}
	release     chan struct{}
	stableError error
}

func newRepairUploadStore(u *uploadStore) *repairUploadStore {
	return &repairUploadStore{uploadStore: u, statuses: map[string]ports.UploadStatus{}, missing: map[string]bool{}, headCalls: map[string]int{}}
}
func (u *repairUploadStore) StatUpload(_ context.Context, in ports.UploadInfo, _ ports.Credential) (ports.UploadStatus, error) {
	op := in.OperationID
	if op == "" {
		op = in.NativeURL[strings.LastIndex(in.NativeURL, "/")+1:]
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.headCalls[op]++
	if u.missing[op] {
		return ports.UploadStatus{}, ports.ErrNotFound
	}
	if st, ok := u.statuses[op]; ok {
		return st, nil
	}
	return ports.UploadStatus{Length: in.Length}, nil
}
func (u *repairUploadStore) Reconcile(ctx context.Context, in ports.ReconcileRequest) (ports.ReconcileResult, error) {
	if !in.TransferCompleted {
		st, e := u.StatUpload(ctx, in.Upload, in.Credential)
		if e != nil {
			return ports.ReconcileResult{}, e
		}
		if !st.Complete {
			return ports.ReconcileResult{State: "outcome_unknown", Reason: "transfer_incomplete"}, nil
		}
	}
	if u.stableError != nil {
		return ports.ReconcileResult{}, u.stableError
	}
	return u.uploadStore.Reconcile(ctx, in)
}
func (u *repairUploadStore) StreamUpload(ctx context.Context, in ports.UploadInfo, _ int64, _ io.Reader, _ int64, _ ports.Credential) (ports.UploadStatus, error) {
	if u.entered != nil {
		close(u.entered)
		select {
		case <-u.release:
		case <-ctx.Done():
			return ports.UploadStatus{}, ctx.Err()
		}
	}
	r, e := u.uploadStore.Reconcile(ctx, ports.ReconcileRequest{Upload: in})
	if e != nil {
		return ports.UploadStatus{}, e
	}
	st := ports.UploadStatus{Offset: in.Length, Length: in.Length, Complete: true, ResourceID: r.Metadata.ResourceID}
	u.mu.Lock()
	u.statuses[in.OperationID] = st
	u.mu.Unlock()
	return st, nil
}

func repairRestart(s *Service) *Service {
	return &Service{DB: s.DB, Config: s.Config, Original: s.Original, Identity: s.Identity}
}

func repairBegin(t *testing.T, s *Service, a Actor, name string) Object {
	t.Helper()
	out, e := s.BeginUpload(context.Background(), a, Object{"name": name, "size_bytes": int64(7), "idempotency_key": name})
	if e != nil {
		t.Fatal(e)
	}
	return out.(Object)
}
func repairAge(t *testing.T, s *Service) {
	t.Helper()
	if _, e := s.DB.Exec(context.Background(), `UPDATE file_operations SET updated_at=now()-interval '1 day',retry_at=now()-interval '1 hour' WHERE state IN ('executing','outcome_unknown','remote_confirmed')`); e != nil {
		t.Fatal(e)
	}
}
func repairState(t *testing.T, s *Service, up Object) string {
	t.Helper()
	var state string
	if e := s.DB.QueryRow(context.Background(), `SELECT state FROM file_operations WHERE id=$1`, str(up, "operation_id")).Scan(&state); e != nil {
		t.Fatal(e)
	}
	return state
}
func repairAssertCommitted(t *testing.T, s *Service, up Object, size int64) {
	t.Helper()
	var versions, jobs, events int
	var reserved, used int64
	var reservation string
	var gate *string
	e := s.DB.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM file_versions WHERE file_id=f.id),
		(SELECT count(*) FROM conversion_jobs j JOIN file_versions v ON v.id=j.source_version_id WHERE v.file_id=f.id),
		(SELECT count(*) FROM outbox WHERE aggregate_id=f.id AND event_type='file.version_committed'),
		q.reserved_bytes,q.used_bytes,r.state,f.active_operation_id::text
		FROM files f JOIN quota_accounts q ON q.owner_id=f.owner_id JOIN upload_sessions u ON u.id=$1 JOIN quota_reservations r ON r.upload_id=u.id WHERE f.id=$2`, str(up, "id"), str(up, "file_id")).Scan(&versions, &jobs, &events, &reserved, &used, &reservation, &gate)
	if e != nil {
		t.Fatal(e)
	}
	if versions != 1 || jobs != 1 || events != 1 || reserved != 0 || used != size || reservation != "committed" || gate != nil {
		t.Fatalf("versions=%d jobs=%d events=%d reserved=%d used=%d reservation=%s gate=%v", versions, jobs, events, reserved, used, reservation, gate)
	}
}

func TestRepairUploadAbandonInFlight(t *testing.T) {
	s, owner, original := uploadFixture(t)
	ctx := context.Background()
	f := seedUpload(t, s, owner, "late.md", 3)
	u := newRepairUploadStore(original)
	u.entered, u.release = make(chan struct{}), make(chan struct{})
	s.Original = u
	begin := func(key string) (any, error) {
		return s.BeginUpload(ctx, owner, Object{"file_id": str(f, "id"), "name": "late.md", "size_bytes": int64(7), "expected_version_id": str(f, "current_version_id"), "expected_metadata_revision": num(f, "metadata_revision"), "idempotency_key": key})
	}
	out, e := begin("old")
	if e != nil {
		t.Fatal(e)
	}
	up := out.(Object)
	epoch, e := s.AdmitUploadPatch(ctx, owner, str(up, "id"), 0, 7)
	if e != nil {
		t.Fatal(e)
	}
	info := ports.UploadInfo{OperationID: str(up, "operation_id"), Length: 7}
	done := make(chan error, 1)
	var once sync.Once
	defer once.Do(func() { close(u.release) })
	go func() {
		st, err := u.StreamUpload(ctx, info, 0, strings.NewReader("xxxxxxx"), 7, ports.Credential{})
		done <- s.CompleteUploadPatch(ctx, str(up, "id"), epoch, st, err)
	}()
	<-u.entered
	if _, e = s.AbandonUpload(ctx, owner, str(up, "id")); domain.ErrorCode(e) != domain.OperationPending {
		t.Fatalf("in-flight abandon = %v", e)
	}
	if _, e = begin("replacement"); domain.ErrorCode(normalizeError(e)) != domain.OperationPending {
		t.Fatalf("overlapping overwrite admitted: %v", e)
	}
	var held int64
	var gate, pending bool
	if e = s.DB.QueryRow(ctx, `SELECT q.reserved_bytes,f.active_operation_id=u.operation_id,u.abandon_requested_at IS NOT NULL FROM upload_sessions u JOIN file_operations o ON o.id=u.operation_id JOIN files f ON f.id=o.file_id JOIN quota_accounts q ON q.owner_id=f.owner_id WHERE u.id=$1`, str(up, "id")).Scan(&held, &gate, &pending); e != nil || held != 4 || !gate || !pending {
		t.Fatalf("gate released: held=%d gate=%v pending=%v err=%v", held, gate, pending, e)
	}
	once.Do(func() { close(u.release) })
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	// 丢弃旧 Service 实例，恢复仅依赖数据库而非内存 gate。
	restarted := repairRestart(s)
	repairAge(t, restarted)
	if e = restarted.reconcileUploads(ctx); e != nil {
		t.Fatal(e)
	}
	var dbETag, nativeID string
	if e = s.DB.QueryRow(ctx, `SELECT v.native_etag,f.native_resource_id FROM files f JOIN file_versions v ON v.id=f.current_version_id WHERE f.id=$1`, str(f, "id")).Scan(&dbETag, &nativeID); e != nil {
		t.Fatal(e)
	}
	meta, e := original.Stat(ctx, ports.ResourceRef{ResourceID: nativeID}, ports.Credential{})
	if e != nil || meta.ETag != dbETag || repairState(t, s, up) != "committed" {
		t.Fatalf("native/DB diverged: %v native=%s DB=%s", e, meta.ETag, dbETag)
	}
	if _, e = s.FinishUpload(ctx, owner, str(up, "id")); e != nil {
		t.Fatal(e)
	}
	var versions int
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM file_versions WHERE file_id=$1`, str(f, "id")).Scan(&versions); e != nil || versions != 2 {
		t.Fatalf("duplicate/missing overwrite version: %d %v", versions, e)
	}
	t.Log("挂起 PATCH 保持门禁/增量配额；abandon 与新覆盖均阻塞；旧写完成后重启恢复，原生 ETag=业务当前 ETag，重复 finalize 不新增版本")
}

func TestRepairUploadAdmissionAbandonAtomic(t *testing.T) {
	for i := 0; i < 8; i++ {
		s, owner, _ := uploadFixture(t)
		ctx := context.Background()
		up := repairBegin(t, s, owner, "atomic")
		start := make(chan struct{})
		admit, abandon := make(chan error, 1), make(chan error, 1)
		go func() { <-start; _, e := s.AdmitUploadPatch(ctx, owner, str(up, "id"), 0, 7); admit <- e }()
		go func() { <-start; _, e := s.AbandonUpload(ctx, owner, str(up, "id")); abandon <- e }()
		close(start)
		a, b := <-admit, <-abandon
		if (a == nil) == (b == nil) {
			t.Fatalf("must admit exactly one contender: PATCH=%v abandon=%v", a, b)
		}
		if a == nil {
			if domain.ErrorCode(b) != domain.OperationPending {
				t.Fatal(b)
			}
			if _, e := s.AdmitUploadPatch(ctx, owner, str(up, "id"), 0, 7); domain.ErrorCode(e) != domain.OperationPending {
				t.Fatalf("concurrent PATCH admitted: %v", e)
			}
			if _, e := s.DB.Exec(ctx, `UPDATE file_operations SET state='failed_confirmed' WHERE id=$1`, str(up, "operation_id")); e == nil {
				t.Fatal("database accepted unresolved failure")
			}
		} else if code := domain.ErrorCode(a); code != domain.OperationPending && code != domain.NotFound {
			t.Fatalf("losing PATCH must be rejected after abandon, got %v", a)
		}
	}
}

func TestRepairUploadCrashBoundaries(t *testing.T) {
	for _, boundary := range []string{"before_admission", "after_admission_before_dispatch", "response_lost", "after_remote_before_progress", "after_progress_before_finalize"} {
		t.Run(boundary, func(t *testing.T) {
			s, owner, original := uploadFixture(t)
			ctx := context.Background()
			u := newRepairUploadStore(original)
			s.Original = u
			up := repairBegin(t, s, owner, boundary)
			uid := str(up, "id")
			if boundary == "before_admission" {
				if _, e := s.AbandonUpload(ctx, owner, uid); e != nil {
					t.Fatal(e)
				}
				if _, e := s.AdmitUploadPatch(ctx, owner, uid, 0, 7); domain.ErrorCode(e) != domain.OperationPending {
					t.Fatalf("late dispatch after abandon: %v", e)
				}
				return
			}
			epoch, e := s.AdmitUploadPatch(ctx, owner, uid, 0, 7)
			if e != nil {
				t.Fatal(e)
			}
			if boundary == "after_admission_before_dispatch" {
				u.missing[str(up, "operation_id")] = true
			} else {
				st, err := u.StreamUpload(ctx, ports.UploadInfo{OperationID: str(up, "operation_id"), Length: 7}, 0, strings.NewReader("xxxxxxx"), 7, ports.Credential{})
				if err != nil {
					t.Fatal(err)
				}
				if boundary == "response_lost" {
					if e = s.CompleteUploadPatch(ctx, uid, epoch, ports.UploadStatus{}, io.ErrUnexpectedEOF); e == nil {
						t.Fatal("lost response reported success")
					}
				}
				if boundary == "after_progress_before_finalize" {
					cancelled, cancel := context.WithCancel(ctx)
					cancel()
					if e = s.CompleteUploadPatch(cancelled, uid, epoch, st, nil); e != nil {
						t.Fatal(e)
					}
					u.missing[str(up, "operation_id")] = true
				}
			}
			restarted := repairRestart(s)
			repairAge(t, restarted)
			if e = restarted.reconcileUploads(ctx); e != nil {
				t.Fatal(e)
			}
			if boundary == "after_admission_before_dispatch" {
				if _, e = restarted.AbandonUpload(ctx, owner, uid); domain.ErrorCode(e) != domain.OperationPending {
					t.Fatalf("unknown 404 released: %v", e)
				}
				var held bool
				if e = s.DB.QueryRow(ctx, `SELECT state='held' FROM quota_reservations WHERE upload_id=$1`, uid).Scan(&held); e != nil || !held {
					t.Fatalf("reservation released: %v", e)
				}
				return
			}
			if repairState(t, s, up) != "committed" {
				t.Fatal("completed native write not recovered")
			}
			for i := 0; i < 3; i++ {
				if e = restarted.reconcileUploads(ctx); e != nil {
					t.Fatal(e)
				}
			}
			repairAssertCommitted(t, s, up, 7)
		})
	}
}
