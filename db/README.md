# PostgreSQL persistence

Requires PostgreSQL 16 or newer (tested with 17.11), UTF-8, and sqlc 1.29.0. Apply numbered `.up.sql` migrations once, each in a transaction. There are deliberately no destructive down migrations: source versions, published conversion outputs, feedback, and their identifying metadata have a permanent retention requirement. Restore from an operator-controlled backup for an infrastructure rollback; never drop historical tables to undo an application deployment.

## Generated client

Run `sqlc generate` from the repository root. `sqlc.yaml` and `db/queries/*.sql` generate `internal/dbgen` using pgx/v5. Generated files are checked in. CI should regenerate and fail on a diff. The application may additionally issue parameterized transactional queries, but the database functions below own critical state transitions.

## Transaction boundaries

1. Upload admission: start one pgx transaction; create a provisional file if needed; call `begin_file_operation`; create `upload_sessions`; call `reserve_upload_quota`; commit. Admission failure must roll back the entire transaction. The native upload starts only after durable admission. Never keep this transaction open while bytes upload.
2. Use `SELECT * FROM begin_file_operation(...)` and the other composite-returning functions. Do not try to read inserted rows through an outer table scan in the same statement: that scan uses an earlier snapshot.
3. Record `prepared -> executing` before the official oCIS mutation. A lost response becomes `outcome_unknown`, not a failure. The active operation and its held quota remain until authoritative reconciliation provides evidence.
4. After verifying native revision, length, resource identity, and integrity, transition to `remote_confirmed`; call `commit_content_operation`. It atomically creates the immutable version, advances the file projection, applies the exact size delta, resolves the quota reservation, and creates a conversion job plus outbox events. Repeated committed calls return the original version.
5. `release_upload_quota` works only after `failed_confirmed`. Expiry, transport timeout, or a failed client response does not establish this state. Repeated releases are no-ops.
6. Directory operations use the same space advisory lock and durable operation intent. Acquire `pg_advisory_xact_lock(hashtextextended(space_id::text, 0))` before checking destination-name/path reservations. Hold it only for short local transactions. Reserve destination paths in unresolved operation intents; check both live files and those intents. A pending operation on any ancestor/descendant excludes overlapping subtree work.
7. After a confirmed native MOVE, mark the operation committed and update projection/lifecycle/path revisions in the same transaction. Projections must never be updated speculatively. Account and file deletion transitions disable shares; restoration never reactivates a grant.

## Conversion and outbox fencing

`request_conversion(version_id, fingerprint, snapshot)` allocates a monotonically increasing generation and durable outbox record together. Store only the sanitized recipe snapshot, not credentials or signed URLs. `ClaimConversionJobs` and `ClaimOutbox` use `FOR UPDATE SKIP LOCKED`; every claim increments a fence. Only the live fence may renew, retry, complete, or publish. Expired workers cannot regain authority merely by finishing late. Every attempt writes a distinct temporary object prefix.

Final artifact metadata is immutable. Wait for the actual Meilisearch task to reach `succeeded`, not just receive a task ID. Then `publish_artifact(job_id, lease_token, artifact_set_id)` checks the active fence, exact source/generation/recipe, successful index status, latest requested generation, file lifecycle and account status. Pointer priority is `(source version sequence, conversion generation)`; older work can be retained but cannot replace a newer searchable source. Published historical artifacts remain available for fixed references. Queue ACK occurs only after the associated database state is durable. JetStream message IDs should be `outbox.id` to deduplicate redelivery; the PostgreSQL state remains authoritative.

## Access and feedback

`file_is_active` checks all ancestors and account status and hides provisional native resources. The application must still check the authoritative identity provider, scope, owner, and source-version authorization on every resource request. It must never return unverified search candidate counts or shared content through the owner-only Agent path. Anonymous preview tickets are not original-download grants.

`share_approvals.request_body` fixes `audience_type`, `audience_id` (null for a link), `rights`, `version_policy` (`latest` or `fixed`), and `version_id` (null for latest). Approve only through an authenticated interactive user session, never an Agent scope. Grant insertion checks the approved fields. Link/session/ticket tokens are SHA-256 digests in the database, never plaintext.

`feedback` intentionally has exactly eight columns and only a UTC date. Only `triage_state` is mutable. No caller identity, precise timestamp, session, request, trace, or operation reference may be added. The feedback handler and gateway path must independently disable identifiable logs/tracing. Do not publish feedback through the normal outbox. The schema alone cannot guarantee that proxy, connector, or client logs are anonymous.

## Tests

Run `DATABASE_URL=postgres://... db/tests/run.sh` against a development PostgreSQL database. The runner creates and drops a uniquely named isolated test schema; it does not reset `public`. Tests execute real functions and constraints, including quota deltas, immutable history, unknown remote outcomes, fencing, publish priority, sharing revocation, anonymous feedback, and ancestor access gates. External oCIS, OAuth, Peri, and the 10 GiB tus acceptance checks remain separate integration gates.
