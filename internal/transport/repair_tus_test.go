package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/application"
	"github.com/agentclouddrive/agent-clouddrive/internal/config"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

type repairTusIdentity struct{ protocolIdentity }

func (repairTusIdentity) ValidateToken(context.Context, string, ports.TokenRequirements) (ports.Subject, error) {
	return ports.Subject{Issuer: "repair-tus", Subject: "owner", ClientID: "test", Scopes: []string{"drive.write"}, ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (repairTusIdentity) DelegatedCredential(context.Context, ports.DelegationRequest) (ports.Credential, error) {
	return ports.Credential{}, nil
}

type repairTusStore struct {
	ports.OriginalStore
	entered        chan struct{}
	release        chan struct{}
	s              *application.Service
	uid            string
	admissionError chan error
}

func (u *repairTusStore) CreateUpload(_ context.Context, in ports.UploadRequest) (ports.UploadInfo, error) {
	return ports.UploadInfo{NativeURL: "http://fixture.invalid/tus", OperationID: in.OperationID, Length: in.Size}, nil
}
func (u *repairTusStore) StreamUpload(ctx context.Context, in ports.UploadInfo, offset int64, body io.Reader, length int64, _ ports.Credential) (ports.UploadStatus, error) {
	var persisted bool
	e := u.s.DB.QueryRow(ctx, `SELECT u.in_flight_patches=1 AND EXISTS(SELECT 1 FROM upload_patch_attempts a WHERE a.upload_id=u.id AND a.state='dispatched') FROM upload_sessions u WHERE id=$1`, u.uid).Scan(&persisted)
	if e == nil && !persisted {
		e = fmt.Errorf("远端派发前无持久 attempt")
	}
	u.admissionError <- e
	close(u.entered)
	select {
	case <-u.release:
	case <-ctx.Done():
		return ports.UploadStatus{}, ctx.Err()
	}
	if body == nil {
		return ports.UploadStatus{}, fmt.Errorf("missing request stream")
	}
	n, e := io.Copy(io.Discard, body)
	if e != nil || n != length {
		return ports.UploadStatus{}, fmt.Errorf("stream length mismatch")
	}
	return ports.UploadStatus{Offset: offset + n, Length: in.Length, Complete: offset+n == in.Length, ResourceID: "fixture-resource"}, nil
}

// 实际监听本机 TCP 的生产 Hertz handler；原生存储和身份显式为替身。
func TestRepairTusLiveHTTPDispatchBeforeAbandon(t *testing.T) {
	pool := protocolDB(t)
	ctx := context.Background()
	pid, space := protocolID(t), protocolID(t)
	if _, e := pool.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES($1,'repair-tus','owner')`, pid); e != nil {
		t.Fatal(e)
	}
	if _, e := pool.Exec(ctx, `INSERT INTO spaces(id,owner_id,native_instance,native_space_id,native_root_resource_id) VALUES($1,$2,'http://fixture.invalid','space','root')`, space, pid); e != nil {
		t.Fatal(e)
	}
	if _, e := pool.Exec(ctx, `INSERT INTO quota_accounts(owner_id,limit_bytes) VALUES($1,100000)`, pid); e != nil {
		t.Fatal(e)
	}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	address := listener.Addr().String()
	if e = listener.Close(); e != nil {
		t.Fatal(e)
	}
	s := &application.Service{DB: pool, Identity: repairTusIdentity{}, Config: config.Config{Listen: address, UploadMax: 1024, NodeID: "repair-tus", WebClientID: "test"}}
	u := &repairTusStore{s: s, entered: make(chan struct{}), release: make(chan struct{}), admissionError: make(chan error, 1)}
	s.Original = u
	actor := application.Actor{Principal: domain.Principal{ID: pid, Type: domain.ActorUser, Active: true, StatusVerified: true, Scopes: domain.AllScopes()}, Subject: ports.Subject{Issuer: "repair-tus", Subject: "owner"}, Token: "fixture-user-token"}
	out, e := s.BeginUpload(ctx, actor, application.Object{"name": "live.bin", "size_bytes": int64(7), "idempotency_key": "live"})
	if e != nil {
		t.Fatal(e)
	}
	up := out.(application.Object)
	u.uid = up["id"].(string)
	h := (&API{Service: s}).Server()
	serverDone := make(chan error, 1)
	go func() { serverDone <- h.Run() }()
	defer func() {
		shutdown, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if e := h.Shutdown(shutdown); e != nil {
			t.Errorf("server shutdown: %v", e)
		}
		select {
		case e := <-serverDone:
			if e != nil {
				t.Errorf("server exit: %v", e)
			}
		case <-time.After(time.Second):
			t.Error("server did not exit after completed shutdown")
		}
	}()
	transport := &http.Transport{DisableKeepAlives: true}
	client := &http.Client{Timeout: 10 * time.Second, Transport: transport}
	defer transport.CloseIdleConnections()
	base := "http://" + address
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("server readiness failed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	request, _ := http.NewRequest("PATCH", base+"/v1/tus/"+u.uid, strings.NewReader("1234567"))
	request.Header.Set("Authorization", "Bearer fixture")
	request.Header.Set("Tus-Resumable", "1.0.0")
	request.Header.Set("Upload-Offset", "0")
	request.Header.Set("Content-Type", "application/offset+octet-stream")
	patchDone := make(chan error, 1)
	go func() {
		resp, err := client.Do(request)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode != 204 || resp.Header.Get("Upload-Offset") != "7" {
				err = fmt.Errorf("PATCH status=%d offset=%s", resp.StatusCode, resp.Header.Get("Upload-Offset"))
			}
		}
		patchDone <- err
	}()
	var once sync.Once
	defer once.Do(func() { close(u.release) })
	select {
	case <-u.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("PATCH not dispatched")
	}
	if e = <-u.admissionError; e != nil {
		t.Fatal(e)
	}
	if _, e = s.AbandonUpload(ctx, actor, u.uid); domain.ErrorCode(e) != domain.OperationPending {
		t.Fatalf("abandon=%v", e)
	}
	once.Do(func() { close(u.release) })
	if e = <-patchDone; e != nil {
		t.Fatal(e)
	}
	var complete bool
	var attempts int
	if e = pool.QueryRow(ctx, `SELECT transfer_completed,(SELECT count(*) FROM upload_patch_attempts WHERE upload_id=$1 AND state='succeeded') FROM upload_sessions WHERE id=$1`, u.uid).Scan(&complete, &attempts); e != nil || !complete || attempts != 1 {
		t.Fatalf("completion=%v attempts=%d err=%v", complete, attempts, e)
	}
	t.Log("真实本机 TCP → Hertz tus → 原生 PG 派发证据 → 挂起远端替身 → abandon 拒绝 → PATCH 204 → 完成证据落盘；非真实 oCIS")
}
