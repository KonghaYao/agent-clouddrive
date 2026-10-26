# Bounded local fault acceptance (R3)

## Evidence boundary

`tests/integration/fault_acceptance.py` is an executable **real local-stack**
driver, not evidence that its scenarios passed. Its import and `--check-only`
paths make no network calls, do not start a listener, and do not invoke process
controls. `tests/integration/test_fault_acceptance.py` is a deterministic
**offline guard suite** using explicitly labeled in-memory responses, process
states, timers and callbacks. Its passing output is not live acceptance.

Read the newly generated private JSON report for an actual run. This driver does
not edit the acceptance matrix, promote a release, modify production code or
download/start a second dependency stack. It does not use a browser or Docker.
The native supervisor owns and starts the official fixture processes separately.

## Scenarios and required independent assertions

| Scenario | Actual injection and required observations |
| --- | --- |
| `worker_restart` | Gracefully exit the supervisor-owned worker, commit the already-uploaded small original while it is absent, independently observe a pending job with no published artifact, restart that worker and drain the job/outbox. This is **not** termination of an already-processing job, SIGKILL, OOM, or an in-flight crash. |
| `nats_recovery` | Gracefully exit the owned NATS process, commit the fixture, observe a fixture-scoped unpublished outbox row with a publication claim (`attempt >= 1`), restart with the same JetStream data directory, and require durable publication plus exact search. The worker's PostgreSQL polling is allowed to make progress during this interval. |
| `meili_recovery` | Gracefully exit the owned Meilisearch process, commit the fixture, observe a retry-queued conversion with `attempt >= 1` and no published artifact while Meili is absent, restart with the same data directory and require the job/index to become ready. Merely observing an unattempted queued job is insufficient. |
| `lost_finalize_response` | A one-use, exact-path loopback proxy forwards the real authenticated finalize once, verifies upstream HTTP 200 with `saved`/`committed`, then closes the downstream socket before emitting any status/headers/body. The client must observe EOF. Replay the identical upload request/idempotency key and finalize path; require the same upload, file and version and no extra quota settlement. |
| `native_move_response_loss` | Explicit `not_run`: process controls cannot preserve the native instance identity or independently prove native retained revision IDs. Requires the pre-provisioned stable logical gateway and exact-MOVE procedure in [fault-injection-acceptance.md](fault-injection-acceptance.md). Do not change persisted native identities or rearm an uncertain MOVE to make this run. |

Every executed scenario verifies:

1. Exactly one fixture logical file, original source version, upload session and
   committed quota reservation, with no held reservation.
2. Used quota increases by exactly the original's byte length, not by attempts,
   artifacts or retries. An isolated owner starts with no pending reservations.
3. Exactly one ready conversion and published artifact, no fixture-scoped
   unpublished outbox rows, and real file stat agrees that the source is ready.
4. The one retained version ID matches the committed source; both current and
   version-pinned API reads return the exact original bytes.
5. Search returns exactly one hit for the unpredictable synthetic marker, with
   exact file/current/returned-version IDs, one consistent artifact-set ID, the
   expected path, `stale=false`, ready completeness and the expected snippet.
   A lucky hit among extras, stale source or false completed state is not a pass.

This run uses one-version fixtures. It does not claim ten-version historical
retention or native MOVE revision preservation. Those are separate scenarios.
Synthetic originals, operation records and versions remain available for
inspection. There is no cleanup DELETE, purge or account removal.

## Safety and runtime hook contract

The API, Keycloak issuer and optional Meili origin must be IP-literal loopback
or exact `localhost`, including for HTTPS. HTTP is permitted only there. TLS
verification, fixed IdP endpoints, authenticated PKCE and existing delegation
helpers are reused; there is no fake token, ambient HTTP proxy or TLS bypass.

Three independent opt-ins must be exactly `yes`:

- `ALLOW_DISPOSABLE_TEST_WRITES`
- `ALLOW_DISPOSABLE_TEST_FAULTS`
- `ALLOW_DISPOSABLE_TEST_OIDC_CONSENT`

The supervisor imports the driver and supplies trusted Python functions:

```python
from deployed_api_harness import Config
from fault_acceptance import Hooks, ServiceControl, run

config = Config.from_environment(env, loopback_http=True, need_reader=False)
hooks = Hooks(
    services={
        "worker": ServiceControl(worker.stop, worker.start, worker.running),
        "nats": ServiceControl(nats.stop, nats.start, nats.running),
        "meili": ServiceControl(meili.stop, meili.start, meili.running),
    },
    snapshot=read_only_fixture_snapshot,
)
exit_code, report = run(config, fresh_private_output_path, hooks, env=env,
                       scenarios=["worker_restart", "nats_recovery",
                                  "meili_recovery", "lost_finalize_response"])
```

Callbacks must be closed over the supervisor's own `Popen` handles, never PIDs
read from arbitrary files or environment variables. They must not target host
services, other users' processes or a dependency started by a different owner.
Only `worker`, `nats` and `meili` are allowlisted. No shell command strings,
dynamic control-module imports or subprocess execution exist in the driver.

- `running()` returns an actual Boolean and is read-only.
- `stop()` sends only graceful SIGTERM to its owned process, verifies exit and
  finishes within two seconds. No SIGKILL, SIGSTOP, OOM, pressure or exhaustion.
- `start()` is thread-safe, completes within two seconds, refuses to duplicate a
  live process, and reuses the exact command/environment and retained data path.
- Callbacks must not delete files, change host configuration or accept new
  credentials/permissions. Their exceptions are never copied to the report.
- A watchdog restarts the process six seconds after confirmed stop; normal restart
  runs in `finally` even on failure or interruption. Including the stop/start
  budgets, observed injected process interruption must be at most ten seconds. A late
  watchdog, bound breach or unconfirmed restart **fails**, not passes.
- Parent controls are a trust boundary: a malicious or indefinitely blocking
  Python callback cannot be made safe by a timing assertion. The supervisor must
  enforce those callback bounds and own its processes. The driver cannot grant
  authority to control a service simply because a callback was supplied.
- Process liveness is **not** dependency health or service readiness. The ten-second
  bound is on process interruption, not on end-to-end availability. Business
  recovery is independently verified within 120 seconds after process restart. Individual API calls
  use three-second timeouts. The fixture observation callback must finish within
  two seconds, be read-only and scoped to the supplied validated UUIDs.

### Read-only snapshot schema

`snapshot(file_id, upload_id)` returns exactly these keys; unknown fields,
Boolean-as-integer counts and unknown states are rejected:

```json
{
  "file_count": 1,
  "version_count": 1,
  "upload_count": 1,
  "committed_reservations": 1,
  "held_reservations": 0,
  "ready_artifact_count": 0,
  "unpublished_outbox_count": 1,
  "max_unpublished_outbox_attempt": 0,
  "job_states": ["queued"],
  "job_attempts": [0]
}
```

This is an **illustrative schema, not recorded execution evidence**. Counts
must come from real application PostgreSQL reads. File/version/upload/job and
artifact queries are scoped to that one fixture; reservations to its upload.
`ready_artifact_count` means published artifacts, not merely a staged row.
Outbox scope includes that file's aggregate/payload IDs and its version/job IDs;
never substitute the entire account/global backlog. The maximum attempt is over
that same unpublished outbox scope, with zero for no rows. The job state/attempt
arrays have matching deterministic order. No direct inserts, status updates,
recipe changes, quota adjustments or fabricated rows are allowed.

## Reporting and execution

A new output path is mandatory. It is reserved exclusively with mode `0600`
before login/writes; existing files, symlinks, hardlink aliases and credential
replacement are refused. Reports contain static check labels, Boolean outcomes,
sanitized failure codes and numeric metrics, not source bodies, native resource
IDs, usernames, passwords, bearer tokens, cookies, origins or database URLs.

Each scenario records injected process downtime, explicitly injected lost responses,
API availability errors during the injected interval, unexpected API errors
outside it and business recovery time separately. `injected_downtime_seconds`
measures the stop invocation to restarted-process liveness, not an HTTP health
endpoint becoming ready. `recovery_seconds` measures from that process restart
to actual durable job/outbox drain and exact search visibility. These definitions
also appear in the JSON report. Errors during a planned outage are
not mixed into unexpected availability metrics, but they do not excuse a failed
commit or invariant. Unexpected API availability errors fail the scenario.

Independent cases retain their results when another fails. If a process restart
cannot be confirmed, later dependent cases are `not_run`, not success. Missing
controls and the unsupported MOVE scenario are always explicitly `not_run`.

- `selected_status=pass` means every requested, supported case actually passed.
- `full_fault_acceptance=partial` means some of the five cases passed but at least
  one did not run. **Do not relabel partial as a full fault-suite pass.**
- Exit `0` is reserved for all five passing; `1` is a live failure; `2` is a
  configuration/report error; `3` is partial/not-run/check-only; `130` interrupted.
- A supervisor intentionally selecting the supported subset may accept exit `3`
  only together with `selected_status=pass`, while preserving `partial` overall.

CLI configuration validation does not install process hooks:

```sh
python3 tests/integration/fault_acceptance.py --check-only --output /private/new-check.json
python3 -m unittest discover -s tests/integration -p test_fault_acceptance.py -v
```

The first command reads the same protected `TEST_CREDENTIALS_FILE`,
`TEST_PUBLIC_URL`, `TEST_OIDC_ISSUER` and optional CA settings as the deployed API
harness. It exits `3` for valid configuration and claims no live acceptance.
Actual process faults run only through the supervisor's owned callbacks.
