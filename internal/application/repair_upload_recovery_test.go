package application

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type repairUploadIdentity struct{ mutationIdentity }

func (repairUploadIdentity) SubjectStatus(_ context.Context, _, subject string) (ports.SubjectStatus, error) {
	if subject == "identity-error" {
		return ports.SubjectStatus{}, ports.ErrUnavailable
	}
	return ports.SubjectStatus{Enabled: true}, nil
}
func (repairUploadIdentity) DelegatedCredential(_ context.Context, in ports.DelegationRequest) (ports.Credential, error) {
	if in.Subject.Subject == "credential-error" {
		return ports.Credential{}, ports.ErrUnavailable
	}
	return ports.Credential{}, nil
}

func TestRepairUploadRecoveryFairClaims(t *testing.T) {
	s, owner, original := uploadFixture(t)
	ctx := context.Background()
	u := newRepairUploadStore(original)
	s.Original = u
	var uploads []Object
	for i := 0; i < 17; i++ {
		actor := owner
		if i%4 == 0 || i%4 == 1 {
			subject := "identity-error"
			if i%4 == 1 {
				subject = "credential-error"
			}
			pid, space := id(), id()
			issuer := fmt.Sprintf("issuer-%d", i)
			if _, e := s.DB.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES($1,$2,$3)`, pid, issuer, subject); e != nil {
				t.Fatal(e)
			}
			if _, e := s.DB.Exec(ctx, `INSERT INTO spaces(id,owner_id,native_instance,native_space_id,native_root_resource_id) VALUES($1,$2,'http://ocis.test',$3,'root')`, space, pid, "native-"+space); e != nil {
				t.Fatal(e)
			}
			if _, e := s.DB.Exec(ctx, `INSERT INTO quota_accounts(owner_id,limit_bytes) VALUES($1,100000)`, pid); e != nil {
				t.Fatal(e)
			}
			actor.Principal.ID = pid
			actor.Subject = ports.Subject{Issuer: issuer, Subject: subject}
		}
		up := repairBegin(t, s, actor, fmt.Sprintf("fair-%02d", i))
		uploads = append(uploads, up)
		if i%4 == 2 {
			u.missing[str(up, "operation_id")] = true
		}
	}
	last := repairBegin(t, s, owner, "completed-after-old-tasks")
	u.statuses[str(last, "operation_id")] = ports.UploadStatus{Offset: 7, Length: 7, Complete: true}
	s.Identity = repairUploadIdentity{}
	repairAge(t, s)
	for i := 0; i < 3; i++ {
		if e := s.reconcileUploads(ctx); e != nil {
			t.Fatal(e)
		}
	}
	if repairState(t, s, last) != "committed" {
		t.Fatal("completed tail upload starved")
	}
	var deferred int
	if e := s.DB.QueryRow(ctx, `SELECT count(*) FROM file_operations WHERE state='executing' AND retry_at>now() AND lease_until IS NULL`).Scan(&deferred); e != nil || deferred != len(uploads) {
		t.Fatalf("unbounded exits: deferred=%d err=%v", deferred, e)
	}
	u.mu.Lock()
	for op, calls := range u.headCalls {
		if calls != 1 {
			t.Errorf("task %s reselected before backoff: %d", op, calls)
		}
	}
	u.mu.Unlock()
	t.Log("18 个跨失败分支任务，3 轮内提交队尾；身份/凭据/HEAD/未完成全部持久退避")
}

func TestRepairUploadCompletionPriorityConcurrentAndLeaseExpiry(t *testing.T) {
	s, owner, original := uploadFixture(t)
	ctx := context.Background()
	u := newRepairUploadStore(original)
	s.Original = u
	for i := 0; i < 12; i++ {
		repairBegin(t, s, owner, fmt.Sprintf("priority-blocker-%d", i))
	}
	up := repairBegin(t, s, owner, "priority-complete")
	epoch, e := s.AdmitUploadPatch(ctx, owner, str(up, "id"), 0, 7)
	if e != nil {
		t.Fatal(e)
	}
	st, e := u.StreamUpload(ctx, ports.UploadInfo{OperationID: str(up, "operation_id"), Length: 7}, 0, strings.NewReader("xxxxxxx"), 7, ports.Credential{})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.CompleteUploadPatch(ctx, str(up, "id"), epoch, st, nil); e != nil {
		t.Fatal(e)
	}
	u.missing[str(up, "operation_id")] = true
	repairAge(t, s)
	// 模拟进程领取后退出；尚未过期时不可偷取。
	if _, e = s.DB.Exec(ctx, `UPDATE file_operations SET lease_token=41,lease_until=now()+interval '1 hour',lease_owner='dead-process' WHERE id=$1`, str(up, "operation_id")); e != nil {
		t.Fatal(e)
	}
	if e = s.reconcileUploads(ctx); e != nil {
		t.Fatal(e)
	}
	if repairState(t, s, up) != "executing" {
		t.Fatal("active lease stolen")
	}
	if _, e = s.DB.Exec(ctx, `UPDATE file_operations SET lease_until=now()-interval '1 second' WHERE id=$1`, str(up, "operation_id")); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); restarted := repairRestart(s); errs <- restarted.reconcileUploads(ctx) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if repairState(t, s, up) != "committed" {
		t.Fatal("expired lease not recovered")
	}
	var token int64
	if e = s.DB.QueryRow(ctx, `SELECT lease_token FROM file_operations WHERE id=$1`, str(up, "operation_id")).Scan(&token); e != nil || token != 42 {
		t.Fatalf("claims=%d err=%v", token, e)
	}
	u.mu.Lock()
	calls := u.headCalls[str(up, "operation_id")]
	u.mu.Unlock()
	if calls != 0 {
		t.Fatalf("persisted completion still requires temporary HEAD: %d", calls)
	}
	var versions, jobs int
	if e = s.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM file_versions WHERE file_id=$1),(SELECT count(*) FROM conversion_jobs j JOIN file_versions v ON v.id=j.source_version_id WHERE v.file_id=$1)`, str(up, "file_id")).Scan(&versions, &jobs); e != nil || versions != 1 || jobs != 1 {
		t.Fatalf("versions=%d jobs=%d err=%v", versions, jobs, e)
	}
	// 过期执行者不能提交或清除新租约。
	_, e = s.commitUploadResult(ctx, up, Object{"id": str(up, "file_id"), "space_id": mustRepairSpace(t, s, up)}, Object{}, ports.ReconcileResult{}, 41, 7, strings.Repeat("a", 64))
	if domain.ErrorCode(e) != domain.LeaseLost {
		t.Fatalf("stale lease write: %v", e)
	}
}
func mustRepairSpace(t *testing.T, s *Service, up Object) string {
	t.Helper()
	var space string
	if e := s.DB.QueryRow(context.Background(), `SELECT space_id::text FROM files WHERE id=$1`, str(up, "file_id")).Scan(&space); e != nil {
		t.Fatal(e)
	}
	return space
}

func TestRepairUploadRemoteConfirmedWithoutTus(t *testing.T) {
	s, owner, original := uploadFixture(t)
	ctx := context.Background()
	u := newRepairUploadStore(original)
	s.Original = u
	up := repairBegin(t, s, owner, "remote-confirmed")
	rec, e := original.Reconcile(ctx, ports.ReconcileRequest{Upload: ports.UploadInfo{OperationID: str(up, "operation_id")}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(ctx, `UPDATE file_operations SET state='remote_confirmed',remote_result=$2::jsonb WHERE id=$1`, str(up, "operation_id"), js(Object{"metadata": rec.Metadata, "revision": rec.Revision})); e != nil {
		t.Fatal(e)
	}
	u.missing[str(up, "operation_id")] = true
	repairAge(t, s)
	if e = repairRestart(s).reconcileUploads(ctx); e != nil {
		t.Fatal(e)
	}
	repairAssertCommitted(t, s, up, 7)
	u.mu.Lock()
	calls := u.headCalls[str(up, "operation_id")]
	u.mu.Unlock()
	if calls != 0 {
		t.Fatalf("remote confirmation still required tus HEAD: %d", calls)
	}
}

func TestRepairUploadCompletedMissingResourceOrIdentityPreservesGate(t *testing.T) {
	for _, failure := range []string{"stable_missing", "identity_unavailable", "no_completion_404"} {
		t.Run(failure, func(t *testing.T) {
			s, owner, original := uploadFixture(t)
			ctx := context.Background()
			u := newRepairUploadStore(original)
			s.Original = u
			up := repairBegin(t, s, owner, failure)
			if failure != "no_completion_404" {
				epoch, e := s.AdmitUploadPatch(ctx, owner, str(up, "id"), 0, 7)
				if e != nil {
					t.Fatal(e)
				}
				if e = s.RecordUploadProgress(ctx, str(up, "id"), epoch, 7, "stable-id"); e != nil {
					t.Fatal(e)
				}
			}
			u.missing[str(up, "operation_id")] = true
			if failure == "stable_missing" {
				u.stableError = ports.ErrNotFound
			}
			if failure == "identity_unavailable" {
				if _, e := s.DB.Exec(ctx, `UPDATE principals SET subject='identity-error' WHERE id=$1`, owner.Principal.ID); e != nil {
					t.Fatal(e)
				}
				s.Identity = repairUploadIdentity{}
			}
			repairAge(t, s)
			if e := s.reconcileUploads(ctx); e != nil {
				t.Fatal(e)
			}
			var held bool
			var gate bool
			if e := s.DB.QueryRow(ctx, `SELECT r.state='held',f.active_operation_id=u.operation_id FROM upload_sessions u JOIN quota_reservations r ON r.upload_id=u.id JOIN file_operations o ON o.id=u.operation_id JOIN files f ON f.id=o.file_id WHERE u.id=$1`, str(up, "id")).Scan(&held, &gate); e != nil || !held || !gate {
				t.Fatalf("uncertainty released: %v", e)
			}
			if repairState(t, s, up) != "executing" {
				t.Fatal("unproven terminal state")
			}
		})
	}
}

func TestRepairUploadMigrationUpgradeAndReentry(t *testing.T) {
	dsn := os.Getenv("DRIVE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("DRIVE_TEST_DATABASE_URL required")
	}
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close()
	schema := "repair_migration_" + strings.ReplaceAll(id(), "-", "")
	if _, e = admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); e != nil {
		t.Fatal(e)
	}
	defer admin.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()
	files, e := filepath.Glob("../../db/migrations/*.up.sql")
	if e != nil {
		t.Fatal(e)
	}
	sort.Strings(files)
	var migration []byte
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(file, "000009_") {
			migration = body
			continue
		}
		if _, e = pool.Exec(ctx, string(body)); e != nil {
			t.Fatal(e)
		}
	}
	pid, space, fid, op, uid := id(), id(), id(), id(), id()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO principals(id,issuer,subject) VALUES($1,'test','legacy')`, []any{pid}},
		{`INSERT INTO spaces(id,owner_id,native_instance,native_space_id,native_root_resource_id) VALUES($1,$2,'native','space','root')`, []any{space, pid}},
		{`INSERT INTO files(id,owner_id,space_id,name,path,kind,native_instance) VALUES($1,$2,$3,'legacy','/legacy','file','native')`, []any{fid, pid, space}},
		{`INSERT INTO file_operations(id,file_id,actor_principal_id,actor_type,kind,expected_revision,idempotency_key,request_hash,intent) VALUES($1,$2,$3,'user','create',1,'legacy',$4,'{}')`, []any{op, fid, pid, strings.Repeat("a", 64)}},
		{`INSERT INTO upload_sessions(id,operation_id,expected_size,node_id,expires_at,state) VALUES($1,$2,7,'legacy',now()+interval '1 day','uploading')`, []any{uid, op}},
		{`UPDATE file_operations SET state='executing' WHERE id=$1`, []any{op}},
	} {
		if _, e = pool.Exec(ctx, q.sql, q.args...); e != nil {
			t.Fatal(e)
		}
	}
	for i := 0; i < 2; i++ {
		if _, e = pool.Exec(ctx, string(migration)); e != nil {
			t.Fatal(e)
		}
	}
	var unknown bool
	if e = pool.QueryRow(ctx, `SELECT legacy_dispatch_unknown FROM upload_sessions WHERE id=$1`, uid).Scan(&unknown); e != nil || !unknown {
		t.Fatalf("legacy evidence guessed away: %v", e)
	}
	if _, e = pool.Exec(ctx, `UPDATE file_operations SET state='failed_confirmed' WHERE id=$1`, op); e == nil {
		t.Fatal("legacy unknown operation released")
	}
	var defaultFalse bool
	if e = pool.QueryRow(ctx, `SELECT column_default='false' FROM information_schema.columns WHERE table_schema=$1 AND table_name='upload_sessions' AND column_name='legacy_dispatch_unknown'`, schema).Scan(&defaultFalse); e != nil || !defaultFalse {
		t.Fatalf("new sessions default: %v", e)
	}
	t.Log("基线 000001..000008 升级 + 000009 重入成功；旧 executing 不推断未派发，数据库拒绝错误释放")
}
