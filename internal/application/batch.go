package application

import (
	"context"
	"encoding/json"

	"github.com/agentclouddrive/agent-clouddrive/internal/domain"
)

type batchCommand struct {
	ID             string   `json:"item_id"`
	Command        string   `json:"command"`
	DependsOn      []string `json:"depends_on"`
	Input          Object   `json:"input"`
	IdempotencyKey string   `json:"idempotency_key,omitempty"`
}

// Batch isolates item transactions; one failure never rolls back an independent
// successful command. Saved inputs and keys make reruns deterministic after a
// client disconnect. Anonymous feedback is deliberately forbidden in batches.
func (s *Service) Batch(ctx context.Context, a Actor, in Object) (any, error) {
	if !a.Principal.Active || !a.Principal.StatusVerified {
		return nil, domain.NewError(domain.AccessDenied, "active principal required")
	}
	key := str(in, "idempotency_key")
	if !validIdempotencyKey(key) {
		return nil, domain.NewError(domain.InvalidArgument, "batch idempotency_key is required (maximum 200 characters)")
	}
	var commands []batchCommand
	if e := json.Unmarshal(js(in["items"]), &commands); e != nil || len(commands) == 0 || len(commands) > 100 {
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
			c.Input = Object{}
		}
		// Caller-provided keys allow retrying only failed items in a later batch.
		if c.IdempotencyKey == "" {
			c.IdempotencyKey = str(c.Input, "idempotency_key")
		}
		if c.IdempotencyKey == "" {
			c.IdempotencyKey = "batch:" + hash(Object{"batch": key, "item": c.ID})
		}
		if !validIdempotencyKey(c.IdempotencyKey) {
			return nil, domain.NewError(domain.InvalidArgument, "item idempotency_key is required (maximum 200 characters)")
		}
		c.Input["idempotency_key"] = c.IdempotencyKey
		graph = append(graph, domain.BatchItem{ID: c.ID, DependsOn: c.DependsOn, Status: domain.BatchPending})
	}
	if _, _, e := domain.ScheduleBatch(graph); e != nil {
		return nil, e
	}
	requestHash := hash(commands)
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	_, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,1))`, a.Principal.ID+":"+key)
	if e != nil {
		return nil, e
	}
	b, e := one(ctx, tx, `SELECT to_jsonb(b) FROM batch_operations b WHERE actor_principal_id=$1 AND idempotency_key=$2`, a.Principal.ID, key)
	if e == nil {
		if str(b, "request_hash") != requestHash {
			return nil, domain.NewError(domain.IdempotencyConflict, "batch key is bound to different items")
		}
	} else if domain.ErrorCode(e) == domain.NotFound {
		b, e = one(ctx, tx, `INSERT INTO batch_operations(actor_principal_id,idempotency_key,request_hash) VALUES($1,$2,$3) RETURNING to_jsonb(batch_operations)`, a.Principal.ID, key, requestHash)
		if e != nil {
			return nil, e
		}
		for _, c := range commands {
			_, e = tx.Exec(ctx, `INSERT INTO batch_items(batch_id,item_id,depends_on,command,input,idempotency_key) VALUES($1,$2,$3,$4,$5::jsonb,$6)`, str(b, "id"), c.ID, c.DependsOn, c.Command, js(c.Input), c.IdempotencyKey)
			if e != nil {
				return nil, normalizeError(e)
			}
		}
	} else {
		return nil, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, e
	}
	batchID := str(b, "id")
	// Holding a session-level lock through one bounded batch execution prevents
	// concurrent callers from racing the same running item. No DB transaction or
	// file lock is held during remote I/O; item idempotency remains the safety net.
	conn, e := s.DB.Acquire(ctx)
	if e != nil {
		return nil, e
	}
	defer conn.Release()
	var locked bool
	if e = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,2))`, batchID).Scan(&locked); e != nil {
		return nil, e
	}
	if !locked {
		return s.batchResult(ctx, batchID, a.Principal.ID)
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1,2))`, batchID)
	for {
		rows, e := many(ctx, s.DB, `SELECT to_jsonb(i) FROM batch_items i WHERE batch_id=$1 ORDER BY item_id`, batchID)
		if e != nil {
			return nil, e
		}
		graph = graph[:0]
		lookup := map[string]Object{}
		for _, r := range rows {
			deps := []string{}
			if raw, ok := r["depends_on"].([]any); ok {
				for _, v := range raw {
					if d, ok := v.(string); ok {
						deps = append(deps, d)
					}
				}
			}
			status := domain.BatchStatus(str(r, "status"))
			// A prior process may have crashed while running. Re-executing its stable
			// per-item key only observes/reconciles the saved operation, never duplicates.
			if status == domain.BatchRunning {
				status = domain.BatchPending
			}
			graph = append(graph, domain.BatchItem{ID: str(r, "item_id"), DependsOn: deps, Status: status})
			lookup[str(r, "item_id")] = r
		}
		ready, blocked, e := domain.ScheduleBatch(graph)
		if e != nil {
			return nil, e
		}
		for _, itemID := range blocked {
			_, e = s.DB.Exec(ctx, `UPDATE batch_items SET status='blocked_dependency',result=$3::jsonb WHERE batch_id=$1 AND item_id=$2 AND status IN('pending','running')`, batchID, itemID, js(Object{"error_code": "BLOCKED_DEPENDENCY"}))
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
			_, e = s.DB.Exec(ctx, `UPDATE batch_items SET status='running' WHERE batch_id=$1 AND item_id=$2 AND status IN('pending','running')`, batchID, itemID)
			if e != nil {
				return nil, e
			}
			result, runErr := s.Execute(ctx, a, str(r, "command"), input)
			status := "succeeded"
			out := Object{"value": result}
			operationID := ""
			if m, ok := result.(map[string]any); ok {
				operationID = str(m, "operation_id")
				switch str(m, "state") {
				case "prepared", "executing", "outcome_unknown", "remote_confirmed":
					status = "running"
				case "failed_confirmed":
					status = "failed"
				}
			}
			if runErr != nil {
				status = "failed"
				out["error"] = Object{"code": domain.ErrorCode(normalizeError(runErr)), "message": normalizeError(runErr).Error()}
			}
			_, e = s.DB.Exec(ctx, `UPDATE batch_items SET status=$3,result=$4::jsonb,operation_id=$5 WHERE batch_id=$1 AND item_id=$2`, batchID, itemID, status, js(out), null(operationID))
			if e != nil {
				return nil, e
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
		_ = s.DB.QueryRow(ctx, `SELECT count(*) FROM batch_items WHERE batch_id=$1 AND status='pending'`, batchID).Scan(&pending)
		if pending == 0 {
			break
		}
	}
	_, e = s.DB.Exec(ctx, `UPDATE batch_operations SET state='completed' WHERE id=$1 AND NOT EXISTS(SELECT 1 FROM batch_items WHERE batch_id=$1 AND status IN('pending','running'))`, batchID)
	if e != nil {
		return nil, e
	}
	return s.batchResult(ctx, batchID, a.Principal.ID)
}
func (s *Service) batchResult(ctx context.Context, batchID, actorID string) (any, error) {
	b, e := one(ctx, s.DB, `SELECT to_jsonb(b) FROM batch_operations b WHERE id=$1 AND actor_principal_id=$2`, batchID, actorID)
	if e != nil {
		return nil, e
	}
	rows, e := many(ctx, s.DB, `SELECT to_jsonb(i)-'input'-'idempotency_key' FROM batch_items i WHERE batch_id=$1 ORDER BY item_id`, batchID)
	if e != nil {
		return nil, e
	}
	succeeded, failed, pending := []Object{}, []Object{}, []Object{}
	for _, r := range rows {
		if result, ok := r["result"].(map[string]any); ok {
			if failure, ok := result["error"]; ok {
				r["error"] = failure
			}
		}
		switch str(r, "status") {
		case "succeeded":
			succeeded = append(succeeded, r)
		case "failed", "blocked_dependency":
			failed = append(failed, r)
		default:
			pending = append(pending, r)
		}
	}
	return Object{"batch_id": batchID, "state": str(b, "state"), "items": rows, "succeeded": succeeded, "failed": failed, "pending": pending}, nil
}
