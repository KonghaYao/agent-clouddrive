-- Calls below are atomic, but upload admission should wrap begin + session + reserve
-- in ONE pgx transaction, so a failed quota check does not leave a locked file.
-- name: BeginFileOperation :one
SELECT * FROM begin_file_operation(
    sqlc.arg(operation_id)::uuid,sqlc.arg(file_id)::uuid,sqlc.arg(actor_id)::uuid,sqlc.arg(actor_type)::text,sqlc.arg(kind)::text,
    sqlc.arg(expected_revision)::bigint,sqlc.narg(expected_version_id)::uuid,sqlc.arg(idempotency_key)::text,sqlc.arg(request_hash)::text,sqlc.arg(intent)::jsonb);
-- name: GetFileOperation :one
SELECT * FROM file_operations WHERE id = $1;
-- name: GetIdempotentOperation :one
SELECT * FROM file_operations WHERE actor_principal_id = $1 AND kind = $2 AND idempotency_key = $3;
-- name: TransitionFileOperation :one
UPDATE file_operations SET state = $2,remote_result = COALESCE(sqlc.narg(remote_result)::jsonb,remote_result),
error_code = sqlc.narg(error_code)::text,updated_at = now() WHERE id = $1 AND state = $3 RETURNING *;
-- name: ClaimReconciliation :many
WITH claim AS (
    SELECT id FROM file_operations WHERE state IN ('executing','outcome_unknown','remote_confirmed')
    AND retry_at <= now() AND (lease_until IS NULL OR lease_until < now()) ORDER BY retry_at,id FOR UPDATE SKIP LOCKED LIMIT $1
)
UPDATE file_operations o SET lease_owner = $2,lease_token = o.lease_token + 1,
lease_until = now() + (sqlc.arg(lease_seconds)::integer * interval '1 second')
FROM claim WHERE o.id = claim.id RETURNING o.*;
-- name: DeferReconciliation :execrows
UPDATE file_operations SET retry_at = $3,lease_owner = NULL,lease_until = NULL,updated_at = now()
WHERE id = $1 AND lease_token = $2 AND lease_until > now() AND state NOT IN ('committed','failed_confirmed');
-- name: CreateUploadSession :one
INSERT INTO upload_sessions(id,operation_id,expected_size,expected_version_id,node_id,expires_at)
VALUES($1,$2,$3,$4,$5,$6) RETURNING *;
-- name: GetUploadSession :one
SELECT * FROM upload_sessions WHERE id = $1;
-- name: BindNativeUpload :one
UPDATE upload_sessions SET native_upload_ref = $2,state = 'uploading',updated_at = now()
WHERE id = $1 AND state = 'created' RETURNING *;
-- name: UpdateUploadOffset :execrows
UPDATE upload_sessions SET received_bytes = $2,updated_at = now()
WHERE id = $1 AND state = 'uploading' AND received_bytes <= $2 AND $2 <= expected_size;
-- name: ReserveUploadQuota :one
SELECT * FROM reserve_upload_quota(sqlc.arg(upload_id)::uuid);
-- name: CommitContentOperation :one
SELECT * FROM commit_content_operation(
    sqlc.arg(operation_id)::uuid,sqlc.arg(version_id)::uuid,sqlc.arg(size)::bigint,sqlc.arg(sha256)::text,
    sqlc.arg(native_etag)::text,sqlc.arg(native_revision_ref)::jsonb,sqlc.arg(native_resource_id)::text,
    sqlc.arg(content_type)::text,sqlc.arg(recipe_fingerprint)::text,sqlc.arg(recipe_snapshot)::jsonb);
-- name: ReleaseUploadQuota :one
SELECT release_upload_quota(sqlc.arg(upload_id)::uuid)::boolean AS released;
