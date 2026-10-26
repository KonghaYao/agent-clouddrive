# Acceptance-driver review, 2026-10-23

## Verdict

The reviewed candidate drivers pass the independent offline checks below.
The frozen maintenance correction has no remaining blocking finding in this
review. Its real native API rerun now also supports the narrow A15 scenario.
This is **not full product acceptance**. No browser, Compose, 10 GiB transfer,
or live response-loss injection was run by this review. The delivered ZIP was
not changed.

## Concrete findings and corrections

- **Owner history after deletion:** the API driver originally required owner
  `/versions` to deny access after trash, contradicting retained-history
  authorization. Current-entry denial and nonowner history denial are now
  separate; explicit owner versions and source bytes are checked while trashed.
- **Unsafe evidence overwrite:** browser/large-upload writers could overwrite
  configured inputs or follow output symlinks. All three live drivers now reserve
  a new private output before service writes, reject existing files and aliases,
  and write through the reserved handle. Regression tests preserve credentials,
  ordinary files, symlinks and hardlinks.
- **Large-upload UI mismatch:** the driver tried to click the background refresh
  button while the upload modal remained open. It now closes the completed panel
  through the real confirmation before refreshing. Genuine Authorization is
  observed with `headerValue`, not the potentially incomplete `headers` view.
- **Signed S3 probe transport:** the host-side evidence client previously allowed
  arbitrary cleartext HTTP endpoints. Both configuration and the signing helper
  now require verified HTTPS or exact localhost/loopback. The compare-only native
  Docker endpoint still supports internal HTTP. No external probe was performed.
- **Runner inventory/configuration drift:** the browser expanded from 14 to 16
  cases. Exact inventories are now contract-tested. Distinct browser/API
  credential-file formats have separate wrapper environment aliases.

## Verified safeguards

- Normal Chromium sandbox and TLS verification remain enabled; no alternate
  insecure launch, authentication-state injection or route mocking exists in
  the live browser drivers. Static diagnostics replace raw auth-bearing errors.
- Anonymous PDF capabilities remain memory-only. The new cases use an unsigned
  context, actual decoded public raster pages and post-revocation requests; they
  do not claim Office/SVG or all sharing permutations.
- The fault helper forwards at most one exact fixture MOVE with strong
  `If-Match` and `Overwrite: F`, drops only an actual upstream 201/204 response,
  and exposes subsequent attempted replays in its counters. Its fake-I/O unit
  tests and `not_run` exit status cannot establish A23 acceptance.
- The wrapper rejects empty/incomplete/duplicate cases, skipped-as-pass results,
  wrong suites, subprocess/report disagreement and a small upload claiming the
  mandatory 10 GiB result. It does not promote the release matrix.

## Independent verification

`scripts/check-acceptance-drivers.sh` completed at 2026-10-23 11:32 UTC:
**100 Python + 37 Node tests passed**, with syntax checks and zero skipped tests.
See [the complete offline log](../../evidence/security-review-driver-checks-final.log).
These are driver/helper tests, not executed browser or storage acceptance.

The separate [native API run](../../evidence/deployed-api-candidate-run1.json)
contains five executed scenarios and 178 assertions. Its real request paths,
exact version/source-byte comparisons, quota checks, restore conflict and
100-item replay assertions were reviewed against the code. It supports the
narrow A14/A16/A17 scenarios, not browser approval, audit grants, full scope
coverage or complete release acceptance. Preserve its original source hashes:
later output-writer hardening and additional subtree-search assertions must not
be retroactively attributed to that run.

The separate [enhanced subtree run](../../evidence/deployed-api-subtree-search-run1.json)
executed at 11:32 UTC and **failed**: its first 14 assertions passed, including
current public search paths after MOVE, but raw Meilisearch `source_path`
convergence timed out. The driver correctly distinguishes the database-backed
public projection from actual index metadata. This failure does not revoke the
separate A14/A16/A17 checks, and cannot be relabeled as an A15 pass.

The subsequent [native API rerun](../../evidence/deployed-api-candidate-run2.json)
completed at 11:47 UTC with **five scenarios and 186 passing assertions**.
Its 55 subtree checks include actual raw Meilisearch paths before MOVE, after
MOVE and after restore, unchanged version/artifact IDs, immediate search and
current-entry denial during trash, and retained owner history. The review
independently matched the report hash and four frozen source-file hashes against
the [prelaunch provenance](../../evidence/deployed-api-candidate-run2-provenance.json).
The executing harness additionally records unchanged source-tree and binary
hashes; this review did not separately relaunch that stack. This new evidence
supports A15's stated directory gate/identity/history/path scenario, while the
earlier failed report remains intact.

## Maintenance correction review

The failed raw-index run exposed slow synchronous identity reconciliation
between individual outbox rows. Directory updates above 100 entries also
exceeded the search adapter's batch limit. The frozen correction separates
outbox draining and reconciliation lanes, joins them on cancellation, drains
backlogs without a per-row sleep, and waits for every owner-scoped path batch of
at most 100 before acknowledging the event.

A first candidate introduced pass-wide deadlines that could skip preclaimed
principal batches or interrupt a 10 GiB recovery hash. Those deadlines were
removed before the passing native rerun. Identity reads instead have individual
five-second bounds and ordered attempts; provider errors still cannot reactivate
an account or a share. Native/upload lanes preserve prior cancellation semantics.

The [focused PostgreSQL/race log](../../evidence/maintenance-scheduling-regression.log)
records passing chunk boundary/failure, backlog, reciprocal identity-revocation,
shutdown-join, slow-first/later-disable and share regressions in 7.858 seconds.
These tests use real PostgreSQL but explicitly fake identity/search/NATS
contracts. They are not substituted for the separate native API rerun.

The additional optional raw-index probe was also reviewed: it searches only exact
fixture file/artifact IDs, requests four metadata fields, uses a separate empty
cookie jar and optional private search key, verifies HTTPS (or explicitly enabled
loopback HTTP), and never forwards the application bearer. Without configuration
it records `raw_index_scope: not_checked`, not a raw-index pass.

## Remaining execution limits

The 16 browser cases, exact 10 GiB default-S3 Compose flow and A23 live fault
scenario remain unexecuted here. The recorded host restriction stopped normal
Chromium before page creation, and a Docker daemon is unavailable. Neither was
retried with reduced security. Separate Peri, explicit audit-grant, identity/group
revocation, full feedback-log privacy, vision and real 200-page OCR acceptance
are outside these newly prepared drivers.
