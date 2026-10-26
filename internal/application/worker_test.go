package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/adapters/artifactstore"
	searchadapter "github.com/agentclouddrive/agent-clouddrive/internal/adapters/search"
	"github.com/agentclouddrive/agent-clouddrive/internal/dbgen"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/nats-io/nats.go"
)

func TestDirectTextTruthfulness(t *testing.T) {
	r, e := directText([]byte("# Immutable original\n\nHello 世界"))
	if e != nil || r.Status != "ready" || r.Completeness.TotalPages != 1 || len(r.Completeness.MissingTextPages) != 0 {
		t.Fatal(r, e)
	}
	for _, bad := range [][]byte{[]byte("\x00binary"), {0xff, 0xfe}, []byte(" \n")} {
		if _, e = directText(bad); e == nil {
			t.Fatal("invalid or empty document accepted")
		}
	}
	if !isPlainText("a.md", "application/octet-stream") || isPlainText("a.pdf", "application/pdf") {
		t.Fatal("text dispatch")
	}
}
func TestPageProvenanceIsNotInvented(t *testing.T) {
	m := conversionManifest{Pages: []ports.ConversionPage{{Page: 2, Block: "p2-b01"}, {Page: 3, Block: "p3-b01"}}, Completeness: ports.Completeness{TotalPages: 3}, Assets: []manifestAsset{{ID: "image2", Page: 2}}}
	blocks := conversionBlocks("Preface\n<!-- page:2 block:p2-b01 -->\nPage two\n<!-- page:3 block:p3-b01 -->\nPage three", m)
	if len(blocks) != 3 || blocks[0].Page != 0 || blocks[1].Page != 2 || blocks[1].BlockID != "p2-b01" || len(blocks[1].AssetIDs) != 1 || blocks[2].Page != 3 {
		t.Fatal(blocks)
	}
	unmarked := conversionBlocks("No page mapping available", m)
	if unmarked[0].Page != 0 {
		t.Fatal("invented page mapping", unmarked)
	}
}
func TestConversionFailureClassification(t *testing.T) {
	state, code, retry := classifyConversionError(ports.ErrLimit)
	if state != "not_converted" || code != "limit_exceeded" || retry {
		t.Fatal(state, code, retry)
	}
	state, code, retry = classifyConversionError(ports.ErrUnverified)
	if state != "blocked" || code != "delegation_unavailable" || retry {
		t.Fatal(state, code, retry)
	}
	_, _, retry = classifyConversionError(ports.ErrUnavailable)
	if !retry {
		t.Fatal("transient dependency error should retry")
	}
}

type workerOriginal struct {
	*mutationStore
	mu    sync.Mutex
	ref   ports.RevisionRef
	data  []byte
	reads int
}

func (w *workerOriginal) OpenRevision(_ context.Context, ref ports.RevisionRef, _ *ports.ByteRange, _ ports.Credential) (ports.Content, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ref != w.ref {
		return ports.Content{}, ports.ErrIntegrity
	}
	w.reads++
	return ports.Content{Body: io.NopCloser(bytes.NewReader(w.data)), Size: int64(len(w.data)), TotalSize: int64(len(w.data)), ContentType: "text/plain"}, nil
}

type workerIdentity struct {
	mutationIdentity
	mu       sync.Mutex
	requests []ports.DelegationRequest
}

func (w *workerIdentity) DelegatedCredential(_ context.Context, in ports.DelegationRequest) (ports.Credential, error) {
	if in.SubjectToken != "" || in.JobID == "" || in.Source.RevisionID == "" {
		return ports.Credential{}, ports.ErrUnauthorized
	}
	w.mu.Lock()
	w.requests = append(w.requests, in)
	w.mu.Unlock()
	return ports.Credential{AccessToken: "fixture-delegated-token", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// This fixture deliberately stubs only identity and oCIS. Tests named Integration
// use real PostgreSQL, local immutable artifact storage, NATS and Meilisearch.
// It is not evidence that an external oCIS/OIDC/Peri deployment passed M0.
func workerFixture(t *testing.T) (*Service, Actor, dbgen.ConversionJob, *workerOriginal, *workerIdentity) {
	t.Helper()
	s, actor, original := mutationFixture(t)
	ctx := context.Background()
	s.Config.ConvertMax = 1 << 20
	s.Config.ConvertPages = 500
	s.Config.Lease = 6 * time.Second
	s.Config.NodeID = "worker-test-" + id()
	s.Config.NATSURL = os.Getenv("DRIVE_TEST_NATS_URL")
	s.Config.NATSStream = "DRIVE_TEST_" + strings.ReplaceAll(id(), "-", "")
	store, e := artifactstore.NewLocal(t.TempDir(), 64<<20)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = store.Close() })
	s.Artifacts = store
	fid, vid := id(), id()
	data := []byte("# Stable source\n\nThe unique keyword aurorafencing proves pinned original conversion.\n")
	ref := ports.RevisionRef{ResourceRef: ports.ResourceRef{Instance: "http://ocis.test", SpaceID: "native-space", ResourceID: "immutable-native-file"}, RevisionID: "native-revision-one", ETag: "etag-one"}
	_, e = s.DB.Exec(ctx, `INSERT INTO files(id,owner_id,space_id,name,path,kind,native_instance,native_resource_id,content_type) SELECT $1,$2,id,'original.txt','/original.txt','file',native_instance,'immutable-native-file','text/plain' FROM spaces WHERE owner_id=$2`, fid, actor.Principal.ID)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DB.Exec(ctx, `INSERT INTO file_versions(id,file_id,version_seq,size,sha256,content_type,native_etag,native_revision_ref) VALUES($1,$2,1,$3,$4,'text/plain','etag-one',$5)`, vid, fid, len(data), contentHash(data), js(ref))
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DB.Exec(ctx, `UPDATE files SET current_version_id=$2,size=$3 WHERE id=$1`, fid, vid, len(data))
	if e != nil {
		t.Fatal(e)
	}
	recipe := s.Recipe()
	job, e := dbgen.New(s.DB).RequestConversion(ctx, dbgen.RequestConversionParams{SourceVersionID: pgUUID(vid), RecipeFingerprint: recipe.Fingerprint, RecipeSnapshot: js(recipe)})
	if e != nil {
		t.Fatal(e)
	}
	w := &workerOriginal{mutationStore: original, ref: ref, data: data}
	identity := &workerIdentity{}
	s.Original = w
	s.Identity = identity
	return s, actor, job, w, identity
}
func TestWorkerLimitPreservesOriginal(t *testing.T) {
	s, _, job, original, _ := workerFixture(t)
	s.Config.ConvertMax = 1
	recipe := s.Recipe()
	q := dbgen.New(s.DB)
	ctx := context.Background()
	limited, e := q.RequestConversion(ctx, dbgen.RequestConversionParams{SourceVersionID: job.SourceVersionID, RecipeFingerprint: recipe.Fingerprint, RecipeSnapshot: js(recipe)})
	if e != nil {
		t.Fatal(e)
	}
	claimed, e := q.ClaimConversionJob(ctx, dbgen.ClaimConversionJobParams{JobID: limited.ID, WorkerID: s.Config.NodeID, LeaseSeconds: s.leaseSeconds()})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.processConversion(ctx, claimed, nil); e != nil {
		t.Fatal(e)
	}
	done, e := q.GetConversionJob(ctx, limited.ID)
	if e != nil {
		t.Fatal(e)
	}
	if done.State != "not_converted" || done.ErrorCode == nil || *done.ErrorCode != "limit_exceeded" {
		t.Fatal(done)
	}
	original.mu.Lock()
	reads := original.reads
	original.mu.Unlock()
	if reads != 0 {
		t.Fatal("over-limit original was loaded", reads)
	}
	var count int
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM file_versions WHERE id=$1`, job.SourceVersionID).Scan(&count); e != nil || count != 1 {
		t.Fatal("original version changed", count, e)
	}
}
func TestWorkerPostgresNATSMeiliIntegration(t *testing.T) {
	if os.Getenv("DRIVE_TEST_NATS_URL") == "" || os.Getenv("DRIVE_TEST_MEILI_URL") == "" {
		t.Skip("set DRIVE_TEST_NATS_URL and DRIVE_TEST_MEILI_URL for real integration")
	}
	s, actor, job, original, identity := workerFixture(t)
	index, e := searchadapter.New(searchadapter.Config{URL: os.Getenv("DRIVE_TEST_MEILI_URL"), APIKey: os.Getenv("DRIVE_TEST_MEILI_KEY"), Index: "worker_test_" + strings.ReplaceAll(id(), "-", ""), PollInterval: 20 * time.Millisecond, TaskTimeout: 10 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	s.Search = index
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	workerDone, maintenanceDone := make(chan error, 1), make(chan error, 1)
	workerStopped := make(chan struct{})
	go func() { defer close(workerStopped); workerDone <- s.RunWorker(ctx) }()
	go func() { maintenanceDone <- s.RunMaintenance(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-workerDone:
		case <-time.After(5 * time.Second):
			t.Error("worker did not stop")
		}
		select {
		case <-maintenanceDone:
		case <-time.After(5 * time.Second):
			t.Error("maintenance did not stop")
		}
	})
	q := dbgen.New(s.DB)
	var completed dbgen.ConversionJob
	for ctx.Err() == nil {
		completed, e = q.GetConversionJob(ctx, job.ID)
		if e != nil {
			t.Fatal(e)
		}
		if completed.State == "ready" {
			break
		}
		if completed.State == "failed" || completed.State == "blocked" {
			t.Fatalf("job failed: %s / %v", completed.State, completed.ErrorCode)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if completed.State != "ready" {
		t.Fatal("conversion did not publish", completed.State, ctx.Err())
	}
	hits, e := s.Search.Query(ctx, ports.SearchQuery{OwnerID: actor.Principal.ID, Query: "aurorafencing", Limit: 20})
	if e != nil || len(hits.Hits) != 1 {
		t.Fatal("real Meilisearch result", hits, e)
	}
	hit := hits.Hits[0]
	if hit.VersionID != uuidText(job.SourceVersionID) || hit.ArtifactSetID != uuidText(completed.ArtifactSetID) || hit.Page != 1 {
		t.Fatal("provenance mismatch", hit)
	}
	a, e := q.GetArtifactSet(ctx, completed.ArtifactSetID)
	if e != nil || a.IndexState != "succeeded" || !a.PublishedAt.Valid {
		t.Fatal("published before index completion", a, e)
	}
	c, e := s.Artifacts.Open(ctx, a.MarkdownKey, nil)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := io.ReadAll(c.Body)
	_ = c.Body.Close()
	if e != nil || !bytes.Equal(raw, original.data) {
		t.Fatal("stored markdown differs", e)
	}
	identity.mu.Lock()
	requests := append([]ports.DelegationRequest(nil), identity.requests...)
	identity.mu.Unlock()
	if len(requests) != 1 || requests[0].JobID != uuidText(job.ID) || requests[0].Source != original.ref || requests[0].SubjectToken != "" {
		t.Fatal("background delegated fixed-source contract", len(requests))
	}
	// A durable publication event repairs path labels when conversion races a rename.
	if _, e = s.DB.Exec(ctx, `UPDATE files SET path='/renamed.txt',name='renamed.txt' WHERE id=$1`, hit.FileID); e != nil {
		t.Fatal(e)
	}
	if e = s.indexMaintenance(ctx, dbgen.Outbox{EventType: "search.published", AggregateID: pgUUID(hit.FileID), Payload: []byte(`{}`)}); e != nil {
		t.Fatal(e)
	}
	refreshed, e := s.Search.Query(ctx, ports.SearchQuery{OwnerID: actor.Principal.ID, Query: "aurorafencing", Limit: 20})
	if e != nil || len(refreshed.Hits) != 1 || refreshed.Hits[0].SourcePath != "/renamed.txt" {
		t.Fatal("publication retained stale path", refreshed, e)
	}
	// Deleting removes search blocks; restoring uses retained Markdown, not OCR.
	if _, e = s.DB.Exec(ctx, `UPDATE files SET lifecycle='trashed' WHERE id=$1`, hit.FileID); e != nil {
		t.Fatal(e)
	}
	event := dbgen.Outbox{EventType: "file.delete", AggregateID: pgUUID(hit.FileID), Payload: []byte(`{}`)}
	if e = s.indexMaintenance(ctx, event); e != nil {
		t.Fatal(e)
	}
	hits, e = s.Search.Query(ctx, ports.SearchQuery{OwnerID: actor.Principal.ID, Query: "aurorafencing", Limit: 20})
	if e != nil || len(hits.Hits) != 0 {
		t.Fatal("deleted chunks remain", e)
	}
	if _, e = s.DB.Exec(ctx, `UPDATE files SET lifecycle='active' WHERE id=$1`, hit.FileID); e != nil {
		t.Fatal(e)
	}
	event.EventType = "file.restore"
	if e = s.indexMaintenance(ctx, event); e != nil {
		t.Fatal(e)
	}
	event.EventType = "file.delete"
	if e = s.indexMaintenance(ctx, event); e != nil {
		t.Fatal("late delete event", e)
	}
	hits, e = s.Search.Query(ctx, ports.SearchQuery{OwnerID: actor.Principal.ID, Query: "aurorafencing", Limit: 20})
	if e != nil || len(hits.Hits) != 1 {
		t.Fatal("restore did not reindex retained content", hits, e)
	}
	original.mu.Lock()
	reads := original.reads
	original.mu.Unlock()
	if reads != 1 {
		t.Fatal("restore reconverted original", reads)
	}
	nc, e := nats.Connect(s.Config.NATSURL)
	if e != nil {
		t.Fatal(e)
	}
	defer nc.Close()
	stream, e := nc.JetStream()
	if e != nil {
		t.Fatal(e)
	}
	info, e := stream.StreamInfo(s.Config.NATSStream)
	if e != nil || info.State.Msgs < 1 {
		t.Fatal("no real durable outbox publication", e)
	}
	// A real durable outbox row must have been acknowledged after server PubAck.
	var published int
	for ctx.Err() == nil {
		e = s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE event_type='conversion.requested' AND state='published'`).Scan(&published)
		if e != nil {
			t.Fatal(e)
		}
		if published == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if published != 1 {
		t.Fatal("outbox was not durably acknowledged")
	}
	cancel()
	select {
	case <-workerStopped:
	case <-time.After(3 * time.Second):
		t.Fatal("worker shutdown timed out")
	}
	if _, e = stream.ConsumerInfo(s.Config.NATSStream, "clouddrive-converter"); e != nil {
		t.Fatal("shutdown deleted durable consumer", e)
	}
}

func TestManifestNeverContainsDelegation(t *testing.T) {
	m := conversionManifest{SchemaVersion: 1, RecipeFingerprint: "fingerprint", Assets: []manifestAsset{{ID: "img1", Key: "images/set/img1.png"}}}
	b, e := json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	for _, secret := range []string{"access_token", "subject_token", "signed_url"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Fatal("credential metadata in manifest")
		}
	}
	_, _, retry := classifyConversionError(errors.New("temporary"))
	if !retry {
		t.Fatal("unexpected retry classification")
	}
}

type pathBatchSearch struct {
	ports.SearchIndex
	mu         sync.Mutex
	owners     []string
	batches    []map[string]string
	waits      []int64
	failUpdate int
	failWait   int
}

func (s *pathBatchSearch) UpdatePaths(_ context.Context, owner string, paths map[string]string) (ports.SearchTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner == "" || len(paths) == 0 || len(paths) > 100 {
		return ports.SearchTask{}, ports.ErrInvalid
	}
	copyPaths := make(map[string]string, len(paths))
	for fileID, sourcePath := range paths {
		copyPaths[fileID] = sourcePath
	}
	s.owners = append(s.owners, owner)
	s.batches = append(s.batches, copyPaths)
	if len(s.batches) == s.failUpdate {
		return ports.SearchTask{}, ports.ErrUnavailable
	}
	return ports.SearchTask{UID: int64(len(s.batches))}, nil
}

func (s *pathBatchSearch) Wait(_ context.Context, task ports.SearchTask) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waits = append(s.waits, task.UID)
	if len(s.waits) == s.failWait {
		return ports.ErrUnavailable
	}
	return nil
}

func TestMaintenancePathUpdatesRespectBatchLimit(t *testing.T) {
	for _, count := range []int{0, 1, 100, 101, 201} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			search := &pathBatchSearch{}
			s := &Service{Search: search}
			paths := map[string]string{}
			for i := 0; i < count; i++ {
				paths[fmt.Sprint(i)] = fmt.Sprintf("/moved/%d.txt", i)
			}
			if e := s.updateSearchPaths(context.Background(), "owner", paths); e != nil {
				t.Fatal(e)
			}
			if len(search.batches) != (count+99)/100 || len(search.waits) != len(search.batches) {
				t.Fatal("batch/task count", len(search.batches), search.waits)
			}
			seen := map[string]bool{}
			for i, batch := range search.batches {
				if search.owners[i] != "owner" || search.waits[i] != int64(i+1) {
					t.Fatal("lost owner scope or task completion", search.owners, search.waits)
				}
				for fileID, sourcePath := range batch {
					if seen[fileID] || paths[fileID] != sourcePath {
						t.Fatal("duplicate or altered path", fileID, sourcePath)
					}
					seen[fileID] = true
				}
			}
			if len(seen) != count {
				t.Fatal("missing descendants", len(seen), count)
			}
		})
	}
}

func TestMaintenancePathBatchFailureIsNotAcknowledged(t *testing.T) {
	for _, failWait := range []bool{false, true} {
		t.Run(fmt.Sprint("wait=", failWait), func(t *testing.T) {
			search := &pathBatchSearch{}
			if failWait {
				search.failWait = 2
			} else {
				search.failUpdate = 2
			}
			s := &Service{Search: search}
			paths := map[string]string{}
			for i := 0; i < 201; i++ {
				paths[fmt.Sprint(i)] = fmt.Sprintf("/moved/%d.txt", i)
			}
			if e := s.updateSearchPaths(context.Background(), "owner", paths); !errors.Is(e, ports.ErrUnavailable) {
				t.Fatal("failed chunk was acknowledged", e)
			}
			wantWaits := 1
			if failWait {
				wantWaits = 2
			}
			if len(search.batches) != 2 || len(search.waits) != wantWaits {
				t.Fatal("continued after failed chunk", len(search.batches), len(search.waits))
			}
		})
	}
}

type stalledMaintenanceIdentity struct {
	mutationIdentity
	started chan time.Time
	stopped chan struct{}
}

func (i *stalledMaintenanceIdentity) SubjectStatus(ctx context.Context, _, _ string) (ports.SubjectStatus, error) {
	deadline, _ := ctx.Deadline()
	i.started <- deadline
	<-ctx.Done()
	close(i.stopped)
	return ports.SubjectStatus{}, ctx.Err()
}

type maintenancePublisher struct {
	nats.JetStreamContext
	publish func() error
}

func (p maintenancePublisher) Publish(_ string, _ []byte, _ ...nats.PubOpt) (*nats.PubAck, error) {
	if p.publish != nil {
		if e := p.publish(); e != nil {
			return nil, e
		}
	}
	return &nats.PubAck{}, nil
}

// Real PostgreSQL scheduling and ancestry, with explicit identity/search/NATS
// contract fakes. This does not claim a native provider or broker integration.
func TestMaintenanceDrainsBacklogWhileIdentityIsUnavailable(t *testing.T) {
	s, actor, _ := mutationFixture(t)
	s.Config.NodeID = "maintenance-backlog-test"
	s.Config.Lease = 6 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root := id()
	if _, e := s.DB.Exec(ctx, `INSERT INTO files(id,owner_id,space_id,name,path,kind,native_instance,native_resource_id) SELECT $1,$2,id,'moved','/moved','directory',native_instance,'moved-root' FROM spaces WHERE owner_id=$2`, root, actor.Principal.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.Exec(ctx, `INSERT INTO files(owner_id,space_id,parent_id,name,path,kind,native_instance,native_resource_id) SELECT $1,s.id,$2,'child-'||n||'.txt','/moved/child-'||n||'.txt','file',s.native_instance,'child-'||n FROM spaces s CROSS JOIN generate_series(1,101) n WHERE s.owner_id=$1`, actor.Principal.ID, root); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.Exec(ctx, `INSERT INTO outbox(event_type,aggregate_id,payload,deduplication_key,created_at) SELECT 'file.mkdir',$1,'{}','backlog-'||n,now()-interval '1 second' FROM generate_series(1,100) n`, root); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.Exec(ctx, `INSERT INTO outbox(event_type,aggregate_id,payload,deduplication_key) VALUES('file.move',$1,'{}','subtree-move')`, root); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.Exec(ctx, `UPDATE principals SET updated_at=now()-interval '2 minutes' WHERE id=$1`, actor.Principal.ID); e != nil {
		t.Fatal(e)
	}
	identity := &stalledMaintenanceIdentity{started: make(chan time.Time, 1), stopped: make(chan struct{})}
	search := &pathBatchSearch{}
	s.Identity, s.Search = identity, search
	done := make(chan struct{})
	go func() { defer close(done); s.runMaintenance(ctx, maintenancePublisher{}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("maintenance did not join canceled reconciliation")
		}
	})
	select {
	case deadline := <-identity.started:
		if deadline.IsZero() || time.Until(deadline) > 5*time.Second {
			t.Fatal("unbounded identity pass", deadline)
		}
	case <-ctx.Done():
		t.Fatal("identity lane was starved")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var published int
		if e := s.DB.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE state='published'`).Scan(&published); e != nil {
			t.Fatal(e)
		}
		if published == 101 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("identity outage or per-row delay starved committed outbox", published)
		}
		time.Sleep(10 * time.Millisecond)
	}
	search.mu.Lock()
	defer search.mu.Unlock()
	if len(search.batches) != 2 || len(search.waits) != 2 {
		t.Fatal("large subtree did not converge in bounded batches", len(search.batches), search.waits)
	}
	seen := map[string]bool{}
	for i, batch := range search.batches {
		if search.owners[i] != actor.Principal.ID {
			t.Fatal("path update escaped owner scope", search.owners)
		}
		for fileID, sourcePath := range batch {
			if seen[fileID] || (sourcePath != "/moved" && !strings.HasPrefix(sourcePath, "/moved/child-")) {
				t.Fatal("incorrect current subtree path", fileID, sourcePath)
			}
			seen[fileID] = true
		}
	}
	if len(seen) != 102 {
		t.Fatal("missing subtree paths", len(seen))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("maintenance shutdown timed out")
	}
	select {
	case <-identity.stopped:
	default:
		t.Fatal("identity goroutine outlived maintenance")
	}
}

func TestMaintenanceIdentitySyncProgressesWhileOutboxIsStalled(t *testing.T) {
	s, actor, _ := mutationFixture(t)
	s.Config.NodeID = "maintenance-revocation-test"
	s.Identity = syncIdentity{enabled: false}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, e := s.DB.Exec(ctx, `INSERT INTO outbox(event_type,aggregate_id,payload,deduplication_key) VALUES('file.mkdir',$1,'{}','blocked-publish')`, id()); e != nil {
		t.Fatal(e)
	}
	started := make(chan struct{})
	stream := maintenancePublisher{publish: func() error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	done := make(chan struct{})
	go func() { defer close(done); s.runMaintenance(ctx, stream) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("maintenance shutdown timed out")
		}
	})
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("outbox did not start")
	}
	if _, e := s.DB.Exec(ctx, `UPDATE principals SET updated_at=now()-interval '2 minutes' WHERE id=$1`, actor.Principal.ID); e != nil {
		t.Fatal(e)
	}
	for ctx.Err() == nil {
		var status string
		if e := s.DB.QueryRow(ctx, `SELECT status FROM principals WHERE id=$1`, actor.Principal.ID).Scan(&status); e != nil {
			t.Fatal(e)
		}
		if status == "disabled" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("stalled outbox suppressed account revocation")
}

type slowFirstMaintenanceIdentity struct {
	mutationIdentity
	seen []string
}

func (i *slowFirstMaintenanceIdentity) SubjectStatus(ctx context.Context, _, subject string) (ports.SubjectStatus, error) {
	i.seen = append(i.seen, subject)
	if subject == "slow" {
		<-ctx.Done()
		return ports.SubjectStatus{}, ctx.Err()
	}
	return ports.SubjectStatus{Enabled: false}, nil
}

func TestMaintenanceSlowPrincipalDoesNotStarveLaterDisable(t *testing.T) {
	s, actor, _ := mutationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, e := s.DB.Exec(ctx, `UPDATE principals SET subject='later-disabled',updated_at=now()-interval '2 minutes' WHERE id=$1`, actor.Principal.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.Exec(ctx, `INSERT INTO principals(issuer,subject,status,disabled_reason,updated_at) VALUES('test','slow','disabled','admin',now()-interval '3 minutes')`); e != nil {
		t.Fatal(e)
	}
	identity := &slowFirstMaintenanceIdentity{}
	s.Identity = identity
	if e := s.syncPrincipalStatuses(ctx); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("expected per-principal provider timeout", e)
	}
	if ctx.Err() != nil || len(identity.seen) != 2 || identity.seen[0] != "slow" || identity.seen[1] != "later-disabled" {
		t.Fatal("one slow principal consumed later check's deadline", identity.seen, ctx.Err())
	}
	var status, reason string
	if e := s.DB.QueryRow(ctx, `SELECT status,disabled_reason FROM principals WHERE id=$1`, actor.Principal.ID).Scan(&status, &reason); e != nil || status != "disabled" || reason != "identity" {
		t.Fatal("later disabled principal was not reconciled", status, reason, e)
	}
	if e := s.DB.QueryRow(ctx, `SELECT status,disabled_reason FROM principals WHERE subject='slow'`).Scan(&status, &reason); e != nil || status != "disabled" || reason != "admin" {
		t.Fatal("failed provider read reactivated a local block", status, reason, e)
	}
}
