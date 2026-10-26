# Infrastructure adapters and verification boundaries

## Interfaces

`internal/ports/ports.go` is the infrastructure contract. Business authorization,
PostgreSQL operation exclusion, job fencing and result reauthorization remain in
the application layer. None of these adapters uses a process-local map as the
source of truth for files, sessions, permissions, tasks or grants.

### oCIS

The adapter uses official Graph `/graph/v1.0/me/drives`, id-addressed WebDAV
`/dav/spaces/{native-resource-id}`, tus POST/HEAD/PATCH and
`/dav/meta/{native-resource-id}/v/{native-revision-id}`. Native user access is
request-scoped. Native upload locations are credentials and are never public
business URLs. Redirects and foreign upload origins are rejected.

A source-version pin is the tuple `(instance, space_id, resource_id, etag)`.
Its serialized `revision_id` initially has the form `etag:<native-etag>`. This
is an explicit application encoding, **not** a claim that oCIS returned a native
history ID. Native current content is not in the history listing until replaced.

The verified oCIS 8.2.2 GET endpoint ignores a stale `If-Match` header. Accordingly:

1. The adapter requests the stable resource, never the current business path.
2. Before exposing any body bytes, it compares the response ETag with the pin.
3. A mismatch closes the body, resolves the old revision by matching the ETag in
   the native history listing, then reads that immutable native revision.
4. The history response ETag is checked again before exposing bytes.
5. An absent or mismatched pin fails closed, with no latest-content fallback.

Repeated identical writes still have separate PostgreSQL source-version IDs.
The native ETag pin identifies content, not the application's logical generation.

Native tus `If-Match` is an admission check, **not final commit CAS**. Two native
sessions admitted against the same ETag can both finish. Safe overwrites require
a single durable PostgreSQL active operation for the file for the entire upload,
controlled native access, and no releasing unknown operations before reconciliation.
The `VerifiedConditionalOverwrite` constructor flag means the deployment's
complete serialized business write path was verified; it does not add a native
CAS feature. All three capability flags default to false.

`ReconcileRequest.TransferCompleted` is trusted server-side transport evidence
persisted after the proxy receives the final native PATCH response and resource
ID. It must never be copied from a client's finalize body. Without that evidence,
the adapter checks native tus completion. A size/path match alone is insufficient.

MOVE has `Overwrite: F`, retains the stable resource ID and verifies retained
history for files. Directory subtree lifecycle and every retained child pin are
verified by the application. No original DELETE or native revision purge exists
in this adapter. Mutation transport errors and server 5xx are outcome-unknown.

### Immutable artifacts

Local storage uses Go's `os.Root` traversal-resistant filesystem API. Writes are
bounded, checksummed into a temporary file, fsynced and atomically hard-linked
without replacing existing keys. Symlink escapes are rejected by `os.Root`.
Only `tmp/{job}/{attempt}/...` is writable as an attempt. Final keys are restricted
to `markdown/`, `images/`, `manifests/` and `previews/`. A duplicate final write is
successful only when the existing bytes have the same SHA-256.

The S3 adapter is a narrow standard-library SigV4 client for **bounded derived
objects**, not an alternate oCIS original store. It uses a disk spool rather than
whole-object memory buffering, validates SHA-256 before PUT, sends
`x-amz-checksum-sha256`, and uses `If-None-Match: *`. A 412 is verified against the
existing bytes before being treated as an idempotent result. Final-object deletion
is intentionally absent. Original 10 GiB multipart upload belongs to official
oCIS's storage driver, not this adapter.

### Identity and explicit offline grants

The Keycloak adapter validates RS256 signature/JWKS, issuer, audience, resource,
expiration, not-before and required scopes. It checks authoritative user enabled
status on every protected validation. Group membership is read live. A dependency
failure is never interpreted as enabled. Status and exchange clients are separate.
Standard same-realm exchange requires explicit verification and always requests
`openid`, because oCIS needs Keycloak UserInfo access. The exchanged token's
signature, issuer, audience and subject are independently checked.

The optional delegation wrapper requires separately consented `offline_access`
and `drive.read`. Its confidential background client is separate from the
non-consent exchange client. The wrapper stores only AES-256-GCM-encrypted refresh
tokens with principal-ID associated data. The key is a 32-byte raw secret file,
not a passphrase. Access tokens are not persisted. PostgreSQL row locks serialize
refresh-token rotation. Every background read checks live account status, persisted
grant state, database ownership, the exact native source pin and ancestor lifecycle.
A conversion job also needs a matching live processing lease. Read-only recovery
of an unresolved metadata operation is limited to its recorded owner, source and
live lease. There is no administrator impersonation fallback.

No-job offline reads reject non-active files. Owner history reads of trashed data
use an authenticated request-scoped token or a separately authorized recovery flow.
Revocation wipes the stored refresh ciphertext. Turning an account back on does
not itself reinstate a share or grant revoked by application policy.

### Search and converters

Meilisearch documents are immutable per artifact generation, with stable SHA-256
chunk IDs. UTF-8 chunking preserves provenance and targets 2–4 KiB. All searches
inject an owner filter, reject any response with another owner and return only
untrusted candidates. Raw hit counts and facets are not exposed. Application
PostgreSQL reauthorization is still mandatory. A task UID is not publication:
`Wait` requires the actual `succeeded` state. Path updates are partial document
updates and preserve content.

Converter HTTP is the project's `/v1/capabilities` and `/v1/execute` protocol, not
an assertion that AnyDoc/PaddleOCR implement it natively. Responses are bounded;
asset checksums, page ranges and completeness are checked. False `ready` results
with missing stages, and blank successful conversions, are rejected. Input URLs
are denied unless their origin/path was explicitly configured. The current
worker uses bounded inline conversion input after verifying the pinned original.

## Test evidence

- `evidence/adapters-real-integration.txt`: real Meilisearch 1.55.0 and RustFS
  1.0.3, including async completion, owner isolation, partial path update,
  immutable conditional writes, checksums and Range.
- `evidence/ocis-adapter-real-integration.txt`: real official oCIS 8.2.2,
  resumable tus chunks, stable ID, old-version pin after replacement, Range and
  MOVE/restore with history. Test-only Basic authenticates the test user's own
  account; it does not establish production external-IdP delegation.
- `evidence/delegation-postgres-integration.txt`: real PostgreSQL 17 with a
  mocked IdP endpoint, encrypted persistence, concurrent refresh rotation,
  source isolation, lifecycle/disable denial and revocation. This is not an
  end-to-end OAuth/Keycloak result.
- All `httptest` tests are protocol/negative-contract tests. Opt-in native tests
  explicitly skip when their dependency environment variables are absent.
- The adapter tests are not the Uppy 10 GiB browser interruption acceptance test,
  Peri interoperability test, full Compose boot or multi-node validation.

## Primary technical references

Checked 2026-10-23. Deployment evidence, not documentation alone, determines gates.

- [Official oCIS WebDAV API](https://owncloud.dev/apis/http/webdav/)
- [Official oCIS upload architecture](https://owncloud.dev/architecture/upload-processing/)
- [Official oCIS storage architecture](https://owncloud.dev/ocis/storage/)
- [Keycloak standard token exchange](https://www.keycloak.org/securing-apps/token-exchange)
- [Keycloak Admin REST API](https://www.keycloak.org/docs-api/latest/rest-api/index.html)
- [S3 PutObject conditional writes and integrity](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObject.html)
- [Go os.Root confinement](https://pkg.go.dev/os@go1.25.0#Root)
- [Meilisearch documents](https://www.meilisearch.com/docs/reference/api/documents)
- [Meilisearch asynchronous tasks](https://www.meilisearch.com/docs/reference/api/tasks)

## Application/transport review regressions (2026-10-23)

The original official Go MCP SDK 1.1.0 client was connected to the production
`API.MCPHandler` through `httptest`, with real PostgreSQL 17 and all migrations.
The test initializes, lists exactly 21 tools, reads owned metadata, verifies
missing-resource and insufficient-scope errors, and rejects a nonexistent
share-approval tool. Anonymous feedback is submitted through the actual MCP
route and verified not to change principal, quota, or outbox records. The
feedback row is checked for absence of identity/request-origin columns.
The identity provider in this protocol test is an explicit test fixture;
this is not a real Peri-client or OAuth-provider compatibility claim.

Additional PostgreSQL regression tests cover these reviewed corrections:

- A recipient with only a fixed-v1 share and no explicit version parameter
  receives v1, never the newer current version. Latest-only grants deny
  historical reads, and agents cannot read shared content.
- A public-share request observing an authoritative disabled owner durably
  disables the grant, so immediate account re-enablement does not revive it.
- An early finalize call with a positively incomplete native transfer keeps
  the upload resumable and does not create a source version.

Native storage is a contract fixture in these application regression tests.
Results are recorded in `evidence/mcp-shared-version-postgres.txt`.
No assertion here claims end-to-end suppression of logging by an independently
configured reverse proxy, identity provider, database, or APM product. Deployment
must retain the provided no-request-logging settings and independently review
any added observability layer.

The final source rerun uses official SDK **1.8.0** and explicitly asserts protocol **2026-07-28** negotiation. See `evidence/selftest/source-checks.log`; older logs remain historical evidence, not the current dependency pin.
