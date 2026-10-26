package application

// These adversarial regressions use real PostgreSQL and local artifact files.
// Identity/original storage are explicit fixtures; the index uses controlled
// contract barriers or the real HTTP adapter against a protocol fixture. They
// are NOT native oCIS/Meilisearch throughput or browser acceptance evidence.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	searchadapter "github.com/agentclouddrive/agent-clouddrive/internal/adapters/search"
	"github.com/agentclouddrive/agent-clouddrive/internal/dbgen"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestStressReviewPlainTextMarkersCannotForgePagesOrAmplifyBlocks(t *testing.T) {
	// Less than one MiB, but previously generated 20,000 structural allocations
	// and invented page 999999 in a one-page plain-text source.
	markdown := strings.Repeat("<!-- page:999999 block:forged -->payload\n", 20000)
	manifest := conversionManifest{
		Engine:       ports.EngineInfo{Name: "utf8-text", Version: "1"},
		Completeness: ports.Completeness{Status: "ready", TotalPages: 1},
		Pages:        []ports.ConversionPage{{Page: 1, Block: "p1-b01", TextStatus: "ready", DescriptionStatus: "not_required"}},
	}
	blocks := conversionBlocks(markdown, manifest)
	if len(blocks) != 1 || blocks[0].Page != 1 || blocks[0].BlockID != "p1-b01" || blocks[0].Text != markdown {
		t.Fatalf("literal source comments became conversion structure: blocks=%d; expected intact one-page text", len(blocks))
	}
}

func TestStressReviewConvertedMarkersMustMatchBoundedManifest(t *testing.T) {
	manifest := conversionManifest{
		Engine:       ports.EngineInfo{Name: "synthetic-converter", Version: "1"},
		Completeness: ports.Completeness{Status: "ready", TotalPages: 2},
		Pages:        []ports.ConversionPage{{Page: 1, Block: "p1"}, {Page: 2, Block: "p2"}},
	}
	for name, markdown := range map[string]string{
		"too_many":      strings.Repeat("<!-- page:1 block:p1 -->text\n", 20000),
		"unknown_page":  "<!-- page:999999 block:p1 -->text",
		"unknown_block": "<!-- page:1 block:forged -->text",
		"duplicate":     "<!-- page:1 block:p1 -->one<!-- page:1 block:p1 -->two",
		"overflow":      "<!-- page:999999999999999999999999999999999999 block:p1 -->text",
	} {
		t.Run(name, func(t *testing.T) {
			blocks := conversionBlocks(markdown, manifest)
			if len(blocks) != 1 || blocks[0].Page != 0 || blocks[0].Text != markdown {
				t.Fatalf("ambiguous converter structure did not preserve unpaged text: blocks=%d", len(blocks))
			}
		})
	}
}

func TestStressReviewBlankParagraphsDoNotAllocatePerSeparator(t *testing.T) {
	text := strings.Repeat("\n\n", 200000) // 400 KiB only, no host memory pressure.
	blocks := []searchadapter.TextBlock{{Text: text}}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	docs, err := searchadapter.Chunk(ports.SearchDocument{}, blocks, 3072)
	runtime.ReadMemStats(&after)
	if err != nil || len(docs) != 0 {
		t.Fatal("blank input produced search documents", len(docs), err)
	}
	// The old strings.Split allocated ~3.2 MiB of string headers for no output.
	// Allow generous fixed overhead while rejecting that structural amplification.
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("blank separators amplified allocation: %d bytes for no chunks", allocated)
	}
}

type reviewDelayedPathIndex struct {
	ports.SearchIndex
	mu        sync.Mutex
	paths     map[string]string
	entered   chan struct{}
	release   chan struct{}
	delayOnce sync.Once
}

func (i *reviewDelayedPathIndex) UpdatePaths(ctx context.Context, _ string, paths map[string]string) (ports.SearchTask, error) {
	for _, p := range paths {
		if p == "/old" {
			wait := false
			i.delayOnce.Do(func() { wait = true; close(i.entered) })
			if wait {
				select {
				case <-i.release:
				case <-ctx.Done():
					return ports.SearchTask{}, ctx.Err()
				}
			}
		}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	for fid, p := range paths {
		i.paths[fid] = p
	}
	return ports.SearchTask{UID: 1}, nil
}

func (*reviewDelayedPathIndex) Wait(context.Context, ports.SearchTask) error { return nil }

func TestStressReviewLatePathSnapshotCannotOverwriteNewerMove(t *testing.T) {
	s, actor, _ := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fid := id()
	if _, err := s.DB.Exec(ctx, `INSERT INTO files(id,owner_id,space_id,name,path,kind,native_instance,native_resource_id) SELECT $1,$2,id,'old','/old','directory',native_instance,'review-path-race' FROM spaces WHERE owner_id=$2`, fid, actor.Principal.ID); err != nil {
		t.Fatal(err)
	}
	index := &reviewDelayedPathIndex{paths: map[string]string{}, entered: make(chan struct{}), release: make(chan struct{})}
	s.Search = index
	var release sync.Once
	unblock := func() { release.Do(func() { close(index.release) }) }
	defer unblock()
	oldDone := make(chan error, 1)
	go func() {
		oldDone <- s.indexMaintenance(ctx, dbgen.Outbox{EventType: "file.move", AggregateID: pgUUID(fid), Payload: []byte(`{}`)})
	}()
	select {
	case <-index.entered:
	case <-ctx.Done():
		t.Fatal("old event did not reach controlled pre-write barrier")
	}
	if _, err := s.DB.Exec(ctx, `UPDATE files SET name='new',path='/new',metadata_revision=metadata_revision+1 WHERE id=$1`, fid); err != nil {
		t.Fatal(err)
	}
	newDone := make(chan error, 1)
	go func() {
		newDone <- s.indexMaintenance(ctx, dbgen.Outbox{EventType: "file.move", AggregateID: pgUUID(fid), Payload: []byte(`{}`)})
	}()
	var newErr error
	newFinished := false
	select {
	case newErr = <-newDone:
		newFinished = true // Unfixed code writes the new path before the old one.
	case <-time.After(time.Second): // A correct serialization fix may wait here.
	case <-ctx.Done():
		t.Fatal("context expired before releasing old index write")
	}
	unblock()
	if err := <-oldDone; err != nil {
		t.Fatal("old maintenance event failed", err)
	}
	if !newFinished {
		newErr = <-newDone
	}
	if errors.Is(newErr, ports.ErrConflict) {
		// A nonblocking cross-worker lock must leave the event retryable.
		newErr = s.indexMaintenance(ctx, dbgen.Outbox{EventType: "file.move", AggregateID: pgUUID(fid), Payload: []byte(`{}`)})
	}
	if newErr != nil {
		t.Fatal("new maintenance event failed", newErr)
	}
	index.mu.Lock()
	got := index.paths[fid]
	index.mu.Unlock()
	if got != "/new" {
		t.Fatalf("both events succeeded but the late snapshot left stale raw index path %q; want /new", got)
	}
}

func TestStressReviewMaintenanceWorksWithOnePoolConnection(t *testing.T) {
	s, actor, _ := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	fid := id()
	if _, err := s.DB.Exec(ctx, `INSERT INTO files(id,owner_id,space_id,name,path,kind,native_instance,native_resource_id) SELECT $1,$2,id,'one','/one','directory',native_instance,'review-single-connection' FROM spaces WHERE owner_id=$2`, fid, actor.Principal.ID); err != nil {
		t.Fatal(err)
	}
	cfg := s.DB.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s.DB = pool
	s.Search = &pathBatchSearch{}
	if err := s.indexMaintenance(ctx, dbgen.Outbox{EventType: "file.move", AggregateID: pgUUID(fid), Payload: []byte(`{}`)}); err != nil {
		t.Fatalf("maintenance holds a pool connection while waiting for a second one: %v", err)
	}
}

func TestStressReviewBlockedOwnerDoesNotSerializeOtherOwners(t *testing.T) {
	s, actor, _ := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fid, otherFile, otherOwner, otherSpace := id(), id(), id(), id()
	if _, err := s.DB.Exec(ctx, `INSERT INTO files(id,owner_id,space_id,name,path,kind,native_instance,native_resource_id) SELECT $1,$2,id,'old','/old','directory',native_instance,'review-blocked-owner' FROM spaces WHERE owner_id=$2`, fid, actor.Principal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES($1,'test','other-review-owner')`, otherOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO spaces(id,owner_id,native_instance,native_space_id,native_root_resource_id) VALUES($1,$2,'http://ocis.test','review-other-space','root')`, otherSpace, otherOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO files(id,owner_id,space_id,name,path,kind,native_instance,native_resource_id) VALUES($1,$2,$3,'other','/other','directory','http://ocis.test','review-other-file')`, otherFile, otherOwner, otherSpace); err != nil {
		t.Fatal(err)
	}
	index := &reviewDelayedPathIndex{paths: map[string]string{}, entered: make(chan struct{}), release: make(chan struct{})}
	s.Search = index
	var release sync.Once
	unblock := func() { release.Do(func() { close(index.release) }) }
	defer unblock()
	oldDone := make(chan error, 1)
	go func() {
		oldDone <- s.indexMaintenance(ctx, dbgen.Outbox{EventType: "file.move", AggregateID: pgUUID(fid), Payload: []byte(`{}`)})
	}()
	select {
	case <-index.entered:
	case <-ctx.Done():
		t.Fatal("blocked owner did not reach barrier")
	}
	otherCtx, otherCancel := context.WithTimeout(ctx, time.Second)
	err := s.indexMaintenance(otherCtx, dbgen.Outbox{EventType: "file.move", AggregateID: pgUUID(otherFile), Payload: []byte(`{}`)})
	otherCancel()
	unblock()
	oldErr := <-oldDone
	if err != nil || oldErr != nil {
		t.Fatal("owner-specific contention affected an unrelated owner", err, oldErr)
	}
}

type reviewDelayedDeleteIndex struct {
	ports.SearchIndex
	mu        sync.Mutex
	docs      map[string]ports.SearchDocument
	entered   chan struct{}
	release   chan struct{}
	delayOnce sync.Once
}

func (i *reviewDelayedDeleteIndex) DeleteArtifactSet(ctx context.Context, _, artifact string) (ports.SearchTask, error) {
	wait := false
	i.delayOnce.Do(func() { wait = true; close(i.entered) })
	if wait {
		select {
		case <-i.release:
		case <-ctx.Done():
			return ports.SearchTask{}, ctx.Err()
		}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	for id, doc := range i.docs {
		if doc.ArtifactSetID == artifact {
			delete(i.docs, id)
		}
	}
	return ports.SearchTask{UID: 1}, nil
}

func (i *reviewDelayedDeleteIndex) Upsert(_ context.Context, docs []ports.SearchDocument) (ports.SearchTask, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, doc := range docs {
		i.docs[doc.ID] = doc
	}
	return ports.SearchTask{UID: 2}, nil
}

func (i *reviewDelayedDeleteIndex) UpdatePaths(_ context.Context, _ string, paths map[string]string) (ports.SearchTask, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for id, doc := range i.docs {
		if p, ok := paths[doc.FileID]; ok {
			doc.SourcePath = p
			i.docs[id] = doc
		}
	}
	return ports.SearchTask{UID: 3}, nil
}

func (*reviewDelayedDeleteIndex) Wait(context.Context, ports.SearchTask) error { return nil }

func TestStressReviewLateDeleteCannotEraseRestoredSearch(t *testing.T) {
	s, _, job, _, _ := workerFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	aid := reviewRetainedArtifact(t, s, uuidText(job.SourceVersionID), 1)
	var fid string
	if err := s.DB.QueryRow(ctx, `UPDATE files SET lifecycle='trashed',search_artifact_set_id=$2 WHERE current_version_id=$1 RETURNING id::text`, job.SourceVersionID, aid).Scan(&fid); err != nil {
		t.Fatal(err)
	}
	index := &reviewDelayedDeleteIndex{docs: map[string]ports.SearchDocument{}, entered: make(chan struct{}), release: make(chan struct{})}
	s.Search = index
	var release sync.Once
	unblock := func() { release.Do(func() { close(index.release) }) }
	defer unblock()
	oldDone := make(chan error, 1)
	go func() {
		oldDone <- s.indexMaintenance(ctx, dbgen.Outbox{EventType: "file.delete", AggregateID: pgUUID(fid), Payload: []byte(`{}`)})
	}()
	select {
	case <-index.entered:
	case <-ctx.Done():
		t.Fatal("old delete did not reach controlled pre-write barrier")
	}
	if _, err := s.DB.Exec(ctx, `UPDATE files SET lifecycle='active',name='restored.txt',path='/restored.txt',metadata_revision=metadata_revision+1 WHERE id=$1`, fid); err != nil {
		t.Fatal(err)
	}
	newDone := make(chan error, 1)
	go func() {
		newDone <- s.indexMaintenance(ctx, dbgen.Outbox{EventType: "file.restore", AggregateID: pgUUID(fid), Payload: []byte(`{}`)})
	}()
	var newErr error
	newFinished := false
	select {
	case newErr = <-newDone:
		newFinished = true
	case <-time.After(time.Second):
	case <-ctx.Done():
		t.Fatal("context expired before releasing old index delete")
	}
	unblock()
	if err := <-oldDone; err != nil {
		t.Fatal("old delete event failed", err)
	}
	if !newFinished {
		newErr = <-newDone
	}
	if errors.Is(newErr, ports.ErrConflict) {
		newErr = s.indexMaintenance(ctx, dbgen.Outbox{EventType: "file.restore", AggregateID: pgUUID(fid), Payload: []byte(`{}`)})
	}
	if newErr != nil {
		t.Fatal("restore event failed", newErr)
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	if len(index.docs) == 0 {
		t.Fatal("both events succeeded but a late delete erased restored search documents")
	}
	for _, doc := range index.docs {
		if doc.FileID != fid || doc.ArtifactSetID != aid || doc.SourcePath != "/restored.txt" {
			t.Fatal("restore rebuilt the wrong artifact or stale path")
		}
	}
}

func TestStressReviewIndexRebuildPersistsDetectedAccountDisable(t *testing.T) {
	s, actor, job, _, _ := workerFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	aid := reviewRetainedArtifact(t, s, uuidText(job.SourceVersionID), 1)
	var fid string
	if err := s.DB.QueryRow(ctx, `UPDATE files SET search_artifact_set_id=$2 WHERE current_version_id=$1 RETURNING id::text`, job.SourceVersionID, aid).Scan(&fid); err != nil {
		t.Fatal(err)
	}
	s.Identity = syncIdentity{enabled: false}
	s.Search = &reviewDelayedDeleteIndex{docs: map[string]ports.SearchDocument{}, entered: make(chan struct{}), release: make(chan struct{})}
	if err := s.indexMaintenance(ctx, dbgen.Outbox{EventType: "file.restore", AggregateID: pgUUID(fid), Payload: []byte(`{}`)}); err == nil {
		t.Fatal("disabled owner was allowed to rebuild index")
	}
	var status, reason string
	if err := s.DB.QueryRow(ctx, `SELECT status,COALESCE(disabled_reason,'') FROM principals WHERE id=$1`, actor.Principal.ID).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "disabled" || reason != "identity" {
		t.Fatalf("authoritative disabled result was rolled back with the index transaction: status=%s reason=%s", status, reason)
	}
}

type reviewSlowPathIndex struct {
	ports.SearchIndex
	entered chan struct{}
}

func (i *reviewSlowPathIndex) UpdatePaths(ctx context.Context, _ string, _ map[string]string) (ports.SearchTask, error) {
	if i.entered != nil {
		close(i.entered)
	}
	select {
	case <-time.After(4 * time.Second):
		return ports.SearchTask{UID: 1}, nil
	case <-ctx.Done():
		return ports.SearchTask{}, ctx.Err()
	}
}

func (*reviewSlowPathIndex) Wait(context.Context, ports.SearchTask) error { return nil }

func TestStressReviewOutboxWorkLongerThanLeaseCanFinish(t *testing.T) {
	s, actor, _ := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.Config.Lease = 3 * time.Second
	s.Search = &reviewSlowPathIndex{}
	cfg := s.DB.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s.DB = pool
	fid := id()
	if _, err := s.DB.Exec(ctx, `INSERT INTO files(id,owner_id,space_id,name,path,kind,native_instance,native_resource_id) SELECT $1,$2,id,'slow','/slow','directory',native_instance,'review-slow-event' FROM spaces WHERE owner_id=$2`, fid, actor.Principal.ID); err != nil {
		t.Fatal(err)
	}
	q := dbgen.New(s.DB)
	if _, err := q.EnqueueOutbox(ctx, dbgen.EnqueueOutboxParams{ID: pgUUID(id()), EventType: "file.move", AggregateID: pgUUID(fid), Payload: []byte(`{}`), DeduplicationKey: "review-slow-outbox"}); err != nil {
		t.Fatal(err)
	}
	rows, err := q.ClaimOutbox(ctx, dbgen.ClaimOutboxParams{WorkerID: "review-long-work", LeaseSeconds: s.leaseSeconds(), BatchSize: 1})
	if err != nil || len(rows) != 1 {
		t.Fatal("could not claim synthetic event", len(rows), err)
	}
	if err := s.publishOutbox(ctx, maintenancePublisher{}, rows[0]); err != nil {
		t.Fatalf("legitimate index work exceeding its lease can never be acknowledged: %v", err)
	}
	var state string
	if err := s.DB.QueryRow(ctx, `SELECT state FROM outbox WHERE id=$1`, rows[0].ID).Scan(&state); err != nil || state != "published" {
		t.Fatal("completed long event remains queued", state, err)
	}
}

func TestStressReviewStolenOutboxFenceCancelsInFlightIndex(t *testing.T) {
	s, actor, _ := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.Config.Lease = 3 * time.Second
	index := &reviewSlowPathIndex{entered: make(chan struct{})}
	s.Search = index
	fid := id()
	if _, err := s.DB.Exec(ctx, `INSERT INTO files(id,owner_id,space_id,name,path,kind,native_instance,native_resource_id) SELECT $1,$2,id,'fenced','/fenced','directory',native_instance,'review-fenced-event' FROM spaces WHERE owner_id=$2`, fid, actor.Principal.ID); err != nil {
		t.Fatal(err)
	}
	q := dbgen.New(s.DB)
	if _, err := q.EnqueueOutbox(ctx, dbgen.EnqueueOutboxParams{ID: pgUUID(id()), EventType: "file.move", AggregateID: pgUUID(fid), Payload: []byte(`{}`), DeduplicationKey: "review-fenced-outbox"}); err != nil {
		t.Fatal(err)
	}
	rows, err := q.ClaimOutbox(ctx, dbgen.ClaimOutboxParams{WorkerID: "review-original", LeaseSeconds: s.leaseSeconds(), BatchSize: 1})
	if err != nil || len(rows) != 1 {
		t.Fatal("could not claim event", len(rows), err)
	}
	var publishMu sync.Mutex
	publishes := 0
	stream := maintenancePublisher{publish: func() error {
		publishMu.Lock()
		publishes++
		publishMu.Unlock()
		return nil
	}}
	done := make(chan error, 1)
	go func() { done <- s.publishOutbox(ctx, stream, rows[0]) }()
	select {
	case <-index.entered:
	case <-ctx.Done():
		t.Fatal("index never reached in-flight barrier")
	}
	if _, err := s.DB.Exec(ctx, `UPDATE outbox SET lease_until=now()-interval '1 second' WHERE id=$1`, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := q.ClaimOutbox(ctx, dbgen.ClaimOutboxParams{WorkerID: "review-new-owner", LeaseSeconds: 30, BatchSize: 1})
	if err != nil || len(reclaimed) != 1 || reclaimed[0].LeaseToken != rows[0].LeaseToken+1 {
		t.Fatal("controlled lease takeover failed", len(reclaimed), err)
	}
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("lost lease was acknowledged as success")
		}
	case <-time.After(2500 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("stale outbox work was not canceled after losing its fence")
	}
	publishMu.Lock()
	count := publishes
	publishMu.Unlock()
	if count != 0 {
		t.Fatal("lost fence still dispatched downstream publication", count)
	}
	var state string
	var token int64
	if err := s.DB.QueryRow(ctx, `SELECT state,lease_token FROM outbox WHERE id=$1`, rows[0].ID).Scan(&state, &token); err != nil || state != "publishing" || token != reclaimed[0].LeaseToken {
		t.Fatal("stale task altered replacement owner fence", state, token, err)
	}
}

type reviewIndexHTTP struct {
	mu          sync.Mutex
	docs        map[string]ports.SearchDocument
	requests    int
	maxBatch    int
	failRequest int
}

func (i *reviewIndexHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	i.mu.Lock()
	defer i.mu.Unlock()
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/tasks/") {
		_, _ = w.Write([]byte(`{"status":"succeeded"}`))
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/indexes/review/documents" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var docs []ports.SearchDocument
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&docs); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	i.requests++
	if len(docs) > i.maxBatch {
		i.maxBatch = len(docs)
	}
	if i.requests == i.failRequest {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	for _, d := range docs {
		i.docs[d.ID] = d
	}
	w.WriteHeader(http.StatusAccepted)
	_, _ = fmt.Fprintf(w, `{"taskUid":%d}`, i.requests)
}

func TestStressReviewLargeArtifactIndexesAndRebuildsInBoundedBatches(t *testing.T) {
	for _, failSecond := range []bool{false, true} {
		t.Run(fmt.Sprint("second_batch_failure=", failSecond), func(t *testing.T) {
			s, _, initial, original, _ := workerFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			s.Config.ConvertMax = 8 << 20
			s.Config.Lease = 30 * time.Second
			original.data = bytes.Repeat([]byte("a"), 3072*1001)
			vid := pgUUID(id())
			original.ref.RevisionID = "large-immutable-revision"
			original.ref.ETag = "large-immutable-etag"
			if _, err := s.DB.Exec(ctx, `INSERT INTO file_versions(id,file_id,version_seq,size,sha256,content_type,native_etag,native_revision_ref) SELECT $2,file_id,2,$3,$4,'text/plain',$5,$6 FROM file_versions WHERE id=$1`, initial.SourceVersionID, vid, len(original.data), contentHash(original.data), original.ref.ETag, js(original.ref)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB.Exec(ctx, `UPDATE files SET size=$2,current_version_id=$3 WHERE current_version_id=$1`, initial.SourceVersionID, len(original.data), vid); err != nil {
				t.Fatal(err)
			}
			protocol := &reviewIndexHTTP{docs: map[string]ports.SearchDocument{}}
			if failSecond {
				protocol.failRequest = 2
			}
			server := httptest.NewServer(protocol)
			defer server.Close()
			index, err := searchadapter.New(searchadapter.Config{URL: server.URL, Index: "review"})
			if err != nil {
				t.Fatal(err)
			}
			s.Search = index
			q := dbgen.New(s.DB)
			recipe := s.Recipe()
			job, err := q.RequestConversion(ctx, dbgen.RequestConversionParams{SourceVersionID: vid, RecipeFingerprint: recipe.Fingerprint, RecipeSnapshot: js(recipe)})
			if err != nil {
				t.Fatal(err)
			}
			job, err = q.ClaimConversionJob(ctx, dbgen.ClaimConversionJobParams{JobID: job.ID, WorkerID: s.Config.NodeID, LeaseSeconds: s.leaseSeconds()})
			if err != nil {
				t.Fatal(err)
			}
			err = s.convertAndPublish(ctx, job)
			if failSecond {
				if !errors.Is(err, ports.ErrUnavailable) {
					t.Fatalf("expected second HTTP batch failure, got %v", err)
				}
				var visible bool
				if err := s.DB.QueryRow(ctx, `SELECT search_artifact_set_id IS NOT NULL FROM files WHERE current_version_id=$1`, job.SourceVersionID).Scan(&visible); err != nil || visible {
					t.Fatal("partial batch became searchable", visible, err)
				}
				err = s.convertAndPublish(ctx, job) // Resume immutable output, do not reconvert.
			}
			if err != nil {
				t.Fatalf("valid 3.1 MiB text cannot finish indexing: %v", err)
			}
			var aid string
			if err := s.DB.QueryRow(ctx, `SELECT search_artifact_set_id::text FROM files WHERE current_version_id=$1`, job.SourceVersionID).Scan(&aid); err != nil || aid == "" {
				t.Fatal("all batches completed without publishing artifact", aid, err)
			}
			protocol.mu.Lock()
			count, maxBatch := len(protocol.docs), protocol.maxBatch
			protocol.docs = map[string]ports.SearchDocument{} // Simulate removed rebuildable index.
			protocol.mu.Unlock()
			if count != 1001 || maxBatch > 1000 {
				t.Fatal("missing index chunks or unbounded batch", count, maxBatch)
			}
			if err := s.rebuildSearchArtifact(ctx, aid); err != nil {
				t.Fatalf("restoration cannot rebuild large retained artifact: %v", err)
			}
			protocol.mu.Lock()
			count = len(protocol.docs)
			protocol.mu.Unlock()
			if count != 1001 || original.reads != 1 {
				t.Fatal("rebuild lost chunks or reconverted immutable source", count, original.reads)
			}
		})
	}
}
