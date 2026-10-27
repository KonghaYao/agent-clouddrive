package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/adapters/artifactstore"
	"github.com/agentclouddrive/agent-clouddrive/internal/application"
	"github.com/agentclouddrive/agent-clouddrive/internal/config"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

type repairPreviewIdentity struct{}

func (repairPreviewIdentity) subject(token string) (ports.Subject, bool) {
	switch token {
	case "owner-token":
		return ports.Subject{Issuer: "test", Subject: "repair-http-owner", ClientID: "web", Scopes: []string{"drive.read", "drive.write", "drive.delete", "drive.restore", "drive.convert", "drive.share.request", "drive.feedback"}, ExpiresAt: time.Now().Add(time.Hour)}, true
	case "receiver-token":
		return ports.Subject{Issuer: "test", Subject: "repair-http-receiver", ClientID: "web", Scopes: []string{"drive.read"}, ExpiresAt: time.Now().Add(time.Hour)}, true
	}
	return ports.Subject{}, false
}
func (i repairPreviewIdentity) ValidateToken(_ context.Context, token string, _ ports.TokenRequirements) (ports.Subject, error) {
	if s, ok := i.subject(token); ok {
		return s, nil
	}
	return ports.Subject{}, ports.ErrUnauthorized
}
func (repairPreviewIdentity) SubjectStatus(context.Context, string, string) (ports.SubjectStatus, error) {
	return ports.SubjectStatus{Enabled: true}, nil
}
func (repairPreviewIdentity) Groups(context.Context, string, string) ([]string, error) {
	return nil, nil
}
func (repairPreviewIdentity) DelegatedCredential(context.Context, ports.DelegationRequest) (ports.Credential, error) {
	return ports.Credential{}, ports.ErrUnverified
}

// IdentityEvents makes the preview fixture a trusted event source so the
// share-resolution path can prove continuity instead of failing closed.
func (repairPreviewIdentity) IdentityEvents(context.Context, string, string) (ports.IdentityEventBatch, error) {
	return ports.IdentityEventBatch{Continuous: true, Anchor: "repair-http-preview-anchor"}, nil
}

func repairHTTPDigest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// B15 HTTP wiring: GET /v1/preview/{id} and /pages/{page} must both carry the
// requested version_id. Two fixed historical grants can be previewed
// independently and the manifest/asset stay on that version.
func TestRepairPreviewHTTPVersionSelection(t *testing.T) {
	pool := protocolDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner, receiver, space, fid, old, newer := protocolID(t), protocolID(t), protocolID(t), protocolID(t), protocolID(t), protocolID(t)
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO principals(id,issuer,subject) VALUES($1,'test','repair-http-owner')`, []any{owner}},
		{`INSERT INTO principals(id,issuer,subject) VALUES($1,'test','repair-http-receiver')`, []any{receiver}},
		{`INSERT INTO spaces(id,owner_id,native_instance,native_space_id,native_root_resource_id) VALUES($1,$2,'http://ocis.test','native-space','root')`, []any{space, owner}},
		{`INSERT INTO files(id,owner_id,space_id,name,path,kind,native_instance,native_resource_id) VALUES($1,$2,$3,'doc.pdf','/doc.pdf','file','http://ocis.test','native-file')`, []any{fid, owner, space}},
		{`INSERT INTO file_versions(id,file_id,version_seq,size,sha256,content_type,native_etag,native_revision_ref) VALUES($1,$2,1,3,$3,'application/pdf','etag-old','{"revision_id":"etag:etag-old"}')`, []any{old, fid, strings.Repeat("a", 64)}},
		{`INSERT INTO file_versions(id,file_id,version_seq,size,sha256,content_type,native_etag,native_revision_ref) VALUES($1,$2,2,3,$3,'application/pdf','etag-new','{"revision_id":"etag:etag-new"}')`, []any{newer, fid, strings.Repeat("b", 64)}},
		{`UPDATE files SET current_version_id=$2,size=3 WHERE id=$1`, []any{fid, newer}},
	}
	for _, q := range statements {
		if _, e := pool.Exec(ctx, q.sql, q.args...); e != nil {
			t.Fatalf("%s: %v", q.sql, e)
		}
	}
	store, e := artifactstore.NewLocal(t.TempDir(), 64<<20)
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	service := &application.Service{DB: pool, Identity: repairPreviewIdentity{}, Artifacts: store, Config: config.Config{PublicURL: "https://drive.test", MCPURL: "https://drive.test/mcp", WebClientID: "web", DefaultQuota: 1 << 30, AllowedOrigins: []string{"https://drive.test"}, PipelineVersion: "repair-http-preview/1", ConvertPages: 10}}

	ownerActor := application.Actor{Principal: domain.Principal{ID: owner, Type: domain.ActorUser, Active: true, StatusVerified: true, Scopes: domain.AllScopes()}, Subject: ports.Subject{Issuer: "test", Subject: "repair-http-owner"}, Interactive: true}
	for _, vid := range []string{old, newer} {
		request, e := service.RequestShare(ctx, ownerActor, application.Object{"file_id": fid, "audience_type": "user", "audience_id": receiver, "version_policy": "fixed", "version_id": vid, "rights": []string{"read"}})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = service.ApproveShare(ctx, ownerActor, repairStr(request.(application.Object), "id")); e != nil {
			t.Fatal(e)
		}
	}
	repairHTTPPublishWithService(t, service, old, "raster-"+old)
	repairHTTPPublishWithService(t, service, newer, "raster-"+newer)

	addr := repairFreeAddr(t)
	service.Config.Listen = addr
	srv := (&API{Service: service}).Server()
	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run() }()
	defer func() {
		shutdown, stop := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer stop()
		_ = srv.Shutdown(shutdown)
		select {
		case <-runErr:
		case <-time.After(time.Second):
		}
	}()
	base := "http://" + addr
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := http.Get(base + "/healthz")
		if err == nil && response.StatusCode == 200 {
			response.Body.Close()
			break
		}
		if response != nil {
			response.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("Hertz server did not become ready: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	get := func(path, token string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode, body
	}
	for _, vid := range []string{old, newer} {
		status, body := get(fmt.Sprintf("/v1/preview/%s?version_id=%s", fid, vid), "receiver-token")
		if status != 200 {
			t.Fatalf("preview info %s status=%d body=%s", vid, status, body)
		}
		var manifest application.PreviewManifest
		if e = json.Unmarshal(body, &manifest); e != nil {
			t.Fatal(e)
		}
		if manifest.VersionID != vid {
			t.Fatalf("HTTP preview requested %s returned %s", vid, manifest.VersionID)
		}
		status, page := get(fmt.Sprintf("/v1/preview/%s/pages/1?version_id=%s", fid, vid), "receiver-token")
		if status != 200 || string(page) != "raster-"+vid {
			t.Fatalf("HTTP page %s status=%d body=%q", vid, status, page)
		}
	}
	status, body := get("/v1/preview/"+fid+"?version_id="+old, "receiver-token")
	if status != 200 {
		t.Fatalf("default version denied: %d %s", status, body)
	}
	if status, _ = get("/v1/preview/"+fid+"?version_id="+protocolID(t), "receiver-token"); status != 404 {
		t.Fatalf("unauthorized version returned %d", status)
	}
	if status, _ = get("/v1/preview/"+fid, "receiver-token"); status != 200 {
		t.Fatalf("default preview status=%d", status)
	}
	t.Log("HTTP preview info and page both selected the requested historical version")
}

func repairHTTPPublishWithService(t *testing.T, s *application.Service, vid, page string) {
	t.Helper()
	// previewPrefix is deterministic from public config; reproduce it locally.
	prefix := "previews/" + vid + "/" + previewPrefixHash(s.Config.PipelineVersion, s.Config.ConvertPages)[:16]
	ctx := context.Background()
	pageKey := prefix + "/page-1.png"
	put := func(tmp, key string, body []byte, media string) {
		digest := repairHTTPDigest(string(body))
		if _, e := s.Artifacts.PutAttempt(ctx, ports.ArtifactWrite{Key: tmp, Body: strings.NewReader(string(body)), Size: int64(len(body)), SHA256: digest, ContentType: media}); e != nil {
			t.Fatal(e)
		}
		if _, e := s.Artifacts.Publish(ctx, tmp, key, digest); e != nil {
			t.Fatal(e)
		}
	}
	put("tmp/repair-http/"+vid+"/page-1.png", pageKey, []byte(page), "image/png")
	manifest, _ := json.Marshal(application.PreviewManifest{VersionID: vid, Pages: []application.PreviewPage{{Page: 1, Key: pageKey}}, Completeness: ports.Completeness{Status: "ready", TotalPages: 1, MissingTextPages: []int{}, MissingDescriptionPages: []int{}}})
	put("tmp/repair-http/"+vid+"/manifest.json", prefix+"/manifest.json", manifest, "application/json")
}

func previewPrefixHash(pipeline string, pages int) string {
	body, _ := json.Marshal(map[string]any{"pipeline": pipeline, "max_pages": pages, "format": "raster-png-v1"})
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

func repairFreeAddr(t *testing.T) string {
	t.Helper()
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	addr := listener.Addr().String()
	listener.Close()
	return addr
}
