package application

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/agentclouddrive/agent-clouddrive/internal/dbgen"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

type repairIndex struct {
	ports.SearchIndex
	mu                   sync.Mutex
	docs                 map[string]ports.SearchDocument
	onUpsert             func() error
	failWait, failDelete bool
}

func (i *repairIndex) Upsert(_ context.Context, docs []ports.SearchDocument) (ports.SearchTask, error) {
	if i.onUpsert != nil {
		f := i.onUpsert
		i.onUpsert = nil
		if err := f(); err != nil {
			return ports.SearchTask{}, err
		}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.docs == nil {
		i.docs = map[string]ports.SearchDocument{}
	}
	for _, d := range docs {
		i.docs[d.ID] = d
	}
	return ports.SearchTask{UID: 1}, nil
}
func (i *repairIndex) Wait(context.Context, ports.SearchTask) error {
	if i.failWait {
		return ports.ErrUnavailable
	}
	return nil
}
func (i *repairIndex) DeleteArtifactSet(_ context.Context, owner, aid string) (ports.SearchTask, error) {
	if i.failDelete {
		return ports.SearchTask{}, ports.ErrUnavailable
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	for id, d := range i.docs {
		if d.OwnerID == owner && d.ArtifactSetID == aid {
			delete(i.docs, id)
		}
	}
	return ports.SearchTask{UID: 2}, nil
}
func (i *repairIndex) UpdatePaths(_ context.Context, _ string, paths map[string]string) (ports.SearchTask, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for id, d := range i.docs {
		if path, ok := paths[d.FileID]; ok {
			d.SourcePath = path
			i.docs[id] = d
		}
	}
	return ports.SearchTask{UID: 3}, nil
}
func (i *repairIndex) Query(_ context.Context, q ports.SearchQuery) (ports.SearchCandidates, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	var hits []ports.SearchDocument
	for _, d := range i.docs {
		if d.OwnerID == q.OwnerID {
			hits = append(hits, d)
		}
	}
	return ports.SearchCandidates{Hits: hits, NextOffset: len(hits)}, nil
}
func repairClaim(t *testing.T, s *Service, job dbgen.ConversionJob) dbgen.ConversionJob {
	t.Helper()
	claimed, err := dbgen.New(s.DB).ClaimConversionJob(context.Background(), dbgen.ClaimConversionJobParams{JobID: job.ID, WorkerID: s.Config.NodeID, LeaseSeconds: s.leaseSeconds()})
	if err != nil {
		t.Fatal(err)
	}
	return claimed
}
func repairProcess(t *testing.T, s *Service, job dbgen.ConversionJob) dbgen.ConversionJob {
	t.Helper()
	ctx := context.Background()
	if err := s.processConversion(ctx, repairClaim(t, s, job), nil); err != nil {
		t.Fatal(err)
	}
	done, err := dbgen.New(s.DB).GetConversionJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	return done
}
func repairSQL(t *testing.T, s *Service, sql string, args ...any) {
	t.Helper()
	if _, err := s.DB.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}
func repairPointer(t *testing.T, s *Service, job dbgen.ConversionJob) string {
	t.Helper()
	var pointer string
	if err := s.DB.QueryRow(context.Background(), `SELECT COALESCE(search_artifact_set_id::text,'') FROM files WHERE current_version_id=$1`, job.SourceVersionID).Scan(&pointer); err != nil {
		t.Fatal(err)
	}
	return pointer
}
func repairVisible(t *testing.T, s *Service, actor Actor, fid string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.markdown(ctx, actor, Object{"file_id": fid}); err != nil {
		t.Fatal("默认 Markdown", err)
	}
	result, err := s.search(ctx, actor, Object{"query": "aurorafencing"})
	if err != nil || len(result.(map[string]any)["items"].([]Object)) == 0 {
		t.Fatal("默认搜索", result, err)
	}
}

func TestRepairWorkerCompletionDuringTrashAndRestore(t *testing.T) {
	for _, when := range []string{"before_delete", "during_trash", "during_restore"} {
		t.Run(when, func(t *testing.T) {
			s, actor, job, original, _ := workerFixture(t)
			ctx := context.Background()
			s.Identity = mutationIdentity{}
			var fid string
			if err := s.DB.QueryRow(ctx, `SELECT file_id::text FROM file_versions WHERE id=$1`, job.SourceVersionID).Scan(&fid); err != nil {
				t.Fatal(err)
			}
			original.objects[original.ref.ResourceID] = ports.OriginalMetadata{ResourceRef: original.ref.ResourceRef, Name: "original.txt", RelativePath: "original.txt", ETag: original.ref.ETag, Size: int64(len(original.data))}
			index := &repairIndex{}
			s.Search = index
			var trashID string
			remove := func() error {
				var revision int64
				if err := s.DB.QueryRow(ctx, `SELECT metadata_revision FROM files WHERE id=$1`, fid).Scan(&revision); err != nil {
					return err
				}
				if _, err := s.Mutate(ctx, actor, "delete", Object{"file_id": fid, "expected_metadata_revision": revision, "idempotency_key": "repair-delete"}); err != nil {
					return err
				}
				return s.DB.QueryRow(ctx, `SELECT id::text FROM trash_entries WHERE root_file_id=$1`, fid).Scan(&trashID)
			}
			restore := func() error {
				var revision int64
				if err := s.DB.QueryRow(ctx, `SELECT metadata_revision FROM files WHERE id=$1`, fid).Scan(&revision); err != nil {
					return err
				}
				_, err := s.Mutate(ctx, actor, "restore", Object{"trash_id": trashID, "expected_metadata_revision": revision, "idempotency_key": "repair-restore"})
				return err
			}
			if when != "before_delete" {
				index.onUpsert = func() error {
					if err := remove(); err != nil {
						return err
					}
					if when == "during_restore" {
						return restore()
					}
					return nil
				}
			}
			done := repairProcess(t, s, job)
			if done.State != "ready" {
				t.Fatal(done.State)
			}
			if when == "during_trash" && repairPointer(t, s, job) != "" {
				t.Fatal("回收站期间提前激活")
			}
			if when == "before_delete" {
				if err := remove(); err != nil {
					t.Fatal(err)
				}
			}
			if when != "during_restore" {
				if err := restore(); err != nil {
					t.Fatal(err)
				}
			}
			// delete/restore 的原生历史保留验证也会 OpenRevision；只比较恢复激活段。
			readsBeforeActivation := original.reads
			for _, event := range []string{"file.delete", "file.restore"} {
				if err := s.indexMaintenance(ctx, dbgen.Outbox{EventType: event, AggregateID: pgUUID(fid), Payload: []byte(`{}`)}); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.reconcileArtifactVisibility(ctx); err != nil {
				t.Fatal(err)
			}
			if pointer := repairPointer(t, s, job); pointer != uuidText(done.ArtifactSetID) {
				t.Fatal(pointer, uuidText(done.ArtifactSetID))
			}
			repairVisible(t, s, actor, fid)
			if original.reads != readsBeforeActivation {
				t.Fatal("恢复激活重读原件", original.reads, readsBeforeActivation)
			}
			t.Log("真实 PG、真实 delete/restore、激活不重读原件、默认 Markdown/搜索一致")
		})
	}
}

func TestRepairWorkerActivationFailureRestartAndDeleteRace(t *testing.T) {
	s, _, job, _, _ := workerFixture(t)
	ctx := context.Background()
	index := &repairIndex{}
	s.Search = index
	index.onUpsert = func() error {
		_, err := s.DB.Exec(ctx, `UPDATE files SET lifecycle='trashed' WHERE current_version_id=$1`, job.SourceVersionID)
		return err
	}
	done := repairProcess(t, s, job)
	if repairPointer(t, s, job) != "" {
		t.Fatal("inactive pointer")
	}
	repairSQL(t, s, `UPDATE files SET lifecycle='active' WHERE current_version_id=$1`, job.SourceVersionID)
	index.failWait = true
	if err := s.reconcileArtifactVisibility(ctx); err != nil {
		t.Fatal(err)
	}
	if repairPointer(t, s, job) != "" {
		t.Fatal("索引未确认却激活")
	}
	index.failWait = false
	// 新 Service 对象代表 worker 重启，任务不依赖原进程内存。
	s = &Service{DB:s.DB,Config:s.Config,Original:s.Original,Artifacts:s.Artifacts,Identity:s.Identity,Search:s.Search,Converter:s.Converter}
	repairSQL(t, s, `UPDATE artifact_activation_tasks SET retry_at=now()`)
	index.onUpsert = func() error {
		_, err := s.DB.Exec(ctx, `UPDATE files SET lifecycle='trashed' WHERE current_version_id=$1`, job.SourceVersionID)
		return err
	}
	if err := s.reconcileArtifactVisibility(ctx); err != nil {
		t.Fatal(err)
	}
	if repairPointer(t, s, job) != "" {
		t.Fatal("再次删除仍激活")
	}
	repairSQL(t, s, `UPDATE files SET lifecycle='active' WHERE current_version_id=$1`, job.SourceVersionID)
	if err := s.reconcileArtifactVisibility(ctx); err != nil {
		t.Fatal(err)
	}
	if repairPointer(t, s, job) != uuidText(done.ArtifactSetID) {
		t.Fatal("重试未激活")
	}
}

func TestRepairWorkerAncestorRestoreWithOldPointer(t *testing.T) {
	s, actor, first, _, _ := workerFixture(t)
	ctx := context.Background()
	index := &repairIndex{}
	s.Search = index
	old := repairProcess(t, s, first)
	s.Identity = mutationIdentity{}
	parent := mutationCreate(t, s, actor, "parent")
	repairSQL(t, s, `UPDATE files SET parent_id=$2,path='/parent/original.txt' WHERE current_version_id=$1`, first.SourceVersionID, str(parent, "id"))
	q := dbgen.New(s.DB)
	recipe := s.Recipe()
	newJob, err := q.RequestConversion(ctx, dbgen.RequestConversionParams{SourceVersionID: first.SourceVersionID, RecipeFingerprint: recipe.Fingerprint, RecipeSnapshot: js(recipe)})
	if err != nil {
		t.Fatal(err)
	}
	index.onUpsert = func() error {
		_, e := s.DB.Exec(ctx, `UPDATE files SET lifecycle='trashed' WHERE id=$1`, str(parent, "id"))
		return e
	}
	newDone := repairProcess(t, s, newJob)
	if repairPointer(t, s, first) != uuidText(old.ArtifactSetID) {
		t.Fatal("旧 pointer 应保留")
	}
	// 清理先行不会删除不可变产物；恢复必须重新确认索引。
	if err = s.reconcileArtifactVisibility(ctx); err != nil {
		t.Fatal(err)
	}
	repairSQL(t, s, `UPDATE files SET lifecycle='active' WHERE id=$1`, str(parent, "id"))
	if err = s.reconcileArtifactVisibility(ctx); err != nil {
		t.Fatal(err)
	}
	if repairPointer(t, s, first) != uuidText(newDone.ArtifactSetID) {
		t.Fatal("祖先恢复未激活最新代")
	}
	if _, err = s.markdown(ctx, actor, Object{"file_id": str(parent, "id")}); err == nil {
		t.Fatal("目录不是 Markdown")
	}
	var fid string
	if err = s.DB.QueryRow(ctx, `SELECT file_id::text FROM file_versions WHERE id=$1`, first.SourceVersionID).Scan(&fid); err != nil {
		t.Fatal(err)
	}
	if _, err = s.markdown(ctx, actor, Object{"file_id": fid, "artifact_set_id": uuidText(old.ArtifactSetID)}); err != nil {
		t.Fatal("固定旧产物不可读", err)
	}
}

func TestRepairWorkerLoserCleanupRestartAndLateUpsert(t *testing.T) {
	s, actor, first, _, _ := workerFixture(t)
	ctx := context.Background()
	index := &repairIndex{}
	s.Search = index
	q := dbgen.New(s.DB)
	recipe := s.Recipe()
	second, err := q.RequestConversion(ctx, dbgen.RequestConversionParams{SourceVersionID: first.SourceVersionID, RecipeFingerprint: recipe.Fingerprint, RecipeSnapshot: js(recipe)})
	if err != nil {
		t.Fatal(err)
	}
	winner := repairProcess(t, s, second)
	loser := repairProcess(t, s, first)
	if repairPointer(t, s, first) != uuidText(winner.ArtifactSetID) {
		t.Fatal("旧代倒退 pointer")
	}
	var stale []ports.SearchDocument
	for _, d := range index.docs {
		if d.ArtifactSetID == uuidText(loser.ArtifactSetID) {
			stale = append(stale, d)
		}
	}
	if len(stale) == 0 {
		t.Fatal("测试没有落败块")
	}
	index.failDelete = true
	if err = s.reconcileArtifactVisibility(ctx); err != nil {
		t.Fatal(err)
	}
	index.failDelete = false
	s = &Service{DB:s.DB,Config:s.Config,Original:s.Original,Artifacts:s.Artifacts,Identity:s.Identity,Search:s.Search,Converter:s.Converter}
	repairSQL(t, s, `UPDATE artifact_cleanup_tasks SET retry_at=now()`)
	if err = s.reconcileArtifactVisibility(ctx); err != nil {
		t.Fatal(err)
	}
	for _, d := range index.docs {
		if d.ArtifactSetID != uuidText(winner.ArtifactSetID) {
			t.Fatal("落败块未清理")
		}
	}
	// 模拟已派发响应未知的旧 Upsert 在清理完成后才到达远端。
	if _, err = index.Upsert(ctx, stale); err != nil {
		t.Fatal(err)
	}
	repairSQL(t, s, `UPDATE artifact_cleanup_tasks SET retry_at=now()`)
	if err = s.reconcileArtifactVisibility(ctx); err != nil {
		t.Fatal(err)
	}
	for _, d := range index.docs {
		if d.ArtifactSetID != uuidText(winner.ArtifactSetID) {
			t.Fatal("迟到 Upsert 复活废块")
		}
	}
	var fid string
	if err = s.DB.QueryRow(ctx, `SELECT file_id::text FROM file_versions WHERE id=$1`, first.SourceVersionID).Scan(&fid); err != nil {
		t.Fatal(err)
	}
	repairVisible(t, s, actor, fid)
	if _, err = s.markdown(ctx, actor, Object{"file_id": fid, "artifact_set_id": uuidText(loser.ArtifactSetID)}); err != nil {
		t.Fatal(err)
	}
	// 旧源版本的完成结果不得覆盖当前版本。
	repairSQL(t, s, `UPDATE files SET search_artifact_set_id=NULL WHERE current_version_id=$1`, first.SourceVersionID)
	var activated bool
	if err = s.DB.QueryRow(ctx, `SELECT activate_artifact((SELECT file_id FROM file_versions WHERE id=$1),$2)`, first.SourceVersionID, winner.ArtifactSetID).Scan(&activated); err != nil || !activated {
		t.Fatal(activated, err)
	}
	t.Log("新代先完成、旧代晚到、删除失败/重启、迟到 Upsert 均收敛；固定旧产物可读")
}

func TestRepairWorkerMigrationReentrantAndPageFence(t *testing.T) {
	s, _, job, _, _ := workerFixture(t)
	ctx := context.Background()
	migration, err := os.ReadFile("../../db/migrations/000011_artifact_visibility_pages.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(ctx, string(migration)); err != nil {
		t.Fatal("迁移重入", err)
	}
	claimed := repairClaim(t, s, job)
	q := dbgen.New(s.DB)
	params := dbgen.SavePageResultParams{JobID: job.ID, PageNo: 1, Stage: "text", Attempt: claimed.Attempt, LeaseToken: claimed.LeaseToken, State: "ready", ResultRef: []byte(`{}`)}
	if _, err = q.SavePageResult(ctx, params); err != nil {
		t.Fatal(err)
	}
	repairSQL(t, s, `UPDATE conversion_jobs SET lease_until=now()-interval '1 second' WHERE id=$1`, job.ID)
	newClaim := repairClaim(t, s, job)
	if newClaim.LeaseToken <= claimed.LeaseToken {
		t.Fatal("fence 未推进")
	}
	if _, err = q.SavePageResult(ctx, params); err == nil {
		t.Fatal("旧 attempt 被接受")
	}
	params.Attempt = newClaim.Attempt
	params.LeaseToken = newClaim.LeaseToken
	if _, err = q.SavePageResult(ctx, params); err != nil {
		t.Fatal(err)
	}
	repairSQL(t, s, `UPDATE conversion_jobs SET state='failed',lease_owner=NULL,lease_until=NULL WHERE id=$1`, job.ID)
	if _, err = q.SavePageResult(ctx, params); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("取消后的页被接受", err)
	}
}
