// Package converters implements the project's bounded internal converter HTTP
// contract. These endpoints are not native AnyDoc/PaddleOCR APIs.
package converters

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

type Config struct {
	BaseURL, BearerToken string
	InputOrigins         []string
	MaxResponseBytes     int64
	Timeout              time.Duration
	Client               *http.Client
}
type HTTP struct {
	cfg    Config
	base   *url.URL
	client *http.Client
}

func New(cfg Config) (*HTTP, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, ports.ErrInvalid
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = 64 << 20
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Minute
	}
	client := &http.Client{Timeout: cfg.Timeout}
	if cfg.Client != nil {
		*client = *cfg.Client
		if client.Timeout == 0 {
			client.Timeout = cfg.Timeout
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTP{cfg: cfg, base: u, client: client}, nil
}
func (c *HTTP) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, e := json.Marshal(in)
		if e != nil {
			return ports.ErrInvalid
		}
		body = bytes.NewReader(b)
	}
	endpoint := *c.base
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + path
	req, e := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if e != nil {
		return e
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.BearerToken)
	}
	resp, e := c.client.Do(req)
	if e != nil {
		return &ports.DependencyError{Dependency: "converter", Operation: path, Code: "transport_failure", Cause: ports.ErrUnavailable}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cause := ports.ErrUnavailable
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			cause = ports.ErrUnauthorized
		}
		return &ports.DependencyError{Dependency: "converter", Operation: path, Status: resp.StatusCode, Code: "http_error", Cause: cause}
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxResponseBytes+1))
	if e != nil {
		return ports.ErrUnavailable
	}
	if int64(len(b)) > c.cfg.MaxResponseBytes {
		return ports.ErrLimit
	}
	if e = json.Unmarshal(b, out); e != nil {
		return fmt.Errorf("converter invalid response: %w", ports.ErrIntegrity)
	}
	return nil
}
func (c *HTTP) Capabilities(ctx context.Context) (ports.Capabilities, error) {
	var out ports.Capabilities
	if e := c.call(ctx, "GET", "/v1/capabilities", nil, &out); e != nil {
		return out, e
	}
	if out.SchemaVersion != 1 || out.Engine.Name == "" || out.Engine.Version == "" || len(out.Capabilities) == 0 {
		return out, ports.ErrIntegrity
	}
	return out, nil
}
func segment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
}
func (c *HTTP) Execute(ctx context.Context, in ports.ConversionRequest) (ports.ConversionResult, error) {
	var out ports.ConversionResult
	if !segment(in.JobID) || !segment(in.AttemptID) || in.OutputScope != "tmp/"+in.JobID+"/"+in.AttemptID+"/" || in.Input.SourceVersionID == "" || in.Recipe.TimeoutSeconds <= 0 || in.Recipe.MaxPages <= 0 {
		return out, ports.ErrInvalid
	}
	if in.Input.URL != "" {
		if in.Input.ContentBase64 != "" {
			return out, ports.ErrInvalid
		}
		u, e := url.Parse(in.Input.URL)
		if e != nil || u.User != nil || u.Fragment != "" || u.Host == "" {
			return out, ports.ErrInvalid
		}
		allowed := false
		for _, origin := range c.cfg.InputOrigins {
			o, e := url.Parse(origin)
			if e == nil && u.Scheme == o.Scheme && u.Host == o.Host && (o.Path == "" || strings.HasPrefix(u.Path, strings.TrimRight(o.Path, "/")+"/")) {
				allowed = true
			}
		}
		if !allowed {
			return out, fmt.Errorf("input origin not allowed: %w", ports.ErrInvalid)
		}
	} else if in.Input.ContentBase64 == "" && in.Input.Size != 0 {
		return out, ports.ErrInvalid
	}
	if e := c.call(ctx, "POST", "/v1/execute", in, &out); e != nil {
		return out, e
	}
	if e := ValidateResult(out, in.Recipe.MaxPages, c.cfg.MaxResponseBytes); e != nil {
		return ports.ConversionResult{}, e
	}
	return out, nil
}

// ValidateResult rejects impossible success claims and assets whose hashes do
// not match bytes. It does not write converter-provided object keys or URLs.
func ValidateResult(out ports.ConversionResult, maxPages int, maxBytes int64) error {
	if out.Engine.Name == "" || out.Engine.Version == "" {
		return ports.ErrIntegrity
	}
	switch out.Status {
	case "ready", "partial", "not_converted", "blocked", "failed":
	default:
		return ports.ErrIntegrity
	}
	if out.Completeness.TotalPages < 0 || (maxPages > 0 && out.Completeness.TotalPages > maxPages) {
		return ports.ErrLimit
	}
	if out.Status == "ready" && (out.Completeness.Status != "ready" || len(out.Completeness.MissingTextPages) > 0 || len(out.Completeness.MissingDescriptionPages) > 0 || len(out.Errors) > 0) {
		return ports.ErrIntegrity
	}
	if out.Status == "partial" && out.Completeness.Status != "partial" {
		return ports.ErrIntegrity
	}
	if (out.Status == "ready" || out.Status == "partial") && strings.TrimSpace(out.Markdown) == "" && len(out.Assets) == 0 {
		return ports.ErrIntegrity
	}
	total := int64(len(out.Markdown))
	ids := map[string]bool{}
	for _, a := range out.Assets {
		if !segment(a.ID) || ids[a.ID] || a.Page < 1 {
			return ports.ErrIntegrity
		}
		ids[a.ID] = true
		data, e := base64.StdEncoding.DecodeString(a.ContentBase64)
		if e != nil {
			return ports.ErrIntegrity
		}
		total += int64(len(data))
		h := sha256.Sum256(data)
		if !strings.EqualFold(a.SHA256, hex.EncodeToString(h[:])) {
			return ports.ErrIntegrity
		}
	}
	if maxBytes > 0 && total > maxBytes {
		return ports.ErrLimit
	}
	for _, list := range [][]int{out.Completeness.MissingTextPages, out.Completeness.MissingDescriptionPages} {
		seen := map[int]bool{}
		for _, p := range list {
			if p < 1 || p > out.Completeness.TotalPages || seen[p] {
				return ports.ErrIntegrity
			}
			seen[p] = true
		}
	}
	for _, p := range out.Pages {
		if p.Page < 1 || p.Page > out.Completeness.TotalPages {
			return ports.ErrIntegrity
		}
		if out.Status == "ready" && (p.TextStatus == "failed" || p.DescriptionStatus == "failed" || p.DescriptionStatus == "blocked") {
			return ports.ErrIntegrity
		}
	}
	return nil
}

var _ ports.Converter = (*HTTP)(nil)
