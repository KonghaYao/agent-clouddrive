package transport

import (
	"encoding/json"
	"github.com/agentclouddrive/agent-clouddrive/internal/application"
	"github.com/agentclouddrive/agent-clouddrive/internal/config"
	"testing"
)

func TestSingleRange(t *testing.T) {
	for _, bad := range []string{"bytes=-10", "bytes=5-4", "bytes=0-1,4-5", "bytes=-1-2", "bytes=a-"} {
		if _, e := parseRange(bad); e == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	r, e := parseRange("bytes=4-12")
	if e != nil || r.Start != 4 || r.End != 12 {
		t.Fatal(r, e)
	}
}
func TestNoForbiddenMCPTools(t *testing.T) {
	names := map[string]bool{}
	for _, d := range ToolDefinitions {
		if names[d.Name] {
			t.Fatal("duplicate tool")
		}
		names[d.Name] = true
		if _, e := json.Marshal(ToolSchema(d)); e != nil {
			t.Fatal(e)
		}
	}
	for _, name := range []string{"drive_approve_share", "drive_purge", "drive_shared_search", "drive_admin"} {
		if names[name] {
			t.Fatal("unsafe tool", name)
		}
	}
	if !names["drive_submit_feedback"] || len(names) != 21 {
		t.Fatal("incomplete tool surface", len(names))
	}
}
func TestExactOrigin(t *testing.T) {
	a := API{Service: &application.Service{Config: config.Config{AllowedOrigins: []string{"https://drive.example"}}}}
	if !a.origin("https://drive.example") || a.origin("https://drive.example.evil") || a.origin("") {
		t.Fatal("origin policy")
	}
}
