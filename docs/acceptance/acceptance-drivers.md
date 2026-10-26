# Live acceptance drivers and scoped results

This document describes the **post-delivery candidate**. The original source ZIP
is unchanged. The new browser, 10 GiB and response-loss drivers have not run their live cases.
The new deployed API suite subsequently ran against the existing real native stack:
Initially **5 scenarios /178 assertions** passed at11:19:55UTC. The frozen updated candidate subsequently passed **5 scenarios /186 assertions** at11:47:02UTC, including actual raw Meilisearch path convergence after the maintenance fix. See [the final result](../evidence/deployed-api-candidate-run2.json) and [prelaunch hash provenance](../evidence/deployed-api-candidate-run2-provenance.json). The earlier result remains retained:
[the sanitized result](../evidence/deployed-api-candidate-run1.json) and
[bound provenance](../evidence/deployed-api-candidate-provenance.json). Offline
unit/static checks alone never count as product acceptance.

## What was missing

The original `web/scripts/browser-smoke.mjs` starts a real browser but substitutes
OIDC discovery and authorization pages. Its purpose is login-page scope selection
and outbound PKCE request shape. It is **not** real login or an authenticated E2E.

`tests/integration/native_application_acceptance.py` has already run against real
Keycloak/oCIS/PostgreSQL/NATS/Meilisearch. It covers a small text upload, conversion,
exact-file search, two-user isolation, scoped denial, sharing, serial overwrites,
fixed originals, trash/restore, and MCP. It does not operate the React UI, transfer
10 GiB, create 100 batch items, or exercise ten retained versions. Its existing
evidence is retained without rerunning it just to prepare new tests.

## Entry point and result rules

From the project root:

```sh
python3 scripts/acceptance.py plan
python3 scripts/acceptance.py check --suite all
ALLOW_DISPOSABLE_TEST_WRITES=yes python3 scripts/acceptance.py run --suite browser
ALLOW_DISPOSABLE_TEST_WRITES=yes python3 scripts/acceptance.py run --suite deployed-api
ALLOW_DISPOSABLE_TEST_WRITES=yes python3 scripts/acceptance.py run --suite uppy-10gib
```

- `plan`: no network, no browser, no writes to services; exits **3**, `not_run`.
- `check`: driver configuration validation only; no browser or service mutations.
  Exits **2** for missing/unsafe setup or **3** for prepared but unexecuted cases.
- `run`: requires the explicit disposable-write opt-in and each driver's own
  prerequisites. Only a genuinely executed full selected suite can exit **0**.
  This is not the same as all release gates passing.
- New result folders are created under `test-results/` (excluded from packaging)
  or a new `--output-dir`; existing folders are never overwritten. Drivers save
  sanitized per-case JSON. Arbitrary subprocess stdout/stderr is intentionally not
  retained by this wrapper, since third-party exceptions may contain auth URLs.
- A missing result, empty cases, duplicate case IDs, skipped cases labeled pass,
  or exit-code/result disagreement fails the wrapper. No runner edits
  `docs/acceptance.json` or fills evidence hashes automatically.
- `python3 scripts/release-readiness.py` remains the independent fail-closed final
  release gate. A prepared script is not release evidence.

No driver disables the Chromium sandbox, ignores TLS errors, injects fake browser
tokens, or routes requests to mocked APIs. Browser authentication uses actual
Keycloak UI. Narrow permission/integrity probes may reuse the browser's genuine
in-memory bearer, explicitly distinguished from UI actions, never persisted.

## Required test environment

Use dedicated disposable accounts and a dedicated test stack. Synthetic fixture
files are retained through normal business trash; tests do not purge originals or
native history. Uploads consume quota and disk space. Do not use production data.

- Actual application public origin, actual configured Keycloak issuer, trusted TLS
  and matching callbacks; normal sandboxed Chromium must work for browser suites.
- The **browser** conversion/preview cases require the owner's legitimately
  consented offline grant to already exist; they do not create it. The **API**
  driver may create the fixture's offline grant only with its separate explicit
  `ALLOW_DISPOSABLE_TEST_OIDC_CONSENT=yes` opt-in. That grant is retained and is
  persistent access, not merely an upload. Never use a production identity.
- Two distinct fixture identities are needed for browser/permission scenarios.
  Store credentials outside the repository in owner-readable-only files; never
  copy tokens, passwords, or client secrets into evidence or CLI arguments.
  The browser and API drivers deliberately use different credential-file schemas.
  When using the wrapper's `--suite all`, set `TEST_BROWSER_CREDENTIALS_FILE` to the
  browser-format file and `TEST_API_CREDENTIALS_FILE` to the API-format file;
  the wrapper maps only the selected file to the child `TEST_CREDENTIALS_FILE`.
  Alternatively run each suite separately with its documented environment.
  The large-upload suite uses its separately documented password/S3 key files.
- The 10 GiB case requires the **default S3 Compose stack**, a real on-disk input of
  exactly 10,737,418,240 bytes, and read-only deployment/database/object-store
  observations. Merely setting an environment variable to `s3` is not proof.
- See [browser setup](../web/scripts/BROWSER-ACCEPTANCE.md),
  [deployed API setup](../tests/integration/DEPLOYED_API_ACCEPTANCE.md), and
  [10 GiB setup](../web/scripts/LARGE_UPLOAD_ACCEPTANCE.md) for exact fields. Some
  driver configuration is deliberately stricter than the old native probe.

There is no measured CPU/RAM or total-disk lower bound to publish. Input, retained
originals, transfer temporaries, versions, and images must coexist. Current cloud
execution has no Docker daemon and normal sandboxed Chromium fails before page
creation with `socket() failed: Operation not permitted`; the new drivers have not
been used to retry or bypass that restriction.

## Prepared coverage and remaining scope

The browser, large-upload and fault rows remain preparation, not `pass` records.
The API row has the bounded native execution result above; this does not imply
all related gates pass.

| Driver | Actual behavior to execute | Related gates, not automatically completed |
| --- | --- | --- |
| `web/scripts/browser-acceptance.mjs` | Real user login, list, Uppy small upload, PDF raster preview, history, trash/restore, human-confirmed sharing, second-user permissions, anonymous PDF resource chain and revocation | A01/A03/A13/A14/A18, G09/A21 only for PDF and the entry points explicitly asserted; Office/SVG and native save-picker UX still missing |
| `tests/integration/deployed_api_acceptance.py` | Ten source versions and retained quota, subtree lifecycle, restore collision, 100-item batch with one conflict, private-entry denial | A01/A14/A15/A16/A17; audit after deletion remains separate |
| `web/scripts/large-upload-acceptance.mjs` | Exact 10 GiB Uppy upload, actual offline/online interruption, same tus session and offset resume, one source/quota settlement, formal download SHA256 | D02, portions of G06/A24; no claim from dry-run or small-file unit tests |
| `tests/integration/ocis_response_loss_proxy.py` | Test-only one-shot loss of a real successful MOVE response; operator then checks real maintenance reconciliation without native replay | A23; proxy unit tests are not the joint scenario |

### Joint scenarios not automatically covered by these additions

These still require separate real fixtures/orchestration and evidence, even after
the above drivers are available. They are not all explained by a missing browser:

1. **G01/A19/A20:** actual IdP disable, re-enable and group-removal propagation
   across every private/shared/public entry point. Use dedicated fixture users,
   snapshot initial state, disable/remove only those identities, assert revocation,
   restore fixture state, and prove shares do not reactivate. Never turn a passing
   database contract test into this live IdP evidence.
2. **G02:** the actual Peri client must perform discovery, consent/PKCE,
   resource/audience/scope checks, refresh/re-authorization, and Streamable HTTP.
   A generic SDK/raw HTTP probe cannot stand in for Peri.
3. **G07/A06/A07/A08:** hold conversion at a known version, prove previous search
   content remains valid but latest-only preview does not leak stale pages; run a
   real 200-page scanned sample with five controlled page failures, verify page
   mapping and incompleteness in both read and search results. A fake OCR result
   does not satisfy this scenario.
4. **A09/A10/A11/D03:** real configured vision service failure/recovery and semantic
   descriptions; actual size-limit change/manual retry; a recipe/model upgrade
   followed by reconversion while earlier artifact citations still resolve.
   Endpoint/model authorization and reproducible fixtures are still required.
5. **G08/A13/A16:** native quota/cleanup configuration and retained-history audit
   under a separately provisioned explicit audit grant. Ordinary owner restore
   checks cannot prove deleted-file auditor access or native garbage-collection
   behavior. Native audit-source credentials must remain legitimately delegated.
6. **G09/A07/A20/A21:** all anonymous preview formats and subresources across latest
   replacement, group/public revocation, images, Range, download, and cached tabs.
   A single preview format or DOM-only button assertion is not all-entry coverage.
7. **G10/A22:** full gateway/application/MCP/APM/message/DB observability inventory
   and retained-log inspection after feedback; prove no identity linkage and no
   Agent read/search path. A token-string grep alone cannot prove unlinkability.
8. **D01/G06/A24:** actually start the delivered Compose graph, inspect health and
   configuration, run full application small-file flows in local and S3 modes,
   then the default-S3 large transfer. Native component processes are not this
   Compose evidence.

Before declaring any gate passed, compare its full original wording with the
executed assertions and inspect sanitized evidence. A driver can expose a product
defect; preparation does not predict its outcome.
