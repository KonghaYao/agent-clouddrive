# Bounded native API pressure driver

`stress_acceptance.py` is an opt-in driver, not a stored passing result. Its
stdlib-only offline unit tests establish guards/accounting, not deployed behavior.
Only a new real execution report with `execution: finished`, `status: pass`, every requested
passing stage and passing invariants is passing evidence for this limited scope.
A safety stop, missing sample, unobserved backlog, insufficient workload coverage,
unknown mutation outcome, or assertion failure is **not** a pass.

## Scope and safety

- Real Keycloak authorization-code/S256 PKCE and the deployed public REST API via
  `deployed_api_harness.py`. The synthetic owner must already have valid offline
  delegation. This driver never creates a new offline grant or changes identities.
- Only exact configured loopback endpoints, including HTTPS. Literal loopback
  IPs or exactly `localhost` are accepted; actual execution checks that every
  resolved `localhost` address is loopback before auth/load. Other DNS names,
  remote endpoints, arbitrary redirects and proxy routing are rejected. Do not expose the disposable stack outside loopback.
- Require a dedicated synthetic owner with no concurrent external writer and a
  drained pipeline/no pending quota reservation at baseline. Other test traffic
  must not run concurrently. All fixture names/keys are unique per run.
- Fixed default concurrency progression: 1, 4, 8, 16. Explicit
  `--max-concurrency 32` adds 32; `--max-concurrency 64` adds 32 and 64. These are
  the only permitted maxima; arbitrary levels or unlimited scaling are rejected.
  Every original byte/work/time/RSS/RAM/disk/error/latency cap remains unchanged.
  Each stage has two bounded races,
  then ten seconds of timed mixed workloads by default, then a functional audit.
  Timed windows are configurable from 1 to 30 seconds; maximum whole-run budget
  is 600 seconds. The driver stops issuing new work ten seconds before that
  deadline for finite in-flight requests and reporting. The native supervisor
  should also enforce a hard process timeout, including startup/teardown.
- No unbounded future queue: at most the selected 16/32/64 active worker tasks, maximum 5,000 new
  fixture-work reservations and maximum 5,000 conservatively summed pending
  conversion/index/outbox records. Counting the same pipeline item in two
  states/tables is deliberately conservative. Read/replay request totals are
  reported separately and may exceed the new-fixture cap.
- Payloads default to 1 KiB; maximum 64 KiB. Before every attempted upload,
  including expected-conflict attempts, reserve `max(8 × payload, 64 KiB)` against
  a hard 512 MiB fixture allocation allowance. Directories reserve 4 KiB each.
  All attempted original payload bytes are independently counted and capped.
  The allowance is conservative but is **not a measurement or universal bound
  on opaque dependency-generated WAL, logs or storage amplification**. Free disk
  and RAM are independently sampled. No huge test files are created.
- Stop on unexpected HTTP/scenario error ratio >10%, P99 >10 seconds, sampled
  native-process-tree plus driver RSS >4 GiB, available RAM <2 GiB, free disk
  <5 GiB, missing required metrics, or any functional invariant failure. Error
  and latency guards apply globally per stage and per workload. Hot-path guards
  use exact incremental count/error/>10-second counters: error count × 10 > n,
  or >10-second count > n − ceil(99n/100). This is mathematically identical to
  the stated thresholds, not approximate sampling; complete latency observations
  remain available for the report's sorted P50/P95/P99 calculations. Missing optional
  native PID configuration is explicitly `not_checked`, never “zero memory”.
- HTTP timeout is at most five seconds. Fixed read-only PostgreSQL SQL has a
  two-second statement timeout and three-second subprocess timeout. No injected
  SQL, server logs, tokens, passwords, usernames or raw error bodies enter reports.
- Synthetic files, versions, failed reports and any unresolved operations remain
  for investigation. No deletion, account cleanup, blind upload retry or report
  overwrite is performed. A supervisor must retain failed results as well.

## Workloads and correctness evidence

Every timed stage must actually execute all ten workloads. Race-only samples
cannot satisfy timed coverage. Reports separately include whole-stage and timed
workload counts, operations/second, nearest-rank P50/P95/P99, unexpected error
count/ratio, expected recognized HTTP 409 count/ratio, and HTTP status counts.
HTTP request latency is separate from end-to-end scenario latency.
`timed_http` uses only timed-load API requests divided by the actual timed window,
including in-flight completion. The existing `http` summary uses all measured
API requests divided by full stage/run wall-clock time, including setup, races,
audits and drain. Each summary explicitly states its unit, denominator seconds
and throughput basis; these are not interchangeable throughput measures.
Each stage records start/end and timed-load offsets relative to the run's
monotonic start. Its `resources` summary filters the unchanged raw observations
through that full-stage inclusive window and reports sample count, sampled peak
RSS and each queue/connections/waiting-lock count, and sampled minimum available
RAM/free disk. The total pending-work peak is computed per observation, not by
adding unrelated individual peaks. These are explicitly sampled extrema, not
continuous system maxima. Missing stage observations are null/not-observed and
cannot satisfy a passing stage. Final summaries are recomputed after the monitor
joins, so window calculations can be reproduced from retained raw samples.
Requested and actually attempted concurrency schedules/maxima are separate, so a
safety stop before 64 never implies that 64 was exercised. Final-audit metrics use
a separate stage ID after the last requested load stage.

| Workload | Required evidence |
| --- | --- |
| Read | Exact immutable original bytes plus exact version IDs, sequences, sizes and SHA-256. |
| List | Exact child identity set; no duplicate IDs/names or extra fixtures. |
| Stat | Current file ID, version and size match the known fixture. |
| Download | Every byte equals the synthetic source. |
| Search | Exact fixture file, current/returned source version, published artifact ID, path, snippet, completeness and non-stale status. Random per-version markers use exact phrase queries to avoid fuzzy/prefix cross-matching fixtures. Artifact ID must match file metadata and remain stable for that source version. |
| Small original upload | One confirmed create/PATCH/finalize, exact offset, file/version identity, bytes and version history. An uncertain finalization fails without replay. |
| Same-file conflict | A real race on one captured version/revision has exactly one winner; others must return recognized `VERSION_CONFLICT` or `OPERATION_PENDING`. Timed stale-revision requests require `VERSION_CONFLICT`; exact full history and latest-only quota are audited afterward. |
| Same-target conflict | A real race to one new path has exactly one winner; others must return `PATH_CONFLICT` or `OPERATION_PENDING`. Timed occupied-target requests require `PATH_CONFLICT`; no duplicate path or extra committed file is allowed. |
| Batch replay | Replay a real four-item batch with stable envelope/item keys; exact committed operation/file IDs and exact four directory identities must remain unchanged. |
| Index backlog | Produce real text originals, observe actual PostgreSQL conversion/index pipeline pending work, then verify every new/changed file's exact public search result. |

Concurrency one races use two sequential contenders; stages 4/8/16 synchronize
all contenders at admission. Recognized 409 responses are expected only at the
specific conflict calls. Other 409s, any unexpected status, or success responses
with wrong data fail the run. Independent final audits check retained versions,
all original bytes, all public search results, exact fixture listings and exact
baseline-plus-latest-sizes quota with zero reservations.

Each stage and the final audit require three zero-pipeline samples spanning at
least one second, after per-file search convergence. A single transient global
zero does not establish recovery. The report identifies this as the
conversion/index pipeline, **not** a direct Meilisearch internal queue measure.

## Private runtime contract

Create a new current-user-owned regular JSON file with mode 0600 or 0400. Do not
commit it. Runtime values and credentials do not appear in the resulting report.
Example with illustrative local paths/ports only:

```json
{
  "schema_version": 1,
  "disposable": true,
  "exclusive_owner": true,
  "public_url": "http://127.0.0.1:8080",
  "oidc_issuer": "http://127.0.0.1:8081/realms/clouddrive",
  "synthetic_username": "synthetic-owner",
  "storage_path": "/tmp/disposable-clouddrive",
  "native_pids": {
    "api": 10001, "worker": 10002, "maintenance": 10003,
    "nats": 10004, "meili": 10005, "keycloak": 10006,
    "ocis": 10007, "postgres": 10008
  },
  "postgres": {
    "psql": "/tmp/native-postgresql/bin/psql",
    "host": "127.0.0.1",
    "port": 5432,
    "database": "clouddrive_fixture",
    "user": "fixture_metrics"
  }
}
```

- Endpoint strings must exactly match `TEST_PUBLIC_URL` and `TEST_OIDC_ISSUER`.
  `synthetic_username` must exactly match the private credentials file owner.
  The attestations must be literal JSON booleans, not strings.
- `storage_path` must be an existing path on the native data filesystem used for
  the free-disk guard. If dependency data spans separate filesystems, use a
  supervisor that monitors all of them; this driver only claims the specified
  filesystem.
- `native_pids` names are safe labels. PIDs must be distinct positive integers.
  The driver enumerates `/proc/*/stat` and derives each root's transitive PPID
  descendants, deduplicating overlapping trees, including PostgreSQL backends
  and shell→Java. It does not rely on `/proc/PID/task/TID/children`, which is
  absent in this execution runtime. Any enumerated live PID with unreadable stat
  fails closed; only a confirmed vanished PID is recorded as an enumeration
  exit. Configured roots disappearing, turning zombie or changing kernel start
  identity also fail. Previously observed live descendants remain monitored
  after reparenting, while reused PIDs do not inherit old attribution.
  `process_tree_proof` reports the PPID method, per-root descendant counts and
  complete enumeration; it explicitly states that sampling is non-atomic.
  Process names and parent PIDs make shell→Java and PostgreSQL backend coverage
  inspectable without reading process arguments or environment variables.
- PostgreSQL connection identifiers are restricted strings and its host must be
  a literal loopback IP. The executable must be an absolute executable file.
  Optional `passfile` must be an owned private regular file; its contents are
  never read into reports. Missing passfile uses `/dev/null`, not ambient secrets.
  Optional `library_path` is an explicit colon-separated list of existing
  absolute library directories for restored private PostgreSQL distributions.
- The metrics subprocess receives a small allowlisted environment, never
  inherited `PGPASSWORD`, `PGSERVICE`, SQL, startup files or arbitrary options.
  It runs with read-only transactions and fixed SQL against conversion jobs,
  artifact index state, outbox pending work, active connections and waiting locks.
  Use a dedicated read-only role if available. Loopback trust is acceptable only
  for the isolated disposable fixture; this driver does not configure it.

The standard private `TEST_CREDENTIALS_FILE` format is:

```json
{"owner": {"username": "synthetic-owner", "password": "fixture-password"}}
```

It must be a private owned regular file, not a symlink. Existing PKCE client,
callback, scopes, oCIS delegation and working native services are prerequisites.

## Invocation

Offline tests do not launch application services or make network requests. One
harmless local parent/child process test validates real PPID/RSS collection and
cleans up both processes; the remainder use pure or mocked evidence:

```sh
python3 -m unittest discover -s tests/integration -p 'test_stress_acceptance.py' -v
```

Check-only validates configuration/files without auth, SQL or network I/O. It
writes a new private `not_run` report and returns 3 when valid, 2 when invalid:

```sh
python3 tests/integration/stress_acceptance.py \
  --runtime-file /tmp/disposable-runtime.json \
  --output /tmp/stress-preflight-new.json --check-only
```

After explicit authorization for disposable writes and fixture OIDC consent:

```sh
export ALLOW_DISPOSABLE_TEST_OIDC_CONSENT=yes
python3 tests/integration/stress_acceptance.py \
  --runtime-file /tmp/disposable-runtime.json \
  --output /tmp/stress-result-new.json \
  --allow-disposable-writes --stage-seconds 10 --max-seconds 600
```

The CLI returns 0 only for a full pass, 1 for an executed failed/stopped run,
2 for invalid configuration/missing authorization, 3 for valid not-run preflight.
It creates its report exclusively as mode 0600 **before any login or writes**.

Supervisor import entry point:

```python
from stress_acceptance import Options, run
config.timeout = 5
result = run(config, runtime, Options(maximum_concurrency=16),
             allow_disposable_writes=True, oidc_consent=True)
```

The import entry performs no startup, downloads, browser work or Docker work.
Its explicit flags must reflect actual user authority, not an inferred permission.
The supervisor owns startup, hard outer timeout, and exclusive private report
reservation/persistence. Configuration/authority validation can raise a safe
`DriverError` before execution; executed failures are returned as failed/stopped
reports. Do not promote a release matrix from this bounded pressure result alone.
