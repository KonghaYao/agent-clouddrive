package converters

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExecuteContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/execute" {
			t.Errorf("path %s", r.URL.Path)
		}
		var v map[string]any
		_ = json.NewDecoder(r.Body).Decode(&v)
		if v["output_scope"] != "tmp/j/a/" {
			t.Errorf("scope %v", v)
		}
		_ = json.NewEncoder(w).Encode(ports.ConversionResult{Status: "ready", Engine: ports.EngineInfo{Name: "text", Version: "1"}, Markdown: "hello", Completeness: ports.Completeness{Status: "ready", TotalPages: 1}})
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL})
	r, e := c.Execute(context.Background(), ports.ConversionRequest{JobID: "j", AttemptID: "a", OutputScope: "tmp/j/a/", Input: ports.ConversionInput{SourceVersionID: "v"}, Recipe: ports.Recipe{MaxPages: 2, TimeoutSeconds: 1}})
	if e != nil || r.Markdown != "hello" {
		t.Fatalf("%+v %v", r, e)
	}
}
func TestRejectExternalURLAndFalseSuccess(t *testing.T) {
	c, _ := New(Config{BaseURL: "http://internal"})
	_, e := c.Execute(context.Background(), ports.ConversionRequest{JobID: "j", AttemptID: "a", OutputScope: "tmp/j/a/", Input: ports.ConversionInput{URL: "http://169.254.169.254/latest/meta-data", SourceVersionID: "v"}, Recipe: ports.Recipe{MaxPages: 2, TimeoutSeconds: 1}})
	if !errors.Is(e, ports.ErrInvalid) {
		t.Fatal(e)
	}
	e = ValidateResult(ports.ConversionResult{Status: "ready", Engine: ports.EngineInfo{Name: "ocr", Version: "1"}, Markdown: "text", Completeness: ports.Completeness{Status: "ready", TotalPages: 2, MissingTextPages: []int{2}}}, 3, 100)
	if !errors.Is(e, ports.ErrIntegrity) {
		t.Fatal(e)
	}
}
