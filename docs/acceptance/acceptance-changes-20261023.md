# Post-delivery acceptance work

The previously delivered ZIP is unchanged. This second source delivery contains the additions below, not a claim that all specification gates passed.

## Actual new native execution

At 2026-10-23 11:19 UTC the new deployed API driver ran against the existing
official native Keycloak 26.8.1, oCIS 8.2.2, PostgreSQL 17.11, NATS 2.15.1 and
Meilisearch 1.55.0 stack, with local artifacts and synthetic fixture accounts.
Services and the test ran in one execution namespace; PostgreSQL used TCP only.

All five scenarios and **178 assertions** passed:

- Ten distinct versions, latest-size-only quota, current-entry denial after
  deletion, exact historical bytes while trashed and after restore.
- Two-level subtree move/delete/restore with stable IDs, child gates, API paths,
  and both historical originals of each child.
- Restore path collision without overwriting its occupant; explicit new target;
  old recipient grant remains disabled until a new interactive CSRF approval.
- Exactly 100 batch items with 99 successes and one path conflict; original and
  new-envelope retries preserve the same successful operation/file identities.
- All 39 bearer-protected REST routes and anonymous Range reject missing auth.

Result: [sanitized JSON](../../evidence/deployed-api-candidate-run1.json).
Provenance: [runtime/source hashes](../../evidence/deployed-api-candidate-provenance.json).
Only A14/A16/A17 were promoted after independent review. The other observations
remain scoped supporting evidence, not all-entry authorization or browser proof.

## Added runnable preparation

- `scripts/acceptance.py`: common plan/check/run entry point; exact case inventories,
  fail-closed result validation, new private output directories, no matrix edits.
- `scripts/check-acceptance-drivers.sh`: offline syntax, pure helper, safety and
  source-contract tests; also wired into `scripts/selftest.sh`.
- `tests/test_acceptance_runner.py`: detects omitted/duplicate cases, fake zero
  exits, unexecuted passes, registry drift and small-file substitution.
- `web/scripts/browser-acceptance*.mjs`, `BROWSER-ACCEPTANCE.md`: real login and
  DOM file lifecycle/preview/sharing, genuine-session backend permission checks,
  anonymous PDF resource chain and revocation; staged safe setup and unit tests.
- `web/scripts/large-upload-*.mjs`, `LARGE_UPLOAD_ACCEPTANCE.md`, deployment
  declaration example: exact 10 GiB input generation, real Uppy upload/interruption,
  same-session/offset proof, read-only database quota/version checks, actual
  Compose/S3/multipart observations, streamed formal-download SHA256.
- `tests/integration/deployed_api_{harness,acceptance}.py`, helper tests and runbook:
  real PKCE and the five joint API scenarios above, with explicit disposable-write
  and separate persistent offline-consent opt-ins.
- `tests/integration/ocis_response_loss_proxy.py`, `tests/test_ocis_fault_proxy.py`,
  `docs/acceptance/fault-injection-acceptance.md`: a narrow one-shot real-MOVE response-loss
  helper and an exact reconciliation procedure. It is **not** a completed A23 test.

## Defect found by the enhanced real test

The enhanced subtree test additionally inspected actual Meilisearch document
metadata. Its initial run failed after 14 successful checks: public paths were
already correct, but raw `source_path` did not converge after MOVE within 120
seconds. The [failure report](../../evidence/deployed-api-subtree-search-run1.json)
and [prelaunch driver hashes](../../evidence/deployed-api-subtree-search-run1-provenance.json)
are retained, not overwritten.

A subsequent read-only PostgreSQL inspection found the MOVE and every event for
that fixture still unpublished behind a prior 100-item batch. Maintenance had
serialized one outbox item with potentially slow identity reconciliation and a
one-second delay. The correction separates independent, cancellation-joined reconciliation lanes from
continuous single-lease outbox draining and chunks large subtree path updates
into owner-scoped batches of at most 100. Individual identity queries have a five-second budget, while native/upload recovery retains its original streaming duration semantics. It does not bypass authoritative request
authorization, change source versions, or trigger OCR again.

The React UI and dependency versions were not changed; web package changes are
test commands only. Historical source self-test evidence remains intact. Final
candidate source/native rerun outcomes are recorded separately below.

## Explicit limits

No Chromium, Docker/Compose, Peri, 10 GiB or new live fault-injection test was
executed. No browser sandbox or TLS protection was weakened. Static/pure tests
cannot prove UI selectors, timing, third-party behavior or an actual large transfer.

The full unresolved scenario inventory and exact commands are in
[acceptance-drivers.md](acceptance-drivers.md). The fail-closed release gate remains
nonzero; new prepared scripts do not automatically complete any gate.


## Final native rerun

At 11:47:02 UTC the frozen, rebuilt candidate passed all **five scenarios /
186 assertions**, including **55 subtree assertions** and
`raw_index_scope: verified`. The raw-index checks require the same source version
and artifact set before and after MOVE/restore; the path update cannot pass by
reconversion or a database-only display projection.

- [Final actual result](../../evidence/deployed-api-candidate-run2.json)
- [Prelaunch source/driver/binary hashes and unchanged-after-run verification](../../evidence/deployed-api-candidate-run2-provenance.json)
- [Focused real-PostgreSQL/race scheduling regression](../../evidence/maintenance-scheduling-regression.log)

These observations retain all original execution boundaries. The focused
scheduling test uses explicit identity/search/NATS contract implementations, while
the separate native API run uses the actual official dependencies.

The first aggregate rerun exposed a test-launcher typo (`-p54222` instead of
`-p 54222`) that prevented the disposable NATS process from starting. The next
rerun passed Go/native worker/frontend but found Paddle's cache defaulting to the
read-only home directory. Both were local test setup issues; the launcher now
uses the correct NATS flag and the existing writable Paddle cache directory,
without changing permissions or bypassing sandbox restrictions. Their failed
logs remain available as `selftest-candidate-initial-runtime-failure.log` and
`selftest-candidate-ocr-cache-setup-failure.log`; they are not pass evidence.


## Final source verification

The frozen candidate completed `scripts/selftest.sh` successfully after the
launcher corrections: Go race/vet, sqlc zero drift, real PostgreSQL invariants,
actual PostgreSQL/NATS/Meilisearch worker tests, 50 frontend tests and production
build, all35 converter tests including actual CPU OCR and Office rasterization,
all5 deployment tests including actual RustFS bucket creation, root Python
regressions, and100Python/37Node driver-preparation checks. The full log is
[evidence/selftest-candidate/source-checks.log](../../evidence/selftest-candidate/source-checks.log).

The executed source digest is bound in `acceptance.json` and agrees with the
prelaunch digest from the final native run. Source verification is `pass`;
release readiness remains **not_ready:11pass,22partial,4blocked**. The browser,
Compose, Peri, real10GiB and remaining specification gates were not converted
into passes.
