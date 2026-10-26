# R3 adversarial stress and recovery review

## Current conclusion

The deterministic defects below were reproduced locally and corrected. The
independent final regression run passes with real PostgreSQL and Go's race
detector. The pressure/fault drivers and native supervisor also pass their
offline guard tests. The independently reviewed native runs additionally pass
bounded 1/4/8/16/32/64-client pressure and four selected recovery scenarios. These are
not capacity-limit or complete fault-acceptance results; exact scope is below.
The final per-stage resource report has been independently recomputed from its
retained raw samples. No unresolved blocking defect was found in this reviewed
scope; the explicit untested limits at the end still apply.

No third-party service was stressed, no real user data was targeted, no browser
security restriction was bypassed, and no previously delivered ZIP was changed.

## Reproduced defects and verified corrections

| Defect | Deterministic reproduction | Correction and regression |
| --- | --- | --- |
| Old path overwrote a newer MOVE | Pause an old index write, commit a new database path, complete its index event, then release the old write. Both events originally succeeded while the index ended at `/old`. | Owner-scoped cross-process index serialization occurs before reading current metadata. Busy events remain retryable. Different owners still progress independently. |
| Late delete erased restored search | Pause the old index deletion, restore/rebuild the same immutable artifact, then release deletion. Both events originally succeeded with no search documents left. | The same serialization covers lifecycle checks, deletion, rebuild and actual search-task completion. |
| Valid text exceeded one index request | A 3,075,072-byte UTF-8 source produces 1,001 chunks, exceeding the real adapter's 1,000-document limit. | Initial indexing and retained-artifact rebuild submit bounded batches and wait for every task. A failed second batch never publishes; resumption preserves chunk IDs and does not reread/reconvert the source. |
| Detected account disable was rolled back | The first serialization candidate detected a disabled owner during rebuild, updated the principal inside its transaction, then rolled that update back with the indexing error. | The wrapper commits only its authoritative security-state updates even when external indexing is deferred. It uses the same transaction for nested database access. |
| Work longer than its lease never completed | A legitimate four-second index operation with a three-second outbox lease could never acknowledge completion, even with one worker. | A dedicated bounded database connection renews the live token without competing for the one-connection business pool. Lost/expired fencing cancels work; cancellation is joined before connection cleanup. Strict final fencing remains intact. |
| Source comments forged pages and amplified allocation | Fewer than one MiB of plain text containing 20,000 page-like comments produced 20,000 structural blocks with invented page 999999. | Plain UTF-8 content never supplies converter provenance. Other engines accept only bounded, unique page/block matches from the manifest; malformed structure preserves the entire text without invented page mapping. |

Additional hardening replaces eager paragraph splitting with incremental scans.
A 400 KiB all-separator input produces no chunks and stays below a generous
one-MiB allocation guard, rather than allocating an array entry per separator.

The regressions explicitly cover a one-connection pool (no nested pool wait),
cross-owner progress, forced lease takeover (no stale publication/acknowledgment),
duplicate/unknown/overflowing page markers, and second-batch failure/recovery.

## Evidence and reproducibility

- [Original failing interleavings and indexing limit](../evidence/stress-review-red-regressions.log).
- [Additional failing revocation and lease cases](../evidence/stress-review-followup-red.log).
- [Failing source-marker case](../evidence/stress-review-marker-amplification-red.log).
- [Final independent PostgreSQL/race run](../evidence/stress-review-final-regressions.log):
  11 top-level tests, 16 leaf scenarios repeated three times, 48 passing scenario
  executions, zero skipped tests, 32.770 seconds.
- [Final offline driver/supervisor checks](../evidence/stress-review-driver-final-unit.log):
  118 acceptance-driver checks and 15 supervisor checks passed.
- [RSS-correction independent checks](../evidence/stress-review-driver-rss-unit.log):
  123 acceptance-driver checks and 21 supervisor checks passed, including an
  actual harmless local parent/child process-tree measurement.
- [Final source-stability guard checks](../evidence/stress-review-supervisor-source-guard.log):
  all 24 supervisor checks passed in that intermediate snapshot.
- [64-client extension checks](../evidence/stress-review-64-driver-unit.log):
  133 acceptance-driver and 36 supervisor checks passed. The independent
  [retention-isolation check](../evidence/stress-review-retention-isolation-unit.log)
  subsequently passed all 37 supervisor checks.
- [Final per-stage observability checks](../evidence/stress-review-observability-unit.log):
  137 acceptance-driver and 37 supervisor checks passed, 174 total. These are
  offline/control-fixture checks, not substitutes for actual deployed execution.
- [Reviewed source hashes](../evidence/stress-review-verified-source-hashes.json):
  post-test snapshots, not an invented prelaunch attestation.
- [Final reviewed source snapshot](../evidence/stress-review-final-source-hashes.json)
  records the subsequent RSS and source-stability guard corrections separately.
- [Final observability source snapshot](../evidence/stress-review-observability-source-hashes.json)
  records the max-64, deadline, retention-isolation and per-stage reporting version.
- [Independent native-result validation](../evidence/stress-review-native-final-validation.json)
  records input-report hashes and independently recomputed stage extrema. It is
  read-only review evidence, not an additional claimed live run.

Run `go test -race -count=3 -v ./internal/application -run '^TestStressReview'`
with `DRIVE_TEST_DATABASE_URL` pointing to a disposable PostgreSQL database.
Each database test creates its own schema. The tests use real PostgreSQL and
local immutable artifact files, but explicitly controlled identity/original/index
contracts; the 1,001-chunk tests use the real search HTTP adapter against a local
protocol fixture. They do not impersonate native oCIS/Meilisearch load tests.

The first exploratory log also contains a test-fixture setup error: it attempted
to change an immutable source version. That trigger correctly rejected the test.
The fixture was corrected to insert a new version before the separately retained
capacity-failure reproduction. No production invariant was relaxed.

## Driver false-pass and safety review

The review required and verified corrections for:

- Exact artifact claims now compare the returned search artifact with the
  file's published artifact pointer, not merely any syntactically valid ID.
- A late monitoring abort cannot leave a pressure report marked passed.
- Every timed pressure stage must execute every declared workload; earlier
  setup/race activity cannot satisfy timed-load coverage.
- Expected recognized HTTP 409 conflicts are separated from unexpected errors.
  Success still requires exact bytes, retained versions, unique identities,
  latest-only quota and zero held reservations, not just HTTP 200 counts.
- Supervisor and driver inventories, supported fault names, write/consent
  opt-ins, PostgreSQL library setup and local issuer validation agree.
- Four supported fault cases cannot be relabeled as complete fault acceptance:
  native MOVE-response loss remains explicit `not_run` and the aggregate remains
  partial. Graceful pending-worker restart is not claimed as an in-flight crash.
- Process interruption is limited to the supervisor's own `Popen` children.
  There is no arbitrary PID targeting, SIGKILL, disabled TLS or browser sandbox
  bypass. Process restart time is distinguished from actual business recovery.
- Evidence files are newly reserved private files, not overwritten inputs.
  Diagnostics avoid raw credential-bearing errors. Resource/work/time guards
  stop the run rather than turn missing or stopped evidence into a pass.
- The original process-tree reader incorrectly treated unavailable Linux
  `task/*/children` files as exited threads, omitting actual descendants. The
  replacement walks all available `/proc/*/stat` PPIDs, pins root start identities,
  deduplicates overlapping trees, retains observed reparented children and fails
  closed on unreadable live process records. Its sampling is explicitly non-atomic.
- Changed, missing or malformed launch/final source hashes now fail the supervisor
  instead of merely annotating an otherwise passing result.
- Extended concurrency is restricted to fixed prefixes ending at 16, 32 or 64.
  All original resource/work limits remain unchanged. Incremental stop counters
  are mathematically equivalent to the original exact nearest-rank P99/error
  thresholds, avoiding a full sample sort for every request without dropping
  observations. Final-audit metrics use a separate stage identifier.
- The whole-supervisor watchdog stops only its owned processes at 570 seconds,
  prevents restart after expiry, joins its callback and reserves cleanup time
  within a 600-second budget. Missing shutdown confirmation cannot pass.
- Repeated native testing exposed a fixture-isolation failure: a fresh application
  database was paired with an earlier run's native reserved trash directory.
  The application correctly refused to adopt the unknown native identity. The
  harness now gives each run its own configured reserved directory shared by all
  three application roles; existing native roots are not adopted or removed.

## Native execution and remaining limits

### Independently reviewed native runs

- [Joint API run](../evidence/r3-native-run2/joint.json): five cases and 186
  assertions passed. The subsequent pressure phase correctly stopped before
  load because the prior workload had not drained; its report is preserved and
  is not pressure evidence. Its old RSS measurements must not be cited.
- [Pressure run](../evidence/r3-native-stress-run1/stress.json): all four
  1/4/8/16-client stages passed, with each of the ten declared workloads observed
  during each timed stage. There were 4,331 HTTP calls, 237 expected conflicts
  and zero unexpected errors. The final audit checked 191 files and 195 versions
  for exact current/retained bytes, IDs, hashes, latest-only quota, zero held
  reservations, path uniqueness and exact public-search version/artifact/path.
  All eight conflict races had exactly one winner. Total run time was 135.37 s;
  each timed-load phase was approximately 10.5 s, with setup/audit/drain measured
  separately rather than mislabeled as sustained throughput.
- That pressure run recorded 352 complete resource samples: the Keycloak Java
  descendant was always included, PostgreSQL had 12–21 observed descendants,
  and other named service roots were included. Peak sampled RSS was
  1,725,644,800 bytes (about 1.61 GiB); conversion/index/outbox maxima were
  37/1/22. Every stage and the final audit required three consecutive zero-backlog
  samples. The [supervisor](../evidence/r3-native-stress-run1/supervisor.json)
  recorded identical launch/final source hashes and graceful child cleanup.
- [Selected fault run](../evidence/r3-native-fault-run1/fault.json): four
  selected cases and 77 assertions passed. These prove durable pending work
  across a graceful worker stop/restart, attempted NATS publication during its
  outage, an attempted failed/requeued index job during Meilisearch absence,
  and a real successful finalize response discarded before reaching the client.
  Replaying that upload's original idempotency key/finalize returned the same
  upload/file/version without duplicate quota or version effects. Recovery
  rechecked exact bytes and the published artifact pointer. The
  [supervisor](../evidence/r3-native-fault-run1/supervisor.json) correctly remains
  `partial`: native MOVE-response loss was `not_run`. Its source hashes matched
  and child cleanup succeeded. Process restart timing is not service-readiness
  timing; actual pipeline/search recovery is reported separately.

### Extended 64-client combined run

The first attempt, [final-run1](../evidence/r3-native-final-run1/supervisor.json),
stopped in the joint test with HTTP 503 before pressure began. Its read-only
[database diagnostic](../evidence/r3-native-final-run1/retention-fixture-diagnostic.log)
and [native probe](../evidence/r3-native-final-run1/retention-native-read-confirmation.log)
confirmed the isolation problem above: no mapped native retention root, an
unknown retention-creation outcome, zero dispatched delete operations, all ten
history versions retained, and an already-existing native reserved directory.
The successful probe used TLS-verified native Graph/PROPFIND; earlier failed
diagnostic attempts remain visible rather than silently discarded.

After correcting only the harness isolation, the
[combined final-run2 supervisor](../evidence/r3-native-final-run2/supervisor.json)
finished in 295.725 seconds with matching launch/final source hashes and graceful
child cleanup. The [joint API cases](../evidence/r3-native-final-run2/joint.json)
passed all 186 assertions, the [pressure run](../evidence/r3-native-final-run2/stress.json)
passed all six requested stages, and the [selected fault cases](../evidence/r3-native-final-run2/fault.json)
passed all 77 assertions. The overall fault status correctly remains partial
because native MOVE-response loss was not run.

That pressure run made 9,725 HTTP calls with 654 expected conflicts and zero
unexpected errors. It verified 420 files and 426 versions and all ten timed
workloads at every level. All twelve same-file/same-target races had exactly one
winner. Global sampled RSS peaked at 1,962,237,952 bytes (about 1.83 GiB), with
688 complete resource samples, queue maxima of 88/1/39 and stable zero backlogs
at completion. These global observations cannot be retroactively assigned to
individual stages: that version did not record stage resource time windows.

At 32 then 64 clients, timed HTTP throughput was 121.70 then 116.04 requests/s,
and HTTP P99 was 0.890 then 1.098 seconds. This suggests a plateau in this local
client/service combination, not a universal backend capacity limit. Full-run
throughput includes setup/audit/drain and is explicitly distinct from these
timed-window rates.

### Final per-stage resource run

The [final stress-only report](../evidence/r3-native-resources-final/stress.json)
passed all six levels on the reporting-complete source. Its
[supervisor](../evidence/r3-native-resources-final/supervisor.json) finished in
266.840 seconds (258.285 seconds inside the pressure driver), with unchanged
launch/final source hash `909c46e59be2b0e43bd87c7b52b2e7529fa0eeced9b6fb973b5233de5a91187b`,
no watchdog expiry, no unfinished watchdog callback, and graceful child cleanup.
There were 11,266 HTTP calls, 726 expected conflicts and zero unexpected errors;
492 files and 498 versions passed the final full invariant audit.

All 749 raw resource samples were retained. Independent review reselected each
stage's explicit monotonic time window and reproduced every reported RSS,
queue, connection and lock peak and RAM/disk minimum exactly. Each level also
contained all ten timed workloads and the required one-winner conflict races.

| Clients | Timed HTTP requests/s | Timed HTTP P99 (s) | Full-stage sampled RSS peak (GiB) | Conversion/index/outbox sampled peaks |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 15.57 | 0.185 | 1.20 | 2 / 1 / 6 |
| 4 | 58.87 | 0.179 | 1.37 | 15 / 1 / 18 |
| 8 | 96.70 | 0.209 | 1.61 | 49 / 1 / 21 |
| 16 | 141.33 | 0.267 | 1.72 | 84 / 1 / 29 |
| 32 | 150.00 | 0.413 | 1.79 | 101 / 1 / 27 |
| 64 | 144.61 | 0.851 | 1.87 | 100 / 1 / 40 |

RSS/queue windows include each stage's races, timed load, audit and drain; the
HTTP rate/P99 columns cover only its timed-load window. The separately sampled
queue maxima are not asserted to occur simultaneously. Global RSS peaked at
2,004,197,376 bytes, below the 4 GiB stop threshold. All stages and the final
audit drained to three consecutive zero-backlog samples. Both 64-client runs
show a throughput plateau with increasing latency, but neither establishes an
absolute backend capacity limit.

The preceding combined result retains its original provenance and is not
rebound to the subsequently changed reporting-only driver. Production inputs
were unchanged; fault and joint acceptance claims remain attached to their
actual combined-run evidence, rather than being invented for this stress-only run.

The executed pressure workload used small (1 KiB) synthetic text and short
timed-load windows (approximately 65 seconds combined in the six-stage run).
It did not establish a saturation limit or reach a 5,000-item queue.
The 512 MiB and 5,000-work-item values are stop budgets, not executed volumes.
RSS snapshots cannot capture every short-lived peak. No result here establishes
multi-host capacity, prolonged soak behavior, browser/Compose acceptance, exact
10 GiB transfer, OOM/SIGKILL or in-flight worker-crash recovery, native
MOVE-response-loss proof, or all possible network partitions.
