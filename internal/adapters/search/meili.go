// Package search treats Meilisearch as a rebuildable, untrusted candidate index.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

type Config struct {
	URL, APIKey, Index        string
	Client                    *http.Client
	PollInterval, TaskTimeout time.Duration
}
type Meili struct {
	cfg    Config
	client *http.Client
}

var validIndex = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func New(cfg Config) (*Meili, error) {
	u, e := url.Parse(cfg.URL)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !validIndex.MatchString(cfg.Index) {
		return nil, ports.ErrInvalid
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 100 * time.Millisecond
	}
	if cfg.TaskTimeout <= 0 {
		cfg.TaskTimeout = 2 * time.Minute
	}
	cl := &http.Client{Timeout: 30 * time.Second}
	if cfg.Client != nil {
		*cl = *cfg.Client
	}
	if cl.Timeout == 0 {
		cl.Timeout = 30 * time.Second
	}
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	return &Meili{cfg: cfg, client: cl}, nil
}
func (m *Meili) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, e := json.Marshal(in)
		if e != nil {
			return ports.ErrInvalid
		}
		body = bytes.NewReader(b)
	}
	r, e := http.NewRequestWithContext(ctx, method, m.cfg.URL+path, body)
	if e != nil {
		return e
	}
	r.Header.Set("Content-Type", "application/json")
	if m.cfg.APIKey != "" {
		r.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	}
	resp, e := m.client.Do(r)
	if e != nil {
		return &ports.DependencyError{Dependency: "meilisearch", Operation: path, Code: "transport_error", Cause: ports.ErrUnavailable}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cause := ports.ErrUnavailable
		if resp.StatusCode == 404 {
			cause = ports.ErrNotFound
		}
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			cause = ports.ErrUnauthorized
		}
		return &ports.DependencyError{Dependency: "meilisearch", Operation: path, Code: "http_error", Status: resp.StatusCode, Cause: cause}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	if e != nil {
		return ports.ErrUnavailable
	}
	if len(b) > 8<<20 {
		return ports.ErrLimit
	}
	if e = json.Unmarshal(b, out); e != nil {
		return fmt.Errorf("invalid search response: %w", ports.ErrIntegrity)
	}
	return nil
}

type taskAck struct {
	TaskUID *int64 `json:"taskUid"`
}

func (m *Meili) task(ctx context.Context, method, path string, in any) (ports.SearchTask, error) {
	var ack taskAck
	if e := m.call(ctx, method, path, in, &ack); e != nil {
		return ports.SearchTask{}, e
	}
	if ack.TaskUID == nil {
		return ports.SearchTask{}, ports.ErrIntegrity
	}
	return ports.SearchTask{UID: *ack.TaskUID}, nil
}
func (m *Meili) Configure(ctx context.Context) error {
	var existing map[string]any
	e := m.call(ctx, "GET", "/indexes/"+m.cfg.Index, nil, &existing)
	if e != nil {
		if d, ok := e.(*ports.DependencyError); !ok || d.Status != 404 {
			return e
		}
		t, e := m.task(ctx, "POST", "/indexes", map[string]string{"uid": m.cfg.Index, "primaryKey": "id"})
		if e != nil {
			return e
		}
		if e = m.Wait(ctx, t); e != nil {
			return e
		}
	}
	t, e := m.task(ctx, "PATCH", "/indexes/"+m.cfg.Index+"/settings", map[string]any{"filterableAttributes": []string{"owner_id", "file_id", "artifact_set_id", "version_id"}, "searchableAttributes": []string{"title", "headings", "content", "source_path"}, "displayedAttributes": []string{"*"}, "pagination": map[string]int{"maxTotalHits": 10000}})
	if e != nil {
		return e
	}
	return m.Wait(ctx, t)
}
func (m *Meili) Upsert(ctx context.Context, docs []ports.SearchDocument) (ports.SearchTask, error) {
	if len(docs) == 0 || len(docs) > 1000 {
		return ports.SearchTask{}, ports.ErrInvalid
	}
	for _, d := range docs {
		if !validIndex.MatchString(d.ID) || d.OwnerID == "" || d.FileID == "" || d.VersionID == "" || d.ArtifactSetID == "" || len(d.Content) > 16384 {
			return ports.SearchTask{}, ports.ErrInvalid
		}
	}
	return m.task(ctx, "POST", "/indexes/"+m.cfg.Index+"/documents", docs)
}
func (m *Meili) Wait(ctx context.Context, t ports.SearchTask) error {
	if t.UID == -1 {
		return nil
	}
	if t.UID < 0 {
		return ports.ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, m.cfg.TaskTimeout)
	defer cancel()
	for {
		var state struct {
			Status string `json:"status"`
			Error  *struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if e := m.call(ctx, "GET", fmt.Sprintf("/tasks/%d", t.UID), nil, &state); e != nil {
			return e
		}
		switch state.Status {
		case "succeeded":
			return nil
		case "failed", "canceled":
			code := "task_" + state.Status
			if state.Error != nil && state.Error.Code != "" {
				code = state.Error.Code
			}
			return &ports.DependencyError{Dependency: "meilisearch", Operation: "wait", Code: code, Cause: ports.ErrUnavailable}
		case "enqueued", "processing":
		default:
			return ports.ErrIntegrity
		}
		timer := time.NewTimer(m.cfg.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func quote(s string) string { b, _ := json.Marshal(s); return string(b) }
func (m *Meili) Query(ctx context.Context, q ports.SearchQuery) (ports.SearchCandidates, error) {
	var result ports.SearchCandidates
	if q.OwnerID == "" || q.Limit < 1 || q.Limit > 100 || q.Offset < 0 || q.Offset >= 10000 || len(q.FileIDs) > 100 || len(q.Query) > 4096 {
		return result, ports.ErrInvalid
	}
	filters := []any{"owner_id = " + quote(q.OwnerID)}
	if len(q.FileIDs) > 0 {
		ids := []string{}
		for _, id := range q.FileIDs {
			ids = append(ids, "file_id = "+quote(id))
		}
		filters = append(filters, ids)
	}
	var response struct {
		Hits []ports.SearchDocument `json:"hits"`
	}
	if e := m.call(ctx, "POST", "/indexes/"+m.cfg.Index+"/search", map[string]any{"q": q.Query, "filter": filters, "offset": q.Offset, "limit": q.Limit}, &response); e != nil {
		return result, e
	}
	for _, d := range response.Hits {
		if d.OwnerID != q.OwnerID {
			return result, ports.ErrIntegrity
		}
	}
	result.Hits = response.Hits
	result.NextOffset = q.Offset + len(response.Hits)
	result.MayHaveMore = len(response.Hits) == q.Limit && result.NextOffset < 10000
	return result, nil
}
func (m *Meili) UpdatePaths(ctx context.Context, owner string, paths map[string]string) (ports.SearchTask, error) {
	if owner == "" || len(paths) == 0 || len(paths) > 100 {
		return ports.SearchTask{}, ports.ErrInvalid
	}
	last := ports.SearchTask{UID: -1}
	for file, p := range paths {
		offset := 0
		for {
			var response struct {
				Results []struct {
					ID string `json:"id"`
				} `json:"results"`
			}
			filter := []string{"owner_id = " + quote(owner), "file_id = " + quote(file)}
			if e := m.call(ctx, "POST", "/indexes/"+m.cfg.Index+"/documents/fetch", map[string]any{"filter": filter, "fields": []string{"id"}, "offset": offset, "limit": 1000}, &response); e != nil {
				return last, e
			}
			if len(response.Results) == 0 {
				break
			}
			updates := []map[string]string{}
			for _, d := range response.Results {
				updates = append(updates, map[string]string{"id": d.ID, "source_path": p})
			}
			t, e := m.task(ctx, "PUT", "/indexes/"+m.cfg.Index+"/documents", updates)
			if e != nil {
				return last, e
			}
			if e = m.Wait(ctx, t); e != nil {
				return last, e
			}
			last = t
			offset += len(response.Results)
			if len(response.Results) < 1000 {
				break
			}
			if offset >= 100000 {
				return last, ports.ErrLimit
			}
		}
	}
	return last, nil
}
func (m *Meili) DeleteArtifactSet(ctx context.Context, owner, artifactSet string) (ports.SearchTask, error) {
	if owner == "" || artifactSet == "" {
		return ports.SearchTask{}, ports.ErrInvalid
	}
	return m.task(ctx, "POST", "/indexes/"+m.cfg.Index+"/documents/delete", map[string]any{"filter": []string{"owner_id = " + quote(owner), "artifact_set_id = " + quote(artifactSet)}})
}

var _ ports.SearchIndex = (*Meili)(nil)
