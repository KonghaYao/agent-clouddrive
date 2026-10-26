// Package artifactstore stores immutable derived artifacts, never originals.
package artifactstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
)

const DefaultMaxBytes int64 = 64 << 20

func validKey(key string) bool {
	if key == "" || len(key) > 1024 || strings.HasPrefix(key, "/") || strings.ContainsAny(key, "\\\x00%") || path.Clean(key) != key {
		return false
	}
	for _, s := range strings.Split(key, "/") {
		if s == "" || s == "." || s == ".." {
			return false
		}
	}
	return true
}
func attemptKey(key string) bool {
	return validKey(key) && len(strings.Split(key, "/")) >= 4 && strings.HasPrefix(key, "tmp/")
}
func finalKey(key string) bool {
	if !validKey(key) {
		return false
	}
	for _, p := range []string{"markdown/", "images/", "manifests/", "previews/"} {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}
func hashValid(s string) bool { b, e := hex.DecodeString(s); return e == nil && len(b) == 32 }
func media(key string) string {
	v := mime.TypeByExtension(path.Ext(key))
	if v == "" {
		return "application/octet-stream"
	}
	return v
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(p)
}

type readerClose struct {
	io.Reader
	io.Closer
}

func bounds(size int64, br *ports.ByteRange) (int64, int64, error) {
	if br == nil {
		return 0, size, nil
	}
	if br.Start < 0 || br.End < -1 || br.Start >= size || (br.End >= 0 && br.End < br.Start) {
		return 0, 0, ports.ErrInvalid
	}
	end := br.End
	if end < 0 || end >= size {
		end = size - 1
	}
	return br.Start, end - br.Start + 1, nil
}

type Local struct {
	root *os.Root
	max  int64
}

func NewLocal(root string, maxBytes int64) (*Local, error) {
	if root == "" {
		return nil, ports.ErrInvalid
	}
	if e := os.MkdirAll(root, 0700); e != nil {
		return nil, e
	}
	r, e := os.OpenRoot(root)
	if e != nil {
		return nil, e
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &Local{root: r, max: maxBytes}, nil
}
func (l *Local) Close() error { return l.root.Close() }
func (l *Local) PutAttempt(ctx context.Context, w ports.ArtifactWrite) (ports.Object, error) {
	if !attemptKey(w.Key) {
		return ports.Object{}, ports.ErrInvalid
	}
	return l.put(ctx, w)
}
func (l *Local) put(ctx context.Context, w ports.ArtifactWrite) (ports.Object, error) {
	var out ports.Object
	if !validKey(w.Key) || w.Body == nil || w.Size < 0 || w.Size > l.max || !hashValid(w.SHA256) {
		return out, ports.ErrInvalid
	}
	if e := l.root.MkdirAll(path.Dir(w.Key), 0700); e != nil {
		return out, e
	}
	nonce := make([]byte, 16)
	if _, e := rand.Read(nonce); e != nil {
		return out, e
	}
	temp := path.Dir(w.Key) + "/.upload-" + hex.EncodeToString(nonce)
	f, e := l.root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return out, e
	}
	defer l.root.Remove(temp)
	h := sha256.New()
	n, e := io.CopyBuffer(io.MultiWriter(f, h), io.LimitReader(contextReader{ctx, w.Body}, w.Size+1), make([]byte, 32<<10))
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return out, e
	}
	if closeErr != nil {
		return out, closeErr
	}
	if n != w.Size || !strings.EqualFold(w.SHA256, hex.EncodeToString(h.Sum(nil))) {
		return out, ports.ErrIntegrity
	}
	// Hard-link publication is atomic and never replaces an existing key.
	if e = l.root.Link(temp, w.Key); e != nil {
		if errors.Is(e, os.ErrExist) {
			old, ve := l.Verify(ctx, w.Key, w.SHA256)
			if ve != nil {
				return out, ports.ErrConflict
			}
			return old, nil
		}
		return out, e
	}
	if d, e := l.root.Open(path.Dir(w.Key)); e == nil {
		e = d.Sync()
		_ = d.Close()
		if e != nil {
			return out, e
		}
	}
	ct := w.ContentType
	if ct == "" {
		ct = media(w.Key)
	}
	return ports.Object{Key: w.Key, Size: n, SHA256: strings.ToLower(w.SHA256), ContentType: ct}, nil
}
func (l *Local) Publish(ctx context.Context, from, to, sha string) (ports.Object, error) {
	if !attemptKey(from) || !finalKey(to) || !hashValid(sha) {
		return ports.Object{}, ports.ErrInvalid
	}
	c, e := l.Open(ctx, from, nil)
	if e != nil {
		return ports.Object{}, e
	}
	defer c.Body.Close()
	return l.put(ctx, ports.ArtifactWrite{Key: to, Body: c.Body, Size: c.Size, SHA256: sha, ContentType: media(to)})
}
func (l *Local) Open(ctx context.Context, key string, br *ports.ByteRange) (ports.Content, error) {
	var out ports.Content
	if !validKey(key) {
		return out, ports.ErrInvalid
	}
	if e := ctx.Err(); e != nil {
		return out, e
	}
	f, e := l.root.Open(key)
	if e != nil {
		if errors.Is(e, os.ErrNotExist) {
			return out, ports.ErrNotFound
		}
		return out, e
	}
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() {
		f.Close()
		return out, ports.ErrInvalid
	}
	start, n, e := bounds(info.Size(), br)
	if e != nil {
		f.Close()
		return out, e
	}
	out = ports.Content{Body: readerClose{contextReader{ctx, io.NewSectionReader(f, start, n)}, f}, Size: n, TotalSize: info.Size(), ContentType: media(key), StatusCode: http.StatusOK}
	if br != nil {
		out.StatusCode = http.StatusPartialContent
		out.ContentRange = fmt.Sprintf("bytes %d-%d/%d", start, start+n-1, info.Size())
	}
	return out, nil
}
func (l *Local) Verify(ctx context.Context, key, expected string) (ports.Object, error) {
	if !hashValid(expected) {
		return ports.Object{}, ports.ErrInvalid
	}
	c, e := l.Open(ctx, key, nil)
	if e != nil {
		return ports.Object{}, e
	}
	defer c.Body.Close()
	if c.Size > l.max {
		return ports.Object{}, ports.ErrLimit
	}
	h := sha256.New()
	n, e := io.CopyBuffer(h, c.Body, make([]byte, 32<<10))
	if e != nil {
		return ports.Object{}, e
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if n != c.Size || !strings.EqualFold(digest, expected) {
		return ports.Object{}, ports.ErrIntegrity
	}
	return ports.Object{Key: key, Size: n, SHA256: digest, ContentType: c.ContentType}, nil
}

var _ ports.ArtifactStore = (*Local)(nil)
