package artifactstore

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// S3 is a deliberately small S3 REST/SigV4 client for bounded derived objects.
// It never writes oCIS's bucket, never deletes final objects, and does not claim
// RustFS interoperability until the deployment's real integration test passes.
type S3Config struct {
	Endpoint, Region, Bucket, AccessKey, SecretKey, SessionToken string
	ForcePathStyle                                               bool
	MaxObjectBytes                                               int64
	Client                                                       *http.Client
	Now                                                          func() time.Time
}
type S3 struct {
	cfg      S3Config
	endpoint *url.URL
	client   *http.Client
}

func NewS3(cfg S3Config) (*S3, error) {
	u, e := url.Parse(cfg.Endpoint)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || cfg.Region == "" || cfg.Bucket == "" || strings.ContainsAny(cfg.Bucket, "/\\") || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, ports.ErrInvalid
	}
	if cfg.MaxObjectBytes <= 0 {
		cfg.MaxObjectBytes = DefaultMaxBytes
	}
	if cfg.MaxObjectBytes > 5<<30 {
		return nil, ports.ErrInvalid
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cl := &http.Client{Timeout: 5 * time.Minute}
	if cfg.Client != nil {
		*cl = *cfg.Client
	}
	if cl.Timeout == 0 {
		cl.Timeout = 5 * time.Minute
	}
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &S3{cfg: cfg, endpoint: u, client: cl}, nil
}
func awsEncode(s string, slash bool) string {
	var b strings.Builder
	const h = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.~", rune(c)) || slash && c == '/' {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(h[c>>4])
			b.WriteByte(h[c&15])
		}
	}
	return b.String()
}
func hmac256(key []byte, value string) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(value))
	return m.Sum(nil)
}
func hexHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func (s *S3) sign(r *http.Request, payloadHash string) {
	now := s.cfg.Now().UTC()
	date := now.Format("20060102")
	stamp := now.Format("20060102T150405Z")
	r.Header.Set("X-Amz-Date", stamp)
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if s.cfg.SessionToken != "" {
		r.Header.Set("X-Amz-Security-Token", s.cfg.SessionToken)
	}
	headers := map[string]string{"host": r.URL.Host}
	for k, v := range r.Header {
		headers[strings.ToLower(k)] = strings.Join(strings.Fields(strings.Join(v, ",")), " ")
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var canonical strings.Builder
	for _, k := range keys {
		canonical.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(keys, ";")
	scope := date + "/" + s.cfg.Region + "/s3/aws4_request"
	request := r.Method + "\n" + r.URL.EscapedPath() + "\n" + strings.ReplaceAll(r.URL.Query().Encode(), "+", "%20") + "\n" + canonical.String() + "\n" + signed + "\n" + payloadHash
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hexHash(request)
	k := hmac256([]byte("AWS4"+s.cfg.SecretKey), date)
	k = hmac256(k, s.cfg.Region)
	k = hmac256(k, "s3")
	k = hmac256(k, "aws4_request")
	signature := hex.EncodeToString(hmac256(k, toSign))
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.cfg.AccessKey+"/"+scope+", SignedHeaders="+signed+", Signature="+signature)
}
func (s *S3) request(ctx context.Context, method, key string, body io.Reader, size int64, hash string, headers http.Header) (*http.Response, error) {
	if !validKey(key) {
		return nil, ports.ErrInvalid
	}
	u := *s.endpoint
	if s.cfg.ForcePathStyle {
		u.Path = strings.TrimRight(u.Path, "/") + "/" + s.cfg.Bucket + "/" + key
	} else {
		u.Host = s.cfg.Bucket + "." + u.Host
		u.Path = strings.TrimRight(u.Path, "/") + "/" + key
	}
	u.RawPath = awsEncode(u.Path, true)
	r, e := http.NewRequestWithContext(ctx, method, u.String(), body)
	if e != nil {
		return nil, e
	}
	r.ContentLength = size
	if headers != nil {
		r.Header = headers.Clone()
	}
	s.sign(r, hash)
	resp, e := s.client.Do(r)
	if e != nil {
		cause := ports.ErrUnavailable
		if method == "PUT" {
			cause = ports.ErrOutcomeUnknown
		}
		return nil, &ports.DependencyError{Dependency: "artifact_s3", Operation: method, Code: "transport_error", Cause: cause}
	}
	return resp, nil
}
func s3Error(resp *http.Response, method string) error {
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	cause := ports.ErrUnavailable
	if resp.StatusCode == 404 {
		cause = ports.ErrNotFound
	}
	if resp.StatusCode == 412 || resp.StatusCode == 409 {
		cause = ports.ErrConflict
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		cause = ports.ErrUnauthorized
	}
	if resp.StatusCode == 501 {
		cause = ports.ErrUnverified
	}
	// A range that starts at or beyond EOF is unsatisfiable, matching the local
	// adapter's ports.ErrInvalid instead of surfacing as a transient dependency
	// failure. Callers can then answer EOF or an explicit offset error.
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		cause = ports.ErrInvalid
	}
	return &ports.DependencyError{Dependency: "artifact_s3", Operation: method, Code: "http_error", Status: resp.StatusCode, Cause: cause}
}
func (s *S3) PutAttempt(ctx context.Context, w ports.ArtifactWrite) (ports.Object, error) {
	if !attemptKey(w.Key) {
		return ports.Object{}, ports.ErrInvalid
	}
	return s.put(ctx, w)
}
func (s *S3) put(ctx context.Context, w ports.ArtifactWrite) (ports.Object, error) {
	if !validKey(w.Key) || w.Size < 0 || w.Size > s.cfg.MaxObjectBytes || w.Body == nil || !hashValid(w.SHA256) {
		return ports.Object{}, ports.ErrInvalid
	}
	f, e := os.CreateTemp("", "clouddrive-artifact-*")
	if e != nil {
		return ports.Object{}, e
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	n, e := io.CopyBuffer(io.MultiWriter(f, h), io.LimitReader(contextReader{ctx, w.Body}, w.Size+1), make([]byte, 32<<10))
	if e != nil {
		return ports.Object{}, e
	}
	digest := h.Sum(nil)
	if n != w.Size || !strings.EqualFold(w.SHA256, hex.EncodeToString(digest)) {
		return ports.Object{}, ports.ErrIntegrity
	}
	if _, e = f.Seek(0, 0); e != nil {
		return ports.Object{}, e
	}
	ct := w.ContentType
	if ct == "" {
		ct = media(w.Key)
	}
	headers := http.Header{"If-None-Match": []string{"*"}, "Content-Type": []string{ct}, "X-Amz-Checksum-Sha256": []string{base64.StdEncoding.EncodeToString(digest)}, "X-Amz-Meta-Sha256": []string{hex.EncodeToString(digest)}}
	resp, e := s.request(ctx, "PUT", w.Key, f, n, hex.EncodeToString(digest), headers)
	if e != nil {
		return ports.Object{}, e
	}
	if resp.StatusCode == 412 {
		resp.Body.Close()
		old, e := s.Verify(ctx, w.Key, w.SHA256)
		if e != nil {
			return ports.Object{}, ports.ErrConflict
		}
		return old, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ports.Object{}, s3Error(resp, "PUT")
	}
	resp.Body.Close()
	return ports.Object{Key: w.Key, Size: n, SHA256: hex.EncodeToString(digest), ContentType: ct}, nil
}
func (s *S3) Publish(ctx context.Context, from, to, sha string) (ports.Object, error) {
	if !attemptKey(from) || !finalKey(to) || !hashValid(sha) {
		return ports.Object{}, ports.ErrInvalid
	}
	c, e := s.Open(ctx, from, nil)
	if e != nil {
		return ports.Object{}, e
	}
	defer c.Body.Close()
	return s.put(ctx, ports.ArtifactWrite{Key: to, Body: c.Body, Size: c.Size, SHA256: sha, ContentType: media(to)})
}
func (s *S3) Open(ctx context.Context, key string, br *ports.ByteRange) (ports.Content, error) {
	var out ports.Content
	h := http.Header{}
	if br != nil {
		if br.Start < 0 || br.End < -1 || br.End >= 0 && br.End < br.Start {
			return out, ports.ErrInvalid
		}
		v := fmt.Sprintf("bytes=%d-", br.Start)
		if br.End >= 0 {
			v += strconv.FormatInt(br.End, 10)
		}
		h.Set("Range", v)
	}
	resp, e := s.request(ctx, "GET", key, nil, 0, hexHash(""), h)
	if e != nil {
		return out, e
	}
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		return out, s3Error(resp, "GET")
	}
	if br != nil && resp.StatusCode != 206 {
		resp.Body.Close()
		return out, ports.ErrIntegrity
	}
	if resp.ContentLength < 0 || resp.ContentLength > s.cfg.MaxObjectBytes {
		resp.Body.Close()
		return out, ports.ErrLimit
	}
	out = ports.Content{Body: resp.Body, Size: resp.ContentLength, TotalSize: resp.ContentLength, ContentType: resp.Header.Get("Content-Type"), ETag: resp.Header.Get("ETag"), ContentRange: resp.Header.Get("Content-Range"), StatusCode: resp.StatusCode}
	if br != nil {
		parts := strings.Split(out.ContentRange, "/")
		if len(parts) != 2 {
			resp.Body.Close()
			return ports.Content{}, ports.ErrIntegrity
		}
		total, e := strconv.ParseInt(parts[1], 10, 64)
		if e != nil {
			resp.Body.Close()
			return ports.Content{}, ports.ErrIntegrity
		}
		var start, end, reportedTotal int64
		count, rangeErr := fmt.Sscanf(out.ContentRange, "bytes %d-%d/%d", &start, &end, &reportedTotal)
		expectedEnd := br.End
		if expectedEnd < 0 || expectedEnd >= total {
			expectedEnd = total - 1
		}
		if count != 3 || rangeErr != nil || reportedTotal != total || start != br.Start || end != expectedEnd || end < start || total <= end || resp.ContentLength != end-start+1 {
			resp.Body.Close()
			return ports.Content{}, ports.ErrIntegrity
		}
		out.TotalSize = total
	}
	return out, nil
}
func (s *S3) Verify(ctx context.Context, key, expected string) (ports.Object, error) {
	if !hashValid(expected) {
		return ports.Object{}, ports.ErrInvalid
	}
	c, e := s.Open(ctx, key, nil)
	if e != nil {
		return ports.Object{}, e
	}
	defer c.Body.Close()
	h := sha256.New()
	n, e := io.CopyBuffer(h, io.LimitReader(c.Body, s.cfg.MaxObjectBytes+1), make([]byte, 32<<10))
	if e != nil {
		return ports.Object{}, e
	}
	hash := hex.EncodeToString(h.Sum(nil))
	if n != c.Size || !strings.EqualFold(hash, expected) {
		return ports.Object{}, ports.ErrIntegrity
	}
	return ports.Object{Key: key, Size: n, SHA256: hash, ContentType: c.ContentType}, nil
}

var _ ports.ArtifactStore = (*S3)(nil)
