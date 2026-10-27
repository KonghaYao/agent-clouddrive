-- name: RequestConversion :one
SELECT * FROM request_conversion(sqlc.arg(source_version_id)::uuid,sqlc.arg(recipe_fingerprint)::text,sqlc.arg(recipe_snapshot)::jsonb);
-- name: GetConversionJob :one
SELECT * FROM conversion_jobs WHERE id = $1;
-- name: GetLatestConversionJob :one
SELECT * FROM conversion_jobs WHERE source_version_id = $1 ORDER BY generation DESC LIMIT 1;
-- name: ClaimConversionJobs :many
WITH claim AS (
    SELECT j.id FROM conversion_jobs j JOIN file_versions v ON v.id = j.source_version_id JOIN files f ON f.id = v.file_id
    JOIN principals p ON p.id = f.owner_id
    WHERE ((j.state IN ('queued','partial') AND j.retry_at <= now()) OR (j.state = 'processing' AND j.lease_until < now()))
    AND j.attempt < j.max_attempts AND p.status = 'active' AND file_is_active(f.id)
    ORDER BY j.retry_at,j.id FOR UPDATE OF j SKIP LOCKED LIMIT sqlc.arg(batch_size)::integer
)
UPDATE conversion_jobs j SET state = 'processing',attempt = j.attempt + 1,lease_token = j.lease_token + 1,
lease_owner = sqlc.arg(worker_id)::text,lease_until = now() + (sqlc.arg(lease_seconds)::integer * interval '1 second'),updated_at = now()
FROM claim WHERE j.id = claim.id RETURNING j.*;
-- name: RenewConversionLease :execrows
UPDATE conversion_jobs SET lease_until = now() + (sqlc.arg(lease_seconds)::integer * interval '1 second'),updated_at = now()
WHERE id = sqlc.arg(job_id)::uuid AND lease_token = sqlc.arg(lease_token)::bigint AND lease_owner = sqlc.arg(worker_id)::text
AND state = 'processing' AND lease_until > now();
-- name: FinishFailedConversion :execrows
UPDATE conversion_jobs SET state = sqlc.arg(state)::text,error_code = sqlc.arg(error_code)::text,error_detail = sqlc.arg(error_detail)::jsonb,
lease_owner = NULL,lease_until = NULL,updated_at = now()
WHERE id = sqlc.arg(job_id)::uuid AND lease_token = sqlc.arg(lease_token)::bigint AND state = 'processing' AND lease_until > now();
-- name: RetryConversion :execrows
UPDATE conversion_jobs SET state = 'queued',retry_at = sqlc.arg(retry_at)::timestamptz,error_code = sqlc.arg(error_code)::text,
lease_owner = NULL,lease_until = NULL,updated_at = now()
WHERE id = sqlc.arg(job_id)::uuid AND lease_token = sqlc.arg(lease_token)::bigint AND state = 'processing' AND lease_until > now() AND attempt < max_attempts;
-- name: ExpireExhaustedJobs :execrows
UPDATE conversion_jobs SET state = 'failed',error_code = 'attempts_exhausted',lease_owner = NULL,lease_until = NULL,updated_at = now()
WHERE state = 'processing' AND lease_until < now() AND attempt >= max_attempts;
-- name: SavePageResult :one
INSERT INTO conversion_page_results(job_id,page_no,stage,attempt,lease_token,state,result_ref,error_code)
VALUES($1,$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT(job_id,page_no,stage,attempt) DO UPDATE SET state = EXCLUDED.state,result_ref = EXCLUDED.result_ref,error_code = EXCLUDED.error_code,recorded_at=clock_timestamp()
RETURNING *;
-- name: ListPageResults :many
SELECT * FROM conversion_page_results WHERE job_id = $1 ORDER BY page_no,attempt DESC,recorded_at DESC,stage;
-- name: CreateArtifactSet :one
INSERT INTO artifact_sets(id,source_version_id,generation,recipe_fingerprint,recipe_snapshot,manifest_key,markdown_key,manifest_sha256,markdown_sha256,completeness,attempt)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING *;
-- name: SetArtifactIndexTask :execrows
UPDATE artifact_sets SET index_task_id = $2,index_state = 'enqueued'
WHERE id = $1 AND published_at IS NULL;
-- name: MarkArtifactIndexed :execrows
UPDATE artifact_sets SET index_state = 'succeeded',indexed_at = now()
WHERE id = $1 AND index_task_id = $2 AND published_at IS NULL;
-- name: PublishArtifact :one
SELECT publish_artifact(sqlc.arg(job_id)::uuid,sqlc.arg(lease_token)::bigint,sqlc.arg(artifact_set_id)::uuid)::boolean AS published;
-- name: GetArtifactForGeneration :one
SELECT * FROM artifact_sets WHERE source_version_id = $1 AND generation = $2 ORDER BY attempt DESC LIMIT 1;
-- name: ClaimConversionJob :one
WITH claim AS (
    SELECT j.id FROM conversion_jobs j JOIN file_versions v ON v.id = j.source_version_id JOIN files f ON f.id = v.file_id
    JOIN principals p ON p.id = f.owner_id WHERE j.id = sqlc.arg(job_id)::uuid
    AND ((j.state IN ('queued','partial') AND j.retry_at <= now()) OR (j.state = 'processing' AND j.lease_until < now()))
    AND j.attempt < j.max_attempts AND p.status = 'active' AND file_is_active(f.id)
    FOR UPDATE OF j SKIP LOCKED
)
UPDATE conversion_jobs j SET state = 'processing',attempt = j.attempt + 1,lease_token = j.lease_token + 1,
lease_owner = sqlc.arg(worker_id)::text,lease_until = now() + (sqlc.arg(lease_seconds)::integer * interval '1 second'),updated_at = now()
FROM claim WHERE j.id = claim.id RETURNING j.*;
