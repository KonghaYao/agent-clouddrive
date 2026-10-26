package application

import (
	"context"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

// claimUploadFinalize 与 PATCH/abandon 竞争同一 operation 锁；过期租约可重领。
func (s *Service) claimUploadFinalize(ctx context.Context, a Actor, uid string) (int64, error) {
	var token int64
	e := s.DB.QueryRow(ctx, `UPDATE file_operations o SET lease_token=lease_token+1,lease_owner=$3,lease_until=now()+interval '2 minutes',retry_at=now()+interval '30 seconds'
		FROM upload_sessions u WHERE u.operation_id=o.id AND u.id=$1 AND o.actor_principal_id=$2
		AND o.state IN ('executing','outcome_unknown','remote_confirmed') AND (o.lease_until IS NULL OR o.lease_until<=now()) RETURNING o.lease_token`, uid, a.Principal.ID, s.Config.NodeID).Scan(&token)
	if e != nil {
		return 0, uploadPending("finalize_already_claimed_or_terminal")
	}
	return token, nil
}

// keepUploadLease 为大文件校验续租；续租失败取消读取，最终提交仍校验 token。
func (s *Service) keepUploadLease(ctx context.Context, uid string, token int64) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewCtx, stop := context.WithTimeout(ctx, 10*time.Second)
				tag, e := s.DB.Exec(renewCtx, `UPDATE file_operations SET lease_until=now()+interval '2 minutes' WHERE id=(SELECT operation_id FROM upload_sessions WHERE id=$1) AND lease_token=$2 AND lease_until>now()`, uid, token)
				stop()
				if e != nil || tag.RowsAffected() != 1 {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() { cancel(); <-done }
}

func (s *Service) releaseUploadLease(ctx context.Context, uid string, token int64, reason string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, _ = s.DB.Exec(ctx, `UPDATE file_operations SET lease_until=NULL,lease_owner=NULL,retry_at=now()+interval '30 seconds',error_code=NULLIF($3,'') WHERE id=(SELECT operation_id FROM upload_sessions WHERE id=$1) AND lease_token=$2`, uid, token, reason)
}

// uploadTransferForFinalize 在尚有未决 PATCH 时只接受原生终结证据。
func (s *Service) uploadTransferForFinalize(ctx context.Context, u Object, token int64, cred ports.Credential) error {
	if num(u, "in_flight_patches") == 0 {
		return nil
	}
	st, e := s.Original.StatUpload(ctx, ports.UploadInfo{NativeURL: str(u, "native_upload_ref"), OperationID: str(u, "operation_id"), Length: num(u, "expected_size")}, cred)
	if e != nil {
		return normalizeError(e)
	}
	if e = s.observeUploadTransfer(ctx, str(u, "id"), token, st); e != nil {
		return e
	}
	var unresolved int
	if e = s.DB.QueryRow(ctx, `SELECT in_flight_patches FROM upload_sessions WHERE id=$1`, str(u, "id")).Scan(&unresolved); e != nil {
		return e
	}
	if unresolved != 0 {
		return uploadPending("native_patch_still_unresolved")
	}
	// 即使最后响应丢失，成功 HEAD 已提供可持久化的完成证据。
	u["transfer_completed"], u["native_resource_id"] = st.Complete, st.ResourceID
	return nil
}

func (s *Service) commitUploadResult(ctx context.Context, u, f, remote Object, rec ports.ReconcileResult, token, size int64, sha string) (Object, error) {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	// 与 commit_content_operation 一致：空间锁 -> file -> operation -> upload。
	if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, str(f, "space_id")); e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, `SELECT 1 FROM files WHERE id=$1 FOR UPDATE`, str(f, "id")); e != nil {
		return nil, e
	}
	result, e := tx.Exec(ctx, `UPDATE file_operations o SET state='remote_confirmed',remote_result=$2::jsonb,updated_at=now()
		WHERE id=$1 AND state IN ('executing','outcome_unknown','remote_confirmed') AND lease_token=$3 AND lease_until>now()
		AND NOT EXISTS (SELECT 1 FROM upload_sessions u WHERE u.operation_id=o.id AND u.in_flight_patches>0)`, str(u, "operation_id"), js(remote), token)
	if e != nil {
		return nil, normalizeError(e)
	}
	if result.RowsAffected() != 1 {
		return nil, domain.NewError(domain.LeaseLost, "upload finalization lease lost")
	}
	recipe := s.Recipe()
	v, e := one(ctx, tx, `SELECT to_jsonb(v) FROM commit_content_operation($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$9,$10::jsonb) v`, str(u, "operation_id"), id(), size, sha, rec.Metadata.ETag, js(rec.Revision), rec.Metadata.ResourceID, str(f, "content_type"), recipe.Fingerprint, js(recipe))
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, e
	}
	return v, nil
}
