package application

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/dbgen"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

func shareLifecycleFixture(t *testing.T) (*Service, Actor, string) {
	t.Helper()
	s, a, job, _, _ := workerFixture(t)
	a.Interactive = true
	var fid string
	if e := s.DB.QueryRow(context.Background(), `SELECT file_id::text FROM file_versions WHERE id=$1`, job.SourceVersionID).Scan(&fid); e != nil {
		t.Fatal(e)
	}
	return s, a, fid
}
func pendingLink(t *testing.T, s *Service, a Actor, fid string) string {
	t.Helper()
	result, e := s.RequestShare(context.Background(), a, Object{"file_id": fid, "audience_type": "link", "version_policy": "latest", "rights": []string{"preview"}})
	if e != nil {
		t.Fatal(e)
	}
	return str(result.(map[string]any), "id")
}
func TestSharePendingDeleteRestoreRequiresFreshConfirmation(t *testing.T) {
	s, a, fid := shareLifecycleFixture(t)
	ctx := context.Background()
	rid := pendingLink(t, s, a, fid)
	for _, state := range []string{"deleting", "trashed", "active"} {
		if _, e := s.DB.Exec(ctx, `UPDATE files SET lifecycle=$2 WHERE id=$1`, fid, state); e != nil {
			t.Fatal(e)
		}
	}
	row, e := dbgen.New(s.DB).GetShareApproval(ctx, pgUUID(rid))
	if e != nil || row.State != "rejected" {
		t.Fatal("stale approval not invalidated", row.State, e)
	}
	if _, e = s.ApproveShare(ctx, a, rid); e == nil {
		t.Fatal("restored file accepted a pre-delete approval")
	}
	fresh := pendingLink(t, s, a, fid)
	result, e := s.ApproveShare(ctx, a, fresh)
	if e != nil {
		t.Fatal("fresh approval failed", e)
	}
	grant := result.(map[string]any)
	if _, e = s.DB.Exec(ctx, `UPDATE files SET lifecycle='deleting' WHERE id=$1`, fid); e != nil {
		t.Fatal(e)
	}
	var state string
	if e = s.DB.QueryRow(ctx, `SELECT state FROM share_grants WHERE id=$1`, str(grant, "id")).Scan(&state); e != nil || state != "disabled" {
		t.Fatal("grant remained active", state, e)
	}
	_, e = s.DB.Exec(ctx, `UPDATE share_grants SET state='active' WHERE id=$1`, str(grant, "id"))
	if e == nil || !strings.Contains(e.Error(), "SHARE_RECONFIRMATION_REQUIRED") {
		t.Fatal("old grant revived", e)
	}
}
func TestSharePendingAccountReenableRequiresFreshConfirmation(t *testing.T) {
	s, a, fid := shareLifecycleFixture(t)
	ctx := context.Background()
	rid := pendingLink(t, s, a, fid)
	if _, e := s.DB.Exec(ctx, `UPDATE principals SET status='disabled' WHERE id=$1`, a.Principal.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.Exec(ctx, `UPDATE principals SET status='active' WHERE id=$1`, a.Principal.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ApproveShare(ctx, a, rid); e == nil {
		t.Fatal("re-enabled account accepted stale confirmation")
	}
	var state string
	if e := s.DB.QueryRow(ctx, `SELECT state FROM share_approvals WHERE id=$1`, rid).Scan(&state); e != nil || state != "rejected" {
		t.Fatal(state, e)
	}
}
func TestShareApprovalExpiryCannotExpand(t *testing.T) {
	s, a, fid := shareLifecycleFixture(t)
	ctx := context.Background()
	expiry := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	result, e := s.RequestShare(ctx, a, Object{"file_id": fid, "audience_type": "user", "audience_id": id(), "rights": []string{"read"}, "version_policy": "latest", "expires_at": expiry.Format(time.RFC3339Nano)})
	if e != nil {
		t.Fatal(e)
	}
	rid := str(result.(map[string]any), "id")
	if _, e = s.DB.Exec(ctx, `UPDATE share_approvals SET state='approved',approved_by=$2,decided_at=now() WHERE id=$1`, rid, a.Principal.ID); e != nil {
		t.Fatal(e)
	}
	_, e = s.DB.Exec(ctx, `INSERT INTO share_grants(approval_id,file_id,creator_principal_id,audience_type,audience_id,rights,version_policy,approved_by,expires_at) SELECT id,file_id,$2,'user',request_body->>'audience_id',ARRAY['read'],'latest',$2,NULL FROM share_approvals WHERE id=$1`, rid, a.Principal.ID)
	if e == nil || !strings.Contains(e.Error(), "APPROVAL_EXPIRY_MISMATCH") {
		t.Fatal("expiry was silently expanded", e)
	}
}
func TestShareApprovalConcurrentDeleteNeverLeavesActiveGrant(t *testing.T) {
	s, a, fid := shareLifecycleFixture(t)
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		if _, e := s.DB.Exec(ctx, `UPDATE files SET lifecycle='active' WHERE id=$1`, fid); e != nil {
			t.Fatal(e)
		}
		rid := pendingLink(t, s, a, fid)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		results := make(chan error, 2)
		go func() { defer wg.Done(); <-start; _, e := s.ApproveShare(ctx, a, rid); results <- e }()
		go func() {
			defer wg.Done()
			<-start
			tx, e := s.DB.Begin(ctx)
			if e != nil {
				results <- e
				return
			}
			defer tx.Rollback(ctx)
			_, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(space_id::text,0)) FROM files WHERE id=$1`, fid)
			if e == nil {
				_, e = tx.Exec(ctx, `UPDATE files SET lifecycle='deleting' WHERE id=$1`, fid)
			}
			if e == nil {
				e = tx.Commit(ctx)
			}
			results <- e
		}()
		close(start)
		wg.Wait()
		close(results)
		for e := range results {
			if e != nil && domain.ErrorCode(e) != domain.ConfirmationExpired && domain.ErrorCode(e) != domain.NotFound && domain.ErrorCode(e) != domain.AccessDenied {
				t.Fatalf("unexpected race result %d: %v", i, e)
			}
		}
		var active int
		if e := s.DB.QueryRow(ctx, `SELECT count(*) FROM share_grants WHERE file_id=$1 AND state='active'`, fid).Scan(&active); e != nil || active != 0 {
			t.Fatalf("active grant after delete round %d: %d %v", i, active, e)
		}
		var pending int
		if e := s.DB.QueryRow(ctx, `SELECT count(*) FROM share_approvals WHERE id=$1 AND state='pending'`, rid).Scan(&pending); e != nil || pending != 0 {
			t.Fatal(fmt.Sprint("pending confirmation survived deletion: ", pending, e))
		}
	}
}

type syncIdentity struct {
	mutationIdentity
	enabled bool
}

func (i syncIdentity) SubjectStatus(context.Context, string, string) (ports.SubjectStatus, error) {
	return ports.SubjectStatus{Enabled: i.enabled}, nil
}
func TestMaintenanceIdentitySyncDoesNotReviveShares(t *testing.T) {
	s, a, fid := shareLifecycleFixture(t)
	ctx := context.Background()
	rid := pendingLink(t, s, a, fid)
	grant, e := s.ApproveShare(ctx, a, rid)
	if e != nil {
		t.Fatal(e)
	}
	gid := str(grant.(map[string]any), "id")
	pending := pendingLink(t, s, a, fid)
	s.Identity = syncIdentity{enabled: false}
	if _, e = s.DB.Exec(ctx, `UPDATE principals SET updated_at=now()-interval '2 minutes' WHERE id=$1`, a.Principal.ID); e != nil {
		t.Fatal(e)
	}
	if e = s.syncPrincipalStatuses(ctx); e != nil {
		t.Fatal(e)
	}
	var status, reason, grantState, pendingState string
	if e = s.DB.QueryRow(ctx, `SELECT status,disabled_reason FROM principals WHERE id=$1`, a.Principal.ID).Scan(&status, &reason); e != nil || status != "disabled" || reason != "identity" {
		t.Fatal(status, reason, e)
	}
	if e = s.DB.QueryRow(ctx, `SELECT state FROM share_grants WHERE id=$1`, gid).Scan(&grantState); e != nil || grantState != "disabled" {
		t.Fatal(grantState, e)
	}
	if e = s.DB.QueryRow(ctx, `SELECT state FROM share_approvals WHERE id=$1`, pending).Scan(&pendingState); e != nil || pendingState != "rejected" {
		t.Fatal(pendingState, e)
	}
	s.Identity = syncIdentity{enabled: true}
	if _, e = s.DB.Exec(ctx, `UPDATE principals SET updated_at=now()-interval '2 minutes' WHERE id=$1`, a.Principal.ID); e != nil {
		t.Fatal(e)
	}
	if e = s.syncPrincipalStatuses(ctx); e != nil {
		t.Fatal(e)
	}
	if e = s.DB.QueryRow(ctx, `SELECT status FROM principals WHERE id=$1`, a.Principal.ID).Scan(&status); e != nil || status != "active" {
		t.Fatal(status, e)
	}
	if e = s.DB.QueryRow(ctx, `SELECT state FROM share_grants WHERE id=$1`, gid).Scan(&grantState); e != nil || grantState != "disabled" {
		t.Fatal("share revived", grantState, e)
	}
	// An explicit local/admin disable is not undone by an enabled IdP account.
	if _, e = s.DB.Exec(ctx, `UPDATE principals SET status='disabled',disabled_reason='admin',updated_at=now()-interval '2 minutes' WHERE id=$1`, a.Principal.ID); e != nil {
		t.Fatal(e)
	}
	if e = s.syncPrincipalStatuses(ctx); e != nil {
		t.Fatal(e)
	}
	if e = s.DB.QueryRow(ctx, `SELECT status FROM principals WHERE id=$1`, a.Principal.ID).Scan(&status); e != nil || status != "disabled" {
		t.Fatal("local block bypassed", status, e)
	}
}

func TestShareApprovalConcurrentAccountDisableNeverLeavesActiveGrant(t *testing.T) {
	s, a, fid := shareLifecycleFixture(t)
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		if _, e := s.DB.Exec(ctx, `UPDATE principals SET status='active',disabled_reason=NULL WHERE id=$1`, a.Principal.ID); e != nil {
			t.Fatal(e)
		}
		rid := pendingLink(t, s, a, fid)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		results := make(chan error, 2)
		go func() { defer wg.Done(); <-start; _, e := s.ApproveShare(ctx, a, rid); results <- e }()
		go func() {
			defer wg.Done()
			<-start
			_, e := s.DB.Exec(ctx, `UPDATE principals SET status='disabled',disabled_reason='identity' WHERE id=$1`, a.Principal.ID)
			results <- e
		}()
		close(start)
		wg.Wait()
		close(results)
		for e := range results {
			if e != nil && domain.ErrorCode(e) != domain.ConfirmationExpired && domain.ErrorCode(e) != domain.NotFound && domain.ErrorCode(e) != domain.AccessDenied {
				t.Fatalf("unexpected account-disable race round %d: %v", i, e)
			}
		}
		var active int
		if e := s.DB.QueryRow(ctx, `SELECT count(*) FROM share_grants WHERE file_id=$1 AND state='active'`, fid).Scan(&active); e != nil || active != 0 {
			t.Fatalf("active grant after account disable %d: %d %v", i, active, e)
		}
	}
}
