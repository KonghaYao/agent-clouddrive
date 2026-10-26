package delegation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func repairDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DRIVE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("需要原生 PostgreSQL")
	}
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := "repair_audit_" + strings.ReplaceAll(testID(), "-", "")
	if _, e = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); e != nil {
		t.Fatal(e)
	}
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		pool.Close()
		_, e := admin.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
		if e != nil {
			t.Error(e)
		}
	})
	migrations, e := filepath.Glob("../../../db/migrations/*.up.sql")
	if e != nil {
		t.Fatal(e)
	}
	sort.Strings(migrations)
	for _, p := range migrations {
		sql, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = pool.Exec(ctx, string(sql)); e != nil {
			t.Fatalf("迁移 %s: %v", p, e)
		}
	}
	return pool
}

func TestRepairAuthVaultFixedVersionAndConcurrentRevoke(t *testing.T) {
	pool := repairDatabase(t)
	ctx := context.Background()
	owner, auditor, grantor, space, file, version, otherFile, otherVersion := testID(), testID(), testID(), testID(), testID(), testID(), testID(), testID()
	subject := ports.Subject{Issuer: "https://issuer.test/realm", Subject: "owner"}
	base := &identityFixture{}
	base.enabled.Store(true)
	w := &Wrapper{cfg: Config{DB: pool, Issuer: subject.Issuer, Base: base}}
	pin := ports.RevisionRef{ResourceRef: ports.ResourceRef{Instance: "native", SpaceID: "space", ResourceID: "resource"}, RevisionID: "fixed-v1", ETag: "etag-v1"}
	raw, _ := json.Marshal(pin)
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO principals(id,issuer,subject) VALUES($1,$4,'owner'),($2,$4,'auditor'),($3,$4,'grantor')`, []any{owner, auditor, grantor, subject.Issuer}},
		{`INSERT INTO spaces(id,owner_id,native_instance,native_space_id) VALUES($1,$2,'native','space')`, []any{space, owner}},
		{`INSERT INTO files(id,owner_id,space_id,name,path,kind,native_instance,native_resource_id) VALUES($1,$3,$4,'one','/one','file','native','resource'),($2,$3,$4,'two','/two','file','native','other')`, []any{file, otherFile, owner, space}},
		{`INSERT INTO file_versions(id,file_id,version_seq,size,sha256,native_etag,native_revision_ref) VALUES($1,$2,1,3,$3,$4,$5),($6,$7,1,3,$3,$4,$5)`, []any{version, file, strings.Repeat("a", 64), pin.ETag, raw, otherVersion, otherFile}},
		{`UPDATE files SET lifecycle='trashed' WHERE id=$1`, []any{file}},
	} {
		if _, e := pool.Exec(ctx, stmt.sql, stmt.args...); e != nil {
			t.Fatal(e)
		}
	}
	var grant string
	if e := pool.QueryRow(ctx, `INSERT INTO audit_grants(principal_id,file_id,granted_by) VALUES($1,$2,$3) RETURNING id::text`, auditor, file, grantor).Scan(&grant); e != nil {
		t.Fatal(e)
	}
	capability := ports.AuditCapability{AuditorPrincipalID: auditor, GrantID: grant, FileID: file, VersionID: version, Purpose: "audit_original"}
	request := ports.DelegationRequest{Subject: subject, Source: pin, Audit: &capability}
	check := func(in ports.DelegationRequest, allowed bool) {
		t.Helper()
		tx, e := pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		e = w.authorizeSource(ctx, tx, in)
		if (e == nil) != allowed {
			t.Fatal("凭据授权结果错误", e)
		}
	}
	check(request, true)
	expires := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
	if _, e := pool.Exec(ctx, `UPDATE audit_grants SET expires_at=$2 WHERE id=$1`, grant, expires); e != nil {
		t.Fatal(e)
	}
	check(request, false) // 能力还绑定创建时看到的 grant 有效期。
	capability.ExpiresAt = expires
	check(request, true)
	if _, e := pool.Exec(ctx, `UPDATE files SET lifecycle='deleting' WHERE id=$1`, file); e != nil {
		t.Fatal(e)
	}
	check(request, false)
	if _, e := pool.Exec(ctx, `UPDATE files SET lifecycle='trashed' WHERE id=$1`, file); e != nil {
		t.Fatal(e)
	}
	bad := capability
	bad.VersionID = otherVersion
	in := request
	in.Audit = &bad
	check(in, false)
	bad = capability
	bad.Purpose = "drive.read"
	in.Audit = &bad
	check(in, false)
	in = request
	in.Audit = nil
	check(in, false)
	if _, e := pool.Exec(ctx, `UPDATE principals SET status='disabled' WHERE id=$1`, grantor); e != nil {
		t.Fatal(e)
	}
	check(request, false)
	if _, e := pool.Exec(ctx, `UPDATE principals SET status='active' WHERE id=$1`, grantor); e != nil {
		t.Fatal(e)
	}
	check(request, true)
	tx, e := pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = w.authorizeSource(ctx, tx, request); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { _, e := pool.Exec(ctx, `DELETE FROM audit_grants WHERE id=$1`, grant); done <- e }()
	select {
	case e := <-done:
		t.Fatal("撤销未与凭据校验串行", e)
	case <-time.After(75 * time.Millisecond):
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	check(request, false)
	// 已提交撤销后的新凭据请求必须立即失败；不能靠进程缓存。
	tx, e = pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if e = w.authorizeSource(ctx, tx, request); !errors.Is(e, ports.ErrUnauthorized) {
		t.Fatal(e)
	}
	t.Log("原生 PG：固定版本属于该文件、grantor 停用、审核与撤销并发串行通过")
}

func TestRepairIdentityMigrationReentrantAndEpochMonotonic(t *testing.T) {
	pool := repairDatabase(t)
	ctx := context.Background()
	sql, e := os.ReadFile("../../../db/migrations/000013_identity_epoch_events.up.sql")
	if e != nil {
		t.Fatal(e)
	}
	for n := 0; n < 2; n++ {
		if _, e = pool.Exec(ctx, string(sql)); e != nil {
			t.Fatal("迁移不可重入", e)
		}
	}
	principal := testID()
	if _, e = pool.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES($1,'test','owner')`, principal); e != nil {
		t.Fatal(e)
	}
	if _, e = pool.Exec(ctx, `UPDATE principals SET status='disabled' WHERE id=$1`, principal); e != nil {
		t.Fatal(e)
	}
	if _, e = pool.Exec(ctx, `UPDATE principals SET status='active' WHERE id=$1`, principal); e != nil {
		t.Fatal(e)
	}
	var epoch int64
	if e = pool.QueryRow(ctx, `SELECT identity_epoch FROM principals WHERE id=$1`, principal).Scan(&epoch); e != nil || epoch != 1 {
		t.Fatal("重新启用改变 epoch", epoch, e)
	}
	if _, e = pool.Exec(ctx, `UPDATE principals SET identity_epoch=0 WHERE id=$1`, principal); e == nil {
		t.Fatal("epoch 可以回退")
	}
	if _, e = pool.Exec(ctx, string(sql)); e != nil {
		t.Fatal("有数据迁移重入失败", e)
	}
	if e = pool.QueryRow(ctx, `SELECT identity_epoch FROM principals WHERE id=$1`, principal).Scan(&epoch); e != nil || epoch != 1 {
		t.Fatal("重入重置 epoch", epoch, e)
	}
	t.Log("原生 PG：基线升级、空库全迁移、000013 空库/有数据重入、epoch 单调通过")
}
