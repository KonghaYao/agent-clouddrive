package application

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

// repairReadsArtifact publishes an immutable Markdown artifact for one source
// version and registers the published artifact_set row. The object store is the
// real local adapter created by workerFixture.
func repairReadsArtifact(t *testing.T, s *Service, vid string, generation int, body string) string {
	t.Helper()
	ctx := context.Background()
	aid := id()
	md, e := s.publishBytes(ctx, "tmp/repair-reads/"+aid+"/content.md", "markdown/"+aid+"/content.md", "text/markdown", []byte(body))
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.DB.Exec(ctx, `INSERT INTO artifact_sets(id,source_version_id,generation,recipe_fingerprint,recipe_snapshot,manifest_key,markdown_key,manifest_sha256,markdown_sha256,completeness,index_state,indexed_at,published_at) VALUES($1,$2,$3,'repair-reads','{}',$4,$5,$6,$6,'{"status":"ready","total_pages":1,"missing_text_pages":[],"missing_description_pages":[]}','succeeded',now(),now())`, aid, vid, generation, "manifests/"+aid+"/manifest.json", md.Key, md.SHA256)
	if e != nil {
		t.Fatal(e)
	}
	return aid
}

func repairSetPointer(t *testing.T, s *Service, fid, aid string) {
	t.Helper()
	if _, e := s.DB.Exec(context.Background(), `UPDATE files SET search_artifact_set_id=$2 WHERE id=$1`, fid, aid); e != nil {
		t.Fatal(e)
	}
}

func repairMarkdownPage(t *testing.T, s *Service, a Actor, in Object) Object {
	t.Helper()
	out, e := s.markdown(context.Background(), a, in)
	if e != nil {
		t.Fatalf("markdown %v: %v", in, e)
	}
	return out.(Object)
}

func repairReadAllMarkdown(t *testing.T, s *Service, a Actor, in Object) (string, []string) {
	t.Helper()
	var out strings.Builder
	artifacts := []string{}
	request := Object{}
	for k, v := range in {
		request[k] = v
	}
	for page := 0; ; page++ {
		result := repairMarkdownPage(t, s, a, request)
		out.WriteString(str(result, "markdown"))
		artifacts = append(artifacts, str(result, "artifact_set_id"))
		cursor := str(result, "next_cursor")
		if cursor == "" {
			break
		}
		if page > 10000 {
			t.Fatal("cursor loop did not terminate")
		}
		delete(request, "offset")
		delete(request, "version_id")
		delete(request, "artifact_set_id")
		request["cursor"] = cursor
	}
	return out.String(), artifacts
}

// B13: a continuation must resume the artifact generation that produced the
// first page, even after the current visible pointer or a newer generation for
// the same source version has been published.
func TestRepairReadsCursorPinsPublishedGeneration(t *testing.T) {
	for _, scope := range []string{"default-current", "explicit-source-version"} {
		t.Run(scope, func(t *testing.T) {
			s, actor, fid, old, newer := reviewHistoricalSource(t)
			bodyA := strings.Repeat("A", 150000)
			bodyB := strings.Repeat("B", 150000)
			aidA := repairReadsArtifact(t, s, old, 1, bodyA)
			repairSetPointer(t, s, fid, aidA)
			request := Object{"file_id": fid}
			if scope == "explicit-source-version" {
				request["version_id"] = old
			}
			first := repairMarkdownPage(t, s, actor, request)
			if str(first, "artifact_set_id") != aidA || str(first, "version_id") != old {
				t.Fatalf("first page selected %s/%s", first["version_id"], first["artifact_set_id"])
			}
			if str(first, "markdown") != bodyA[:65536] || str(first, "next_cursor") == "" {
				t.Fatalf("unexpected first page: bytes=%d cursor=%q", len(str(first, "markdown")), first["next_cursor"])
			}
			vidB, generationB := newer, 1
			if scope == "explicit-source-version" {
				vidB, generationB = old, 2
			}
			aidB := repairReadsArtifact(t, s, vidB, generationB, bodyB)
			repairSetPointer(t, s, fid, aidB)
			request["cursor"] = first["next_cursor"]
			second := repairMarkdownPage(t, s, actor, request)
			if str(second, "artifact_set_id") != aidA || str(second, "version_id") != old {
				t.Fatalf("continuation switched generation: %s/%s", second["version_id"], second["artifact_set_id"])
			}
			if str(second, "markdown") != bodyA[65536:131072] {
				t.Fatalf("continuation read the wrong bytes: %d", len(str(second, "markdown")))
			}
			// A caller may repeat the binding explicitly as long as it agrees.
			matched := repairMarkdownPage(t, s, actor, Object{"file_id": fid, "cursor": first["next_cursor"], "artifact_set_id": aidA, "version_id": old})
			if str(matched, "artifact_set_id") != aidA || str(matched, "markdown") != str(second, "markdown") {
				t.Fatal("matching explicit bindings changed the continuation")
			}
			// The pinned artifact is short enough to finish on this page.
			if str(second, "next_cursor") == "" {
				t.Fatalf("expected a third page for %d bytes", len(bodyA))
			}
			request["cursor"] = second["next_cursor"]
			third := repairMarkdownPage(t, s, actor, request)
			if str(third, "artifact_set_id") != aidA || str(third, "markdown") != bodyA[131072:] {
				t.Fatal("tail page did not stay on the pinned artifact")
			}
			if str(third, "next_cursor") != "" {
				t.Fatal("unexpected cursor at EOF")
			}
		})
	}
}

// B13: cursors are bearer secrets bound to one principal, file, artifact,
// version and offset. Tampering, replay by another subject, conflicting explicit
// parameters, expiry and pre-fix (decimal) cursors are rejected explicitly.
func TestRepairReadsCursorRejectsForeignTamperedExpiredAndLegacy(t *testing.T) {
	s, actor, fid, old, newer := reviewHistoricalSource(t)
	ctx := context.Background()
	body := strings.Repeat("x", 150000)
	aid := repairReadsArtifact(t, s, old, 1, body)
	repairSetPointer(t, s, fid, aid)
	first := repairMarkdownPage(t, s, actor, Object{"file_id": fid})
	cursor := str(first, "next_cursor")
	if cursor == "" {
		t.Fatal("fixture did not paginate")
	}

	tampered := cursor[:len(cursor)-1] + "A"
	if tampered == cursor {
		tampered = cursor[:len(cursor)-1] + "B"
	}
	foreign := actor
	foreign.Principal.ID = id()
	cases := []struct {
		name string
		req  Object
	}{
		{"tampered", Object{"file_id": fid, "cursor": tampered}},
		{"another-principal", Object{"file_id": fid, "cursor": cursor}},
		{"another-file", Object{"file_id": id(), "cursor": cursor}},
		{"artifact-mismatch", Object{"file_id": fid, "cursor": cursor, "artifact_set_id": id()}},
		{"version-mismatch", Object{"file_id": fid, "cursor": cursor, "version_id": newer}},
		{"offset-conflict", Object{"file_id": fid, "cursor": cursor, "offset": int64(0)}},
		{"legacy-decimal", Object{"file_id": fid, "cursor": "65536"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			who := actor
			if c.name == "another-principal" {
				who = foreign
			}
			_, e := s.markdown(ctx, who, c.req)
			repairRequireCode(t, e, domain.InvalidArgument)
		})
	}

	if _, e := s.DB.Exec(ctx, `UPDATE read_cursors SET expires_at=now()-interval '1 minute' WHERE byte_offset=$1`, int64(65536)); e != nil {
		t.Fatal(e)
	}
	_, e := s.markdown(ctx, actor, Object{"file_id": fid, "cursor": cursor})
	repairRequireCode(t, e, domain.InvalidArgument)
}

// B13: every continuation is re-authorized. Losing the read scope, disabling
// the account and trashing a current-version read all stop the next page. An
// explicitly historical cursor keeps the owner's documented retained-history
// capability instead of widening a current-version read.
func TestRepairReadsCursorReauthorizesEveryPage(t *testing.T) {
	s, actor, fid, old, _ := reviewHistoricalSource(t)
	ctx := context.Background()
	body := strings.Repeat("y", 150000)
	aid := repairReadsArtifact(t, s, old, 1, body)
	repairSetPointer(t, s, fid, aid)

	defaultFirst := repairMarkdownPage(t, s, actor, Object{"file_id": fid})
	defaultCursor := str(defaultFirst, "next_cursor")
	if defaultCursor == "" {
		t.Fatal("fixture did not paginate")
	}
	noScope := actor
	noScope.Principal.Scopes = domain.ParseScopes("drive.feedback")
	_, e := s.markdown(ctx, noScope, Object{"file_id": fid, "cursor": defaultCursor})
	repairRequireCode(t, e, domain.InsufficientScope)

	disabled := actor
	disabled.Principal.Active = false
	_, e = s.markdown(ctx, disabled, Object{"file_id": fid, "cursor": defaultCursor})
	repairRequireCode(t, e, domain.AccessDenied)

	historyFirst := repairMarkdownPage(t, s, actor, Object{"file_id": fid, "version_id": old})
	historyCursor := str(historyFirst, "next_cursor")
	if historyCursor == "" {
		t.Fatal("historical fixture did not paginate")
	}
	if _, e = s.DB.Exec(ctx, `UPDATE files SET lifecycle='trashed' WHERE id=$1`, fid); e != nil {
		t.Fatal(e)
	}
	_, e = s.markdown(ctx, actor, Object{"file_id": fid, "cursor": defaultCursor})
	repairRequireCode(t, e, domain.NotFound)
	// Retained history remains an explicit owner capability (delivery §10.3).
	continued := repairMarkdownPage(t, s, actor, Object{"file_id": fid, "cursor": historyCursor})
	if str(continued, "artifact_set_id") != aid {
		t.Fatal("authorized retained-history cursor lost its pinned artifact")
	}
}

type repairCountingStore struct {
	ports.ArtifactStore
	opens int
	bytes int64
}

type repairCountingBody struct {
	io.ReadCloser
	owner *repairCountingStore
}

func (b repairCountingBody) Read(p []byte) (int, error) {
	n, e := b.ReadCloser.Read(p)
	b.owner.bytes += int64(n)
	return n, e
}

func (s *repairCountingStore) Open(ctx context.Context, key string, r *ports.ByteRange) (ports.Content, error) {
	s.opens++
	c, e := s.ArtifactStore.Open(ctx, key, r)
	if e == nil {
		c.Body = repairCountingBody{c.Body, s}
	}
	return c, e
}

// B14: two pages of a 4 MiB body must read page-sized ranges, not the whole
// artifact twice. The output contract is unchanged.
func TestRepairReadsPaginationIsBounded(t *testing.T) {
	s, actor, fid, old, _ := reviewHistoricalSource(t)
	body := strings.Repeat("A", 4<<20)
	aid := repairReadsArtifact(t, s, old, 1, body)
	counting := &repairCountingStore{ArtifactStore: s.Artifacts}
	s.Artifacts = counting
	first := repairMarkdownPage(t, s, actor, Object{"file_id": fid, "artifact_set_id": aid})
	second := repairMarkdownPage(t, s, actor, Object{"file_id": fid, "artifact_set_id": aid, "cursor": first["next_cursor"]})
	delivered := len(str(first, "markdown")) + len(str(second, "markdown"))
	if delivered != 2*65536 {
		t.Fatalf("delivered=%d", delivered)
	}
	if counting.opens != 2 {
		t.Fatalf("expected one ranged open per page, got %d", counting.opens)
	}
	if counting.bytes > 2*(65536+3) {
		t.Fatalf("read %d bytes for %d delivered bytes", counting.bytes, delivered)
	}
	if str(first, "markdown")+str(second, "markdown") != body[:delivered] {
		t.Fatal("bounded pages changed the content")
	}
	t.Logf("two pages: object bytes read=%d delivered=%d opens=%d", counting.bytes, delivered, counting.opens)
}

// B14: UTF-8 boundaries, exact EOF, empty/short bodies and out-of-range or
// mid-rune offsets are handled without losing or duplicating characters.
func TestRepairReadsUTF8AndEOF(t *testing.T) {
	s, actor, fid, old, _ := reviewHistoricalSource(t)
	ctx := context.Background()

	t.Run("multibyte-walk", func(t *testing.T) {
		body := strings.Repeat("中文", 30000)
		aid := repairReadsArtifact(t, s, old, 1, body)
		got, artifacts := repairReadAllMarkdown(t, s, actor, Object{"file_id": fid, "artifact_set_id": aid})
		if got != body {
			t.Fatalf("multibyte reconstruction mismatch: got %d want %d", len(got), len(body))
		}
		if !utf8.ValidString(got) {
			t.Fatal("reconstructed content is not valid UTF-8")
		}
		for _, id := range artifacts {
			if id != aid {
				t.Fatal("page switched artifact during a walk")
			}
		}
	})

	t.Run("boundary-split", func(t *testing.T) {
		body := strings.Repeat("a", 65535) + "😀" + strings.Repeat("b", 100)
		aid := repairReadsArtifact(t, s, old, 2, body)
		first := repairMarkdownPage(t, s, actor, Object{"file_id": fid, "artifact_set_id": aid})
		if len(str(first, "markdown")) != 65535 || str(first, "markdown") != body[:65535] {
			t.Fatalf("page split inside a rune: %d bytes", len(str(first, "markdown")))
		}
		second := repairMarkdownPage(t, s, actor, Object{"file_id": fid, "artifact_set_id": aid, "cursor": first["next_cursor"]})
		if !strings.HasPrefix(str(second, "markdown"), "😀") {
			t.Fatal("next page did not resume at the rune boundary")
		}
		if str(first, "markdown")+str(second, "markdown") != body {
			t.Fatal("boundary walk lost or duplicated bytes")
		}
	})

	t.Run("exact-eof", func(t *testing.T) {
		body := strings.Repeat("e", 65536)
		aid := repairReadsArtifact(t, s, old, 3, body)
		page := repairMarkdownPage(t, s, actor, Object{"file_id": fid, "artifact_set_id": aid})
		if str(page, "markdown") != body || str(page, "next_cursor") != "" {
			t.Fatalf("exact EOF page: bytes=%d cursor=%q", len(str(page, "markdown")), page["next_cursor"])
		}
	})

	t.Run("empty", func(t *testing.T) {
		aid := repairReadsArtifact(t, s, old, 4, "")
		page := repairMarkdownPage(t, s, actor, Object{"file_id": fid, "artifact_set_id": aid})
		if str(page, "markdown") != "" || str(page, "next_cursor") != "" {
			t.Fatalf("empty artifact returned %q cursor=%q", page["markdown"], page["next_cursor"])
		}
	})

	t.Run("short", func(t *testing.T) {
		aid := repairReadsArtifact(t, s, old, 5, "short text")
		page := repairMarkdownPage(t, s, actor, Object{"file_id": fid, "artifact_set_id": aid})
		if str(page, "markdown") != "short text" || str(page, "next_cursor") != "" {
			t.Fatal("short artifact paging changed")
		}
	})

	t.Run("offset-eof", func(t *testing.T) {
		body := "1234567890"
		aid := repairReadsArtifact(t, s, old, 6, body)
		page := repairMarkdownPage(t, s, actor, Object{"file_id": fid, "artifact_set_id": aid, "offset": int64(len(body))})
		if str(page, "markdown") != "" || str(page, "next_cursor") != "" {
			t.Fatal("offset at EOF must be an empty final page")
		}
	})

	t.Run("offset-beyond", func(t *testing.T) {
		aid := repairReadsArtifact(t, s, old, 7, "1234567890")
		_, e := s.markdown(ctx, actor, Object{"file_id": fid, "artifact_set_id": aid, "offset": int64(11)})
		repairRequireCode(t, e, domain.InvalidArgument)
	})

	t.Run("offset-mid-rune", func(t *testing.T) {
		aid := repairReadsArtifact(t, s, old, 8, "中文中文")
		_, e := s.markdown(ctx, actor, Object{"file_id": fid, "artifact_set_id": aid, "offset": int64(1)})
		repairRequireCode(t, e, domain.InvalidArgument)
	})
}

// B07: non-integer parent revisions are rejected at the application boundary,
// independently of JSON-schema validation in a transport.
func TestRepairMCPMkdirParentRevisionTypeValidation(t *testing.T) {
	valid := []any{int64(1), json.Number("2"), 3, float64(2)}
	for _, v := range valid {
		if e := validateParentRevision(Object{"expected_parent_revision": v}); e != nil {
			t.Fatalf("valid revision %v rejected: %v", v, e)
		}
	}
	for _, v := range []any{"1", true, int64(0), -1, json.Number("1.5"), json.Number("x"), 0.5} {
		e := validateParentRevision(Object{"expected_parent_revision": v})
		repairRequireCode(t, e, domain.InvalidArgument)
	}
	if e := validateParentRevision(Object{}); e != nil {
		t.Fatalf("root mkdir without a revision must pass this validator: %v", e)
	}
}

type repairShortStore struct{ ports.ArtifactStore }

type repairShortBody struct {
	io.Reader
	io.Closer
}

func (s repairShortStore) Open(ctx context.Context, key string, r *ports.ByteRange) (ports.Content, error) {
	c, e := s.ArtifactStore.Open(ctx, key, r)
	if e != nil {
		return c, e
	}
	c.Body = repairShortBody{io.LimitReader(c.Body, c.Size/2), c.Body}
	return c, nil
}

// A store that returns fewer bytes than its advertised range must fail closed:
// no truncated page may be returned as if it were a complete page.
func TestRepairReadsShortObjectReadFailsClosed(t *testing.T) {
	s, actor, fid, old, _ := reviewHistoricalSource(t)
	aid := repairReadsArtifact(t, s, old, 1, strings.Repeat("z", 1000))
	s.Artifacts = repairShortStore{ArtifactStore: s.Artifacts}
	_, e := s.markdown(context.Background(), actor, Object{"file_id": fid, "artifact_set_id": aid})
	repairRequireCode(t, e, domain.DependencyUnavailable)
}
