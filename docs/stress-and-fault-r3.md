# R3 bounded stress and recoverable-fault verification

This report concerns a disposable, loopback-only native deployment. It is not a
production-capacity certification, an unbounded stress test, Docker Compose
acceptance, real-browser acceptance, or a 10 GiB upload result.

## Safety envelope

The executable load driver uses the fixed prefix 1, 4, 8, 16, 32, and 64 (the default stops at 16). Each timed stage
must actually exercise every selected workload. Expected version/path conflict
409 responses are recorded separately from unexpected failures. It stops rather
than marking success on: unexpected error ratio >10%, P99 >10 seconds, sampled
aggregate process RSS >4 GiB, available RAM <2 GiB, free disk <5 GiB, pipeline
backlog >5,000, missing required observations, or broken correctness invariants.
Its maximum run budget is 600 seconds, with a conservative 512 MiB synthetic
write reservation. These are safety limits, not measured capacity guarantees.
Dependency-internal amplification, WAL and logs are not claimed to fit that
logical write reservation; free-space and memory guards remain independent.

Fault hooks target only supervisor-owned local worker, NATS and Meilisearch
processes. They use graceful termination, bounded restart supervision and no
host-wide kill, OOM injection, permanent deletion or external traffic. An
interruption's process restart time and eventual business recovery are separate
measurements. The lost-finalize-response scenario drops an actual response,
then checks idempotency and immutable bytes rather than inventing a success.

## Reproduced and corrected source defects

The independently retained red logs precede their corresponding green tests.
Real PostgreSQL is used for durable state and concurrency. Controlled identity,
original-store and search contracts in these deterministic regressions are
explicitly not native end-to-end service evidence.

1. Two maintenance workers could apply an older path after a newer MOVE and
   acknowledge both events, leaving the raw index permanently stale.
2. A delayed delete task could remove documents already rebuilt by RESTORE.
   Owner-scoped transactional advisory serialization now covers snapshot reads
   and external index tasks. Try-lock contention retries without acknowledgement;
   nested reads reuse the same transaction, including with a one-slot DB pool.
3. A valid 3,075,072-byte (~2.93 MiB) plain-text document could exceed 1,000 search chunks, while
   the real adapter rejects an oversized Upsert request. Initial indexing and
   retained-artifact rebuild now submit <=1,000-document batches and wait for
   every task before publication. Partial failure cannot publish success.
4. The new transaction initially rolled back a detected account disable when
   indexing was deferred. Security-state updates now commit independently of the
   retryable external outcome; the outbox is not acknowledged on that error.
5. A legitimate task longer than its outbox lease could never acknowledge and
   repeated forever. A dedicated, bounded PostgreSQL heartbeat connection renews
   only the unexpired current token. Lost/reclaimed fencing cancels processing;
   shutdown joins the heartbeat and closes it. This avoids pool-one deadlock.
6. Plain UTF-8 source comments could forge page provenance and create 20,000
   structural blocks from less than 1 MiB. Plain text is now literal one-page
   content. Converter markers are bounded by, and matched to, the page manifest;
   ambiguous markers retain text without invented page provenance.
7. Paragraph splitting allocated a slice per separator before producing any
   chunk. Incremental scanning now avoids separator-count amplification.

## Execution records

The native reports below distinguish complete workload runs from setup failures.
A prepared driver, a passed offline unit suite, or a stopped run is not a passing
deployed stress result. Earlier
failure reports and the previously delivered R2 ZIP are retained unchanged.

Native dependencies: Keycloak 26.8.1, oCIS 8.2.2, PostgreSQL 17.11,
NATS JetStream 2.15.1, Meilisearch 1.55.0; direct UTF-8 conversion and local
artifact storage for this load scope. Setup provenance is documented separately.

### Retained preliminary executions

- `evidence/r3-native-run1/supervisor.json`: failed before application execution
  because the PG-only helper did not create a NATS configuration. The supervisor
  now creates its own bounded private configuration. This is a harness setup
  failure, not an application load result.
- `evidence/r3-native-run2/joint.json`: all five native functional scenarios and
  186 assertions passed on the R3 production fixes.
- `evidence/r3-native-run2/stress.json`: rejected before any load stage because
  five prior outbox rows had not drained. The baseline guard was not disabled;
  the supervisor now waits for three consecutive empty observations. The first
  RSS collector also missed descendants because this runtime omits Linux task
  `children` files. Its ~398 MB value is incomplete and must not be used as a
  memory result. The replacement full PPID-snapshot collector passed an actual child-process
  allocation check and captured the Java and PostgreSQL descendants in later runs.
- `evidence/r3-native-fault-run1/fault.json`: four selected real native scenarios
  passed 77 checks; their complete suite remains `partial` because native MOVE
  response loss was not run. The supervisor records an unchanged source hash
  during this execution and graceful shutdown of its children.

| Preliminary native fault | Process interruption | Business recovery |
|---|---:|---:|
| Worker termination/restart | 0.6173 s | 2.1963 s |
| NATS stop/restart with observed publish attempt | 0.9509 s | 1.4832 s |
| Meilisearch stop/restart with observed indexing attempt | 0.5750 s | 3.9062 s |
| One real finalize response dropped | Not a process outage | 1.6893 s |

These samples are not a recovery SLA or statistical tail estimate. Each checked
exact original and historical bytes, identity/version, latest-only quota,
reservation release and exact published search artifact. Unexpected API errors
were zero in these four bounded cases. Native MOVE response loss, long outages,
OOM, host/storage corruption and full-process-crash durability are not thereby
accepted. The final combined execution below remains separate from these preliminary logs.

### Fresh-database fixture isolation failure

The first expanded combined run stopped before pressure on a real HTTP 503 during
its ten-version delete scenario. Read-only diagnosis found zero native retention
identity mappings and zero delete operations in the fresh application database,
while TLS-verified native Graph/PROPFIND confirmed the previously retained native
path still existed. Ten immutable source versions remained. The harness had
incorrectly combined a new database with an older native retention directory.

The product's refusal to adopt an unknown native identity was preserved. The
supervisor now assigns each fresh test database its own configured reserved
retention-directory name, shared by API, worker and maintenance. Prior native
folders/data and failed reports are not deleted or adopted. The corrected combined run independently passed deletion, restoration and all
pressure assertions, as recorded below.
See `evidence/r3-native-final-run1/`; the early read-probe setup failures are also
retained, while `retention-native-read-confirmation.log` contains the successful
read-only confirmation. No native mutation was retried to diagnose this failure.

## Actual combined six-stage execution

`evidence/r3-native-final-run2/` contains the actual combined run on the repaired
production code. Keycloak, oCIS, PostgreSQL, NATS and Meilisearch were real local
processes; originals were native oCIS files. Conversion was direct UTF-8 and
artifacts used local disk. The 1 KiB payload and short timed windows are deliberate
bounds, not large-file, endurance or production-capacity evidence.

- Joint API: all five scenarios and 186 assertions passed.
- Pressure: six stages passed; 9,725 HTTP requests, 654 explicitly expected 409s,
  zero unexpected responses; 420 synthetic files and 426 source versions verified.
- Pressure wall time: 244.612 s, including setup, conflict races, consistency audit
  and queue drain. Timed workload windows totalled 64.610 s. Full-run HTTP
  throughput was 39.757 requests/s; timed-window throughput was 86.488 requests/s.
  These are HTTP requests, not uploads or completed business operations per second.
- All ten timed workloads executed in every stage. Each concurrent same-file and
  same-target admission race had exactly one winner and recognized conflict losers.
- 688 complete resource observations; full-stack sampled RSS peaked at
  1,962,237,952 bytes (1.827 GiB), including Java and PostgreSQL descendants.
  Conversion/index/outbox backlogs peaked at 88/1/39, then returned to zero for
  three consecutive samples. The 5,000 queue limit was not approached.
- Four selected recoverable faults passed all 77 checks. MOVE-response-loss was
  not executed, so the fault suite and supervisor correctly returned `partial`.
- Whole supervisor execution: 295.725 s; no watchdog trigger, all owned children
  stopped gracefully, and source hashes at launch and finish were identical.

| Concurrency | Timed HTTP requests/s | Full-stage HTTP requests/s | Timed P50 ms | Timed P95 ms | Timed P99 ms | Expected 409s (whole stage) |
|---:|---:|---:|---:|---:|---:|---:|
| 1 | 18.09 | 17.57 | 38.7 | 104.2 | 195.6 | 16 |
| 4 | 48.54 | 36.24 | 67.5 | 176.9 | 308.5 | 44 |
| 8 | 96.32 | 48.45 | 69.1 | 163.1 | 198.6 | 88 |
| 16 | 112.04 | 44.29 | 116.9 | 286.6 | 378.7 | 118 |
| 32 | 121.70 | 44.53 | 213.2 | 462.5 | 890.1 | 162 |
| 64 | 116.04 | 48.52 | 446.9 | 956.2 | 1097.9 | 226 |

At 32 to 64 concurrent scenarios, timed HTTP throughput flattened and latency rose.
This describes this client, dependency configuration and shared single-machine
combination, not a measured isolated service saturation point. No further load
was added merely to reach an arbitrary failure. HTTP P99 is not the latency of a
whole multi-request upload scenario; both distributions remain in the raw report.

The combined run has valid global RSS/backlog observations but did not record
per-stage resource offsets. Its global values are not retroactively attributed
to stages. A subsequent observation-only driver change and a separate actual
stress rerun provide those stage windows and sampled summaries below.

| Final combined fault | Process restart interval | Business recovery |
|---|---:|---:|
| Worker stopped before new work, then restarted | 0.3684 s | 1.8078 s |
| NATS unavailable with actual publish attempt | 0.5785 s | 1.8256 s |
| Meilisearch unavailable with actual index retry | 0.7238 s | 3.1362 s |
| One successful finalize response actually dropped | Not a process outage | 1.9042 s |

The worker case proves persistent pending work and restart recovery, not a kill
during processing. Process restart intervals end when the process is alive, not
when the service is ready; business recovery is independently measured. Finalize
retry reused the original operation/version without double quota or duplicate
commit. Every selected fault checked exact source/history bytes and search state.

## Final source-bound stage-resource execution

`evidence/r3-native-resources-final/` is a separate actual stress-only rerun after
the observation-only driver change. It does not overwrite or relabel the combined
run. Production application binaries were unchanged. Source launch/finish hashes
both equal `909c46e59be2b0e43bd87c7b52b2e7529fa0eeced9b6fb973b5233de5a91187b`.

All six stages and all ten timed workloads per stage passed. Across 258.285 s of
pressure wall time: 11,266 HTTP requests, 726 expected conflicts and zero unexpected
errors. The timed windows totalled 63.987 s, with 6,564 HTTP requests: 102.584
requests/s timed versus 43.618 requests/s including setup, audit and draining.
492 files and 498 immutable source versions were checked against exact fixture
bytes; latest-only quota, zero held reservations, paths and search artifacts were
verified. Final pipeline queues were zero on three consecutive observations.

There were 749 valid resource observations and no observation errors. The table
uses full-stage windows, including conflict races, timed work and drain, matched
to retained raw sample timestamps. Measurements are sampled/non-atomic, not
continuous maxima; missing observations would fail the stage rather than become
zero. RSS includes configured native processes and discovered descendants.

| Concurrent scenarios | Timed HTTP req/s | Full-stage HTTP req/s | Timed P50/P95/P99 ms | Sampled RSS peak MiB | Queue peaks C/I/O | Samples |
|---:|---:|---:|---:|---:|---:|---:|
| 1 | 15.57 | 14.86 | 57.3/121.7/185.3 | 1232.84 | 2/1/6 | 38 |
| 4 | 58.87 | 43.68 | 55.3/137.8/179.4 | 1399.25 | 15/1/18 | 57 |
| 8 | 96.70 | 51.90 | 69.9/170.6/209.5 | 1644.25 | 49/1/21 | 86 |
| 16 | 141.33 | 58.09 | 94.1/212.1/267.2 | 1762.77 | 84/1/29 | 118 |
| 32 | 150.00 | 58.59 | 174.9/357.0/413.4 | 1833.22 | 101/1/27 | 134 |
| 64 | 144.61 | 55.64 | 368.7/753.6/851.3 | 1911.35 | 100/1/40 | 137 |

C/I/O mean conversion, index and outbox pending counts; individual maxima need
not occur at the same instant. Sampled total pending work peaked at 141. The
full-run RSS maximum was 2,004,197,376 bytes (1.867 GiB); sampled available RAM
remained above 7.53 GiB and disk above 25.0 GiB. Source-byte reservations totalled
1,253,376 bytes and conservative fixture allocation reservations 80,248,832 bytes,
not the 512 MiB ceiling. The supervisor finished in 266.840 s, below its 600 s
budget, without watchdog intervention; owned children stopped gracefully.

32 to 64 timed throughput again flattened (150.00 to 144.61 HTTP requests/s) while
HTTP P99 increased (413.4 to 851.3 ms). This repeat supports describing a plateau
for this bounded local combination, not production sizing or a universal maximum.

Final source verification combines the successful complete production aggregate
with v6 guards for the changed test drivers: 221 Python and 37 Node checks, with
no source drift. The independent real-PG/race adversarial suite passed 48 leaf
scenario checks. Exact overlaps, skips and manifests are documented in
`evidence/source-r3/README.md`; these counts must not be summed as unique tests.

## Remaining boundaries

No unbounded limit, OOM recovery, long-duration soak, host corruption, 5,000-job
backlog, 512 MiB transferred workload, actual browser, 10 GiB resumed upload,
full Docker Compose startup or Peri acceptance is claimed. The 512 MiB and other
resource values above are stopping budgets. The source suite, native component
checks, deterministic contract races and native deployed pressure each retain
their own scope and provenance. Full release readiness remains false.
