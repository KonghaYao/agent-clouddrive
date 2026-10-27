package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/application"
	"github.com/agentclouddrive/agent-clouddrive/internal/config"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// B07 protocol integration: the official MCP SDK client drives the production
// Streamable HTTP handler and real PostgreSQL. Only OIDC and native oCIS
// directory creation are fixtures; the cursor-independent mkdir contract and
// optimistic parent revision checks are real.
type repairMCPIdentity struct{}

func (repairMCPIdentity) ValidateToken(_ context.Context, token string, _ ports.TokenRequirements) (ports.Subject, error) {
	if token != "repair-mcp-fixture" {
		return ports.Subject{}, ports.ErrUnauthorized
	}
	return ports.Subject{Issuer: "https://identity.test", Subject: "repair-mcp-owner", ClientID: "peri", Scopes: []string{"drive.read", "drive.write", "drive.delete", "drive.restore", "drive.convert", "drive.share.request", "drive.feedback"}, ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (repairMCPIdentity) SubjectStatus(context.Context, string, string) (ports.SubjectStatus, error) {
	return ports.SubjectStatus{Enabled: true}, nil
}
func (repairMCPIdentity) Groups(context.Context, string, string) ([]string, error) { return nil, nil }
func (repairMCPIdentity) DelegatedCredential(context.Context, ports.DelegationRequest) (ports.Credential, error) {
	return ports.Credential{AccessToken: "repair-native-token", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

type repairMCPNative struct {
	mu    sync.Mutex
	count int
	dirs  map[string]ports.OriginalMetadata
}

func (n *repairMCPNative) CreateDirectory(_ context.Context, r ports.DirectoryRequest) (ports.OriginalMetadata, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.count++
	meta := ports.OriginalMetadata{ResourceRef: ports.ResourceRef{Instance: r.Parent.Instance, SpaceID: r.Parent.SpaceID, ResourceID: "native-dir-" + strconv.Itoa(n.count)}, RelativePath: r.Name, Name: r.Name, ETag: "etag", IsDirectory: true}
	n.dirs[meta.ResourceID] = meta
	return meta, nil
}
func (n *repairMCPNative) Stat(_ context.Context, r ports.ResourceRef, _ ports.Credential) (ports.OriginalMetadata, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if v, ok := n.dirs[r.ResourceID]; ok {
		return v, nil
	}
	return ports.OriginalMetadata{}, ports.ErrNotFound
}
func (n *repairMCPNative) CreateUpload(context.Context, ports.UploadRequest) (ports.UploadInfo, error) {
	return ports.UploadInfo{}, ports.ErrUnverified
}
func (n *repairMCPNative) StatUpload(context.Context, ports.UploadInfo, ports.Credential) (ports.UploadStatus, error) {
	return ports.UploadStatus{}, ports.ErrUnverified
}
func (n *repairMCPNative) OpenRevision(context.Context, ports.RevisionRef, *ports.ByteRange, ports.Credential) (ports.Content, error) {
	return ports.Content{}, ports.ErrUnverified
}
func (n *repairMCPNative) Revisions(context.Context, ports.ResourceRef, ports.Credential) ([]ports.NativeRevision, error) {
	return nil, nil
}
func (n *repairMCPNative) Move(context.Context, ports.MoveRequest) (ports.OriginalMetadata, error) {
	return ports.OriginalMetadata{}, ports.ErrUnverified
}
func (n *repairMCPNative) Reconcile(context.Context, ports.ReconcileRequest) (ports.ReconcileResult, error) {
	return ports.ReconcileResult{}, ports.ErrUnverified
}

type repairMCPTransport struct{ base http.RoundTripper }

func (t repairMCPTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer repair-mcp-fixture")
	return t.base.RoundTrip(r)
}

func repairMCPResult(t *testing.T, out *mcp.CallToolResult) application.Object {
	t.Helper()
	if len(out.Content) != 1 {
		t.Fatalf("unexpected content: %v", out.Content)
	}
	text, ok := out.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected content type: %T", out.Content[0])
	}
	var value application.Object
	if e := json.Unmarshal([]byte(text.Text), &value); e != nil {
		t.Fatalf("decode tool result %q: %v", text.Text, e)
	}
	return value
}

func repairMCPErrorCode(t *testing.T, out *mcp.CallToolResult) (string, string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	text := out.Content[0].(*mcp.TextContent).Text
	if e := json.Unmarshal([]byte(text), &body); e != nil {
		t.Fatalf("decode tool error %q: %v", text, e)
	}
	return body.Error.Code, body.Error.Message
}

func TestRepairMCPMkdirNestedContract(t *testing.T) {
	pool := protocolDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner, space := protocolID(t), protocolID(t)
	if _, e := pool.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES($1,'https://identity.test','repair-mcp-owner')`, owner); e != nil {
		t.Fatal(e)
	}
	if _, e := pool.Exec(ctx, `INSERT INTO spaces(id,owner_id,native_instance,native_space_id,native_root_resource_id) VALUES($1,$2,'https://ocis.test','native-space','native-root')`, space, owner); e != nil {
		t.Fatal(e)
	}
	native := &repairMCPNative{dirs: map[string]ports.OriginalMetadata{}}
	service := &application.Service{DB: pool, Identity: repairMCPIdentity{}, Original: native, Config: config.Config{PublicURL: "https://drive.test", MCPURL: "https://drive.test/mcp", WebClientID: "web", OCISAudience: "ocis", DefaultQuota: 1 << 30, AllowedOrigins: []string{"https://drive.test"}}}
	server := httptest.NewServer((&API{Service: service}).MCPHandler())
	defer server.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "repair-mkdir", Version: "1"}, nil)
	session, e := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: repairMCPTransport{http.DefaultTransport}}, MaxRetries: -1}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()

	listed, e := session.ListTools(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	var mkdirSchema map[string]any
	for _, tool := range listed.Tools {
		if tool.Name == "drive_mkdir" {
			b, _ := json.Marshal(tool.InputSchema)
			_ = json.Unmarshal(b, &mkdirSchema)
		}
	}
	properties, _ := mkdirSchema["properties"].(map[string]any)
	revisionSchema, _ := properties["expected_parent_revision"].(map[string]any)
	if revisionSchema["type"] != "integer" || revisionSchema["minimum"] != float64(1) {
		t.Fatalf("drive_mkdir does not advertise expected_parent_revision: %v", properties["expected_parent_revision"])
	}

	call := func(args map[string]any) *mcp.CallToolResult {
		t.Helper()
		out, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "drive_mkdir", Arguments: args})
		if e != nil {
			t.Fatal(e)
		}
		return out
	}

	rootOut := call(map[string]any{"name": "projects", "idempotency_key": "root-projects"})
	if rootOut.IsError {
		t.Fatalf("root mkdir failed: %v", repairMCPResult(t, rootOut))
	}
	root := repairMCPResult(t, rootOut)
	if repairStr(root, "state") != "committed" || repairStr(root, "file_id") == "" {
		t.Fatalf("root mkdir not committed: %v", root)
	}
	rootID := repairStr(root, "file_id")
	statOut, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "drive_stat", Arguments: map[string]any{"file_id": rootID}})
	if e != nil || statOut.IsError {
		t.Fatalf("stat after mkdir: %v %v", e, statOut)
	}
	statted := repairMCPResult(t, statOut)
	revision := int64(repairNum(statted, "metadata_revision"))
	if revision < 1 {
		t.Fatalf("stat returned revision %d", revision)
	}

	childOut := call(map[string]any{"parent_id": rootID, "name": "reports", "expected_parent_revision": revision, "idempotency_key": "child-reports"})
	if childOut.IsError {
		t.Fatalf("nested mkdir failed: %v", repairMCPResult(t, childOut))
	}
	child := repairMCPResult(t, childOut)
	if repairStr(child, "state") != "committed" || repairStr(child, "file_id") == "" {
		t.Fatalf("nested mkdir not committed: %v", child)
	}
	listOut, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "drive_list", Arguments: map[string]any{"parent_id": rootID}})
	if e != nil || listOut.IsError {
		t.Fatalf("list after nested mkdir: %v %v", e, listOut)
	}
	listedChildren := repairMCPResult(t, listOut)
	items, _ := listedChildren["items"].([]any)
	found := false
	for _, item := range items {
		if m, ok := item.(map[string]any); ok && m["name"] == "reports" {
			found = true
		}
	}
	if !found {
		t.Fatalf("nested directory missing from list: %v", items)
	}

	missing := call(map[string]any{"parent_id": rootID, "name": "missing", "idempotency_key": "missing-revision"})
	if !missing.IsError {
		t.Fatal("non-root mkdir without expected_parent_revision succeeded")
	}
	code, message := repairMCPErrorCode(t, missing)
	if code != string(domain.InvalidArgument) || !strings.Contains(message, "expected_parent_revision") {
		t.Fatalf("missing revision error %s: %s", code, message)
	}

	stale := call(map[string]any{"parent_id": rootID, "name": "stale", "expected_parent_revision": revision + 5, "idempotency_key": "stale-revision"})
	if !stale.IsError {
		t.Fatal("stale parent revision succeeded")
	}
	code, message = repairMCPErrorCode(t, stale)
	if code != string(domain.VersionConflict) {
		t.Fatalf("stale revision error %s: %s", code, message)
	}

	// JSON-schema validation may reject this before the tool handler; both a
	// protocol error and a structured tool error are acceptable, but success is
	// not.
	if wrong, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "drive_mkdir", Arguments: map[string]any{"parent_id": rootID, "name": "wrong-type", "expected_parent_revision": "two", "idempotency_key": "wrong-type"}}); err == nil && !wrong.IsError {
		t.Fatal("stringly typed expected_parent_revision succeeded")
	}

	// The root path remains valid without a fabricated revision.
	secondRoot := call(map[string]any{"name": "archive", "idempotency_key": "root-archive"})
	if secondRoot.IsError || repairStr(repairMCPResult(t, secondRoot), "state") != "committed" {
		t.Fatalf("second root mkdir failed: %v", repairMCPResult(t, secondRoot))
	}
	t.Logf("official SDK root mkdir -> stat(revision=%d) -> nested mkdir -> list closed loop; missing and stale revisions rejected", revision)
}
