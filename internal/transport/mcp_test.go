package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/application"
	"github.com/agentclouddrive/agent-clouddrive/internal/config"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The official MCP SDK client exercises the production Streamable HTTP handler
// and real PostgreSQL migrations/application authorization. Only OIDC is a
// contract fixture. This is NOT a Peri-client or real OAuth-provider test.
type protocolIdentity struct{}

func (protocolIdentity) ValidateToken(_ context.Context, token string, _ ports.TokenRequirements) (ports.Subject, error) {
	if token != "protocol-fixture" {
		return ports.Subject{}, ports.ErrUnauthorized
	}
	return ports.Subject{Issuer: "https://identity.test", Subject: "protocol-owner", ClientID: "peri", Scopes: []string{"drive.read", "drive.feedback"}, ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (protocolIdentity) SubjectStatus(context.Context, string, string) (ports.SubjectStatus, error) {
	return ports.SubjectStatus{Enabled: true}, nil
}
func (protocolIdentity) Groups(context.Context, string, string) ([]string, error) { return nil, nil }
func (protocolIdentity) DelegatedCredential(context.Context, ports.DelegationRequest) (ports.Credential, error) {
	return ports.Credential{}, ports.ErrUnverified
}

type protocolTransport struct{ base http.RoundTripper }

func (t protocolTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer protocol-fixture")
	return t.base.RoundTrip(r)
}
func protocolID(t *testing.T) string {
	t.Helper()
	id, e := domain.NewID()
	if e != nil {
		t.Fatal(e)
	}
	return id
}
func protocolDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DRIVE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("DRIVE_TEST_DATABASE_URL not set; real PostgreSQL MCP integration")
	}
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := "mcp_" + strings.ReplaceAll(protocolID(t), "-", "")
	if _, e = admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); e != nil {
		admin.Close()
		t.Fatal(e)
	}
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		admin.Close()
	})
	files, e := filepath.Glob("../../db/migrations/*.up.sql")
	if e != nil {
		t.Fatal(e)
	}
	sort.Strings(files)
	for _, file := range files {
		b, e := os.ReadFile(file)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = pool.Exec(ctx, string(b)); e != nil {
			t.Fatalf("migration %s: %v", file, e)
		}
	}
	return pool
}
func TestMCPProtocolIntegration(t *testing.T) {
	pool := protocolDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	owner, space, fid, vid := protocolID(t), protocolID(t), protocolID(t), protocolID(t)
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO principals(id,issuer,subject) VALUES($1,'https://identity.test','protocol-owner')`, []any{owner}},
		{`INSERT INTO spaces(id,owner_id,native_instance,native_space_id,native_root_resource_id) VALUES($1,$2,'https://ocis.test','space','root')`, []any{space, owner}},
		{`INSERT INTO files(id,owner_id,space_id,name,path,kind,native_instance,native_resource_id) VALUES($1,$2,$3,'document.txt','/document.txt','file','https://ocis.test','native-file')`, []any{fid, owner, space}},
		{`INSERT INTO file_versions(id,file_id,version_seq,size,sha256,native_etag,native_revision_ref) VALUES($1,$2,1,3,$3,'etag','{"revision_id":"etag:etag"}')`, []any{vid, fid, strings.Repeat("a", 64)}},
		{`UPDATE files SET current_version_id=$2,size=3 WHERE id=$1`, []any{fid, vid}},
	}
	for _, q := range statements {
		if _, e := pool.Exec(ctx, q.sql, q.args...); e != nil {
			t.Fatal(e)
		}
	}
	service := &application.Service{DB: pool, Identity: protocolIdentity{}, Config: config.Config{PublicURL: "https://drive.test", MCPURL: "https://drive.test/mcp", WebClientID: "web", DefaultQuota: 1 << 30, AllowedOrigins: []string{"https://drive.test"}}}
	api := &API{Service: service}
	server := httptest.NewServer(api.MCPHandler())
	defer server.Close()
	response, e := http.Get(server.URL + "/mcp")
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if response.StatusCode != 401 || !strings.Contains(response.Header.Get("WWW-Authenticate"), "oauth-protected-resource/mcp") {
		t.Fatal("missing protected-resource challenge", response.StatusCode)
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/mcp", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://drive.test.attacker")
	req.Header.Set("Authorization", "Bearer protocol-fixture")
	response, e = http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("untrusted origin accepted")
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "protocol-selftest", Version: "1"}, nil)
	session, e := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: protocolTransport{http.DefaultTransport}}, MaxRetries: -1}, nil)
	if e != nil {
		t.Fatal("official SDK initialize", e)
	}
	defer session.Close()
	if session.InitializeResult().ProtocolVersion != "2026-07-28" {
		t.Fatalf("unexpected negotiated protocol: %s", session.InitializeResult().ProtocolVersion)
	}
	if session.InitializeResult().ServerInfo.Name != "agent-clouddrive" {
		t.Fatal("wrong initialized server")
	}
	listed, e := session.ListTools(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	names := map[string]bool{}
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	if len(names) != 21 {
		t.Fatalf("got %d tools", len(names))
	}
	for _, forbidden := range []string{"drive_approve_share", "drive_purge", "drive_shared_search", "drive_admin"} {
		if names[forbidden] {
			t.Fatal("forbidden tool", forbidden)
		}
	}
	call := func(name string, args map[string]any, wantError bool) *mcp.CallToolResult {
		t.Helper()
		out, e := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if e != nil {
			t.Fatal(name, e)
		}
		if out.IsError != wantError {
			t.Fatalf("%s: error=%v content=%v", name, out.IsError, out.Content)
		}
		return out
	}
	call("drive_stat", map[string]any{"file_id": fid}, false)
	call("drive_stat", map[string]any{"file_id": protocolID(t)}, true)
	call("drive_stat", map[string]any{}, true)
	call("drive_mkdir", map[string]any{"name": "denied", "idempotency_key": "denied"}, true)
	if _, e = session.CallTool(ctx, &mcp.CallToolParams{Name: "drive_approve_share", Arguments: map[string]any{}}); e == nil {
		t.Fatal("unknown approval tool did not produce a protocol error")
	}
	snapshot := func() string {
		t.Helper()
		var value string
		e := pool.QueryRow(ctx, `SELECT jsonb_build_object('principals',(SELECT jsonb_agg(to_jsonb(p)) FROM principals p),'quota',(SELECT jsonb_agg(to_jsonb(q)) FROM quota_accounts q),'outbox',(SELECT jsonb_agg(to_jsonb(o)) FROM outbox o))::text`).Scan(&value)
		if e != nil {
			t.Fatal(e)
		}
		return value
	}
	before := snapshot()
	marker := "protocol privacy fixture paragraph missing"
	result := call("drive_submit_feedback", map[string]any{"file_id": fid, "version_id": vid, "category": "text", "body": marker}, false)
	if before != snapshot() {
		t.Fatal("anonymous feedback changed identifiable principal/quota/outbox activity")
	}
	var body string
	var row []byte
	if e = pool.QueryRow(ctx, `SELECT body,to_jsonb(f) FROM feedback f`).Scan(&body, &row); e != nil || body != marker {
		t.Fatal("feedback not stored", e)
	}
	var fields map[string]any
	if e = json.Unmarshal(row, &fields); e != nil {
		t.Fatal(e)
	}
	for _, key := range []string{"principal_id", "owner_id", "subject", "ip", "user_agent", "request_id", "trace_id", "session_id", "created_at"} {
		if _, ok := fields[key]; ok {
			t.Fatal("identifying feedback field", key)
		}
	}
	encoded, _ := json.Marshal(result)
	for _, secret := range []string{"protocol-fixture", "protocol-owner", marker} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("feedback result echoed private request data")
		}
	}
	t.Log(fmt.Sprintf("official SDK initialized; %d tools; scoped reads, missing-resource errors, no approval tool, feedback identity-write exclusion passed", len(names)))
}
