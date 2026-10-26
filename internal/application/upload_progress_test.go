package application

import (
	"context"
	"testing"
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
	if err = s.RecordUploadProgress(ctx, uid, 4, ""); err != nil {
		t.Fatal(err)
	}
	if received, completed, native := read(); received != 4 || completed || native != nil {
		t.Fatalf("partial progress = %d completed=%v native=%v", received, completed, native)
	}
	// A late/duplicate chunk with a smaller offset must never move progress backwards.
	if err = s.RecordUploadProgress(ctx, uid, 3, ""); err != nil {
		t.Fatal(err)
	}
	if received, _, _ := read(); received != 4 {
		t.Fatalf("progress regressed to %d", received)
	}
	if err = s.RecordUploadProgress(ctx, uid, 10, "native-resource"); err != nil {
		t.Fatal(err)
	}
	if received, completed, native := read(); received != 10 || !completed || native == nil || *native != "native-resource" {
		t.Fatalf("completed progress = %d completed=%v native=%v", received, completed, native)
	}
	// Even an impossible native offset must not violate received_bytes <= expected_size.
	if err = s.RecordUploadProgress(ctx, uid, 99, ""); err != nil {
		t.Fatalf("oversized offset: %v", err)
	}
	if received, completed, _ := read(); received != 10 || !completed {
		t.Fatalf("oversized progress = %d completed=%v", received, completed)
	}
}
