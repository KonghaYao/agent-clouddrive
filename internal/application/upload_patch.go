package application

import (
	"context"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

func uploadPending(reason string) error {
	e := domain.NewError(domain.OperationPending, "upload write outcome requires reconciliation")
	e.Details = map[string]any{"reason": reason, "retryable": true, "retry_after_seconds": 30}
	return e
}

// AdmitUploadPatch 在远端派发前落盘；与 abandon 共用 operation/upload 行锁。
func (s *Service) AdmitUploadPatch(ctx context.Context, a Actor, uid string, offset, length int64) (int64, error) {
	if _, _, e := s.UploadNative(ctx, a, uid); e != nil {
		return 0, e
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return 0, e
	}
	defer tx.Rollback(ctx)
	o, e := one(ctx, tx, `SELECT to_jsonb(o) FROM file_operations o JOIN upload_sessions u ON u.operation_id=o.id WHERE u.id=$1 AND o.actor_principal_id=$2 FOR UPDATE OF o`, uid, a.Principal.ID)
	if e != nil {
		return 0, e
	}
	u, e := one(ctx, tx, `SELECT to_jsonb(u)||jsonb_build_object('leased',o.lease_until>now()) FROM upload_sessions u JOIN file_operations o ON o.id=u.operation_id WHERE u.id=$1 FOR UPDATE OF u`, uid)
	if e != nil {
		return 0, e
	}
	if str(o, "state") != "executing" || str(u, "state") != "uploading" || u["transfer_completed"] == true || u["legacy_dispatch_unknown"] == true || num(u, "in_flight_patches") != 0 || u["leased"] == true {
		return 0, uploadPending("patch_or_finalize_pending")
	}
	if offset != num(u, "received_bytes") || offset > num(u, "expected_size") || length < 0 || length > num(u, "expected_size")-offset {
		return 0, domain.NewError(domain.VersionConflict, "PATCH offset or length conflicts with persisted progress")
	}
	epoch := num(u, "patch_epoch") + 1
	if _, e = tx.Exec(ctx, `INSERT INTO upload_patch_attempts(upload_id,epoch,state,offset_start,offset_end) VALUES($1,$2,'dispatched',$3,$4)`, uid, epoch, offset, offset+length); e != nil {
		return 0, e
	}
	if _, e = tx.Exec(ctx, `UPDATE upload_sessions SET patch_epoch=$2,in_flight_patches=1,updated_at=now() WHERE id=$1`, uid, epoch); e != nil {
		return 0, e
	}
	return epoch, tx.Commit(ctx)
}

// CompleteUploadPatch 不使用已取消的 HTTP context，响应丢失仍保留未决证据。
func (s *Service) CompleteUploadPatch(ctx context.Context, uid string, epoch int64, st ports.UploadStatus, remoteErr error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if remoteErr == nil && (st.Offset < 0 || st.Length < 0 || st.Offset > st.Length || st.Complete != (st.Offset == st.Length)) {
		remoteErr = ports.ErrIntegrity
	}
	if remoteErr != nil {
		_, e := s.DB.Exec(ctx, `UPDATE upload_patch_attempts SET state='outcome_unknown',error_code='patch_response_unknown' WHERE upload_id=$1 AND epoch=$2 AND state='dispatched'`, uid, epoch)
		if e != nil {
			return e
		}
		return normalizeError(remoteErr)
	}
	return s.RecordUploadProgress(ctx, uid, epoch, st.Offset, st.ResourceID)
}

// RecordUploadProgress 仅接受当前未决 epoch 的完整响应，不截断、不忽略零行。
func (s *Service) RecordUploadProgress(ctx context.Context, uid string, epoch, offset int64, resourceID string) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SELECT 1 FROM file_operations WHERE id=(SELECT operation_id FROM upload_sessions WHERE id=$1) FOR UPDATE`, uid); e != nil {
		return e
	}
	u, e := one(ctx, tx, `SELECT to_jsonb(u) FROM upload_sessions u WHERE id=$1 FOR UPDATE`, uid)
	if e != nil {
		return e
	}
	result, e := tx.Exec(ctx, `UPDATE upload_sessions u SET received_bytes=$3,
		native_resource_id=COALESCE(NULLIF($4,''),native_resource_id),transfer_completed=($3=expected_size),
		in_flight_patches=0,updated_at=now()
		WHERE id=$1 AND state='uploading' AND patch_epoch=$2 AND in_flight_patches=1
		AND $3 BETWEEN received_bytes AND expected_size AND (native_resource_id IS NULL OR $4='' OR native_resource_id=$4)
		AND EXISTS (SELECT 1 FROM upload_patch_attempts a WHERE a.upload_id=u.id AND a.epoch=$2 AND a.state IN ('dispatched','outcome_unknown') AND a.offset_end=$3)
		AND EXISTS (SELECT 1 FROM file_operations o WHERE o.id=u.operation_id AND o.state IN ('executing','outcome_unknown'))`, uid, epoch, offset, resourceID)
	if e != nil {
		return normalizeError(e)
	}
	if result.RowsAffected() != 1 {
		if _, e = tx.Exec(ctx, `UPDATE upload_patch_attempts SET state='outcome_unknown',error_code='patch_progress_conflict' WHERE upload_id=$1 AND epoch=$2 AND state IN ('dispatched','outcome_unknown')`, uid, epoch); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `UPDATE file_operations SET error_code='patch_progress_conflict',retry_at=now() WHERE id=$1 AND state NOT IN ('committed','failed_confirmed')`, str(u, "operation_id")); e != nil {
			return e
		}
		if e = tx.Commit(ctx); e != nil {
			return e
		}
		return uploadPending("patch_progress_conflict")
	}
	if _, e = tx.Exec(ctx, `UPDATE upload_patch_attempts SET state='succeeded',ended_at=now(),error_code=NULL WHERE upload_id=$1 AND epoch=$2 AND state IN ('dispatched','outcome_unknown')`, uid, epoch); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// observeUploadTransfer 仅用原生 offset 已覆盖 attempt 上界的证据解决未决写。
// HEAD 404/低 offset 不能证明挂起的 PATCH 已终止，也不能释放门禁。
func (s *Service) observeUploadTransfer(ctx context.Context, uid string, token int64, st ports.UploadStatus) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	o, e := one(ctx, tx, `SELECT to_jsonb(o) FROM file_operations o JOIN upload_sessions u ON u.operation_id=o.id WHERE u.id=$1 AND o.lease_token=$2 AND o.lease_until>now() FOR UPDATE OF o`, uid, token)
	if e != nil {
		return uploadPending("reconcile_lease_lost")
	}
	u, e := one(ctx, tx, `SELECT to_jsonb(u) FROM upload_sessions u WHERE id=$1 FOR UPDATE`, uid)
	if e != nil {
		return e
	}
	if st.Offset < num(u, "received_bytes") || st.Offset > num(u, "expected_size") || st.Length != num(u, "expected_size") || st.Complete != (st.Offset == st.Length) || (str(u, "native_resource_id") != "" && st.ResourceID != "" && str(u, "native_resource_id") != st.ResourceID) {
		return uploadPending("native_progress_conflict")
	}
	if _, e = tx.Exec(ctx, `UPDATE upload_patch_attempts SET state='reconciled',ended_at=now(),error_code='native_offset_confirmed' WHERE upload_id=$1 AND state IN ('dispatched','outcome_unknown') AND offset_end<=$2 AND offset_end>offset_start`, uid, st.Offset); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `UPDATE upload_sessions SET received_bytes=$2,transfer_completed=$3,
		native_resource_id=COALESCE(NULLIF($4,''),native_resource_id),
		in_flight_patches=(SELECT count(*) FROM upload_patch_attempts WHERE upload_id=$1 AND state IN ('dispatched','outcome_unknown')),updated_at=now() WHERE id=$1`, uid, st.Offset, st.Complete, st.ResourceID); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `UPDATE file_operations SET error_code='native_offset_reconciled' WHERE id=$1`, str(o, "id")); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
