package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

func TestRepairIdentityReplayPaginationAndGaps(t *testing.T) {
	enabled, details := true, true
	events := []map[string]any{}
	for n := 0; n < 102; n++ {
		events = append(events, map[string]any{"id": strconv.Itoa(n), "time": int64(1000 + n), "resourceType": "USER", "resourcePath": "users/owner", "operationType": "UPDATE", "representation": `{"enabled":true}`})
	}
	// 同一毫秒的乱序事件也必须遍历，不能在遇到 anchor 时提前漏掉停用。
	events[101]["time"] = int64(1050)
	events[101]["representation"] = `{"enabled":false}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/realms/test/protocol/openid-connect/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": r.FormValue("client_secret"), "token_type": "Bearer"})
		case "/admin/realms/test/events/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"adminEventsEnabled": enabled, "adminEventsDetailsEnabled": details})
		case "/admin/realms/test/admin-events":
			first, _ := strconv.Atoi(r.URL.Query().Get("first"))
			end := first + 100
			if end > len(events) {
				end = len(events)
			}
			if first > end {
				first = end
			}
			_ = json.NewEncoder(w).Encode(events[first:end])
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	k, e := NewKeycloak(Config{Issuer: server.URL + "/realms/test", Audience: "drive", ServiceClientID: "identity", ServiceClientSecret: server.URL})
	if e != nil {
		t.Fatal(e)
	}
	batch, e := k.IdentityEvents(context.Background(), k.cfg.Issuer, "50")
	if e != nil || !batch.Continuous || len(batch.Events) != 102 || !batch.Events[101].Disable {
		t.Fatal("分页/乱序漏事件", len(batch.Events), e)
	}
	batch, e = k.IdentityEvents(context.Background(), k.cfg.Issuer, "missing")
	if e != nil || batch.Continuous {
		t.Fatal("缺失锚点未报告缺口", e)
	}
	enabled = false
	if _, e = k.IdentityEvents(context.Background(), k.cfg.Issuer, "50"); !errors.Is(e, ports.ErrUnverified) {
		t.Fatal("事件关闭未拒绝", e)
	}
	enabled = true
	details = false
	if _, e = k.IdentityEvents(context.Background(), k.cfg.Issuer, "50"); !errors.Is(e, ports.ErrUnverified) {
		t.Fatal("无 representation 未拒绝", e)
	}
	details = true
	if _, e = k.IdentityEvents(context.Background(), "https://foreign.test/realm", "50"); !errors.Is(e, ports.ErrUnauthorized) {
		t.Fatal("跨 issuer 未拒绝", e)
	}
	events = nil
	if _, e = k.IdentityEvents(context.Background(), k.cfg.Issuer, "missing"); !errors.Is(e, ports.ErrUnverified) {
		t.Fatal("空日志无可靠锚点却允许新分享", e)
	}
	events = []map[string]any{{"id": "", "time": int64(1000)}}
	events[0]["id"] = ""
	if _, e = k.IdentityEvents(context.Background(), k.cfg.Issuer, "50"); !errors.Is(e, ports.ErrIntegrity) {
		t.Fatal("事件无 ID 未拒绝", e)
	}
}
