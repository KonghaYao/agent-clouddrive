package application

import (
	"context"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"testing"
)

type blockedAdmissionIdentity struct {
	mutationIdentity
	entered, release chan struct{}
}

func (b blockedAdmissionIdentity) DelegatedCredential(ctx context.Context, r ports.DelegationRequest) (ports.Credential, error) {
	close(b.entered)
	select {
	case <-b.release:
		return b.mutationIdentity.DelegatedCredential(ctx, r)
	case <-ctx.Done():
		return ports.Credential{}, ctx.Err()
	}
}
func TestPreparedAdmissionRecoveryFencesLateNativeDispatch(t *testing.T) {
	s, a, native := uploadFixture(t)
	ctx := context.Background()
	b := blockedAdmissionIdentity{entered: make(chan struct{}), release: make(chan struct{})}
	s.Identity = b
	done := make(chan error, 1)
	go func() {
		_, e := s.BeginUpload(ctx, a, Object{"name": "unstarted.txt", "size_bytes": int64(21), "idempotency_key": "unstarted"})
		done <- e
	}()
	<-b.entered
	if _, e := s.DB.Exec(ctx, `UPDATE file_operations SET updated_at=now()-interval '11 minutes' WHERE idempotency_key='unstarted'`); e != nil {
		t.Fatal(e)
	}
	if e := s.expireUnstartedUploads(ctx); e != nil {
		t.Fatal(e)
	}
	close(b.release)
	if e := <-done; e == nil {
		t.Fatal("expired admission dispatched native upload")
	}
	if native.calls != 0 {
		t.Fatal("late native dispatch bypassed prepared CAS")
	}
	var state string
	var reserved int64
	if e := s.DB.QueryRow(ctx, `SELECT o.state,q.reserved_bytes FROM file_operations o JOIN quota_accounts q ON q.owner_id=o.actor_principal_id WHERE o.idempotency_key='unstarted'`).Scan(&state, &reserved); e != nil {
		t.Fatal(e)
	}
	if state != "failed_confirmed" || reserved != 0 {
		t.Fatal(state, reserved)
	}
	s.Identity = mutationIdentity{}
	if _, e := s.BeginUpload(ctx, a, Object{"name": "unstarted.txt", "size_bytes": int64(21), "idempotency_key": "started"}); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.Exec(ctx, `UPDATE file_operations SET updated_at=now()-interval '1 day' WHERE idempotency_key='started'`); e != nil {
		t.Fatal(e)
	}
	if e := s.expireUnstartedUploads(ctx); e != nil {
		t.Fatal(e)
	}
	if e := s.DB.QueryRow(ctx, `SELECT o.state,q.reserved_bytes FROM file_operations o JOIN quota_accounts q ON q.owner_id=o.actor_principal_id WHERE o.idempotency_key='started'`).Scan(&state, &reserved); e != nil {
		t.Fatal(e)
	}
	if state != "executing" || reserved != 21 {
		t.Fatal("age alone released potentially dispatched write", state, reserved)
	}
}
