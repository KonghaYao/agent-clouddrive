package application

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

// Review regressions use a real disposable PostgreSQL schema, while explicitly
// stubbing native storage and identity through the shared application fixtures.
func reviewHistoricalSource(t *testing.T) (*Service, Actor, string, string, string) {
	t.Helper()
	s, a, job, _, _ := workerFixture(t)
	a.Interactive = true
	ctx := context.Background()
	old := uuidText(job.SourceVersionID)
	var fid string
	if e := s.DB.QueryRow(ctx, `SELECT file_id::text FROM file_versions WHERE id=$1`, old).Scan(&fid); e != nil {
		t.Fatal(e)
	}
	newer := id()
	if _, e := s.DB.Exec(ctx, `INSERT INTO file_versions(id,file_id,version_seq,size,sha256,content_type,native_etag,native_revision_ref) SELECT $2,file_id,2,10,sha256,content_type,'newer-etag',native_revision_ref FROM file_versions WHERE id=$1`, old, newer); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.Exec(ctx, `UPDATE files SET current_version_id=$2,size=10 WHERE id=$1`, fid, newer); e != nil {
		t.Fatal(e)
	}
	return s, a, fid, old, newer
}

type reviewBlockedMove struct {
	*uploadStore
	started chan struct{}
	release chan struct{}
}

func (b *reviewBlockedMove) Move(ctx context.Context, request ports.MoveRequest) (ports.OriginalMetadata, error) {
	close(b.started)
	select {
	case <-b.release:
		return b.uploadStore.Move(ctx, request)
	case <-ctx.Done():
		return ports.OriginalMetadata{}, ctx.Err()
	}
}

func TestReviewUploadCannotEnterPendingMoveTarget(t *testing.T) {
	s, a, native := uploadFixture(t)
	f := seedUpload(t, s, a, "source.txt", 3)
	blocked := &reviewBlockedMove{uploadStore: native, started: make(chan struct{}), release: make(chan struct{})}
	s.Original = blocked
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	moved := make(chan error, 1)
	go func() {
		_, e := s.Mutate(ctx, a, "rename", Object{"file_id": str(f, "id"), "name": "target.txt", "expected_metadata_revision": num(f, "metadata_revision"), "idempotency_key": "pending-target-rename"})
		moved <- e
	}()
	select {
	case <-blocked.started:
	case e := <-moved:
		t.Fatal("rename failed before native boundary", e)
	case <-ctx.Done():
		t.Fatal("rename never reached native boundary")
	}
	_, uploadErr := s.BeginUpload(ctx, a, Object{"name": "target.txt", "size_bytes": int64(7), "idempotency_key": "competing-target-upload"})
	close(blocked.release)
	moveErr := <-moved
	if uploadErr == nil {
		t.Fatalf("new tus upload was admitted into persisted MOVE target; native upload calls=%d; eventual MOVE commit=%v", native.calls, moveErr)
	}
	code := domain.ErrorCode(uploadErr)
	if code != domain.PathConflict && code != domain.OperationPending {
		t.Fatalf("wrong admission error: %v", uploadErr)
	}
	if native.calls != 1 || moveErr != nil {
		t.Fatalf("reservation failed to protect native commit: native upload calls=%d move error=%v", native.calls, moveErr)
	}
}

func TestReviewAnonymousFixedShareSelectsAuthorizedSource(t *testing.T) {
	s, a, fid, old, newer := reviewHistoricalSource(t)
	ctx := context.Background()
	pending, e := s.RequestShare(ctx, a, Object{"file_id": fid, "audience_type": "link", "version_policy": "fixed", "version_id": old, "rights": []string{"preview"}})
	if e != nil {
		t.Fatal(e)
	}
	approved, e := s.ApproveShare(ctx, a, str(pending.(Object), "id"))
	if e != nil {
		t.Fatal(e)
	}
	raw := strings.TrimPrefix(str(approved.(Object), "url"), strings.TrimRight(s.Config.PublicURL, "/")+"/s/")
	f, _, e := s.linkFile(ctx, raw, "")
	if e != nil || str(f, "preview_version_id") != old {
		t.Fatalf("explicit historical link cannot open its authorized source: version=%q error=%v", str(f, "preview_version_id"), e)
	}
	if _, _, e = s.linkFile(ctx, raw, old); e != nil {
		t.Fatal("authorized historical page denied", e)
	}
	if _, _, e = s.linkFile(ctx, raw, newer); e == nil {
		t.Fatal("fixed historical link admitted the unauthorized current source")
	}
}

func TestReviewFixedReceivedShareDoesNotExposeCurrentVersionMetadata(t *testing.T) {
	s, a, fid, old, newer := reviewHistoricalSource(t)
	ctx := context.Background()
	receiver := a
	receiver.Principal.ID = id()
	pending, e := s.RequestShare(ctx, a, Object{"file_id": fid, "audience_type": "user", "audience_id": receiver.Principal.ID, "version_policy": "fixed", "version_id": old, "rights": []string{"read", "download"}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ApproveShare(ctx, a, str(pending.(Object), "id")); e != nil {
		t.Fatal(e)
	}
	listed, e := s.Shared(ctx, receiver)
	if e != nil {
		t.Fatal(e)
	}
	items := listed.(Object)["items"].([]Object)
	if len(items) != 1 {
		t.Fatalf("expected one received share, got %d", len(items))
	}
	f := items[0]["file"].(Object)
	if str(f, "current_version_id") == newer || str(f, "current_version_id") != old || num(f, "size") == 10 {
		t.Fatalf("historical grant leaked newer source metadata: current=%q size=%d", str(f, "current_version_id"), num(f, "size"))
	}
	if _, _, e = s.SharedFile(ctx, receiver, fid, newer, domain.ActionDownload); e == nil {
		t.Fatal("fixed received grant admitted the unauthorized current source")
	}
	receiver.Principal.Type = domain.ActorAgent
	if _, e = s.Shared(ctx, receiver); e == nil {
		t.Fatal("Agent admitted received-share listing")
	}
}

func TestReviewPinnedMarkdownSurvivesBusinessTrash(t *testing.T) {
	s, a, fid, old, _ := reviewHistoricalSource(t)
	ctx := context.Background()
	aid := id()
	md, e := s.publishBytes(ctx, "tmp/review/1/content.md", "markdown/"+aid+"/content.md", "text/markdown", []byte("# Retained historical text"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(ctx, `INSERT INTO artifact_sets(id,source_version_id,generation,recipe_fingerprint,recipe_snapshot,manifest_key,markdown_key,manifest_sha256,markdown_sha256,completeness,index_state,indexed_at,published_at) VALUES($1,$2,1,$3,'{}',$4,$5,$6,$6,'{"status":"ready","total_pages":1,"missing_text_pages":[],"missing_description_pages":[]}','succeeded',now(),now())`, aid, old, "review-recipe", "manifests/"+aid+"/manifest.json", md.Key, md.SHA256); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(ctx, `UPDATE files SET lifecycle='trashed' WHERE id=$1`, fid); e != nil {
		t.Fatal(e)
	}
	if _, e = s.markdown(ctx, a, Object{"file_id": fid, "artifact_set_id": aid}); e != nil {
		t.Fatalf("owner's pinned historical artifact stopped resolving after retained delete: %v", e)
	}
	other := a
	other.Principal.ID = id()
	if _, e = s.markdown(ctx, other, Object{"file_id": fid, "artifact_set_id": aid}); e == nil {
		t.Fatal("other principal read retained historical Markdown")
	}
}

func reviewRetainedArtifact(t *testing.T, s *Service, vid string, generation int) string {
	t.Helper()
	ctx := context.Background()
	aid := id()
	md, e := s.publishBytes(ctx, "tmp/review/"+aid+"/content.md", "markdown/"+aid+"/content.md", "text/markdown", []byte("# Explicit audit content"))
	if e != nil {
		t.Fatal(e)
	}
	asset, e := s.publishBytes(ctx, "tmp/review/"+aid+"/image", "images/"+aid+"/review-image.png", "image/png", []byte("retained-image-bytes"))
	if e != nil {
		t.Fatal(e)
	}
	manifest, e := s.publishBytes(ctx, "tmp/review/"+aid+"/manifest.json", "manifests/"+aid+"/manifest.json", "application/json", js(Object{"assets": []Object{{"id": "review-image", "key": asset.Key}}}))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(ctx, `INSERT INTO artifact_sets(id,source_version_id,generation,recipe_fingerprint,recipe_snapshot,manifest_key,markdown_key,manifest_sha256,markdown_sha256,completeness,index_state,indexed_at,published_at) VALUES($1,$2,$3,'audit-review','{}',$4,$5,$6,$7,'{"status":"ready","total_pages":1,"missing_text_pages":[],"missing_description_pages":[]}','succeeded',now(),now())`, aid, vid, generation, manifest.Key, md.Key, manifest.SHA256, md.SHA256); e != nil {
		t.Fatal(e)
	}
	return aid
}

func TestReviewAuditRequiresSpecificActiveHumanGrant(t *testing.T) {
	s, owner, fid, old, _ := reviewHistoricalSource(t)
	ctx := context.Background()
	s.Identity = mutationIdentity{} // Explicitly a lawful-delegation contract stub.
	aid := reviewRetainedArtifact(t, s, old, 1)
	auditor := owner
	auditor.Principal.ID = id()
	auditor.Subject.Subject = "review-auditor"
	auditor.Principal.Scopes = domain.ScopeSet{}
	if _, e := s.DB.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES($1,'test','review-auditor')`, auditor.Principal.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.Exec(ctx, `INSERT INTO reviewer_grants(principal_id,can_view_jobs,can_triage_feedback,granted_by) VALUES($1,true,true,$2)`, auditor.Principal.ID, owner.Principal.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.AuditVersions(ctx, auditor, fid); e == nil {
		t.Fatal("generic reviewer/admin role granted private history access")
	}
	if _, e := s.DB.Exec(ctx, `INSERT INTO audit_grants(principal_id,file_id,granted_by,expires_at) VALUES($1,$2,$3,now()+interval '1 hour')`, auditor.Principal.ID, fid, owner.Principal.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.Exec(ctx, `UPDATE files SET lifecycle='trashed' WHERE id=$1`, fid); e != nil {
		t.Fatal(e)
	}
	versions, e := s.AuditVersions(ctx, auditor, fid)
	if e != nil || len(versions.(Object)["items"].([]Object)) != 2 {
		t.Fatal("explicit audit cannot inspect retained history", versions, e)
	}
	if _, e = s.AuditContent(ctx, auditor, fid, "", nil); e == nil {
		t.Fatal("audit silently selected a mutable current version")
	}
	content, e := s.AuditContent(ctx, auditor, fid, old, nil)
	if e != nil {
		t.Fatal("explicit pinned original audit denied", e)
	}
	content.Body.Close()
	content, e = s.AuditMarkdown(ctx, auditor, fid, aid)
	if e != nil {
		t.Fatal("explicit pinned artifact audit denied", e)
	}
	body, readErr := io.ReadAll(content.Body)
	content.Body.Close()
	if readErr != nil || string(body) != "# Explicit audit content" {
		t.Fatal("wrong retained artifact", string(body), readErr)
	}
	content, e = s.AuditAsset(ctx, auditor, fid, aid, "review-image")
	if e != nil {
		t.Fatal("explicit audit cannot read retained referenced image", e)
	}
	body, readErr = io.ReadAll(content.Body)
	content.Body.Close()
	if readErr != nil || string(body) != "retained-image-bytes" {
		t.Fatal("wrong retained image", string(body), readErr)
	}
	for _, asset := range []string{"", "not-in-manifest", "../review-image", "folder\\review-image"} {
		if _, e = s.AuditAsset(ctx, auditor, fid, aid, asset); e == nil {
			t.Fatal("audit image escaped manifest or accepted path traversal", asset)
		}
	}
	otherFID := id()
	if _, e = s.DB.Exec(ctx, `INSERT INTO files(id,owner_id,space_id,name,path,kind,native_instance,native_resource_id) SELECT $2,owner_id,space_id,'other.txt','/other.txt','file',native_instance,'review-other-native' FROM files WHERE id=$1`, fid, otherFID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.AuditVersions(ctx, auditor, otherFID); e == nil {
		t.Fatal("audit grant expanded to a different file")
	}
	if _, e = s.DB.Exec(ctx, `INSERT INTO audit_grants(principal_id,file_id,granted_by) VALUES($1,$2,$3)`, auditor.Principal.ID, otherFID, owner.Principal.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.AuditMarkdown(ctx, auditor, otherFID, aid); e == nil {
		t.Fatal("artifact source/file binding was not enforced")
	}
	agent := auditor
	agent.Principal.Type = domain.ActorAgent
	if _, e = s.AuditVersions(ctx, agent, fid); e == nil {
		t.Fatal("Agent obtained audit even with a matching per-file grant")
	}
	if _, e = s.DB.Exec(ctx, `UPDATE principals SET status='disabled' WHERE id=$1`, owner.Principal.ID); e != nil {
		t.Fatal(e)
	}
	content, e = s.AuditMarkdown(ctx, auditor, fid, aid)
	if e != nil {
		t.Fatal("owner disable incorrectly removed independent retained artifact audit", e)
	}
	content.Body.Close()
	if _, e = s.DB.Exec(ctx, `UPDATE audit_grants SET expires_at=now()-interval '1 second' WHERE principal_id=$1 AND file_id=$2`, auditor.Principal.ID, fid); e != nil {
		t.Fatal(e)
	}
	if _, e = s.AuditMarkdown(ctx, auditor, fid, aid); e == nil {
		t.Fatal("expired audit grant retained artifact access")
	}
	if _, e = s.DB.Exec(ctx, `UPDATE audit_grants SET expires_at=NULL WHERE principal_id=$1 AND file_id=$2`, auditor.Principal.ID, fid); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(ctx, `UPDATE principals SET status='disabled' WHERE id=$1`, auditor.Principal.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.AuditVersions(ctx, auditor, fid); e == nil {
		t.Fatal("disabled auditor retained access from stale actor state")
	}
	var reads int
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM audit_reads WHERE principal_id=$1 AND file_id=$2`, auditor.Principal.ID, fid).Scan(&reads); e != nil || reads != 5 {
		t.Fatalf("audit reads not explicitly recorded or rejected reads logged as success: count=%d error=%v", reads, e)
	}
}

func TestReviewUploadReloadsParentAfterSpaceSerialization(t *testing.T) {
	s, a, _ := uploadFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dir, e := s.Mutate(ctx, a, "mkdir", Object{"name": "before", "idempotency_key": "review-parent"})
	if e != nil {
		t.Fatal(e)
	}
	pid := str(dir.(Object), "file_id")
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(context.Background())
	if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(space_id::text,0)) FROM files WHERE id=$1`, pid); e != nil {
		t.Fatal(e)
	}
	type result struct {
		value any
		err   error
	}
	uploaded := make(chan result, 1)
	go func() {
		value, err := s.BeginUpload(ctx, a, Object{"parent_id": pid, "name": "child.txt", "size_bytes": int64(7), "idempotency_key": "review-delayed-child"})
		uploaded <- result{value, err}
	}()
	for {
		var waiting bool
		if e = s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND wait_event='advisory')`).Scan(&waiting); e != nil {
			t.Fatal(e)
		}
		if waiting {
			break
		}
		select {
		case early := <-uploaded:
			t.Fatal("upload did not wait for space lock", early.err)
		case <-ctx.Done():
			t.Fatal("no deterministic upload admission barrier")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// Simulate the committed projection of a parent rename while this transaction
	// owns the serialization lock. Stable native parent identity is unchanged.
	if _, e = tx.Exec(ctx, `UPDATE files SET name='after',path='/after',metadata_revision=metadata_revision+1 WHERE id=$1`, pid); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	out := <-uploaded
	if out.err != nil {
		t.Fatal(out.err)
	}
	var path string
	if e = s.DB.QueryRow(ctx, `SELECT path FROM files WHERE id=$1`, str(out.value.(Object), "file_id")).Scan(&path); e != nil || path != "/after/child.txt" {
		t.Fatalf("new source used stale pre-serialization parent path: %q error=%v", path, e)
	}
}

type reviewCancelCredential struct {
	mutationIdentity
	cancel context.CancelFunc
}

func (c reviewCancelCredential) DelegatedCredential(context.Context, ports.DelegationRequest) (ports.Credential, error) {
	c.cancel()
	return ports.Credential{}, context.Canceled
}

func TestReviewCanceledBeforeNativeUploadReleasesReservation(t *testing.T) {
	s, a, native := uploadFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Identity = reviewCancelCredential{cancel: cancel}
	if _, e := s.BeginUpload(ctx, a, Object{"name": "cancelled.txt", "size_bytes": int64(123), "idempotency_key": "cancel-before-native"}); e == nil {
		t.Fatal("cancelled credential resolution unexpectedly succeeded")
	}
	var state, reservation string
	var reserved int64
	if e := s.DB.QueryRow(context.Background(), `SELECT o.state,r.state,q.reserved_bytes FROM file_operations o JOIN upload_sessions u ON u.operation_id=o.id JOIN quota_reservations r ON r.upload_id=u.id JOIN quota_accounts q ON q.owner_id=r.owner_id WHERE o.idempotency_key='cancel-before-native'`).Scan(&state, &reservation, &reserved); e != nil {
		t.Fatal(e)
	}
	if state != "failed_confirmed" || reservation != "released" || reserved != 0 || native.calls != 0 {
		t.Fatalf("pre-dispatch cancellation stranded upload admission: operation=%s reservation=%s held_bytes=%d native_calls=%d", state, reservation, reserved, native.calls)
	}
	s.Identity = mutationIdentity{}
	if _, e := s.BeginUpload(context.Background(), a, Object{"name": "cancelled.txt", "size_bytes": int64(123), "idempotency_key": "retry-after-cancel"}); e != nil {
		t.Fatal("definitively unstarted upload retained its path reservation", e)
	}
}
