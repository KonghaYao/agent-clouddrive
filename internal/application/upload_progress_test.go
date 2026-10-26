package application

import (
	"context"
	"testing"

	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
)

func TestRecordUploadProgressPersistsEachAcceptedChunk(t *testing.T) {
	s, owner, _ := uploadFixture(t)
	ctx := context.Background()
	created, err := s.BeginUpload(ctx, owner, Object{"name": "progress.bin", "size_bytes": int64(10), "idempotency_key": "progress-chunks"})
	if err != nil {
		t.Fatal(err)
	}
	uid := str(created.(Object), "id")
	read := func() (int64, bool, *string) {
		t.Helper()
		var received int64
		var completed bool
		var native *string
		if err = s.DB.QueryRow(ctx, `SELECT received_bytes,transfer_completed,native_resource_id FROM upload_sessions WHERE id=$1`, uid).Scan(&received, &completed, &native); err != nil {
			t.Fatal(err)
		}
		return received, completed, native
	}
	epoch, err := s.AdmitUploadPatch(ctx, owner, uid, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordUploadProgress(ctx, uid, epoch, 4, ""); err != nil {
		t.Fatal(err)
	}
	if received, completed, native := read(); received != 4 || completed || native != nil {
		t.Fatalf("partial progress = %d completed=%v native=%v", received, completed, native)
	}
	// 旧 epoch、重复响应、回退和越界都必须明确冲突，不得截断为成功。
	for _, offset := range []int64{3, 4, 99} {
		if err = s.RecordUploadProgress(ctx, uid, epoch, offset, ""); domain.ErrorCode(err) != domain.OperationPending {
			t.Fatalf("late offset %d: %v", offset, err)
		}
	}
	if received, _, _ := read(); received != 4 {
		t.Fatalf("progress changed to %d", received)
	}
	epoch, err = s.AdmitUploadPatch(ctx, owner, uid, 4, 6)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordUploadProgress(ctx, uid, epoch, 10, "native-resource"); err != nil {
		t.Fatal(err)
	}
	if received, completed, native := read(); received != 10 || !completed || native == nil || *native != "native-resource" {
		t.Fatalf("completed progress = %d completed=%v native=%v", received, completed, native)
	}
}
