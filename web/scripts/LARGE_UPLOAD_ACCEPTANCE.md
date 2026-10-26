# Real-browser 10 GiB interrupted-upload acceptance

This is an executable driver, **not a record that the test passed**. The current
workspace cannot run normally sandboxed Chromium, and no browser was started to
test this driver. Neither unit tests, check-only output, nor a smaller upload
satisfies engineering handoff §18.2. Never add `--no-sandbox`, disable TLS checks,
route mocks, or replace the browser upload with a Python/native tus uploader.

## Requirements and scope

Run from the repository root on a suitable operator-controlled test host with:

- Node.js 24, `npm ci` completed in `web`, a normally sandboxed Chromium, `docker`
  and `psql` executables on PATH.
- An actual **disposable**, single-node Compose deployment in the default S3 mode,
  official oCIS `s3ng`, RustFS, and the real compiled web UI. The driver requires
  explicit permission for writes and the fixture's OIDC consent. Never use a real
  user or production data. It does not clean up retained originals or purge data.
- An idle dedicated Keycloak test user, all browser scopes, no existing held
  uploads, and at least 10,737,418,240 bytes of remaining business quota. Native
  quota and storage must also permit the upload and retained versions.
- A unique source basename not already in the test user's root. The actual input
  must exist on disk at **exactly 10,737,418,240 bytes**. Allow substantial spare
  capacity for the source, native temporary chunks, final S3 object and history.
- Read-only access to the same application's PostgreSQL tables (upload_sessions,
  file_operations, files, file_versions, quota_reservations), Docker inspection
  and `docker exec <gateway> nginx -T`, and S3 ListBucket/GetObject metadata for
  the originals bucket. The DB transaction is explicitly read-only.
- The runner must reach RustFS through an authorized private/operator endpoint.
  The shipped Compose deliberately does not publish native services. Run the
  test from a host/network where these are already privately reachable, or use
  an authorized test-only loopback mapping. Do not expose native storage publicly.
  The host-side `probe_endpoint` must use verified HTTPS, or HTTP on exact
  `localhost`, `127.0.0.1` or `::1`; signed S3 authorization is never sent over
  non-loopback cleartext HTTP. The compare-only `native_endpoint` may remain
  the Compose-internal `http://rustfs:9000` origin.

The declared component versions are operator provenance, not a self-verifying
version attestation. The report separately records the actual Chromium version,
container image IDs/tags/start times, whitelisted effective storage settings and
live gateway configuration digest. No supplied declaration alone can pass S3.
The driver performs real signed S3 LIST and HEAD and requires exactly one new
exact-size object with a multi-part ETag after the transfer. Use a dedicated
bucket/deployment so another simultaneous exact-size object cannot be mistaken
for this test. S3 implementations without multipart ETags will fail this check
rather than silently accepting configuration as proof.

## Prepare a reproducible real input

```sh
node web/scripts/large-upload-source.mjs --output /private/test-input/uppy-10gib-unique.bin
```

The helper writes a deterministic repeated 1 MiB xorshift32 block, syncs it, prints
its exact size and streaming SHA256, and refuses to overwrite an existing file.
It is non-sparse. The driver independently re-reads and hashes the disk file before
selecting it in Uppy. Both paths keep memory bounded. Input creation itself is
never acceptance evidence.

Copy `web/scripts/fixtures/large-upload-deployment.example.json` to a private test
configuration and replace the actual container IDs, project, URLs and versions.
`docker compose -f deploy/compose/compose.yaml ps -q api` (likewise `ocis`,
`rustfs`, `gateway`) identifies the containers. The manifest is not credentials
storage. Actual Compose labels, mode, driver, bucket, endpoint, multipart settings
and running state must match the live Docker inspection.

Use private (mode 0600) files for the password and S3 credentials; do not put their
contents in shell history or the report. Set these environment variables:

```sh
export TEST_PUBLIC_URL=http://drive.localhost:8080
export TEST_OIDC_ISSUER=http://drive.localhost:8080/identity/realms/clouddrive
export TEST_USERNAME=disposable-large-upload-user
export TEST_PASSWORD_FILE=/private/test-secrets/user-password
export TEST_WEB_CLIENT_ID=clouddrive-web
export CHROMIUM_PATH=/usr/bin/chromium
export LARGE_UPLOAD_SOURCE_FILE=/private/test-input/uppy-10gib-unique.bin
export LARGE_UPLOAD_DEPLOYMENT_FILE=/private/large-upload-deployment.json
export LARGE_UPLOAD_S3_ACCESS_KEY_FILE=/private/test-secrets/s3-access-key
export LARGE_UPLOAD_S3_SECRET_KEY_FILE=/private/test-secrets/s3-secret-key
export ALLOW_DISPOSABLE_TEST_WRITES=yes
export ALLOW_DISPOSABLE_TEST_OIDC_CONSENT=yes
```

Also provide `LARGE_UPLOAD_DATABASE_URL` securely to the process environment. Its
value is passed to `psql` through PGDATABASE, never command-line arguments or
report output. A read-only database role is recommended in addition to the
transaction enforcement. Clear DEBUG, PWDEBUG and NODE_DEBUG: verbose protocol
traces can expose passwords or authorization codes and are rejected.
`NODE_TLS_REJECT_UNAUTHORIZED=0` is also rejected before credentials are read.
Errors are recorded as static failure codes, not raw Playwright messages/stacks.

Optional: `LARGE_UPLOAD_TIMEOUT_MS` defaults to 7,200,000 (two hours; accepted
range one minute–24 hours), `LARGE_UPLOAD_OFFLINE_MS` defaults to 8,000 (accepted
range 5–30 seconds), and `LARGE_UPLOAD_HEADED=yes` uses a visible browser. The
offline setting affects only the browser context, not the receiver or S3 service.

## Commands and honest exit status

```sh
node web/scripts/large-upload-acceptance.mjs --check-only --output evidence/uppy-10gib-check.json
node web/scripts/large-upload-acceptance.mjs --run --output evidence/uppy-10gib-run.json
node --test web/scripts/large-upload-unit.mjs
```

- `--check-only` reads local config/file metadata only. **No network, Docker,
  psql, source hash or browser execution** occurs. Missing/invalid config exits 2;
  valid local config exits 3. Both emit `execution: not_run`, `status: not_run`,
  every case `not_run`, and `accepts_10gib: false`. It never exits 0 as E2E proof.
- `--run` exits 0 only after all assertions pass, 1 for an executed failed case,
  or 2 for incomplete/invalid local configuration. The JSON declares every case
  ahead of time. Cases not reached stay `not_run`, not passed or silently skipped.
- Use a new evidence output filename on each run. Existing files, leaf symlinks
  and configured input paths are refused before any network/browser execution;
  the output descriptor is reserved exclusively and reused for progress updates.
  Evidence is written mode 0600. No HAR, trace, storage state, raw auth headers,
  cookies, password, DB DSN or S3 signing secret is persisted. The native upload
  reference and S3 object key are recorded as SHA256, not raw internal URLs/keys.
- Failure does not authorize blind retries, a fresh upload, or fixture deletion.
  Inspect the recorded session/operation and unknown outcome before starting
  another run with a new unique source basename.

## What a successful run actually asserts

1. Live Compose/S3/gateway configuration and streaming paths; source size/hash.
2. Real Keycloak authorization-code + PKCE login, real `/v1/me`, genuine rendered
   Uppy dashboard browse/filechooser selection of the disk file and upload button.
3. Successful browser HTTP 204 PATCH at a positive partial offset, followed by
   `context.setOffline(true)`, an observed failed tus browser request, and network
   restoration without closing the browser or receiver.
4. A successful post-reconnection HEAD on the same controlled session with a
   positive offset, then real successful PATCH from that offset (never zero),
   ultimately acknowledging the exact total bytes. No injected HTTP stack or
   direct driver upload is used. Uppy's visible retry button is used only if its
   actual retry UI appears, at most once, for the existing session.
5. A single UI finalize and saved UI state; one file/list entry and one retained
   source version whose server SHA256 equals the input. Only after saved, the
   upload panel is closed through its normal confirmation so the real refresh
   button can keep subsequent read probes on a current browser token. An explicit identical
   finalize replay returns the same file/version without adding a source version.
6. Read-only DB snapshots prove the same upload/operation/file/native reference
   and receiver node; exactly one committed reservation and version. `/v1/me`
   proves the initial reservation, one committed size delta and unchanged replay.
7. A real streaming GET through the formal authenticated version-pinned
   `/v1/files/:id/content?version_id=:id` endpoint, without redirects or whole-file
   buffering, hashes every returned byte and compares exact length and SHA256.
   This downloads through Node's bounded stream using the live browser session;
   it does **not** claim to test the Chromium File System Access save dialog.
8. Live S3 new-object HEAD/multipart proof; an above-limit original has explicit
   `not_converted` / `limit_exceeded` rather than an upload failure; receiver
   container IDs/start times remain unchanged and the browser/page remain open.

The driver records chronological HTTP method/status/session/offset events, failure
and reconnect times, browser/build declarations, live backend configuration,
source/version IDs and hashes, quota snapshots, and streamed download timing and
memory-high-water metadata. It is a correctness test, not a throughput benchmark.

## Explicitly separate small smoke

```sh
node web/scripts/large-upload-source.mjs --output /private/test-input/uppy-smoke-unique.bin --smoke-bytes 134217728
# Point LARGE_UPLOAD_SOURCE_FILE at that new file, keeping all real-deployment safeguards.
node web/scripts/large-upload-acceptance.mjs --run --smoke-bytes 134217728 --output evidence/uppy-small-smoke.json
```

Smoke inputs must be 96 MiB–1 GiB so interruption occurs between real chunks.
The suite name becomes `uppy-small-smoke`, mode is `small_smoke_not_10gib`, and
`accepts_10gib` remains false even if every smoke assertion passes. No automatic
fallback to a smaller file exists. Unit-test evidence objects are intentionally
manufactured and only test validators, never the browser/deployment gate.
