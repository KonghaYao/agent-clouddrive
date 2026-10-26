#!/usr/bin/env node
// Stage 1 is a local plan only. Stages 2/3 never start a browser or modify a server.
import { main } from './browser-acceptance.mjs';
const args = process.argv.slice(2);
if (!args.length || (args.length === 1 && args[0] === '--plan')) {
  console.log(`STAGE 1: prepare an isolated disposable Agent CloudDrive stack.
- Use deploy/compose/setup.py and up.sh on a permitted Docker host. Do not use production accounts.
- Install exact web dependencies with npm ci and an official Chromium package with its normal sandbox.
- Serve the real built UI, /config.json, /auth/callback, /v1 and the Keycloak realm on configured origins.
- Register the real public client with Authorization Code + PKCE S256 and callback PUBLIC_URL/auth/callback.
- Provision TWO distinct dedicated fixture users. Disable temporary-password/first-login required actions for these synthetic accounts.
- Log in as fixture owner once and explicitly approve background conversion via the app's offline-access flow. The driver will not grant this persistent access.
- Prepare preview/conversion services. The automated preview fixture is a synthetic text PDF, not Office/SVG.
- Supply TEST_PUBLIC_URL, TEST_OIDC_ISSUER, TEST_USERNAME, TEST_USERNAME_B and CHROMIUM_PATH.
- Supply TEST_PASSWORD_FILE and TEST_PASSWORD_FILE_B (0600/0400 files), or TEST_CREDENTIALS_FILE (private JSON with user and optional user_b).
- Export ALLOW_DISPOSABLE_TEST_WRITES=yes and ALLOW_DISPOSABLE_TEST_OIDC_CONSENT=yes only after approving synthetic uploads/versions, a one-day read/download share to fixture B, a one-day anonymous preview-only link to the synthetic PDF, soft-delete/restore, and the browser client's ordinary consent.
- Never export DEBUG, PWDEBUG or NODE_DEBUG. No credentials go on the command line.
STAGE 2: npm run test:browser:acceptance -- --check-only --output test-results/browser-check.json
  No network/browser. Exit 3 = config valid but every behavioral case not_run. Exit 2 = config invalid.
STAGE 3: npm run test:browser:acceptance -- --preflight --output test-results/browser-preflight.json
  Read-only config/discovery/API health requests. No browser. Exit 3 = preflight ready, cases still not_run.
STAGE 4: npm run test:browser:acceptance -- --run --output test-results/browser-live.json
  Real browser actions, real Keycloak consent and real backend/storage. Exit 0 only if all declared cases pass.
  A normal-sandbox launch failure is not_run, not a pass. Never retry by disabling sandbox/TLS or injecting login state.
  Choose a fresh --output filename for each invocation; existing files and credential paths are never overwritten.
  Failure can leave uniquely named fixtures or one-day fixture/anonymous shares. Inspect retained_fixtures and dispose using the UI.
  Successful cleanup moves the synthetic source to trash; retained originals/versions continue consuming test storage.
  Native save-picker UX, Office/SVG preview, offline-grant setup and 10GiB resume are NOT covered by this suite.`);
  process.exitCode = 3;
} else if (args.includes('--run')) {
  console.error('Setup never runs browser or writes to the deployment. Use browser-acceptance.mjs --run explicitly.');
  process.exitCode = 2;
} else process.exitCode = await main(args);
