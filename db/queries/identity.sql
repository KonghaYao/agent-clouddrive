-- name: GetPrincipal :one
SELECT * FROM principals WHERE id = $1;
-- name: GetPrincipalByIdentity :one
SELECT * FROM principals WHERE issuer = $1 AND subject = $2;
-- name: UpsertPrincipal :one
INSERT INTO principals(id,issuer,subject,display_name) VALUES($1,$2,$3,$4)
ON CONFLICT (issuer,subject) DO UPDATE SET display_name = EXCLUDED.display_name, updated_at = now() RETURNING *;
-- name: SetPrincipalStatus :one
UPDATE principals SET status = $2, updated_at = now() WHERE id = $1 RETURNING *;
-- name: CreateSpace :one
INSERT INTO spaces(id,owner_id,native_instance,native_space_id) VALUES($1,$2,$3,$4) RETURNING *;
-- name: GetOwnerSpace :one
SELECT * FROM spaces WHERE owner_id = $1;
-- name: CreateQuotaAccount :exec
INSERT INTO quota_accounts(owner_id,limit_bytes) VALUES($1,$2) ON CONFLICT(owner_id) DO NOTHING;
-- name: GetQuotaAccount :one
SELECT * FROM quota_accounts WHERE owner_id = $1;
-- name: CreateWebSession :exec
INSERT INTO web_sessions(token_hash,csrf_hash,principal_id,expires_at) VALUES($1,$2,$3,$4);
-- name: GetWebSession :one
SELECT s.* FROM web_sessions s JOIN principals p ON p.id = s.principal_id
WHERE s.token_hash = $1 AND s.expires_at > now() AND p.status = 'active';
-- name: DeleteWebSession :exec
DELETE FROM web_sessions WHERE token_hash = $1;
-- name: PruneExpiredSessions :exec
DELETE FROM web_sessions WHERE expires_at < now();
-- name: CreateAccessTicket :exec
INSERT INTO access_tickets(token_hash,principal_id,share_grant_id,file_id,version_id,artifact_set_id,scope,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8);
-- name: GetAccessTicket :one
SELECT * FROM access_tickets WHERE token_hash = $1 AND expires_at > now();
-- name: PruneExpiredTickets :exec
DELETE FROM access_tickets WHERE expires_at < now();
