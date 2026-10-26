# Worker and persistence verification

Executed 2026-10-23 in the available isolated native runtime, not Docker Compose.

- PostgreSQL 17.11: migrations and actual transactional invariant tests passed.
- sqlc 1.29.0: generation passed; pgx/v5 generated client compiled and is used by the production worker.
- NATS JetStream 2.15.1 and actual Meilisearch: `go test -race` pipeline integration passed with PostgreSQL and immutable local artifact storage.
- Share lifecycle and status synchronization: six tests passed, including twelve concurrent approval/deletion rounds and eight concurrent approval/account-disable rounds, invalidating old pending approvals after delete/restore and disable/re-enable, and enforcing approved expiry.

The worker integration uses explicit in-process identity and original-source contract fixtures. These fixtures verify that worker reads are bound to the exact source revision and job and do not depend on a browser token. They do **not** establish external oCIS, OAuth, Keycloak, or Peri compatibility; those require their own evidence.

Verified worker behavior: durable outbox publication after real JetStream acknowledgement; exact source hash validation; Markdown/manifest immutable publication; real Meilisearch completion before searchable pointer publication; fixed version/artifact/page provenance; over-limit files remain intact and unconverted; deletion removes searchable blocks; restoration rebuilds search from retained artifacts without rerunning conversion; delayed deletion events obey the current lifecycle.

Verified database behavior: latest-only quota with exact growth/shrink delta; idempotent committed operations; unresolved remote outcomes cannot release quota or clear the operation gate; immutable source history and published artifacts; stale worker rejection; source-version/generation publication order; retained anonymous feedback and no source fields; ancestor lifecycle gates; share disabling and no automatic restoration.

The final full application run passed 26 top-level tests plus two subtests with the race detector. It additionally verifies that the explicitly created JetStream durable consumer survives worker shutdown, and that publication events repair source-path labels after a concurrent rename.

Logs: `application-postgres-nats-meili-test.log` (final full run), `worker-pipeline-test.log`, `share-lifecycle-race-test.log`, `share-privacy-status-regression.log`, `feedback-privacy-test.log`, `postgresql-invariants-test.log`.
