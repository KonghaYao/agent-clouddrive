package application

import (
	"context"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

// 激活、初次发布和清理都持有同一个 owner advisory lock。清理周期性复核非当前
// 产物，因此崩溃前已提交但响应丢失的迟到 Upsert 也会再次收敛。
func (s *Service) reconcileArtifactVisibility(ctx context.Context) error {
	if s.Search == nil || s.Artifacts == nil {
		return ports.ErrUnavailable
	}
	if err := s.reconcileArtifactTasks(ctx, true); err != nil {
		return err
	}
	return s.reconcileArtifactTasks(ctx, false)
}

func (s *Service) reconcileArtifactTasks(ctx context.Context, activation bool) error {
	table := "artifact_cleanup_tasks"
	if activation {
		table = "artifact_activation_tasks"
	}
	rows, err := s.DB.Query(ctx, `WITH picked AS (
		SELECT artifact_set_id FROM `+table+` WHERE retry_at<=now() AND (state='queued' OR state='leased' AND lease_until<now())
		ORDER BY retry_at,artifact_set_id FOR UPDATE SKIP LOCKED LIMIT 10
	) UPDATE `+table+` t SET state='leased',attempt=attempt+1,lease_owner=$1,lease_token=lease_token+1,lease_until=now()+interval '2 minutes'
	FROM picked WHERE t.artifact_set_id=picked.artifact_set_id RETURNING t.artifact_set_id::text,t.lease_token`, s.Config.NodeID)
	if err != nil {
		return err
	}
	type claim struct {
		aid   string
		token int64
	}
	var claims []claim
	for rows.Next() {
		var c claim
		if err = rows.Scan(&c.aid, &c.token); err != nil {
			rows.Close()
			return err
		}
		claims = append(claims, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, c := range claims {
		err = s.reconcileArtifactTask(ctx, table, c.aid, c.token, activation)
		if err != nil {
			// 只保存稳定错误码，不把依赖错误消息/凭据持久化。
			_, saveErr := s.DB.Exec(ctx, `UPDATE `+table+` SET state='queued',retry_at=now()+interval '5 seconds',last_error='reconciliation_pending',lease_owner=NULL,lease_until=NULL WHERE artifact_set_id=$1 AND lease_token=$2 AND lease_owner=$3`, c.aid, c.token, s.Config.NodeID)
			if saveErr != nil {
				return saveErr
			}
		}
	}
	return nil
}

func (s *Service) reconcileArtifactTask(parent context.Context, table, aid string, token int64, activation bool) error {
	ctx, cancel := context.WithTimeout(parent, time.Minute)
	defer cancel()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var owner, fileID string
	if err = tx.QueryRow(ctx, `SELECT f.owner_id::text,f.id::text FROM artifact_sets a JOIN file_versions v ON v.id=a.source_version_id JOIN files f ON f.id=v.file_id WHERE a.id=$1`, aid).Scan(&owner, &fileID); err != nil {
		return err
	}
	var locked bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('search-owner:'||$1,0))`, owner).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return ports.ErrConflict
	}
	// 行锁让租约过期后的领取者不能和旧持有者同时执行外部副作用。
	var valid bool
	if err = tx.QueryRow(ctx, `SELECT state='leased' AND lease_token=$2 AND lease_owner=$3 AND lease_until>now() FROM `+table+` WHERE artifact_set_id=$1 FOR UPDATE`, aid, token, s.Config.NodeID).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ports.ErrConflict
	}
	state := "done"
	retrySeconds := 30
	if activation {
		var eligible, active bool
		if err = tx.QueryRow(ctx, `SELECT a.published_at IS NOT NULL AND a.source_version_id=f.current_version_id
		AND NOT EXISTS(SELECT 1 FROM artifact_sets newer WHERE newer.source_version_id=a.source_version_id AND newer.published_at IS NOT NULL AND (newer.generation,newer.attempt)>(a.generation,a.attempt)),file_is_active(f.id)
		FROM artifact_sets a JOIN files f ON f.id=$2 WHERE a.id=$1`, aid, fileID).Scan(&eligible, &active); err != nil {
			return err
		}
		if eligible && !active {
			return ports.ErrConflict
		}
		if eligible {
			if err = s.rebuildSearchArtifactWithDB(ctx, tx, aid); err != nil {
				return err
			}
			var activated bool
			if err = tx.QueryRow(ctx, `SELECT activate_artifact($1,$2)`, fileID, aid).Scan(&activated); err != nil {
				return err
			}
			if !activated {
				return ports.ErrConflict
			}
		}
	} else {
		var referenced, inflight bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM files WHERE search_artifact_set_id=$1),
		EXISTS(SELECT 1 FROM artifact_sets a JOIN conversion_jobs j ON j.source_version_id=a.source_version_id AND j.generation=a.generation WHERE a.id=$1 AND a.published_at IS NULL AND j.state IN ('queued','processing'))`, aid).Scan(&referenced, &inflight); err != nil {
			return err
		}
		if !referenced && !inflight {
			task, e := s.Search.DeleteArtifactSet(ctx, owner, aid)
			if e != nil {
				return e
			}
			if e = s.Search.Wait(ctx, task); e != nil {
				return e
			}
		}
		// 不能把响应未知的远端 Upsert 当作已终结：保留低频持久复核。
		state = "queued"
	}
	_, err = tx.Exec(ctx, `UPDATE `+table+` SET state=$3,retry_at=now()+($4*interval '1 second'),last_error=NULL,lease_owner=NULL,lease_until=NULL WHERE artifact_set_id=$1 AND lease_token=$2`, aid, token, state, retrySeconds)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
