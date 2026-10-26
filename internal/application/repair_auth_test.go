package application

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	delegationadapter "github.com/agentclouddrive/agent-clouddrive/internal/adapters/delegation"
	identityadapter "github.com/agentclouddrive/agent-clouddrive/internal/adapters/identity"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

type repairAuditIdentity struct {
	mutationIdentity
	owner    ports.Subject
	disabled string
}

func (i *repairAuditIdentity) ValidateToken(context.Context, string, ports.TokenRequirements) (ports.Subject, error) {
	return i.owner, nil
}
func (i *repairAuditIdentity) SubjectStatus(_ context.Context, _, subject string) (ports.SubjectStatus, error) {
	return ports.SubjectStatus{Enabled: subject != i.disabled}, nil
}

type repairAuditOriginal struct {
	*mutationStore
	ref ports.RevisionRef
}

func (o *repairAuditOriginal) OpenRevision(_ context.Context, ref ports.RevisionRef, _ *ports.ByteRange, _ ports.Credential) (ports.Content, error) {
	if ref != o.ref {
		return ports.Content{}, ports.ErrIntegrity
	}
	return ports.Content{Body: io.NopCloser(strings.NewReader("retained-v1")), ETag: ref.ETag, Size: 11}, nil
}

func TestRepairAuthRetainedOriginalRealVault(t *testing.T) {
	s, owner, fid, vid, newer := reviewHistoricalSource(t)
	ctx := context.Background()
	// 不用静态测试凭据；仅测试进程内随机值。
	refreshValue, accessValue, secret := id(), id(), id()
	refresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": accessValue, "refresh_token": refreshValue, "token_type": "Bearer"})
	}))
	defer refresh.Close()
	base := &repairAuditIdentity{owner: owner.Subject}
	vault, e := delegationadapter.New(delegationadapter.Config{Base: base, DB: s.DB, Issuer: owner.Subject.Issuer, TokenURL: refresh.URL, ClientID: "background", ClientSecret: secret, Key: bytes.Repeat([]byte{7}, 32), Verified: true})
	if e != nil {
		t.Fatal(e)
	}
	s.Identity = vault
	if e = vault.StoreGrant(ctx, owner.Subject, refreshValue, []string{"offline_access", "drive.read"}); e != nil {
		t.Fatal(e)
	}
	auditor := owner
	auditor.Principal.ID = id()
	auditor.Subject.Subject = "auditor"
	if _, e = s.DB.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES($1,$2,$3)`, auditor.Principal.ID, owner.Subject.Issuer, auditor.Subject.Subject); e != nil {
		t.Fatal(e)
	}
	var grantID string
	if e = s.DB.QueryRow(ctx, `INSERT INTO audit_grants(principal_id,file_id,granted_by) VALUES($1,$2,$3) RETURNING id::text`, auditor.Principal.ID, fid, owner.Principal.ID).Scan(&grantID); e != nil {
		t.Fatal(e)
	}
	var raw []byte
	if e = s.DB.QueryRow(ctx, `SELECT native_revision_ref FROM file_versions WHERE id=$1`, vid).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var ref ports.RevisionRef
	if e = json.Unmarshal(raw, &ref); e != nil {
		t.Fatal(e)
	}
	s.Original = &repairAuditOriginal{ref: ref}
	read := func() {
		t.Helper()
		c, err := s.AuditContent(ctx, auditor, fid, vid, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(c.Body)
		c.Body.Close()
		if err != nil || string(b) != "retained-v1" || c.ETag != ref.ETag {
			t.Fatal("固定字节/ETag 不正确", err)
		}
	}
	read()
	if _, e = s.DB.Exec(ctx, `UPDATE files SET lifecycle='trashed' WHERE id=$1`, fid); e != nil {
		t.Fatal(e)
	}
	read()
	// 移回 active，仅父目录处于 trash，审核仍应成功。
	if _, e = s.DB.Exec(ctx, `UPDATE files SET lifecycle='active' WHERE id=$1`, fid); e != nil {
		t.Fatal(e)
	}
	var parent string
	if e = s.DB.QueryRow(ctx, `INSERT INTO files(owner_id,space_id,name,path,kind,native_instance,native_resource_id) SELECT owner_id,space_id,'parent','/parent','directory',native_instance,'parent' FROM files WHERE id=$1 RETURNING id::text`, fid).Scan(&parent); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(ctx, `UPDATE files SET parent_id=$2,path='/parent/file' WHERE id=$1`, fid, parent); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec(ctx, `UPDATE files SET lifecycle='trashed' WHERE id=$1`, parent); e != nil {
		t.Fatal(e)
	}
	read()
	cap := ports.AuditCapability{AuditorPrincipalID: auditor.Principal.ID, GrantID: grantID, FileID: fid, VersionID: vid, Purpose: "audit_original"}
	request := ports.DelegationRequest{Subject: owner.Subject, Audience: "ocis", Source: ref, Audit: &cap}
	reject := func(in ports.DelegationRequest) {
		t.Helper()
		if c, err := vault.DelegatedCredential(ctx, in); err == nil {
			_ = c
			t.Fatal("无效能力获得原生凭据")
		}
	}
	ordinary := request
	ordinary.Audit = nil
	reject(ordinary)
	fake := request
	fake.JobID = id()
	reject(fake)
	bad := cap
	bad.FileID = id()
	fake = request
	fake.Audit = &bad
	reject(fake)
	bad = cap
	bad.VersionID = id()
	fake.Audit = &bad
	reject(fake)
	fake = request
	fake.Source.ETag = "other-version"
	reject(fake)
	bad = cap
	bad.GrantID = id()
	fake = request
	fake.Audit = &bad
	reject(fake)
	fake = request
	fake.SubjectToken = id()
	reject(fake)
	agent := auditor
	agent.Principal.Type = domain.ActorAgent
	if _, e = s.AuditContent(ctx, agent, fid, vid, nil); e == nil {
		t.Fatal("Agent 获得审核能力")
	}
	base.disabled = auditor.Subject.Subject
	reject(request)
	base.disabled = ""
	base.disabled = owner.Subject.Subject
	reject(request)
	base.disabled = ""
	if _, e = s.DB.Exec(ctx, `UPDATE audit_grants SET expires_at=now()-interval '1 second' WHERE id=$1`, grantID); e != nil {
		t.Fatal(e)
	}
	reject(request)
	if _, e = s.DB.Exec(ctx, `DELETE FROM audit_grants WHERE id=$1`, grantID); e != nil {
		t.Fatal(e)
	}
	reject(request)
	if _, e = s.DB.Exec(ctx, `INSERT INTO audit_grants(principal_id,file_id,granted_by) VALUES($1,$2,$3)`, auditor.Principal.ID, fid, owner.Principal.ID); e != nil {
		t.Fatal(e)
	}
	reject(request) // 同一文件新授权不能让旧 capability 复活。
	if e = vault.Revoke(ctx, owner.Subject); e != nil {
		t.Fatal(e)
	}
	if _, e = s.AuditContent(ctx, auditor, fid, newer, nil); e == nil {
		t.Fatal("owner 离线授权撤销未关闭")
	}
	var reads int
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM audit_reads WHERE file_id=$1 AND read_kind='original'`, fid).Scan(&reads); e != nil || reads != 3 {
		t.Fatal("审核日志丢失", reads, e)
	}
	t.Log("真实 PG + 真实 vault：trash/ancestor-trash 固定字节和 ETag、Agent、跨对象、撤销/过期、离线授权边界通过；原生字节/IdP 为 fixture")
}

type repairEventIdentity struct {
	mutationIdentity
	mu      sync.Mutex
	events  []ports.IdentityEvent
	gap     bool
	failure error
}

func (i *repairEventIdentity) Groups(context.Context, string, string) ([]string, error) {
	return []string{"group"}, nil
}

func (i *repairEventIdentity) IdentityEvents(_ context.Context, _, anchor string) (ports.IdentityEventBatch, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.failure != nil {
		return ports.IdentityEventBatch{}, i.failure
	}
	out := ports.IdentityEventBatch{Anchor: "empty", Continuous: anchor == "empty"}
	if len(i.events) > 0 {
		out.Anchor = i.events[0].ID
	}
	for _, event := range i.events {
		if event.ID == anchor {
			out.Continuous = true
			break
		}
		out.Events = append(out.Events, event)
	}
	if i.gap {
		out.Continuous = false
	}
	return out, nil
}
func repairApprove(t *testing.T, s *Service, a Actor, fid, kind, target string) Object {
	t.Helper()
	r, e := s.RequestShare(context.Background(), a, Object{"file_id": fid, "audience_type": kind, "audience_id": target, "rights": []string{map[bool]string{true: "preview", false: "read"}[kind == "link"]}, "version_policy": "latest"})
	if e != nil {
		t.Fatal(e)
	}
	g, e := s.ApproveShare(context.Background(), a, str(r.(Object), "id"))
	if e != nil {
		t.Fatal(e)
	}
	return g.(Object)
}
func TestRepairAuthIdentityEpochShortDisableReplay(t *testing.T) {
	s, owner, fid := shareLifecycleFixture(t)
	ctx := context.Background()
	source := &repairEventIdentity{}
	s.Identity = source
	reader := owner
	reader.Principal.ID = id()
	reader.Subject.Subject = "reader"
	grants := []Object{repairApprove(t, s, owner, fid, "link", ""), repairApprove(t, s, owner, fid, "user", reader.Principal.ID), repairApprove(t, s, owner, fid, "group", "group")}
	raw := strings.TrimPrefix(str(grants[0], "url"), strings.TrimRight(s.Config.PublicURL, "/")+"/s/")
	if _, _, e := s.linkFile(ctx, raw, ""); e != nil {
		t.Fatal(e)
	}
	if _, _, e := s.SharedFile(ctx, reader, fid, "", domain.ActionRead); e != nil {
		t.Fatal("有效内部分享失败", e)
	}
	// enabled 从未变成 false 的访问采样；只在日志留下 disable → enable。
	now := time.Now().UTC()
	source.events = []ports.IdentityEvent{{ID: id(), Subject: owner.Subject.Subject, Time: now}, {ID: id(), Subject: owner.Subject.Subject, Disable: true, Time: now.Add(-time.Millisecond)}}
	var workers sync.WaitGroup
	errs := make(chan error, 6)
	for n := 0; n < 6; n++ {
		workers.Add(1)
		go func() { defer workers.Done(); errs <- s.syncIdentityEvents(ctx) }()
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if e := s.syncPrincipalStatuses(ctx); e != nil {
		t.Fatal(e)
	}
	if _, _, e := s.linkFile(ctx, raw, ""); e == nil {
		t.Fatal("旧公开分享复活")
	}
	if _, _, e := s.SharedFile(ctx, reader, fid, "", domain.ActionRead); e == nil {
		t.Fatal("旧内部分享复活")
	}
	for _, grant := range grants {
		var state string
		var valid bool
		if e := s.DB.QueryRow(ctx, `SELECT state,share_identity_valid(id) FROM share_grants WHERE id=$1`, str(grant, "id")).Scan(&state, &valid); e != nil || state != "revoked" || valid {
			t.Fatal("旧公开/用户/组分享未撤销", state, valid, e)
		}
	}
	var epoch int64
	if e := s.DB.QueryRow(ctx, `SELECT identity_epoch FROM principals WHERE id=$1`, owner.Principal.ID).Scan(&epoch); e != nil {
		t.Fatal(e)
	}
	fresh := repairApprove(t, s, owner, fid, "link", "")
	newRaw := strings.TrimPrefix(str(fresh, "url"), strings.TrimRight(s.Config.PublicURL, "/")+"/s/")
	if _, _, e := s.linkFile(ctx, newRaw, ""); e != nil {
		t.Fatal("新确认分享失败", e)
	}
	// 重启新 Service、重复/乱序事件：去重只依赖数据库。
	s = &Service{DB: s.DB, Config: s.Config, Identity: source}
	source.events = []ports.IdentityEvent{source.events[1], source.events[0]}
	if e := s.syncIdentityEvents(ctx); e != nil {
		t.Fatal(e)
	}
	var after int64
	if e := s.DB.QueryRow(ctx, `SELECT identity_epoch FROM principals WHERE id=$1`, owner.Principal.ID).Scan(&after); e != nil || after != epoch {
		t.Fatal("重复事件改变 epoch", after, epoch, e)
	}
	if _, _, e := s.linkFile(ctx, newRaw, ""); e != nil {
		t.Fatal(e)
	}
	// 缺失游标：不能猜测停机期间没有停用，永久关闭现有分享。
	if _, e := s.DB.Exec(ctx, `DELETE FROM identity_event_cursors`); e != nil {
		t.Fatal(e)
	}
	if _, _, e := s.linkFile(ctx, newRaw, ""); e == nil {
		t.Fatal("游标缺口仍可读旧分享")
	}
	var count int
	if e := s.DB.QueryRow(ctx, `SELECT count(*) FROM identity_events`).Scan(&count); e != nil || count != 2 {
		t.Fatal("事件未持久去重", count, e)
	}
	// 权威事件接口不可用时，拒绝分享并持久关闭现有授权；恢复不能复活。
	fresh = repairApprove(t, s, owner, fid, "link", "")
	newRaw = strings.TrimPrefix(str(fresh, "url"), strings.TrimRight(s.Config.PublicURL, "/")+"/s/")
	source.failure = ports.ErrUnavailable
	if _, _, e := s.linkFile(ctx, newRaw, ""); e == nil {
		t.Fatal("事件接口故障未关闭")
	}
	source.failure = nil
	if _, _, e := s.linkFile(ctx, newRaw, ""); e == nil {
		t.Fatal("事件接口恢复复活旧分享")
	}
	t.Log("真实 PG：零访问短暂停用撤销公开/用户/组分享，新确认、事件重投乱序、并发、重启、接口故障和游标缺失通过")
}

// 由隔离真实 Keycloak runner 设置；不回显配置或令牌。
func TestRepairIdentityLiveKeycloak(t *testing.T) {
	issuer := os.Getenv("REPAIR_KEYCLOAK_ISSUER")
	if issuer == "" {
		t.Skip("需要隔离真实 Keycloak runner")
	}
	s, owner, fid := shareLifecycleFixture(t)
	ctx := context.Background()
	subject := os.Getenv("REPAIR_KEYCLOAK_SUBJECT")
	if _, e := s.DB.Exec(ctx, `UPDATE principals SET issuer=$2,subject=$3 WHERE id=$1`, owner.Principal.ID, issuer, subject); e != nil {
		t.Fatal(e)
	}
	owner.Subject = ports.Subject{Issuer: issuer, Subject: subject}
	k, e := identityadapter.NewKeycloak(identityadapter.Config{Issuer: issuer, Audience: "drive", ServiceClientID: os.Getenv("REPAIR_KEYCLOAK_CLIENT"), ServiceClientSecret: os.Getenv("REPAIR_KEYCLOAK_SECRET")})
	if e != nil {
		t.Fatal(e)
	}
	s.Identity = k
	grants := []Object{repairApprove(t, s, owner, fid, "link", ""), repairApprove(t, s, owner, fid, "user", id()), repairApprove(t, s, owner, fid, "group", "test-group")}
	// 管理 token 仅存在本测试进程内，不打印响应体。
	admin := os.Getenv("REPAIR_KEYCLOAK_ADMIN_TOKEN")
	change := func(enabled bool) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"enabled": enabled})
		req, _ := http.NewRequest("PUT", os.Getenv("REPAIR_KEYCLOAK_ADMIN_URL")+"/users/"+subject, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+admin)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal("管理员状态请求失败")
		}
		resp.Body.Close()
		if resp.StatusCode != 204 {
			t.Fatalf("管理员状态请求 HTTP %d", resp.StatusCode)
		}
	}
	change(false)
	change(true) // 中间没有云盘请求/轮询。
	if e = s.syncPrincipalStatuses(ctx); e != nil {
		t.Fatal(e)
	}
	status, e := k.SubjectStatus(ctx, issuer, subject)
	if e != nil || !status.Enabled {
		t.Fatal("重新启用状态不正确", e)
	}
	for _, g := range grants {
		var state string
		if e = s.DB.QueryRow(ctx, `SELECT state FROM share_grants WHERE id=$1`, str(g, "id")).Scan(&state); e != nil || state != "revoked" {
			t.Fatal("真实事件未撤销旧分享", state, e)
		}
	}
	raw := strings.TrimPrefix(str(grants[0], "url"), strings.TrimRight(s.Config.PublicURL, "/")+"/s/")
	if _, _, e = s.linkFile(ctx, raw, ""); e == nil {
		t.Fatal("真实停用事件后旧链接可用")
	}
	fresh := repairApprove(t, s, owner, fid, "link", "")
	raw = strings.TrimPrefix(str(fresh, "url"), strings.TrimRight(s.Config.PublicURL, "/")+"/s/")
	if _, _, e = s.linkFile(ctx, raw, ""); e != nil {
		t.Fatal("真实事件后新确认分享失败", e)
	}
	// 真实 IdP 日志清空不能保留旧分享；空日志期间也不接受新确认。
	req, _ := http.NewRequest("DELETE", os.Getenv("REPAIR_KEYCLOAK_ADMIN_URL")+"/admin-events", nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal("清理隔离 realm 事件失败")
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatal("清理隔离 realm 事件 HTTP", resp.StatusCode)
	}
	if _, _, e = s.linkFile(ctx, raw, ""); e == nil {
		t.Fatal("真实日志清空未关闭旧分享")
	}
	change(true) // 建立新保留锚点，仍不能复活旧分享。
	if _, _, e = s.linkFile(ctx, raw, ""); e == nil {
		t.Fatal("真实日志恢复复活旧分享")
	}
	repairApprove(t, s, owner, fid, "link", "")
	t.Log("真实 Keycloak Admin Events + 原生 PG + application：两次采样之间 disabled→enabled、零访问、旧三类分享撤销、新确认、日志清空/恢复通过")
}
