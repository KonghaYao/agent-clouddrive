# Self-test guide and evidence

## Scope and honest stopping point

`scripts/selftest.sh` verifies the source, generated query code, browser application build, converter behavior, and configured native integrations. A passing source run is **not** equivalent to acceptance of a complete Docker Compose deployment or a 10 GiB browser transfer. Missing integration endpoints and optional browser/OCR checks are printed as `SKIPPED`, not `PASS`.

Use isolated development services only. Integration tests create temporary database schemas, native files, search indexes, and object-store buckets. Do not point them at production accounts or production data.

## Run from a clean source tree

Prerequisites: Go 1.25+, a C compiler for Go's race detector, Node/npm compatible with the locked Vite version, Python 3.12, `diff`, and network access to the standard package registries. Python PDF rendering may also require the platform libraries needed by the pinned PDF/image packages. All commands run from the repository root:

```sh
./scripts/selftest.sh
```

The script runs:

1. `go test -race -count=1 -v ./...` and `go vet ./...` with module files read-only.
2. sqlc v1.29.0 generation in a disposable copy, followed by a recursive diff against `internal/dbgen`. This works without a `.git` directory and never regenerates over the delivered source.
3. PostgreSQL migration/function invariants in a temporary schema when `DRIVE_TEST_DATABASE_URL` is supplied.
4. `npm ci`, frontend unit tests, TypeScript checking, and a Vite production build in an isolated source copy, without disturbing existing frontend dependencies or build output.
5. Python converter/security and deployment-generation tests. By default it creates an isolated environment with the pinned converter requirements and exact test helpers.
6. Optional real CPU OCR and real Chromium smoke checks only when explicitly enabled.

Logs go to `evidence/selftest/source-checks.log`. `SELFTEST_LOG_DIR` selects another directory. The runner does not print service credentials or dump environment variables. Treat logs as local test artifacts and review before sharing.

Useful tool overrides are `GO_BIN`, `PYTHON_BIN`, `SQLC_BIN`, and `PSQL`. A pinned sqlc binary is installed into temporary storage if no matching binary is supplied. `SELFTEST_USE_CURRENT_PYTHON=1` uses an already prepared Python environment instead of creating one; that environment must contain the pinned requirements plus `reportlab==4.4.9` and `PyYAML==6.0.3`.

## Enable native test layers

Supply only endpoints for disposable local services:

| Environment variable | Enables |
| --- | --- |
| `DRIVE_TEST_DATABASE_URL` | Actual PostgreSQL migrations, database invariants, file mutations, quota and upload admission, privacy checks, encrypted refresh-token rotation |
| `DRIVE_TEST_NATS_URL` | Actual JetStream portion of worker tests |
| `DRIVE_TEST_MEILI_URL` and optional `DRIVE_TEST_MEILI_KEY` | Actual Meilisearch async indexing/search portion |
| `DRIVE_TEST_S3_URL`, `DRIVE_TEST_S3_ACCESS_KEY`, `DRIVE_TEST_S3_SECRET_KEY` | Actual S3-compatible artifact operations |
| `DRIVE_TEST_OCIS_URL`, `DRIVE_TEST_OCIS_USERNAME`, `DRIVE_TEST_OCIS_PASSWORD` | Actual native oCIS adapter checks against a test account |
| `DRIVE_TEST_OCIS_CA_FILE` | Trust the explicitly supplied test CA without disabling TLS verification |
| `RUN_REAL_OCR=1`, `PADDLE_MODELS_DIR` | Real local PaddleOCR model inference |
| `SELFTEST_BROWSER=1`, optional `CHROMIUM_PATH` | Real Chromium login/scope-selection/PKCE smoke test |

`SELFTEST_REQUIRE_NATIVE=1` fails immediately if any of the PostgreSQL, NATS, Meilisearch, S3, or oCIS endpoint variables are absent. It does not certify Keycloak, Peri, preview, or the complete Compose stack. SQL tests also need the `psql` client. The worker's native integration needs PostgreSQL, NATS, and Meilisearch together. Individual unit tests remain clearly distinguished from integration tests in verbose output.

The Chromium smoke intentionally retains the Chromium sandbox. It uses a controlled identity-discovery fixture to inspect the outbound PKCE authorization request; it does not mint identity tokens, simulate a logged-in account, or prove the live identity-provider login flow.

## Executed domain and database-backed application checks

`evidence/domain-mutations-upload-tests.log` records a real PostgreSQL 17 run with Go's race detector. The application tests use an explicitly labeled original-store contract implementation. They prove the database/application protocol, not native oCIS transport semantics.

Confirmed checks include:

- NFC-normalized, case-sensitive paths; traversal, invalid UTF-8, control characters, and reserved namespaces rejected.
- Ownership, authoritative account status, scopes, ancestry lifecycle, explicit audit access, shared-version restrictions, and Agent exclusion from received shares and share approval.
- Immutable share request hashes, expiry, recipient/permission substitution rejection, and high-entropy link-token digests.
- Quota delta reservations, overflow handling, zero-growth recovery after a quota reduction, and no release while a remote outcome is unknown.
- Source/file/citation identity separation, optimistic revision checks, legal operation transitions, completeness reporting, conversion lease fencing, and monotonic publication priority.
- Native intent recorded before mutation, stable identity on rename, ancestor gates during deletion, restore path conflicts, and independent batch results with blocked dependencies.
- Lost MOVE response reconciled from native target evidence without replaying MOVE.
- Two simultaneous overwrites admitted through a barrier: exactly one reaches native upload creation, while the other receives `OPERATION_PENDING`; the unresolved gate remains after tus creation and until final commit.
- Same-key admission and finalization replay reuse one upload/source version; mismatched request bodies conflict.
- Both explicit unknown outcomes and plain connection-loss errors retain reservation and file gate; the database rejects premature release.
- Definitively failed new uploads/directories relinquish the live path while retaining their operation history.
- Trashing and restoring an uploaded file preserve logical quota, source versions, and anonymous feedback without double charging.
- Anonymous feedback authentication/submission does not update principal activity or emit a correlating outbox record, including rejected disabled-account requests.

Additional evidence files cover SQL invariants, native adapters, share-lifecycle races, refresh-token rotation, and the real PostgreSQL + NATS + Meilisearch worker pipeline. Read each file's integration boundary before treating it as an end-to-end claim.

## Remaining acceptance boundaries

The execution environment used for the implementation does not provide a usable Docker daemon, so successful static Compose generation/validation is not reported as a complete container-stack deployment. Native binaries were used for the recorded service integration tests.

Actual browser execution depends on whether a sandboxed Chromium launch is permitted by the host. A missing/blocked browser must remain a blocked check; disabling the browser sandbox is not a workaround used by this project.

Live Keycloak/OIDC delegation and account-state wiring, Peri interoperability, kkFileView controlled preview, configured external vision-provider behavior, and the one-time 10 GiB interrupted/resumed Uppy+tus transfer still require their specific environment and evidence. Native oCIS overwrite behavior must be interpreted together with the application's exclusive operation gate; a native tus preflight condition alone is not asserted to protect the later upload commit.

Unknown MKCOL outcomes without a proven native resource ID stay gated for operational reconciliation. The application never blindly repeats the create or adopts an unrelated existing directory. Existing known-ID MOVE outcomes can be reconciled automatically after destination identity and retained source reads are verified.
