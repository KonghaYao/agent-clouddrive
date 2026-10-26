# R3 source self-test evidence

Executed on 2026-10-23 against synthetic, disposable loopback-only services. The aggregate runner exited 0. This is source/native-component evidence, not full deployed-product acceptance.

## Aggregate snapshot

`aggregate/source-before.json` identifies the source files exercised by this run. `aggregate/source-drift.json` confirms those files did not change during this aggregate execution. Subsequent integration-driver changes require the separate final driver validation and binding described below; this snapshot alone is not a claim about later files.

- Go: `go test -race -count=1 -v ./...` and `go vet ./...` passed. 10 tested packages, 92 top-level test/fuzz seed-entry passes, one explicit native oCIS test skip. Subtest counts are not added to this top-level count.
- Native services: PostgreSQL 17.11, NATS 2.15.1, Meilisearch 1.55.0 and RustFS 1.0.3. The S3 integration exercised actual SigV4 requests, checksum-verified puts, conditional conflicts, immutable publication and Range reads.
- sqlc v1.29.0 generation matched committed generated code.
- PostgreSQL migration/function invariant suite passed in a disposable schema.
- Frontend: clean `npm ci`, 50 tests in five files, TypeScript check and Vite production build passed.
- Converters: 35 discovered, **34 passed and one skipped**. Actual AnyDoc samples, pinned PaddleOCR CPU HTTP inference and scanned-PDF recovery passed. The kkFileView/LibreOffice HTTP preview test was skipped because its JAR/config were not restored for this run. Existing earlier preview evidence was not overwritten or reclassified as a new pass.
- Deployment: five tests passed, including local/S3 generated configuration validation through standalone Compose 2.39.4 and signed bucket initialization against a separate real RustFS process.
- Release-readiness discovery: 67 tests passed.
- Offline acceptance-driver preparation: 182 Python tests and 37 Node tests passed. This includes all integration `test_*.py` modules present in the snapshot. The Python guard totals overlap some release-readiness tests and must not be summed as unique cases.
- The prepared Python environment had no broken package requirements. Exact installed versions are recorded in `python-environment.txt`.

The full output is `aggregate/source-checks.log`; focused driver output is `aggregate/acceptance-driver-preparation.log`. Runtime/download versions and checksums are in `runtime-provenance.json`.

## Reproducibility defect exposed and repaired

A genuinely fresh environment containing the original self-test installation list failed five of the 33 base converter tests: `python-docx`, `openpyxl` and `python-pptx` were missing. These packages create test fixtures; the production adapter lock does not require them.

`clean-python-fixture-dependency-failure.log` preserves that failure. `scripts/selftest.sh` now installs exact test-only pins (`python-docx==1.2.0`, `openpyxl==3.1.5`, `python-pptx==1.0.2`) alongside its existing test dependencies. All 33 base tests then passed, recorded in `repaired-python-fixture-dependencies.log`. The aggregate used this explicitly prepared environment through `SELFTEST_USE_CURRENT_PYTHON=1`; it did not pretend to execute the fresh-venv branch again.

`scripts/check-acceptance-drivers.sh` now syntax-checks all integration Python files and discovers all integration `test_*.py` guards, rather than silently excluding newly added pressure/fault/supervisor tests. Its checks remain offline and never launch a browser, a deployed scenario or a 10 GiB transfer.

## Explicit exclusions

No native oCIS adapter endpoint was supplied to this source aggregate. No real Chromium smoke, complete Docker Compose startup, end-to-end Keycloak-to-Peri consent, kkFileView native preview rerun, real external vision-provider run or 10 GiB interrupted/resumed browser upload was executed by it. Those exclusions are not acceptance passes. Separate native application pressure/fault evidence, if provided, must be read independently.

## Subsequent frozen-driver binding

The pressure/supervisor drivers changed after the aggregate without changing production inputs. Earlier results are preserved rather than overwritten:

- `final-drivers/`: 14:10:26 UTC; 191 Python guards and 37 Node guards passed.
- `final-drivers-v2/`: 14:14:39 UTC; 194 Python guards and 37 Node guards passed, including supervisor source-hash mismatch/unknown fail-closed behavior.
- `final-drivers-v3/`: 14:21:06 UTC; 215 Python guards and 37 Node guards passed, **but a test source changed during the run**. Its recorded drift invalidates a single frozen-source binding for that run; it is retained only as intermediate evidence.
- `final-drivers-v4/`: 14:25:07 UTC; 216 Python guards and 37 Node guards passed without drift, covering the bounded 32/64 concurrency extension and supervisor watchdog guards.
- `final-drivers-v5/`: 14:29:46 UTC; 217 Python guards and 37 Node guards passed, with no skipped tests or source drift. Since v4 only the supervisor and its guard test changed: a unique reserved-trash directory is configured per disposable run and shared by its three application roles, avoiding a fresh-database/retained-native-folder test-harness collision without weakening the product's fail-closed behavior.

- **`final-drivers-v6/`: 14:40:22 UTC; 221 Python guards and 37 Node guards passed**, with no skipped tests or source drift. Since v5 only the pressure driver, its test and its documentation changed. The observability patch records explicit per-stage windows and throughput bases, plus sample counts, sampled peak RSS/queue/connections/waiting-lock values and sampled minimum RAM/disk; it does not change production code or broaden the safety budgets.

The latest log is `final-drivers-v6/acceptance-driver-preparation.log`. `final-drivers-v6/source-inputs.json` records its source snapshot. `final-drivers-v6/source-delta.json` lists every change from the successful aggregate snapshot: the pressure driver, supervisor, their two unit-test files, and pressure-driver documentation only. It also records the exact three-file delta from v5. All production Go, SQL, converter, web, and shell-runner inputs remained byte-identical. `final-drivers-v6/source-drift.json` confirms no changed, added or deleted source files during the latest offline run.

Consequently the aggregate results remain applicable to the unchanged production inputs, while the latest separate 221/37 guard rerun covers the changed integration drivers. This is a scoped aggregate-plus-driver-delta binding, not a claim that the entire expensive aggregate was rerun on every documentation or driver revision. It also does not claim the later native pressure/fault scenario has passed; its execution evidence remains separate. Any source edits after this snapshot require another comparison and applicable rerun.

- Aggregate input manifest SHA256: `9aac7a4840319ac3bc3207fd4d77e9e40a8179a93a0984313c8fcf83b0df3ab4`
- Latest input manifest SHA256: `0f25747e4c86208573c99b64a0752a59f5a32f4b92edde7863db841158669a63`
