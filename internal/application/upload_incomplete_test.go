package application

import (
	"context"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"testing"
)

// The native boundary positively proves an incomplete transfer, not an unknown
// remote commit. PostgreSQL must retain the live resumable upload operation.
type incompleteUploadStore struct{ *uploadStore }

func (s incompleteUploadStore) Reconcile(context.Context, ports.ReconcileRequest) (ports.ReconcileResult, error) {
	return ports.ReconcileResult{State: "outcome_unknown", Reason: "transfer_incomplete"}, nil
}
func TestEarlyFinalizePreservesResumableUpload(t *testing.T) {
	s, owner, original := uploadFixture(t)
	s.Original = incompleteUploadStore{original}
	ctx := context.Background()
	created, e := s.BeginUpload(ctx, owner, Object{"name": "resumable.txt", "size_bytes": int64(5), "idempotency_key": "early-finalize"})
	if e != nil {
		t.Fatal(e)
	}
	upload := created.(Object)
	result, e := s.FinishUpload(ctx, owner, str(upload, "id"))
	if e != nil {
		t.Fatal(e)
	}
	if str(result.(Object), "status") != "uploading" {
		t.Fatal("incomplete upload was incorrectly marked unknown", result)
	}
	if _, _, e = s.UploadNative(ctx, owner, str(upload, "id")); e != nil {
		t.Fatal("early finalize broke resumability", e)
	}
	var op string
	var versions int
	if e = s.DB.QueryRow(ctx, `SELECT state FROM file_operations WHERE id=$1`, str(upload, "operation_id")).Scan(&op); e != nil || op != "executing" {
		t.Fatal(op, e)
	}
	if e = s.DB.QueryRow(ctx, `SELECT count(*) FROM file_versions WHERE file_id=$1`, str(upload, "file_id")).Scan(&versions); e != nil || versions != 0 {
		t.Fatal("early commit happened", versions, e)
	}
	s.Original = original
	if _, e = s.FinishUpload(ctx, owner, str(upload, "id")); e != nil {
		t.Fatal("later completed upload could not commit", e)
	}
}
