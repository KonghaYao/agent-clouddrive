-- name: CreateShareApproval :one
INSERT INTO share_approvals(id,requester_principal,file_id,request_hash,request_body,expires_at) VALUES($1,$2,$3,$4,$5,$6) RETURNING *;
-- name: GetShareApproval :one
SELECT * FROM share_approvals WHERE id = $1;
-- name: ApproveShareRequest :one
UPDATE share_approvals SET state = 'approved',approved_by = $2,decided_at = now()
WHERE id = $1 AND state = 'pending' AND expires_at > now() AND request_hash = $3 RETURNING *;
-- name: RejectShareRequest :execrows
UPDATE share_approvals SET state = 'rejected',decided_at = now() WHERE id = $1 AND state = 'pending';
-- name: ExpireShareRequests :execrows
UPDATE share_approvals SET state = 'expired',decided_at = now() WHERE state = 'pending' AND expires_at <= now();
-- name: CreateShareGrant :one
INSERT INTO share_grants(id,approval_id,file_id,creator_principal_id,audience_type,audience_id,token_hash,rights,version_policy,version_id,approved_by,expires_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING *;
-- name: GetActiveShareGrant :one
SELECT g.* FROM share_grants g JOIN principals p ON p.id = g.creator_principal_id
WHERE g.id = $1 AND g.state = 'active' AND share_identity_valid(g.id) AND p.status = 'active' AND (g.expires_at IS NULL OR g.expires_at > now()) AND file_is_active(g.file_id);
-- name: GetActivePublicShare :one
SELECT g.* FROM share_grants g JOIN principals p ON p.id = g.creator_principal_id
WHERE g.token_hash = $1 AND g.audience_type = 'link' AND g.state = 'active' AND share_identity_valid(g.id) AND p.status = 'active'
AND (g.expires_at IS NULL OR g.expires_at > now()) AND file_is_active(g.file_id);
-- name: ListOwnedShareGrants :many
SELECT * FROM share_grants WHERE creator_principal_id = $1 ORDER BY created_at DESC;
-- name: RevokeShareGrant :execrows
UPDATE share_grants SET state = 'revoked',disabled_reason = 'owner_revoked',revoked_at = now()
WHERE id = $1 AND creator_principal_id = $2 AND state = 'active';
-- name: ListDirectReceivedShares :many
SELECT g.* FROM share_grants g WHERE g.audience_type = 'user' AND g.audience_id = $1 AND g.state = 'active' AND share_identity_valid(g.id)
AND (g.expires_at IS NULL OR g.expires_at > now()) AND file_is_active(g.file_id) ORDER BY g.created_at DESC;
-- This is the ONLY feedback write path. Do not wrap in audit/outbox/tracing middleware.
-- name: SubmitAnonymousFeedback :one
INSERT INTO feedback(id,file_id,version_id,artifact_set_id,category,body) VALUES($1,$2,$3,$4,$5,$6) RETURNING id;
-- Explicit support authorization is required in the service; never expose via Agent read.
-- name: ListFeedbackForTriage :many
SELECT * FROM feedback WHERE triage_state = $1 ORDER BY received_day,id LIMIT $2;
-- name: TriageFeedback :execrows
UPDATE feedback SET triage_state = $2 WHERE id = $1;
