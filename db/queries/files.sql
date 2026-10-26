-- name: CreateFileProjection :one
INSERT INTO files(id,owner_id,space_id,parent_id,name,path,kind,native_instance,native_resource_id,content_type)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING *;
-- name: GetFile :one
SELECT * FROM files WHERE id = $1;
-- name: LockFile :one
SELECT * FROM files WHERE id = $1 FOR UPDATE;
-- name: GetOwnedActiveFile :one
SELECT f.* FROM files f WHERE f.id = $1 AND f.owner_id = $2 AND file_is_active(f.id);
-- name: ListOwnedActiveFiles :many
SELECT f.* FROM files f WHERE f.owner_id = $1 AND f.parent_id IS NOT DISTINCT FROM sqlc.narg(parent_id)::uuid
AND file_is_active(f.id) AND (f.name,f.id) > (sqlc.arg(after_name)::text,sqlc.arg(after_id)::uuid)
ORDER BY f.name,f.id LIMIT sqlc.arg(page_limit)::integer;
-- name: GetOwnedVersion :one
SELECT v.* FROM file_versions v JOIN files f ON f.id = v.file_id JOIN principals p ON p.id = f.owner_id
WHERE v.id = $1 AND f.owner_id = $2 AND p.status = 'active';
-- name: ListFileVersions :many
SELECT * FROM file_versions WHERE file_id = $1 ORDER BY version_seq DESC;
-- name: GetArtifactSet :one
SELECT * FROM artifact_sets WHERE id = $1 AND published_at IS NOT NULL;
-- name: ValidateSearchCandidate :one
SELECT f.*, v.id AS returned_version_id, v.version_seq, a.completeness, a.markdown_key,
       v.id <> f.current_version_id AS stale
FROM files f JOIN artifact_sets a ON a.id = f.search_artifact_set_id JOIN file_versions v ON v.id = a.source_version_id
WHERE f.id = $1 AND f.owner_id = $2 AND a.id = $3 AND file_is_active(f.id);
-- name: ListSubtree :many
WITH RECURSIVE subtree AS (
    SELECT f.* FROM files f WHERE f.id = $1
    UNION ALL SELECT f.* FROM files f JOIN subtree s ON f.parent_id = s.id
) SELECT * FROM subtree ORDER BY path;
-- name: SetFileLifecycle :one
UPDATE files SET lifecycle = $2, updated_at = now() WHERE id = $1 AND active_operation_id = $3 RETURNING *;
-- name: CommitFileMetadata :one
UPDATE files SET parent_id = $2,name = $3,path = $4,lifecycle = $5,metadata_revision = metadata_revision + 1,
active_operation_id = NULL,updated_at = now()
WHERE id = $1 AND active_operation_id = $6 AND metadata_revision = $7 RETURNING *;
-- name: UpdateDescendantPaths :execrows
UPDATE files SET metadata_revision = metadata_revision + 1,path = sqlc.arg(new_prefix)::text || substring(path FROM length(sqlc.arg(old_prefix)::text) + 1),updated_at = now()
WHERE space_id = sqlc.arg(space_id)::uuid AND left(path,length(sqlc.arg(old_prefix)::text) + 1) = sqlc.arg(old_prefix)::text || '/';
-- name: CreateTrashEntry :one
INSERT INTO trash_entries(id,root_file_id,operation_id,original_parent_id,original_name,original_path,reserved_location)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING *;
-- name: GetTrashEntry :one
SELECT t.* FROM trash_entries t JOIN files f ON f.id = t.root_file_id WHERE t.id = $1 AND f.owner_id = $2;
-- name: ListTrashEntries :many
SELECT t.* FROM trash_entries t JOIN files f ON f.id = t.root_file_id WHERE f.owner_id = $1 AND t.state NOT IN ('restored','failed_confirmed') ORDER BY deleted_at DESC;
-- name: SetTrashState :one
UPDATE trash_entries SET state = $2,restored_at = CASE WHEN $2 = 'restored' THEN now() ELSE NULL END WHERE id = $1 RETURNING *;
