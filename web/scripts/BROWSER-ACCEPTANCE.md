# Real-browser acceptance driver

This is a prepared, **unexecuted live-acceptance driver**, not evidence that the application passed a real browser test. Its Node unit tests validate the driver only. It does not update `docs/acceptance.json`, rewrite the delivered ZIP, or elevate existing acceptance statuses.

`npm run test:browser` remains the separately labelled scope-selection/PKCE **mocked discovery smoke**. The new `test:browser:acceptance` suite never routes or mocks requests, supplies fake tokens, or injects authenticated browser storage.

## Staged setup

From `web`, run `npm run test:browser:setup -- --plan` for the exact staged checklist. This prints a local plan and exits 3; it does not provision anything, connect to a deployment, or run Chromium.

1. Prepare an isolated real stack using the project's deployment instructions and pinned dependencies. Use an official Chromium installation with its normal security sandbox. Do not use production or personal accounts.
2. Provision two distinct dedicated Keycloak fixture users with non-temporary synthetic passwords and no first-login required actions. The driver does not change identity-provider settings or create accounts.
3. Register the real public client for Authorization Code + PKCE S256 and the exact public `/auth/callback`. Serve the real runtime configuration and compiled application at `TEST_PUBLIC_URL`.
4. As the owner fixture user, explicitly complete the app's offline background-conversion consent manually. That persistent grant is a prerequisite, not something the test automatically creates. The driver refuses to silently create it. Have working preview/conversion, native storage and delegation services.
5. Export the settings below. Passwords must be in private files, not command-line flags. Files must have no group/world permissions, e.g. 0600 or 0400. Never disable `NODE_TLS_REJECT_UNAUTHORIZED` or run with `DEBUG`, `PWDEBUG` or `NODE_DEBUG` because those may disclose authentication data.

Required environment:

- `TEST_PUBLIC_URL`: exact HTTP(S) origin of the deployed application.
- `TEST_OIDC_ISSUER`: exact Keycloak realm issuer ending in `/realms/<realm>`.
- `TEST_USERNAME` and `TEST_USERNAME_B`: distinct fixture owner and recipient usernames.
- `TEST_PASSWORD_FILE` and `TEST_PASSWORD_FILE_B`: paths to private UTF-8 fixture-password files. Alternatively, `TEST_CREDENTIALS_FILE` is a private JSON file with `user` and optional `user_b` password values; absent `user_b` means both fixture accounts use the same synthetic password. Never commit these files.
- `ALLOW_DISPOSABLE_TEST_WRITES=yes`: explicit approval for synthetic PDFs, a second source version, soft-delete/restore, a one-day read/download share to the other fixture account, a one-day anonymous preview-only link to the same synthetic PDF, revocation and final soft-delete. Failed permission checks could leave their uniquely named synthetic directory if the server has a security bug.
- `ALLOW_DISPOSABLE_TEST_OIDC_CONSENT=yes`: explicit approval to accept the configured browser client's ordinary seven operation scopes for the owner and read-only scope for the recipient. This does not approve offline access, new clients, password updates, or other persistent grants.

Optional:

- `CHROMIUM_PATH` defaults to `/usr/bin/chromium`.
- `TEST_WEB_CLIENT_ID` defaults to `clouddrive-web`.
- `TEST_BROWSER_TIMEOUT_MS` defaults to 120000, allowed range 10000–900000.

HTTP is allowed only for loopback and `.localhost` endpoints. HTTPS certificates must be trusted normally by Node and Chromium. The driver never ignores certificate errors or browser security warnings.

## Commands and truthful exit statuses

- `npm run test:browser:driver`: Node driver-unit tests, no browser, no network. This is not live acceptance.
- `npm run test:browser:acceptance -- --check-only --output test-results/browser-check.json`: local configuration/password-file/executable validation only. No network and no browser. Exit 2 means invalid/missing configuration; exit 3 means valid configuration but **all behavioral cases remain `not_run`**. This is also the default mode.
- `npm run test:browser:acceptance -- --preflight --output test-results/browser-preflight.json`: read-only requests to deployed runtime configuration, real discovery and health endpoints. No browser. Exit 3 means those readiness checks passed, but all behavioral cases remain `not_run`. Compose's plain `ok` health reply verifies the gateway edge only, not the business API. Authenticated cases check the business API later.
- `npm run test:browser:acceptance -- --run --output test-results/browser-live.json`: launches normal-sandbox Chromium exactly once and executes the live suite. Exit 0 requires every declared case to pass; exit 1 indicates execution/startup failure. If the browser cannot launch, execution and all cases stay `not_run`, not pass.

Do not retry a sandbox/socket/access denial using disabled sandbox flags, elevated alternate launchers, CDP attachment or authentication injection. Resolve the host's supported browser capability through the appropriate authorized setup instead.

## Implemented live test scenarios

All 16 case IDs are exported in `CASE_IDS` and is present in every result, including preflight failures.

- Real Keycloak owner login: seven selected UI scopes, real PKCE redirect, actual credentials/consent, callback scrubbing, `/v1/me` identity.
- Real independent recipient login: scope picker empty-selection safeguard, `drive.read` only, distinct verified business identity.
- Actual own-file table and API list.
- Read-only recipient disabled create/upload controls plus an authenticated backend write attempt requiring `403 INSUFFICIENT_SCOPE`.
- Existing owner offline delegation prerequisite. This is an authenticated prerequisite check, not proof that granting or refreshing delegation works.
- Real Uppy file input/upload/finalize, visible committed status, and file listing.
- Cross-user private metadata/source/history denial using the recipient's real browser-issued session.
- A synthetically generated text PDF rendered through the real preview service, correct source version and controlled asset paths, and a decoded raster image in the DOM.
- DOM overwrite upload and history selection, stable file identity/new source version, then byte-hash comparison of both actual pinned originals via authenticated same-origin backend reads.
- DOM soft-delete, real trash listing, restore, same stable file/latest version and historical source preservation.
- DOM creation of a one-day share to the recipient's verified system ID, pending-request denial, disabled approval before explicit checkbox, then actual approval.
- Recipient's real shared-with-me UI, absence of owner-only controls/history/Markdown, preview rendering, actual current-source read and historical-source denial.
- DOM revocation, immediate backend source denial and disappearance from recipient listing.
- Anonymous preview-only PDF link created and explicitly confirmed through the owner UI, viewed in a fresh unsigned context with decoded PNG pages, no active HTML/original/download/internal resources, and authenticated source endpoints denied. The public capability URL, HTML and resource URLs stay memory-only.
- Anonymous link revoked through the owner UI, then its already-issued raster page URL and public HTML are requested again and must deny access. Previously decoded pixels cannot be recalled, so the assertion is about new requests.
- Successful-run cleanup by DOM soft-delete of only the generated fixture. There is no permanent-delete operation.

Backend probes are labelled as such. They use only the real bearer observed from the application's own authenticated requests, held in process memory, sent to the exact configured origin without following redirects, then cleared. They are not injected credentials and are not a substitute for real DOM login.

## Evidence and retention

Result JSON is mode 0600 and created exclusively with no-follow semantics. An existing file, hardlink, symlink or credential-input path is never overwritten. The output is reserved before any browser/network work; choose a fresh --output filename for every invocation. The schema is `{schema_version:1, suite:"browser", execution, status, cases:[{id,status,...}], ...}`. Status values are `not_run`, `pass`, and `fail`. A failed case is recorded as fail; dependent cases remain not_run. All failures use fixed sanitized codes, never raw Playwright errors, exception stacks, form contents, request headers or auth URLs.

No screenshot, trace, video, HAR, browser profile export or storage-state file is recorded. Passwords, tokens, CSRF values and identities are not added to evidence. `retained_fixtures` contains only generated synthetic filenames/resource IDs and lifecycle/revocation state so cleanup can be verified. It is not a token or download-link inventory.

A failed run can leave a source/version, pending request or one-day user/anonymous share created earlier in that run. The test does not blindly replay an uncertain mutation or perform destructive cleanup after a failure. Use the generated `browser-acceptance-<uuid>.pdf` filename and evidence to inspect the isolated fixture account. An unexpected successful forbidden directory create is additionally identified in `unexpected_mutations` when its response ID is available. Successful cleanup puts the PDF into trash; retained originals/history still consume test storage.

## Explicitly outside this suite

- Native `showSaveFilePicker`/OS save-dialog UX. Playwright's upload filechooser does not control the native save picker. The driver neither stubs it nor pretends it was tested; actual small source downloads are authenticated HTTP byte comparisons after DOM version selection.
- Office, SVG, image-only and mixed-scan preview/OCR visual correctness; this suite's actual preview is a text PDF only.
- Automated creation of persistent offline consent, SSO refresh/expiry, first-login actions, MFA, WebAuthn/passkeys and administrative account provisioning.
- Agent/Peri/MCP flows, fixed-version/group sharing, folder move/rename/batch operations, and search-index completeness.
- Mobile/responsive layout and screenshot-based visual review.
- 10 GiB disconnect/resume, which has a separate driver and cannot be inferred from the small PDF upload.

These omissions must not be reported as passed because preparation or driver-unit tests succeeded.
