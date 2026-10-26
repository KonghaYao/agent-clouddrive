package migrate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRepairUpgradeFromBaseline verifies that the repair migrations can be
// applied on top of a populated baseline schema and recover the known abnormal
// states without inventing evidence. It uses the native PostgreSQL instance
// from DRIVE_TEST_DATABASE_URL and an isolated schema.
func TestRepairUpgradeFromBaseline(t *testing.T) {
	url := os.Getenv("DRIVE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("DRIVE_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	schema := fmt.Sprintf("repair_upgrade_%d", time.Now().UnixNano())
	base, e := pgxpool.New(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = base.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); e != nil {
		base.Close()
		t.Fatal(e)
	}
	t.Cleanup(func() {
		_, _ = base.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		base.Close()
	})

	cfg, e := pgxpool.ParseConfig(url)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()

	// Apply only the audited baseline migrations first.
	baseline, e := os.MkdirTemp("", "repair-baseline-*")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(baseline)
	files, _ := filepath.Glob("../../db/migrations/00000[1-8]_*.up.sql")
	for _, f := range files {
		data, e := os.ReadFile(f)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(baseline, filepath.Base(f)), data, 0o600); e != nil {
			t.Fatal(e)
		}
	}
	if e = Apply(ctx, pool, baseline); e != nil {
		t.Fatalf("baseline migrations: %v", e)
	}
	seedBaseline(t, ctx, pool)

	// Apply the repair migrations on top of the populated baseline.
	if e = Apply(ctx, pool, "../../db/migrations"); e != nil {
		t.Fatalf("repair migrations: %v", e)
	}

	owner := "11111111-1111-1111-1111-111111111111"
	fileID := "55555555-5555-5555-5555-555555555555"
	opID := "88888888-8888-8888-8888-888888888888"
	versionID := "66666666-6666-6666-6666-666666666666"
	artifactID := "77777777-7777-7777-7777-777777777777"

	var legacyUnknown bool
	if e = pool.QueryRow(ctx, `SELECT legacy_dispatch_unknown FROM upload_sessions WHERE operation_id=$1`, opID).Scan(&legacyUnknown); e != nil {
		t.Fatal(e)
	}
	if !legacyUnknown {
		t.Fatal("executing upload must keep legacy_dispatch_unknown so unknown native writes are not released by age")
	}

	var sourceHolds, targetHolds int
	if e = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE hold_kind='source'), count(*) FILTER (WHERE hold_kind='target_parent') FROM file_operation_holds WHERE operation_id=$1`, opID).Scan(&sourceHolds, &targetHolds); e != nil {
		t.Fatal(e)
	}
	if sourceHolds != 1 || targetHolds != 0 {
		t.Fatalf("overwrite holds not backfilled: source=%d target=%d", sourceHolds, targetHolds)
	}
	moveOp := "dddddddd-dddd-dddd-dddd-dddddddddddd"
	var moveSource, moveTarget int
	if e = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE hold_kind='source'), count(*) FILTER (WHERE hold_kind='target_parent') FROM file_operation_holds WHERE operation_id=$1`, moveOp).Scan(&moveSource, &moveTarget); e != nil {
		t.Fatal(e)
	}
	if moveSource != 1 || moveTarget != 1 {
		t.Fatalf("move holds not backfilled: source=%d target=%d", moveSource, moveTarget)
	}

	var pointer string
	if e = pool.QueryRow(ctx, `SELECT COALESCE(search_artifact_set_id::text,'') FROM files WHERE id=$1`, fileID).Scan(&pointer); e != nil {
		t.Fatal(e)
	}
	if pointer != artifactID {
		t.Fatalf("published pointer changed during upgrade: %s", pointer)
	}

	var activation int
	if e = pool.QueryRow(ctx, `SELECT count(*) FROM artifact_activation_tasks WHERE file_id=$1 AND artifact_set_id=$2`, fileID, artifactID).Scan(&activation); e != nil {
		t.Fatal(e)
	}
	if activation != 1 {
		t.Fatalf("activation backfill missing: %d", activation)
	}

	var epoch int64
	if e = pool.QueryRow(ctx, `SELECT identity_epoch FROM principals WHERE id=$1`, owner).Scan(&epoch); e != nil {
		t.Fatal(e)
	}
	if epoch != 0 {
		t.Fatalf("existing principal epoch must start at 0, got %d", epoch)
	}
	var shareEpoch int64
	var shareState string
	if e = pool.QueryRow(ctx, `SELECT identity_epoch,state FROM share_grants WHERE file_id=$1`, fileID).Scan(&shareEpoch, &shareState); e != nil {
		t.Fatal(e)
	}
	if shareEpoch != 0 || shareState != "active" {
		t.Fatalf("existing share not preserved: epoch=%d state=%s", shareEpoch, shareState)
	}
	var auditID string
	if e = pool.QueryRow(ctx, `SELECT id::text FROM audit_grants WHERE file_id=$1`, fileID).Scan(&auditID); e != nil {
		t.Fatal(e)
	}
	if auditID == "" {
		t.Fatal("audit grant id backfill missing")
	}

	// The current job/artifact must remain readable and its version must not move.
	var currentVersion string
	if e = pool.QueryRow(ctx, `SELECT current_version_id::text FROM files WHERE id=$1`, fileID).Scan(&currentVersion); e != nil {
		t.Fatal(e)
	}
	if currentVersion != versionID {
		t.Fatalf("version changed: %s", currentVersion)
	}
}

func seedBaseline(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []string{
		`INSERT INTO principals(id,issuer,subject,display_name,status) VALUES ('11111111-1111-1111-1111-111111111111','iss','owner','Owner','active'),('22222222-2222-2222-2222-222222222222','iss','auditor','Auditor','active')`,
		`INSERT INTO spaces(id,owner_id,native_instance,native_space_id) VALUES ('33333333-3333-3333-3333-333333333333','11111111-1111-1111-1111-111111111111','https://ocis','space-1')`,
		`INSERT INTO quota_accounts(owner_id,limit_bytes,used_bytes,reserved_bytes) VALUES ('11111111-1111-1111-1111-111111111111',1000000,100,50)`,
		`INSERT INTO files(id,owner_id,space_id,parent_id,name,path,kind,lifecycle,size,native_instance,native_resource_id) VALUES ('44444444-4444-4444-4444-444444444444','11111111-1111-1111-1111-111111111111','33333333-3333-3333-3333-333333333333',NULL,'docs','/docs','directory','active',0,'https://ocis','dir-1')`,
		`INSERT INTO files(id,owner_id,space_id,parent_id,name,path,kind,lifecycle,size,native_instance,native_resource_id) VALUES ('55555555-5555-5555-5555-555555555555','11111111-1111-1111-1111-111111111111','33333333-3333-3333-3333-333333333333','44444444-4444-4444-4444-444444444444','a.txt','/docs/a.txt','file','active',100,'https://ocis','file-1')`,
		`INSERT INTO file_versions(id,file_id,version_seq,size,sha256,native_etag,native_revision_ref) VALUES ('66666666-6666-6666-6666-666666666666','55555555-5555-5555-5555-555555555555',1,100,repeat('a',64),'etag1','{"rev":"1"}')`,
		`UPDATE files SET current_version_id='66666666-6666-6666-6666-666666666666' WHERE id='55555555-5555-5555-5555-555555555555'`,
		`INSERT INTO artifact_sets(id,source_version_id,generation,recipe_fingerprint,manifest_key,markdown_key,manifest_sha256,markdown_sha256,completeness,index_state,indexed_at,published_at) VALUES ('77777777-7777-7777-7777-777777777777','66666666-6666-6666-6666-666666666666',1,'fp','manifest/1','markdown/1',repeat('b',64),repeat('c',64),'{"status":"ready"}','succeeded',now(),now())`,
		`UPDATE files SET search_artifact_set_id='77777777-7777-7777-7777-777777777777' WHERE id='55555555-5555-5555-5555-555555555555'`,
		`INSERT INTO share_approvals(id,requester_principal,file_id,request_hash,request_body,expires_at,state) VALUES ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa','11111111-1111-1111-1111-111111111111','55555555-5555-5555-5555-555555555555',repeat('e',64),'{"audience_type":"link","audience_id":null,"version_policy":"latest","version_id":null,"rights":["preview"]}',now()+interval '1 day','pending')`,
		`UPDATE share_approvals SET state='approved',approved_by='11111111-1111-1111-1111-111111111111',decided_at=now() WHERE id='aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa'`,
		`INSERT INTO share_grants(id,approval_id,file_id,creator_principal_id,audience_type,token_hash,rights,version_policy,state,approved_by) VALUES ('bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb','aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa','55555555-5555-5555-5555-555555555555','11111111-1111-1111-1111-111111111111','link',decode(repeat('ab',32),'hex'),ARRAY['preview']::text[],'latest','active','11111111-1111-1111-1111-111111111111')`,
		`INSERT INTO audit_grants(principal_id,file_id,granted_by,expires_at) VALUES ('22222222-2222-2222-2222-222222222222','55555555-5555-5555-5555-555555555555','11111111-1111-1111-1111-111111111111',now()+interval '1 day')`,
		`INSERT INTO file_operations(id,file_id,actor_principal_id,actor_type,kind,expected_revision,idempotency_key,request_hash,intent,state) VALUES ('88888888-8888-8888-8888-888888888888','55555555-5555-5555-5555-555555555555','11111111-1111-1111-1111-111111111111','user','overwrite',1,'idem-1',repeat('d',64),'{"target_parent_id":"44444444-4444-4444-4444-444444444444"}','prepared')`,
		`UPDATE file_operations SET state='executing',lease_owner='w1',lease_token=1,lease_until=now()+interval '5 min' WHERE id='88888888-8888-8888-8888-888888888888'`,
		`INSERT INTO upload_sessions(id,operation_id,expected_size,node_id,state,received_bytes,expires_at) VALUES ('99999999-9999-9999-9999-999999999999','88888888-8888-8888-8888-888888888888',200,'node1','uploading',50,now()+interval '1 hour')`,
		`UPDATE files SET active_operation_id='88888888-8888-8888-8888-888888888888' WHERE id='55555555-5555-5555-5555-555555555555'`,
		`INSERT INTO files(id,owner_id,space_id,parent_id,name,path,kind,lifecycle,size,native_instance,native_resource_id) VALUES ('cccccccc-cccc-cccc-cccc-cccccccccccc','11111111-1111-1111-1111-111111111111','33333333-3333-3333-3333-333333333333','44444444-4444-4444-4444-444444444444','b.txt','/docs/b.txt','file','active',0,'https://ocis','file-2')`,
		`INSERT INTO file_operations(id,file_id,actor_principal_id,actor_type,kind,expected_revision,idempotency_key,request_hash,intent,state) VALUES ('dddddddd-dddd-dddd-dddd-dddddddddddd','cccccccc-cccc-cccc-cccc-cccccccccccc','11111111-1111-1111-1111-111111111111','user','move',1,'idem-2',repeat('f',64),'{"target_parent_id":"44444444-4444-4444-4444-444444444444","target_name":"b.txt"}','prepared')`,
		`UPDATE file_operations SET state='executing',lease_owner='w2',lease_token=1,lease_until=now()+interval '5 min' WHERE id='dddddddd-dddd-dddd-dddd-dddddddddddd'`,
	}
	for _, stmt := range stmts {
		if _, e := pool.Exec(ctx, stmt); e != nil {
			t.Fatalf("seed: %v\n%s", e, stmt)
		}
	}
}
