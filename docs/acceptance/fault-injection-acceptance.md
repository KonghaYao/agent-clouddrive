# A23: real successful MOVE with a lost response

## Status and boundary

**Prepared, not executed.** `tests/integration/ocis_response_loss_proxy.py` is a
test-only fault injector, not an A23 acceptance driver. No live oCIS fault,
deployed application reconciliation, browser, container, or full acceptance run
is claimed by this document. `tests/test_ocis_fault_proxy.py` uses explicitly
fake in-memory sockets/responses and proves only proxy-unit control flow.
Neither file modifies `docs/acceptance/acceptance.json`, release readiness, or a ZIP.

The existing application has real interfaces for this procedure:

- `PATCH /v1/files/{file_id}/location` submits a move/rename.
- `GET /v1/operations/{operation_id}` reads the owner's operation state with the
  appropriate scope. A returned `outcome_unknown` is not success.
- `cmd/maintenance` runs `Service.ReconcileMutations`: it looks up the exact
  stored destination, requires the previously known native ID, verifies retained
  source-version reads, and commits metadata without sending another MOVE.

The deployed API harness may supply authenticated sessions and fixture helpers,
but its ordinary commit polling does **not** run or certify this fault scenario.
An operator must execute and capture the assertions below. There is no fabricated
operation-result driver or automatic A23 pass report.

## Safety envelope

Use only a dedicated disposable oCIS deployment, application database, account,
and synthetic small file. Do not use production identities or retained user data.
The explicit environment gate is `ALLOW_DISPOSABLE_TEST_FAULTS=yes` and starting
the listener additionally requires `--arm`.

The proxy:

- Binds an IP-literal loopback address only (`127.0.0.1:18923` by default). Place
  it in the same host/network namespace as the calling app or test gateway. Do
  not publish its cleartext loopback listener to a network; it carries bearer
  credentials. IPv6 `::1` is supported explicitly.
- Connects only to the single operator-configured HTTPS oCIS origin. It verifies
  the certificate and hostname with the system trust store or `TEST_OCIS_CA_FILE`.
  There is no insecure-TLS mode, redirect following, CONNECT tunnel, dynamic
  target selection, or use of ambient HTTP proxy environment variables.
- Forwards the app's normal Authorization header only on that fixed connection.
  It discards cookies, Proxy-Authorization and forwarded-host headers. A request
  Host value never determines the connection target.
- Allows bounded GET, HEAD and PROPFIND reads under `/dav/` or `/graph/v1.0/`.
  Its only allowed write is one exact configured native MOVE path to one exact
  configured destination. The destination name must start with `a23-`, the MOVE
  must carry a strong `If-Match` and `Overwrite: F`, and no request body is allowed.
- Denies native DELETE, purge, uploads, MKCOL, COPY and other writes. It does not
  clean up data, purge history, alter application tables or change permissions.
  The MOVE is a reversible fixture rename, not a permanent deletion.
- Consumes its one-shot arm before forwarding the first valid matching MOVE.
  Failed connections/statuses do not silently rearm. Any further matching MOVE
  receives a local 409 and increments the attempt count without being forwarded.
  This count is essential: the guard must not conceal an application replay bug.
- Waits for real upstream HTTP 201 or 204, then shuts down the downstream socket
  **before emitting any downstream HTTP status, interim response, headers or
  body**. Other upstream statuses are never called a successful injection.
- Uses a 20-second deadline and socket timeouts by default (configurable 1–120
  seconds), rejects chunked/Expect framing, and limits request/response bodies to
  8 MiB (configurable up to 16 MiB). This is not a large-upload proxy.
- Disables access logs, request reflection and exception tracebacks. Fault proof
  contains only the method, upstream status and counts. No token, body, native
  path, resource ID, username, cookie or URL is printed.

If an upstream attempt fails or its outcome is uncertain, preserve its state and
inspect the fixture read-only. Do not restart/rearm to repeat that mutation.
Any second run needs a new disposable fixture and separately recorded baseline.

## Preserve native identity when routing through the proxy

The current bootstrap uses `DRIVE_OCIS_INTERNAL_URL` as the stored native instance
identity. Changing that URL after creating a file causes an instance mismatch;
do not edit persisted identities to make the test proceed.

Use a dedicated test gateway as the app's **stable logical oCIS origin from the
start**. Before injection, the gateway routes normally to the real HTTPS oCIS
upstream so the app can create its fixture. After preparation, route that
gateway's `/dav/` and `/graph/v1.0/` paths through this loopback proxy, without
changing the app's `DRIVE_OCIS_INTERNAL_URL` or any stored native references.
Configure the gateway to preserve methods/Authorization, disable request logs,
and **never retry, intercept errors, or replace a lost upstream response with a
synthetic success**. A real EOF or a gateway 502 must reach the application's
adapter; both are an unknown mutation outcome, not a confirmed failure.

`TEST_OCIS_APP_ORIGIN` is that stable logical origin. The incoming MOVE Destination
must equal this origin plus the exact configured destination path; the proxy
rewrites only its origin to the fixed real upstream. The real upstream must not
route back into the fault gateway. The same route must apply to the API and
maintenance processes. Merely adding a proxy while leaving their actual traffic
on another route is not fault evidence.

If no gateway preserving this identity is available, that setup is a blocker,
not permission to modify application rows or claim the scenario ran. In a test
deployment already provisioned with an equivalent stable transport route, retain
that route rather than inventing another origin.

## Operator configuration and local validation

Supply these values through a protected operator environment, not command-line
bearer tokens or shell tracing. The proxy does not need a credential file: the
application supplies its normally delegated credentials on individual requests.

| Variable | Required meaning |
| --- | --- |
| `ALLOW_DISPOSABLE_TEST_FAULTS` | Exactly `yes`, after verifying a disposable deployment |
| `TEST_OCIS_UPSTREAM_URL` | Real fixed HTTPS oCIS origin, with no credentials, query or path |
| `TEST_OCIS_APP_ORIGIN` | Stable logical oCIS origin used by this app; HTTPS, or HTTP loopback only |
| `TEST_OCIS_FAULT_MOVE_PATH` | Exact wire path `/dav/spaces/<known-native-file-resource-id>` |
| `TEST_OCIS_FAULT_DESTINATION_PATH` | Exact `/dav/spaces/<known-native-parent-resource-id>/a23-<unique-run>-moved.txt` |
| `TEST_OCIS_CA_FILE` | Optional test-CA PEM; verification remains enabled |
| `TEST_OCIS_PROXY_BIND`, `TEST_OCIS_PROXY_PORT` | Optional loopback literal and port; default `127.0.0.1:18923` |
| `TEST_OCIS_PROXY_TIMEOUT`, `TEST_OCIS_PROXY_MAX_BODY` | Optional bounded seconds/bytes limits above |

Obtain the native source and parent IDs from the isolated fixture's authenticated
native reads or a narrowly scoped **read-only** database lookup. A logical app UUID
is not a native resource ID. Do not export raw operation intent, token responses,
database connection strings or request headers into evidence.

Run from the repository root:

```sh
python3 tests/integration/ocis_response_loss_proxy.py --check-only
```

Exit 3 with `status: not_run` means configuration validation only. It does not
bind a socket, resolve the upstream, connect to any service, verify a live
certificate, or execute a fault. Missing/invalid setup exits 2. In particular,
`--check-only --arm` still never starts a listener. No network call is performed
merely by importing the module.

After all fixture, route and baseline checks are ready, start it in a protected
terminal and route the dedicated test gateway through it:

```sh
python3 tests/integration/ocis_response_loss_proxy.py --arm
```

The `armed` message still says acceptance is `not_run`. A sanitized success/drop
event looks like this, but the example is **not execution evidence**:

```json
{"method":"MOVE","status":201,"counts":{"matching_attempts":1,"forwarded":1,"upstream_success":1,"responses_dropped":1}}
```

Stop with Ctrl-C after the checks. The final counters must also be captured so
any hidden replay attempt is visible. The helper exits 3, never a product-pass
exit code, even if it injected a fault successfully.

## Exact application acceptance sequence

1. With normal gateway routing and no active fault, log in through the real IdP
   using the disposable owner and enable the normal oCIS delegation. Create one
   uniquely named small synthetic file through the real application's upload
   flow. Wait for its committed result. Do not substitute a fake bearer token,
   fabricated operation row, or direct database insert.
2. Record a protected baseline: logical file ID, source-version ID, exactly one
   application source version, metadata revision, owner/space, native source and
   parent IDs, native ETag, complete native revision-ID set, original content
   digest and quota usage. Confirm the destination does not exist. Also check the
   native source points to the expected synthetic file rather than just a name.
   For a stronger history variant, prepare a second source version first and
   record both pinned content digests; then assert the count remains exactly two,
   not one. Do not silently substitute this for the one-version baseline claim.
3. Pause the disposable maintenance process long enough to observe the unknown
   result; no other process should reconcile this test concurrently. Configure,
   check and arm the exact source/destination pair above. Switch the dedicated
   stable gateway route through the proxy. Leave all capability verification
   gates unchanged; do not enable an unverified capability just to pass a test.
4. Submit exactly one authenticated application request:
   `PATCH /v1/files/{file_id}/location` with the original metadata revision,
   the logical target parent ID (empty for root), the exact `a23-...-moved.txt`
   target name, and a new run-specific idempotency key. The JSON fields are
   `target_parent_id`, `target_name`, `expected_metadata_revision`, and
   `idempotency_key`. Keep this exact input in protected memory for replay.
5. Require both independent facts: the proxy observed native 201/204 and dropped
   one response, and the application returned an operation ID with
   `state: outcome_unknown`. Confirm it using authenticated
   `GET /v1/operations/{operation_id}`. If no operation ID is obtainable, no
   upstream success was recorded, or the app reports committed before an unknown
   state was observable, stop and report the exact failed/blocked assertion.
   A synthetic 502 injected before native execution does not meet this scenario.
6. Before resuming maintenance, resend the **same application request** with the
   same idempotency key and unchanged body. Require the same operation ID and
   still-unknown state. The proxy must still show one matching/forwarded MOVE.
   Do not resend native MOVE, use a new key, or refresh the metadata revision.
7. Resume the existing disposable maintenance process, or start the deployed
   `cmd/maintenance` binary with the same real database, IdP delegation and oCIS
   routing configuration. It is the application maintenance loop, not a new
   test-only reconciliation algorithm. Poll the owner's operation endpoint
   until `committed` within the deployment's configured bounded test deadline
   (allow at least the one-minute retry interval). If it remains unknown or
   fails, retain the gate and diagnose; never mark a timeout as pass.
8. Assert the committed result refers to the original operation/file IDs. Read
   the file and versions through the real API. Require the expected target name
   and parent, one metadata-revision increment, the original current source
   version, **exactly one source version** for the one-version baseline, one
   logical file and no duplicate target. Confirm unchanged quota accounting.
9. Independently verify the exact native target resolves to the **same complete
   native resource ID** as before, with the same owner/space, and that all native
   revision IDs from the baseline remain. Verify every retained application
   source-version read and digest using its version ID; for the two-version
   variant require both historical and current content. Match by known native
   ID and retained history, never only by destination name or file size.
10. Replay the exact original application request again after commit. Require
    the same operation ID, file ID, committed state and semantic result; no new
    source version, operation, logical file, native revision or quota charge is
    allowed. Compare the operation identity/state and stable file fields rather
    than unrelated timestamps. The final proxy counts must be exactly one
    matching attempt, one forwarded MOVE, one upstream success and one drop.
    A second attempt blocked by the proxy is a **failed no-replay assertion**,
    even if no duplicate data was created.

Retain the synthetic fixture for inspection; no purge or cleanup DELETE belongs
to this procedure. Disable the fault route after collecting the final counters
without changing persisted native identities.

## Evidence and failure reporting

Record method/status/count proof plus allowlisted assertion names and booleans
(unknown observed, same operation, same native ID, history retained, source
version count unchanged, duplicate count zero, idempotent result reused). Keep
the true live-run timestamp and environment/build provenance separately from
unit-test output. Review surrounding gateway/application logs for secrets before
sharing; the proxy cannot suppress logging in other processes.

Do not publish raw operation responses: the current operation endpoint exposes
stored intent and therefore can contain native references. Do not print body
digests or fixture identifiers merely because they are synthetic if a boolean
comparison suffices. Never record tokens, passwords, cookies, Authorization
headers, database URLs, or token-bearing native upload URLs.

A live scenario may be declared passed only when all application and native
assertions were actually executed successfully. A proxy event alone, a prepared
gateway, a configuration check, an in-memory test, or maintenance source review
is insufficient. Update acceptance evidence only through the project's normal
review process after such a real run; this preparation makes no matrix changes.

Run the non-live checks separately:

```sh
python3 -m unittest discover -s tests -p test_ocis_fault_proxy.py -v
```

These tests use no browser, containers, listening sockets, real credentials, or
live upstream. They explicitly cover fixed-target forwarding, TLS configuration,
one-shot accounting, loss before any downstream bytes, refusal of redirects and
destructive methods, duplicate framing, bounds, sanitized diagnostics, and the
network-free `--check-only` path. Their result is **proxy-unit only**.
