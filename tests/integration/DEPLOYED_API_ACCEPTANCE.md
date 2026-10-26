# Prepared deployed API acceptance driver

This driver was subsequently executed on 2026-10-23 at 11:19 UTC against the
existing real native Keycloak/oCIS/PostgreSQL application stack: all five scenarios
and 178 assertions passed. See [result](../../evidence/deployed-api-candidate-run1.json)
and [provenance](../../evidence/deployed-api-candidate-provenance.json). This is not
browser, Compose, 10 GiB or full release acceptance. The old native probe/evidence
are unchanged. The driver and unit tests never update acceptance decisions; the
new results were separately inspected before promoting only A14/A16/A17.

## What is executable

`deployed_api_acceptance.py` uses the actual configured public REST API, real
Keycloak authorization-code + S256 PKCE, and the application's actual delegated
oCIS access. It never mints or substitutes identity tokens. Standard-library
Python 3.12 is sufficient; no Python package installation is needed.

| Scenario / output ID | Requirement association | Assertions in an actual run |
| --- | --- | --- |
| `retained_versions` | A16; supporting G04/G05/G08 | Upload ten distinct versions; check exact version IDs/sequences and every byte; verify latest-size-only quota after each overwrite; delete without quota release; deny current-entry reads while allowing all ten explicit owner-history reads during deletion; restore with same ID and all ten bytes/versions and no extra quota charge. |
| `subtree` | A15; supporting G04/G05 | Two-level directory, two children with two versions each; move into a separate directory; verify stable IDs/parents and changed path projections; read every retained version; delete ancestor and reject descendant current stat/content/Markdown/Range/list but preserve explicit owner source-history access; restore under a new name and verify identities, paths, all versions and unchanged quota; real marker search before move, after move/delete/restore must keep exact current source/artifact IDs and new paths, with immediate deleted-subtree exclusion. Optional raw Meilisearch checks verify source_path convergence. |
| `restore_conflict` | A14; part of A13/A18 | Independently log in a second real fixture user; require interactive session/CSRF for an internal synthetic share; delete and reject recipient reads; create a different occupying file; require `409 PATH_CONFLICT` without overwriting it; restore to new name; require old share remain disabled; explicitly approve a new grant before recipient can read again. |
| `batch100` | A17 | Pre-create one colliding child; submit exactly 100 independent `mkdir` items; require 99 committed successes and one exact path conflict, with no pending items; replay same batch and a new envelope with stable per-item keys; require identical successful operation/file IDs and exactly 100 child directories including the original collision. |
| `unauthorized` | Bounded access-control regression | Require HTTP 401 for every one of the 39 actual bearer-protected REST routes with an independent empty cookie jar and no Authorization header; also test anonymous Range. |

These scopes are intentionally narrower than full acceptance:

- No authorized audit-history reads while deleted. Audit grants must be
  provisioned by a deployment operator for individual files; the driver does not
  invent an audit API or modify the database to grant itself privileges. Owner
  `GET /versions` and explicit `content?version_id=...` are intentionally allowed
  even while deleted, and both cases verify this actual product contract. These
  owner reads are not separate-auditor-grant acceptance.
- The newer subtree scenario uses unique text markers and waits up to 120 seconds
  per stage for actual converted/indexed search results. It checks exact fixture
  file IDs, current and returned version IDs, stable artifact IDs, complete text,
  new paths and no stale flag. Immediately after deletion it requires no API hits,
  without waiting for index cleanup. These expanded checks were added after the
  five-case 178-assertion run linked above; that historical proof does not imply
  execution of the newer checks.
- Public search paths are projected from authoritative PostgreSQL metadata.
  Raw Meilisearch `source_path` convergence is separately checked only when
  `TEST_MEILI_URL` is explicitly supplied. The subtree case reports
  `raw_index_scope: "not_checked"` otherwise, or `"verified"` only after successful
  before-move, after-move and after-restore raw checks. This still does not claim
  every possible subtree depth, format, version count or production deployment.
- Anonymous-route rejection does not establish every authenticated foreign-user,
  reduced-scope, admin, group or audit combination. Public capability routes,
  OAuth callbacks, health/discovery/OPTIONS and MCP are not bearer routes and are
  excluded from this inventory. A source-contract test detects inventory drift.
- No browser, Peri, Compose provenance, external vision service, 200-page OCR,
  full deployed log privacy scan, identity disable/re-enable, 10 GiB, or network
  fault-injection acceptance is implied. A14's API-level CSRF checks are not an
  actual human-browser approval flow.
- A successful run proves only these scenarios against the configured stack.
  The operator must separately establish that the endpoints really use the
  intended unmodified services/storage mode. It is not a full release pass.

## Disposable fixture and consent contract

Use an isolated deployment and dedicated accounts that are not concurrently
used by any other writer. Tests deliberately retain synthetic files, historical
versions, directories, share records and offline grants. They **do not purge**
anything or restore quota to its starting value. A16 checks quota relative to
its baseline, and requires no pending reservations at its start. Existing
unrelated content is never selected as a test target.

Provide endpoints explicitly:

- `TEST_PUBLIC_URL`: exact public application origin, e.g.
  `https://drive.test.example` (no subpath, query or embedded credentials).
- `TEST_OIDC_ISSUER`: exact Keycloak realm issuer, e.g.
  `https://identity.test.example/realms/clouddrive`.
- `TEST_CREDENTIALS_FILE`: path to a **regular, non-symlink file owned by the
  current user, with mode 0600 or 0400**. Group/other permissions are rejected.
- `TEST_CA_FILE`: optional PEM CA bundle for the disposable deployment. If unset,
  the operating system's normal trusted roots are used. Certificate verification
  is never disabled.
- `ALLOW_DISPOSABLE_TEST_OIDC_CONSENT=yes`: separate explicit authorization for
  real fixture login/consent and, if absent, a new persistent offline delegation
  grant. Required before any network access for actual non-`unauthorized` runs.
  The CLI cannot infer or invent this authorization.

Credential JSON format (replace the illustrative strings locally, never commit
real credentials):

```json
{
  "owner": {"username": "fixture-owner", "password": "fixture-owner-password"},
  "reader": {"username": "fixture-reader", "password": "fixture-reader-password"}
}
```

Optional read-only raw-index verification for `subtree`:

- `TEST_MEILI_URL`: explicit Meilisearch origin. It uses the same verified HTTPS
  rules, or literal loopback HTTP only with `--allow-loopback-http`.
- `TEST_MEILI_INDEX`: index name (default `clouddrive`), restricted to letters,
  digits, underscore and hyphen. The native disposable fixture uses `native_e2e`.
- `TEST_MEILI_KEY_FILE`: optional private, current-user-owned non-symlink regular
  file containing only the test index API key. Group/other permissions are
  rejected. This is separate from application credentials, and is never printed.

Raw-index requests use a separate empty cookie jar, never the application's
bearer token. Requests are restricted to exact newly-created file IDs plus their
published artifact IDs, retrieving only file/version/artifact IDs and source
paths. No index mutations, settings changes or other accounts are queried.
Unavailable services, invalid responses, missing matches or failure to converge
within 120 seconds fail the case, rather than skipping a configured check.

The two usernames must differ. `reader` is required only for `restore_conflict`
(and therefore `all`). This deliberately differs from the older probe's
`{"user": "...", "backend": "..."}` format: no backend/admin client secret is
needed or consumed. Do not use privileged or production fixture users.

The configured realm's `clouddrive-web` client must permit the application's
`/auth/callback`, requested drive scopes and PKCE. Its backend offline-delegation
client and lawful oCIS access must already be configured as described in the
main deployment documentation. Unsupported MFA/required-action pages fail
closed rather than being bypassed. Tokens refresh through the same pinned
Keycloak token endpoint if necessary. Each fixture user has an independent
cookie jar and token state.

The driver defaults to HTTPS and verifies hostname and trust chain. For a
strictly local disposable test only, `--allow-loopback-http` permits exact
`localhost`, literal loopback IPv4, or literal loopback IPv6 origins. It does
**not** permit arbitrary DNS aliases, including `drive.localhost`. Use trusted
HTTPS or configure an explicit loopback origin consistently in the application
and Keycloak. There is no `--insecure` option. Environment proxy settings are
not used, preventing accidental credential transmission through a proxy.

## Commands and exit statuses

Run commands from the repository root. First configure the environment and
protected credential file locally. Neither command below embeds passwords in
arguments or reports.

```sh
python3 tests/integration/deployed_api_acceptance.py \
  --scenario all --check-only --output /tmp/deployed-api-preflight.json
```

`--check-only` only validates local configuration and the protected credential
file. It performs **zero network requests**, no auth and no mutations. Its
result always has `execution: "not_run"`, `status: "not_run"`, and every requested
case `not_run`. It returns **3** if configuration is valid, **2** if invalid.
Missing write/consent authorization does not make preflight an acceptance run.

After explicitly choosing to create synthetic files, consent to the fixture's
offline grant, and approve fixture-user-only internal shares:

```sh
# Set this only after explicitly authorizing disposable fixture OIDC consent.
export ALLOW_DISPOSABLE_TEST_OIDC_CONSENT=yes
python3 tests/integration/deployed_api_acceptance.py \
  --scenario all --allow-disposable-writes --output /tmp/deployed-api-result.json
```

`--allow-disposable-writes` authorizes the listed synthetic writes, logical
move/delete/restore, batch operations and internal fixture share approvals. It
is not production authorization. The separate consent environment opt-in is
also required for non-anonymous scenarios, even when a fixture grant already
exists, to make unattended runner authority unambiguous. The reusable
`API.delegate()` helper separately defaults to refusing a new grant.

Choose a subset using a comma-separated list, for example
`--scenario retained_versions,subtree`. Anonymous-route-only runs do not log in
or create grants, but still require the write opt-in because they exercise
mutation routes that must reject unauthenticated requests. Configuration
validation still requires a protected owner fixture file.

Actual-run exit statuses:

- `0`: all selected cases were actually executed and passed.
- `1`: setup or a selected scenario failed after execution was attempted.
- `2`: missing/unsafe configuration, missing explicit write/consent permission,
  or failure to persist the report safely. No implied acceptance pass.
- `130`: interrupted; outcomes of in-flight operations may be unconfirmed.

The complete JSON report is printed to stdout and optionally written to the
requested **new** path with private mode 0600. A new file is reserved exclusively
before any login or service write, and its owned descriptor is retained for the
final result. Existing ordinary files, symlinks and hardlinks are never
overwritten; choose a unique output filename for each run. It has `schema_version: 1`,
`suite: "deployed-api"`, `execution`, `status`, and `cases`. All requested case
IDs are declared before execution; each has a separate `acceptance_gate`,
`status`, and named checks. On failure the run stops, unexecuted cases remain
`not_run`, and the top-level result cannot pass. A fixture prefix is included
for finding retained synthetic data through the ordinary application interface.

Only fixed local diagnostic codes, static check labels, counts/flags, times,
scenario names and the synthetic fixture prefix are reported. No passwords,
usernames, tokens, cookies, request/response bodies or authorization URLs are
printed. Reports refuse to overwrite any existing path, including credentials.
Raw-index checks never print their API key or index response bodies.
Unexpected exceptions have no raw traceback/body output.

The driver never blindly repeats a MOVE after an unknown result. It polls the
existing operation ID for at most 120 seconds, then records failure/inconclusive
operation state if maintenance has not reconciled it. Finalization is bounded
and reuses the existing upload ID. Network failures stop rather than generating
a new idempotency key for the uncertain action. Re-running the whole CLI creates
a distinct fixture prefix, not a continuation of an uncertain operation; review
that operation before rerunning or cleaning up fixture data.

## Offline verification, not acceptance

```sh
python3 -m py_compile tests/integration/deployed_api_harness.py \
  tests/integration/deployed_api_acceptance.py
python3 -m unittest discover -s tests/integration \
  -p 'test_deployed_api_acceptance.py' -v
```

These tests validate local security guards, credential permissions, callback
state/path, exclusive output reservation before side effects, bounded real-search
result assertions, isolated raw-index credentials, scenario/100-item assertions
and the actual Go route and field contracts. They do not start services, mint tokens, call external APIs or
claim a deployed end-to-end pass.
