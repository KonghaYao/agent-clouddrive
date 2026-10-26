package application

import (
	"context"
	"errors"
	"testing"

	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
)

func conflictDetails(t *testing.T, err error) *domain.Error {
	t.Helper()
	var de *domain.Error
	if !errors.As(normalizeError(err), &de) {
		t.Fatalf("error = %v, want domain error", err)
	}
	return de
}

func TestBeginUploadReportsIncompleteOwnerUpload(t *testing.T) {
	s, owner, _ := uploadFixture(t)
	ctx := context.Background()
	first, err := s.BeginUpload(ctx, owner, Object{"name": "hidden.pdf", "size_bytes": int64(10), "idempotency_key": "conflict-first"})
	if err != nil {
		t.Fatal(err)
	}
	up := first.(Object)
	_, err = s.BeginUpload(ctx, owner, Object{"name": "hidden.pdf", "size_bytes": int64(10), "idempotency_key": "conflict-second"})
	de := conflictDetails(t, err)
	if de.Code != domain.PathConflict {
		t.Fatalf("code = %s, want %s", de.Code, domain.PathConflict)
	}
	if de.Details["reason"] != "incomplete_upload" {
		t.Fatalf("details = %v, want incomplete_upload", de.Details)
	}
	if de.Details["upload_id"] != str(up, "id") {
		t.Fatalf("upload_id = %v, want %s", de.Details["upload_id"], str(up, "id"))
	}
	if num(de.Details, "expected_size") != 10 || de.Details["expires_at"] == nil {
		t.Fatalf("incomplete upload details are incomplete: %v", de.Details)
	}
	var files, ops int
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM files WHERE path='/hidden.pdf'`).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM file_operations WHERE kind='create'`).Scan(&ops); err != nil {
		t.Fatal(err)
	}
	if files != 1 || ops != 1 {
		t.Fatalf("conflict wrote rows: files=%d operations=%d", files, ops)
	}
}

func TestBeginUploadReportsNameTakenForVisibleFile(t *testing.T) {
	s, owner, _ := uploadFixture(t)
	seedUpload(t, s, owner, "visible.txt", 3)
	_, err := s.BeginUpload(context.Background(), owner, Object{"name": "visible.txt", "size_bytes": int64(4), "idempotency_key": "name-taken"})
	de := conflictDetails(t, err)
	if de.Code != domain.PathConflict || de.Details["reason"] != "name_taken" {
		t.Fatalf("conflict = %v, want PATH_CONFLICT/name_taken", de)
	}
	if _, ok := de.Details["upload_id"]; ok {
		t.Fatalf("visible file conflict leaked upload details: %v", de.Details)
	}
}

func TestAbandonUploadReleasesQuotaAndFreesPath(t *testing.T) {
	s, owner, _ := uploadFixture(t)
	ctx := context.Background()
	created, err := s.BeginUpload(ctx, owner, Object{"name": "abandon.pdf", "size_bytes": int64(1000), "idempotency_key": "abandon-first"})
	if err != nil {
		t.Fatal(err)
	}
	up, uid, opID, fileID := created.(Object), "", "", ""
	uid, opID, fileID = str(up, "id"), str(up, "operation_id"), str(up, "file_id")
	var reserved int64
	if err = s.DB.QueryRow(ctx, `SELECT reserved_bytes FROM quota_accounts WHERE owner_id=$1`, owner.Principal.ID).Scan(&reserved); err != nil || reserved != 1000 {
		t.Fatalf("reserved before abandon = %d, %v", reserved, err)
	}
	out, err := s.AbandonUpload(ctx, owner, uid)
	if err != nil {
		t.Fatal(err)
	}
	if str(out.(Object), "status") != "abandoned" {
		t.Fatalf("abandon result = %v", out)
	}
	var opState, errorCode, sessionState, reservationState, lifecycle string
	var fileOp, currentVersion *string
	if err = s.DB.QueryRow(ctx, `SELECT state,error_code FROM file_operations WHERE id=$1`, opID).Scan(&opState, &errorCode); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT state FROM upload_sessions WHERE id=$1`, uid).Scan(&sessionState); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT state FROM quota_reservations WHERE upload_id=$1`, uid).Scan(&reservationState); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT lifecycle,active_operation_id::text,current_version_id::text FROM files WHERE id=$1`, fileID).Scan(&lifecycle, &fileOp, &currentVersion); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT reserved_bytes FROM quota_accounts WHERE owner_id=$1`, owner.Principal.ID).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if opState != "failed_confirmed" || errorCode != "abandoned_by_user" || sessionState != "failed_confirmed" || reservationState != "released" || lifecycle != "trashed" || fileOp != nil || reserved != 0 {
		t.Fatalf("abandon state: op=%s/%s session=%s reservation=%s lifecycle=%s active_op=%v reserved=%d", opState, errorCode, sessionState, reservationState, lifecycle, fileOp, reserved)
	}
	if _, err = s.BeginUpload(ctx, owner, Object{"name": "abandon.pdf", "size_bytes": int64(1000), "idempotency_key": "abandon-after"}); err != nil {
		t.Fatalf("same-name upload after abandon: %v", err)
	}
	// Repeating an abandon is safe and does not resurrect the operation or quota.
	if out, err = s.AbandonUpload(ctx, owner, uid); err != nil || str(out.(Object), "status") != "abandoned" {
		t.Fatalf("repeat abandon = %v, %v", out, err)
	}
}

func TestAbandonUploadKeepsVisibleFile(t *testing.T) {
	s, owner, _ := uploadFixture(t)
	ctx := context.Background()
	f := seedUpload(t, s, owner, "visible-overwrite.txt", 3)
	created, err := s.BeginUpload(ctx, owner, Object{"file_id": str(f, "id"), "name": "visible-overwrite.txt", "size_bytes": int64(50), "expected_version_id": str(f, "current_version_id"), "expected_metadata_revision": num(f, "metadata_revision"), "idempotency_key": "abandon-overwrite"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AbandonUpload(ctx, owner, str(created.(Object), "id")); err != nil {
		t.Fatal(err)
	}
	var lifecycle, currentVersion, activeOp string
	if err = s.DB.QueryRow(ctx, `SELECT lifecycle,current_version_id::text,coalesce(active_operation_id::text,'') FROM files WHERE id=$1`, str(f, "id")).Scan(&lifecycle, &currentVersion, &activeOp); err != nil {
		t.Fatal(err)
	}
	if lifecycle != "active" || currentVersion != str(f, "current_version_id") || activeOp != "" {
		t.Fatalf("visible file changed: lifecycle=%s version=%s active_op=%s", lifecycle, currentVersion, activeOp)
	}
}

func TestAbandonUploadRefusesAcceptedTransferData(t *testing.T) {
	cases := map[string]struct {
		bytes, completed int64
		native           string
		want             string
	}{
		"bytes received":     {bytes: 5, completed: 0, want: "bytes_received"},
		"transfer completed": {bytes: 10, completed: 1, native: "native-resource", want: "transfer_completed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, owner, _ := uploadFixture(t)
			ctx := context.Background()
			created, err := s.BeginUpload(ctx, owner, Object{"name": "refuse.pdf", "size_bytes": int64(10), "idempotency_key": "refuse-" + name})
			if err != nil {
				t.Fatal(err)
			}
			up := created.(Object)
			if _, err = s.DB.Exec(ctx, `UPDATE upload_sessions SET received_bytes=$2,transfer_completed=$3,native_resource_id=NULLIF($4,'') WHERE id=$1`, str(up, "id"), tc.bytes, tc.completed == 1, tc.native); err != nil {
				t.Fatal(err)
			}
			_, err = s.AbandonUpload(ctx, owner, str(up, "id"))
			de := conflictDetails(t, err)
			if de.Code != domain.InvalidTransition || de.Details["reason"] != tc.want {
				t.Fatalf("refusal = %v, want INVALID_TRANSITION/%s", de, tc.want)
			}
			var opState, sessionState, reservationState, lifecycle string
			if err = s.DB.QueryRow(ctx, `SELECT state FROM file_operations WHERE id=$1`, str(up, "operation_id")).Scan(&opState); err != nil {
				t.Fatal(err)
			}
			if err = s.DB.QueryRow(ctx, `SELECT state FROM upload_sessions WHERE id=$1`, str(up, "id")).Scan(&sessionState); err != nil {
				t.Fatal(err)
			}
			if err = s.DB.QueryRow(ctx, `SELECT state FROM quota_reservations WHERE upload_id=$1`, str(up, "id")).Scan(&reservationState); err != nil {
				t.Fatal(err)
			}
			if err = s.DB.QueryRow(ctx, `SELECT lifecycle FROM files WHERE id=$1`, str(up, "file_id")).Scan(&lifecycle); err != nil {
				t.Fatal(err)
			}
			if opState != "executing" || sessionState != "uploading" || reservationState != "held" || lifecycle != "active" {
				t.Fatalf("refused abandon mutated state: op=%s session=%s reservation=%s lifecycle=%s", opState, sessionState, reservationState, lifecycle)
			}
		})
	}
}

func TestAbandonUploadIsOwnerScoped(t *testing.T) {
	s, owner, _ := uploadFixture(t)
	ctx := context.Background()
	created, err := s.BeginUpload(ctx, owner, Object{"name": "private.pdf", "size_bytes": int64(10), "idempotency_key": "private"})
	if err != nil {
		t.Fatal(err)
	}
	up := created.(Object)
	other := owner
	other.Principal.ID = id()
	if _, err = s.AbandonUpload(ctx, other, str(up, "id")); domain.ErrorCode(normalizeError(err)) != domain.NotFound {
		t.Fatalf("foreign abandon = %v, want NOT_FOUND", err)
	}
	var opState, sessionState, reservationState string
	if err = s.DB.QueryRow(ctx, `SELECT state FROM file_operations WHERE id=$1`, str(up, "operation_id")).Scan(&opState); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT state FROM upload_sessions WHERE id=$1`, str(up, "id")).Scan(&sessionState); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, `SELECT state FROM quota_reservations WHERE upload_id=$1`, str(up, "id")).Scan(&reservationState); err != nil {
		t.Fatal(err)
	}
	if opState != "executing" || sessionState != "uploading" || reservationState != "held" {
		t.Fatalf("foreign abandon mutated state: op=%s session=%s reservation=%s", opState, sessionState, reservationState)
	}
}
