package batch

import (
	"context"
	"encoding/json"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/application/core"
	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
)

type batchCommand struct {
	ID             string      `json:"item_id"`
	Command        string      `json:"command"`
	DependsOn      []string    `json:"depends_on"`
	Input          core.Object `json:"input"`
	IdempotencyKey string      `json:"idempotency_key,omitempty"`
}

type Executor func(ctx context.Context, a core.Actor, name string, in core.Object) (any, error)

type Service struct{ d core.Deps }

func New(d core.Deps) *Service { return &Service{d: d} }

// Batch isolates item transactions; one failure never rolls back an independent
// successful command. Saved inputs and keys make reruns deterministic after a
// client disconnect. Anonymous feedback is deliberately forbidden in batches.
func (s *Service) Batch(ctx context.Context, a core.Actor, in core.Object, exec Executor) (any, error) {
	ctx, budgetCancel := context.WithTimeout(ctx, 90*time.Second)
	defer budgetCancel()
	if !a.Principal.Active || !a.Principal.StatusVerified {
		return nil, domain.NewError(domain.AccessDenied, "active principal required")
	}
	key := core.Str(in, "idempotency_key")
	if !core.ValidIdempotencyKey(key) {
		return nil, domain.NewError(domain.InvalidArgument, "batch idempotency_key is required (maximum 200 characters)")
	}
	var commands []batchCommand
	if e := json.Unmarshal(core.JSONBytes(in["items"]), &commands); e != nil || len(commands) == 0 || len(commands) > 100 {
		return nil, domain.NewError(domain.InvalidArgument, "batch requires 1–100 valid items")
	}
	graph := []domain.BatchItem{}
	for i := range commands {
		c := &commands[i]
		if c.DependsOn == nil {
			c.DependsOn = []string{}
		}
		switch c.Command {
		case "mkdir", "move", "rename", "delete", "restore", "create_text":
		default:
			return nil, domain.NewError(domain.InvalidArgument, "unsupported batch command")
		}
		if c.Input == nil {
			c.Input = core.Object{}
		}
		// Caller-provided keys allow retrying only failed items in a later batch.
		if c.IdempotencyKey == "" {
			c.IdempotencyKey = core.Str(c.Input, "idempotency_key")
		}
		if c.IdempotencyKey == "" {
			c.IdempotencyKey = "batch:" + core.Hash(core.Object{"batch": key, "item": c.ID})
		}
		if !core.ValidIdempotencyKey(c.IdempotencyKey) {
			return nil, domain.NewError(domain.InvalidArgument, "item idempotency_key is required (maximum 200 characters)")
		}
		c.Input["idempotency_key"] = c.IdempotencyKey
		graph = append(graph, domain.BatchItem{ID: c.ID, DependsOn: c.DependsOn, Status: domain.BatchPending})
	}
	if _, _, e := domain.ScheduleBatch(graph); e != nil {
		return nil, e
	}
	requestHash := core.Hash(commands)
	tx, e := s.d.DB.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	_, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,1))`, a.Principal.ID+":"+key)
	if e != nil {
		return nil, e
	}
	b, e := core.One(ctx, tx, `SELECT to_jsonb(b) FROM batch_operations b WHERE actor_principal_id=$1 AND idempotency_key=$2`, a.Principal.ID, key)
	if e == nil {
		if core.Str(b, "request_hash") != requestHash {
			return nil, domain.NewError(domain.IdempotencyConflict, "batch key is bound to different items")
		}
	} else if domain.ErrorCode(e) == domain.NotFound {
		b, e = core.One(ctx, tx, `INSERT INTO batch_operations(actor_principal_id,idempotency_key,request_hash) VALUES($1,$2,$3) RETURNING to_jsonb(batch_operations)`, a.Principal.ID, key, requestHash)
		if e != nil {
			return nil, e
		}
		for _, c := range commands {
			_, e = tx.Exec(ctx, `INSERT INTO batch_items(batch_id,item_id,depends_on,command,input,idempotency_key) VALUES($1,$2,$3,$4,$5::jsonb,$6)`, core.Str(b, "id"), c.ID, c.DependsOn, c.Command, core.JSONBytes(c.Input), c.IdempotencyKey)
			if e != nil {
				return nil, core.NormalizeError(e)
			}
		}
	} else {
		return nil, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, e
	}
	batchID := core.Str(b, "id")
	// 短 SQL 领取后立即归还连接；执行预算短于租约，过期持有者不能写回结果。
	owner := core.ID()
	lease, e := core.One(ctx, s.d.DB, `UPDATE batch_operations SET lease_owner=$2,lease_token=lease_token+1,lease_until=now()+interval '2 minutes' WHERE id=$1 AND state='running' AND (lease_until IS NULL OR lease_until<=now()) RETURNING to_jsonb(batch_operations)`, batchID, owner)
	if domain.ErrorCode(e) == domain.NotFound {
		return s.batchResult(ctx, batchID, a.Principal.ID)
	}
	if e != nil {
		return nil, e
	}
	fence := core.Num(lease, "lease_token")
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	defer func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer done()
		_, _ = s.d.DB.Exec(cleanup, `UPDATE batch_operations SET lease_owner=NULL,lease_until=NULL WHERE id=$1 AND lease_token=$2 AND lease_owner=$3`, batchID, fence, owner)
	}()
	for {
		rows, e := core.Many(ctx, s.d.DB, `SELECT to_jsonb(i) FROM batch_items i WHERE batch_id=$1 ORDER BY item_id`, batchID)
		if e != nil {
			return nil, e
		}
		graph = graph[:0]
		lookup := map[string]core.Object{}
		for _, r := range rows {
			deps := []string{}
			if raw, ok := r["depends_on"].([]any); ok {
				for _, v := range raw {
					if d, ok := v.(string); ok {
						deps = append(deps, d)
					}
				}
			}
			status := domain.BatchStatus(core.Str(r, "status"))
			// A prior process may have crashed while running. Re-executing its stable
			// per-item key only observes/reconciles the saved operation, never duplicates.
			if status == domain.BatchRunning {
				status = domain.BatchPending
			}
			graph = append(graph, domain.BatchItem{ID: core.Str(r, "item_id"), DependsOn: deps, Status: status})
			lookup[core.Str(r, "item_id")] = r
		}
		ready, blocked, e := domain.ScheduleBatch(graph)
		if e != nil {
			return nil, e
		}
		for _, itemID := range blocked {
			_, e = s.d.DB.Exec(ctx, `UPDATE batch_items SET status='blocked_dependency',result=$3::jsonb WHERE batch_id=$1 AND item_id=$2 AND status IN('pending','running') AND EXISTS(SELECT 1 FROM batch_operations WHERE id=$1 AND lease_token=$4 AND lease_until>now())`, batchID, itemID, core.JSONBytes(core.Object{"error_code": "BLOCKED_DEPENDENCY"}), fence)
			if e != nil {
				return nil, e
			}
		}
		if len(ready) == 0 {
			break
		}
		progressed := false
		for _, itemID := range ready {
			r := lookup[itemID]
			input, _ := r["input"].(map[string]any)
			claim, claimErr := s.d.DB.Exec(ctx, `UPDATE batch_items SET status='running' WHERE batch_id=$1 AND item_id=$2 AND status IN('pending','running') AND EXISTS(SELECT 1 FROM batch_operations WHERE id=$1 AND lease_token=$3 AND lease_until>now())`, batchID, itemID, fence)
			if claimErr != nil {
				return nil, claimErr
			}
			if claim.RowsAffected() != 1 {
				return nil, domain.NewError(domain.LeaseLost, "batch lease lost")
			}
			result, runErr := exec(ctx, a, core.Str(r, "command"), input)
			status := "succeeded"
			out := core.Object{"value": result}
			operationID := ""
			if m, ok := result.(map[string]any); ok {
				operationID = core.Str(m, "operation_id")
				switch core.Str(m, "state") {
				case "prepared", "executing", "outcome_unknown", "remote_confirmed":
					status = "running"
				case "failed_confirmed":
					status = "failed"
				}
			}
			if runErr != nil {
				status = "failed"
				out["error"] = core.Object{"code": domain.ErrorCode(core.NormalizeError(runErr)), "message": core.NormalizeError(runErr).Error()}
			}
			written, writeErr := s.d.DB.Exec(ctx, `UPDATE batch_items SET status=$3,result=$4::jsonb,operation_id=$5 WHERE batch_id=$1 AND item_id=$2 AND EXISTS(SELECT 1 FROM batch_operations WHERE id=$1 AND lease_token=$6 AND lease_until>now())`, batchID, itemID, status, core.JSONBytes(out), core.Null(operationID), fence)
			if writeErr != nil {
				return nil, writeErr
			}
			if written.RowsAffected() != 1 {
				return nil, domain.NewError(domain.LeaseLost, "batch result lease lost")
			}
			if status != "running" {
				progressed = true
			}
		}
		if !progressed {
			break
		}
		// A running unknown item must not spin; continue only if a newly successful
		// independent branch may have unblocked another item.
		pending := 0
		_ = s.d.DB.QueryRow(ctx, `SELECT count(*) FROM batch_items WHERE batch_id=$1 AND status='pending'`, batchID).Scan(&pending)
		if pending == 0 {
			break
		}
	}
	_, e = s.d.DB.Exec(ctx, `UPDATE batch_operations SET state='completed' WHERE id=$1 AND lease_token=$2 AND lease_until>now() AND NOT EXISTS(SELECT 1 FROM batch_items WHERE batch_id=$1 AND status IN('pending','running'))`, batchID, fence)
	if e != nil {
		return nil, e
	}
	return s.batchResult(ctx, batchID, a.Principal.ID)
}
func (s *Service) batchResult(ctx context.Context, batchID, actorID string) (any, error) {
	b, e := core.One(ctx, s.d.DB, `SELECT to_jsonb(b) FROM batch_operations b WHERE id=$1 AND actor_principal_id=$2`, batchID, actorID)
	if e != nil {
		return nil, e
	}
	rows, e := core.Many(ctx, s.d.DB, `SELECT to_jsonb(i)-'input'-'idempotency_key' FROM batch_items i WHERE batch_id=$1 ORDER BY item_id`, batchID)
	if e != nil {
		return nil, e
	}
	succeeded, failed, pending := []core.Object{}, []core.Object{}, []core.Object{}
	for _, r := range rows {
		if result, ok := r["result"].(map[string]any); ok {
			if failure, ok := result["error"]; ok {
				r["error"] = failure
			}
		}
		switch core.Str(r, "status") {
		case "succeeded":
			succeeded = append(succeeded, r)
		case "failed", "blocked_dependency":
			failed = append(failed, r)
		default:
			pending = append(pending, r)
		}
	}
	return core.Object{"batch_id": batchID, "state": core.Str(b, "state"), "items": rows, "succeeded": succeeded, "failed": failed, "pending": pending}, nil
}
