-- name: CreateBatchOperation :one
INSERT INTO batch_operations(id,actor_principal_id,idempotency_key,request_hash) VALUES($1,$2,$3,$4) RETURNING *;
-- name: GetIdempotentBatch :one
SELECT * FROM batch_operations WHERE actor_principal_id = $1 AND idempotency_key = $2;
-- name: CreateBatchItem :one
INSERT INTO batch_items(batch_id,item_id,depends_on,command,input,idempotency_key) VALUES($1,$2,$3,$4,$5,$6) RETURNING *;
-- name: SetBatchItemResult :execrows
UPDATE batch_items SET operation_id = $3,status = $4,result = $5 WHERE batch_id = $1 AND item_id = $2 AND status <> 'succeeded';
-- name: ListBatchItems :many
SELECT * FROM batch_items WHERE batch_id = $1 ORDER BY item_id;
-- name: FinishBatchOperation :execrows
UPDATE batch_operations SET state = 'completed' WHERE id = $1 AND NOT EXISTS(SELECT 1 FROM batch_items WHERE batch_id = $1 AND status IN ('pending','running'));
