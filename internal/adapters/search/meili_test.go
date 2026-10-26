package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestMandatoryOwnerFilterAndNoTotals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		b, _ := json.Marshal(body["filter"])
		if !strings.Contains(string(b), `owner_id = \"owner\"`) {
			t.Errorf("missing filter %s", b)
		}
		fmt.Fprint(w, `{"hits":[],"estimatedTotalHits":999}`)
	}))
	defer srv.Close()
	m, _ := New(Config{URL: srv.URL, Index: "test"})
	v, e := m.Query(context.Background(), ports.SearchQuery{OwnerID: "owner", Limit: 10})
	if e != nil || len(v.Hits) != 0 || v.MayHaveMore {
		t.Fatalf("%+v %v", v, e)
	}
	_, e = m.Query(context.Background(), ports.SearchQuery{Limit: 10})
	if !errors.Is(e, ports.ErrInvalid) {
		t.Fatal(e)
	}
}
func TestWaitRequiresSucceeded(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n < 3 {
			fmt.Fprint(w, `{"status":"processing"}`)
		} else {
			fmt.Fprint(w, `{"status":"failed","error":{"code":"invalid_document_id"}}`)
		}
	}))
	defer srv.Close()
	m, _ := New(Config{URL: srv.URL, Index: "test", PollInterval: time.Millisecond})
	if e := m.Wait(context.Background(), ports.SearchTask{UID: 2}); e == nil || n != 3 {
		t.Fatalf("%v %d", e, n)
	}
}
func TestChunkUTF8AndStableProvenance(t *testing.T) {
	base := ports.SearchDocument{FileID: "f", VersionID: "v", ArtifactSetID: "a"}
	text := strings.Repeat("中文段落", 1000)
	v, e := Chunk(base, []TextBlock{{Text: text, Page: 12, BlockID: "p12-b1"}}, 2048)
	if e != nil || len(v) < 2 {
		t.Fatal(e)
	}
	var joined string
	for _, d := range v {
		if !utf8.ValidString(d.Content) || len(d.Content) > 2048 || d.Page != 12 || d.BlockID != "p12-b1" || !validIndex.MatchString(d.ID) {
			t.Fatalf("invalid chunk %+v", d)
		}
		joined += d.Content
	}
	if joined != text {
		t.Fatal("content lost")
	}
}

// TestMeiliIntegration is a real Meilisearch round-trip, opt-in and independent
// of the httptest contract tests. It creates and removes its own unique index.
func TestMeiliIntegration(t *testing.T) {
	endpoint := os.Getenv("DRIVE_TEST_MEILI_URL")
	if endpoint == "" {
		t.Skip("set DRIVE_TEST_MEILI_URL for a real Meilisearch integration")
	}
	m, e := New(Config{URL: endpoint, Index: fmt.Sprintf("adapter_test_%d", time.Now().UnixNano()), APIKey: os.Getenv("DRIVE_TEST_MEILI_KEY"), PollInterval: 10 * time.Millisecond})
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	defer func() {
		task, e := m.task(ctx, "DELETE", "/indexes/"+m.cfg.Index, nil)
		if e == nil {
			_ = m.Wait(ctx, task)
		}
	}()
	if e = m.Configure(ctx); e != nil {
		t.Fatal(e)
	}
	docs := []ports.SearchDocument{{ID: "a", OwnerID: "alice", FileID: "f", VersionID: "v", ArtifactSetID: "art", Content: "private moon telescope", SourcePath: "/old"}, {ID: "b", OwnerID: "bob", FileID: "g", VersionID: "w", ArtifactSetID: "bar", Content: "private moon telescope"}}
	task, e := m.Upsert(ctx, docs)
	if e != nil {
		t.Fatal(e)
	}
	if e = m.Wait(ctx, task); e != nil {
		t.Fatal(e)
	}
	got, e := m.Query(ctx, ports.SearchQuery{OwnerID: "alice", Query: "moon", Limit: 10})
	if e != nil || len(got.Hits) != 1 || got.Hits[0].ID != "a" {
		t.Fatalf("owner isolation %+v %v", got, e)
	}
	_, e = m.UpdatePaths(ctx, "alice", map[string]string{"f": "/new"})
	if e != nil {
		t.Fatal(e)
	}
	got, e = m.Query(ctx, ports.SearchQuery{OwnerID: "alice", Query: "moon", Limit: 10})
	if e != nil || got.Hits[0].SourcePath != "/new" || got.Hits[0].Content != docs[0].Content {
		t.Fatalf("partial update lost content %+v %v", got, e)
	}
	task, e = m.DeleteArtifactSet(ctx, "alice", "art")
	if e != nil {
		t.Fatal(e)
	}
	if e = m.Wait(ctx, task); e != nil {
		t.Fatal(e)
	}
	got, e = m.Query(ctx, ports.SearchQuery{OwnerID: "alice", Query: "moon", Limit: 10})
	if e != nil || len(got.Hits) != 0 {
		t.Fatal("deletion not visible", e)
	}
	t.Log("real Meilisearch: configure, async completion, owner filtering, partial path update, artifact-set deletion passed")
}
