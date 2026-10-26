package artifactstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func digest(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func exerciseStore(t *testing.T, s ports.ArtifactStore) {
	t.Helper()
	ctx := context.Background()
	data := "immutable contents 世界"
	w := ports.ArtifactWrite{Key: "tmp/job/attempt/content.md", Body: strings.NewReader(data), Size: int64(len(data)), SHA256: digest(data), ContentType: "text/markdown"}
	obj, e := s.PutAttempt(ctx, w)
	if e != nil || obj.Size != w.Size {
		t.Fatalf("put %+v %v", obj, e)
	}
	obj, e = s.Publish(ctx, w.Key, "markdown/art/content.md", w.SHA256)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.Publish(ctx, w.Key, "markdown/art/content.md", w.SHA256)
	if e != nil {
		t.Fatal("idempotent immutable publish", e)
	}
	c, e := s.Open(ctx, obj.Key, &ports.ByteRange{Start: 2, End: 5})
	if e != nil {
		t.Fatal(e)
	}
	got, _ := io.ReadAll(c.Body)
	c.Body.Close()
	if string(got) != data[2:6] || c.StatusCode != 206 {
		t.Fatalf("range %q %+v", got, c)
	}
	_, e = s.Verify(ctx, obj.Key, digest("wrong"))
	if !errors.Is(e, ports.ErrIntegrity) {
		t.Fatalf("checksum mismatch %v", e)
	}
	w.Body = strings.NewReader("different")
	w.Size = 9
	w.SHA256 = digest("different")
	_, e = s.PutAttempt(ctx, w)
	if !errors.Is(e, ports.ErrConflict) {
		t.Fatalf("overwrite %v", e)
	}
	for _, key := range []string{"../escape", "tmp/j/a/../../escape", "/absolute", "tmp/j/a/%2e%2e/x", "markdown/direct.md"} {
		w.Key = key
		w.Body = strings.NewReader("different")
		if _, e = s.PutAttempt(ctx, w); !errors.Is(e, ports.ErrInvalid) {
			t.Fatalf("unsafe key %s: %v", key, e)
		}
	}
}
func TestLocalImmutableRoundTrip(t *testing.T) {
	l, e := NewLocal(t.TempDir(), 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	exerciseStore(t, l)
}
func TestLocalSymlinkEscapeAndConcurrentPublish(t *testing.T) {
	root := t.TempDir()
	l, _ := NewLocal(root, 1<<20)
	defer l.Close()
	outside := t.TempDir()
	if e := os.Symlink(outside, filepath.Join(root, "tmp")); e != nil {
		t.Fatal(e)
	}
	_, e := l.PutAttempt(context.Background(), ports.ArtifactWrite{Key: "tmp/j/a/file", Body: strings.NewReader("x"), Size: 1, SHA256: digest("x")})
	if e == nil {
		t.Fatal("symlink escape allowed")
	}
	os.Remove(filepath.Join(root, "tmp"))
	ctx := context.Background()
	for _, a := range []string{"a", "b"} {
		_, e := l.PutAttempt(ctx, ports.ArtifactWrite{Key: "tmp/j/" + a + "/f", Body: strings.NewReader(a), Size: 1, SHA256: digest(a)})
		if e != nil {
			t.Fatal(e)
		}
	}
	var wg sync.WaitGroup
	out := make(chan error, 2)
	for _, a := range []string{"a", "b"} {
		wg.Add(1)
		go func(a string) {
			defer wg.Done()
			_, e := l.Publish(ctx, "tmp/j/"+a+"/f", "markdown/same/f", digest(a))
			out <- e
		}(a)
	}
	wg.Wait()
	close(out)
	ok, conflict := 0, 0
	for e := range out {
		if e == nil {
			ok++
		} else if errors.Is(e, ports.ErrConflict) {
			conflict++
		} else {
			t.Fatal(e)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("race %d %d", ok, conflict)
	}
}
func TestS3ConditionalAndChecksumHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.Header.Get("If-None-Match") != "*" || r.Header.Get("X-Amz-Checksum-Sha256") == "" || !strings.Contains(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
			t.Errorf("missing integrity/conditional headers")
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	s, _ := NewS3(S3Config{Endpoint: srv.URL, Region: "us-east-1", Bucket: "artifacts", AccessKey: "test", SecretKey: "test", ForcePathStyle: true})
	_, e := s.PutAttempt(context.Background(), ports.ArtifactWrite{Key: "tmp/j/a/f", Body: strings.NewReader("x"), Size: 1, SHA256: digest("x")})
	if e != nil {
		t.Fatal(e)
	}
}
func TestS3Integration(t *testing.T) {
	endpoint := os.Getenv("DRIVE_TEST_S3_URL")
	if endpoint == "" {
		t.Skip("set DRIVE_TEST_S3_URL for real S3/RustFS integration")
	}
	bucket := fmt.Sprintf("adapter-test-%d", time.Now().UnixNano())
	s, e := NewS3(S3Config{Endpoint: endpoint, Region: "us-east-1", Bucket: bucket, AccessKey: os.Getenv("DRIVE_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("DRIVE_TEST_S3_SECRET_KEY"), ForcePathStyle: true})
	if e != nil {
		t.Fatal(e)
	}
	req, _ := http.NewRequest("PUT", strings.TrimRight(endpoint, "/")+"/"+bucket, nil)
	s.sign(req, hexHash(""))
	resp, e := s.client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create test bucket HTTP %d %s", resp.StatusCode, b)
	}
	resp.Body.Close()
	exerciseStore(t, s)
	t.Log("real S3: SigV4, checksum-verified puts, If-None-Match conflict, immutable publish, and Range passed")
}
