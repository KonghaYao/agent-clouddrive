package artifactstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

// B14 contract: a range that starts at or beyond EOF is a client range error,
// not a dependency outage. Empty objects expose their zero length through an
// un-ranged open.
func TestRepairReadsRangeEOFAndEmptyObject(t *testing.T) {
	ctx := context.Background()
	l, e := NewLocal(t.TempDir(), 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	empty := ports.ArtifactWrite{Key: "tmp/j/a/empty.md", Body: strings.NewReader(""), Size: 0, SHA256: digest(""), ContentType: "text/markdown"}
	if _, e = l.PutAttempt(ctx, empty); e != nil {
		t.Fatal(e)
	}
	if _, e = l.Publish(ctx, empty.Key, "markdown/empty/content.md", empty.SHA256); e != nil {
		t.Fatal(e)
	}
	if _, e = l.Open(ctx, "markdown/empty/content.md", &ports.ByteRange{Start: 0, End: 0}); !errors.Is(e, ports.ErrInvalid) {
		t.Fatalf("empty ranged open: %v", e)
	}
	c, e := l.Open(ctx, "markdown/empty/content.md", nil)
	if e != nil {
		t.Fatal(e)
	}
	if c.TotalSize != 0 || c.Size != 0 {
		t.Fatalf("empty size: %+v", c)
	}
	c.Body.Close()

	body := "0123456789"
	w := ports.ArtifactWrite{Key: "tmp/j/a/ten.md", Body: strings.NewReader(body), Size: int64(len(body)), SHA256: digest(body), ContentType: "text/markdown"}
	if _, e = l.PutAttempt(ctx, w); e != nil {
		t.Fatal(e)
	}
	if _, e = l.Publish(ctx, w.Key, "markdown/ten/content.md", w.SHA256); e != nil {
		t.Fatal(e)
	}
	if _, e = l.Open(ctx, "markdown/ten/content.md", &ports.ByteRange{Start: int64(len(body)), End: int64(len(body))}); !errors.Is(e, ports.ErrInvalid) {
		t.Fatalf("EOF ranged open: %v", e)
	}
	tail, e := l.Open(ctx, "markdown/ten/content.md", &ports.ByteRange{Start: 9, End: 100})
	if e != nil {
		t.Fatal(e)
	}
	b, _ := io.ReadAll(tail.Body)
	tail.Body.Close()
	if string(b) != "9" || tail.TotalSize != int64(len(body)) || tail.StatusCode != http.StatusPartialContent {
		t.Fatalf("tail range: %q total=%d status=%d", b, tail.TotalSize, tail.StatusCode)
	}
}

func TestRepairReadsS3UnsatisfiableRangeIsInvalid(t *testing.T) {
	e := s3Error(&http.Response{StatusCode: http.StatusRequestedRangeNotSatisfiable, Body: io.NopCloser(strings.NewReader(""))}, "GET")
	if !errors.Is(e, ports.ErrInvalid) {
		t.Fatalf("416 mapped to %v", e)
	}
}
