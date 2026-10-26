-- name: EnqueueOutbox :one
INSERT INTO outbox(id,event_type,aggregate_id,payload,deduplication_key) VALUES($1,$2,$3,$4,$5)
ON CONFLICT(deduplication_key) DO UPDATE SET deduplication_key = EXCLUDED.deduplication_key RETURNING *;
-- name: ClaimOutbox :many
WITH claim AS (
    SELECT id FROM outbox WHERE (state = 'pending' AND retry_at <= now()) OR (state = 'publishing' AND lease_until < now())
    ORDER BY created_at,id FOR UPDATE SKIP LOCKED LIMIT sqlc.arg(batch_size)::integer
)
UPDATE outbox o SET state = 'publishing',attempt = o.attempt + 1,lease_token = o.lease_token + 1,
lease_owner = sqlc.arg(worker_id)::text,lease_until = now() + (sqlc.arg(lease_seconds)::integer * interval '1 second')
FROM claim WHERE o.id = claim.id RETURNING o.*;
-- name: MarkOutboxPublished :execrows
UPDATE outbox SET state = 'published',published_at = now(),lease_owner = NULL,lease_until = NULL
WHERE id = $1 AND lease_token = $2 AND state = 'publishing' AND lease_until > now();
-- name: RetryOutbox :execrows
UPDATE outbox SET state = 'pending',retry_at = $3,lease_owner = NULL,lease_until = NULL
WHERE id = $1 AND lease_token = $2 AND state = 'publishing' AND lease_until > now();

-- name: RenewOutboxLease :execrows
UPDATE outbox SET lease_until = now() + (sqlc.arg(lease_seconds)::integer * interval '1 second')
WHERE id = sqlc.arg(id) AND lease_token = sqlc.arg(lease_token)
AND state = 'publishing' AND lease_until > now();
