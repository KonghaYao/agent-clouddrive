// Unit tests for DRIVER safety/evidence only. No browser, network, IdP or oCIS executes.
import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, readFile, rm, stat, chmod, symlink, link } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import { AcceptanceError, failureCode, validateURL, loadConfig, preflight, verifyAuthorizationURL, newSafeContext, createEvidence, writeEvidence, runCase, parseArgs, observeSession, validateAnonymousLink, anonymousResourceKind, verifyAnonymousHTML } from './browser-acceptance-lib.mjs';
import { CASE_IDS, fixturePDF } from './browser-acceptance.mjs';

const config = { publicURL: 'http://drive.localhost:8080', issuerURL: 'http://drive.localhost:8080/identity/realms/clouddrive', clientId: 'clouddrive-web', timeoutMs: 10000 };
function authURL(overrides = {}) {
  const query = new URLSearchParams({ client_id: config.clientId, redirect_uri: `${config.publicURL}/auth/callback`, response_type: 'code', code_challenge_method: 'S256', code_challenge: 'unit-test-only-not-a-real-login', state: 'unit-test-only', scope: 'openid profile drive.read', ...overrides });
  return `${config.issuerURL}/protocol/openid-connect/auth?${query}`;
}
async function withFixtureConfig(action) {
  const dir = await mkdtemp(join(tmpdir(), 'clouddrive-driver-unit-'));
  try {
    const password = join(dir, 'fixture-password'); await writeFile(password, 'UNIT-ONLY-NOT-A-REAL-CREDENTIAL\n', { mode: 0o600 });
    const env = { TEST_PUBLIC_URL: config.publicURL, TEST_OIDC_ISSUER: config.issuerURL, TEST_USERNAME: 'unit-owner', TEST_USERNAME_B: 'unit-recipient', TEST_PASSWORD_FILE: password, TEST_PASSWORD_FILE_B: password, CHROMIUM_PATH: process.execPath, ALLOW_DISPOSABLE_TEST_WRITES: 'yes', ALLOW_DISPOSABLE_TEST_OIDC_CONSENT: 'yes' };
    await action(env, dir);
  } finally { await rm(dir, { recursive: true, force: true }); }
}
test('all declared real-browser cases initially remain not_run', () => {
  const evidence = createEvidence(CASE_IDS);
  assert.equal(CASE_IDS.length, 16); assert.equal(new Set(CASE_IDS).size, 16);
  assert.equal(evidence.execution, 'not_run'); assert.equal(evidence.status, 'not_run');
  assert.equal(evidence.cases.every(entry => entry.status === 'not_run'), true);
});
test('default mode is network-free check-only; conflicting/unknown flags reject', () => {
  assert.equal(parseArgs([]).mode, 'check-only'); assert.equal(parseArgs(['--run', '--output', 'example.json']).mode, 'run');
  for (const args of [['--run', '--check-only'], ['--no-sandbox'], ['--output'], ['--output', '--run']]) assert.throws(() => parseArgs(args), AcceptanceError);
});
test('endpoint validation rejects credentials, queries and remote cleartext', () => {
  assert.equal(validateURL('https://drive.example.test/', true), 'https://drive.example.test');
  for (const url of ['https://user:secret@drive.example.test', 'https://drive.example.test/?token=x', 'http://drive.example.test', 'file:///tmp/test']) assert.throws(() => validateURL(url, true), AcceptanceError);
  assert.throws(() => validateURL('https://drive.example.test/nested', true), AcceptanceError);
});
test('configuration requires two distinct accounts, both explicit opt-ins and private files', async () => withFixtureConfig(async env => {
  const loaded = await loadConfig(env); assert.equal(loaded.owner.username, 'unit-owner');
  for (const patch of [{ TEST_USERNAME_B: env.TEST_USERNAME }, { ALLOW_DISPOSABLE_TEST_WRITES: 'no' }, { ALLOW_DISPOSABLE_TEST_OIDC_CONSENT: '' }, { DEBUG: 'pw:api' }, { PWDEBUG: '1' }, { NODE_DEBUG: 'http' }, { NODE_TLS_REJECT_UNAUTHORIZED: '0' }]) await assert.rejects(loadConfig({ ...env, ...patch }), AcceptanceError);
  await chmod(env.TEST_PASSWORD_FILE, 0o644); await assert.rejects(loadConfig(env), /fixture_password_file_must_be_private/);
}));
test('legacy synthetic credentials JSON can specify separate B password', async () => withFixtureConfig(async (env, dir) => {
  const path = join(dir, 'credentials.json'); await writeFile(path, JSON.stringify({ user: 'UNIT-A', user_b: 'UNIT-B' }), { mode: 0o600 });
  const loaded = await loadConfig({ ...env, TEST_CREDENTIALS_FILE: path }); assert.equal(loaded.owner.password, 'UNIT-A'); assert.equal(loaded.recipient.password, 'UNIT-B');
}));
test('check-only preflight cannot call fetch', async () => {
  const original = globalThis.fetch; globalThis.fetch = () => { throw new Error('NETWORK MUST NOT RUN'); };
  try { const result = await preflight(config, { network: false }); assert.equal(result.network_used, false); assert.equal(result.status, 'not_run'); }
  finally { globalThis.fetch = original; }
});
test('read-only preflight accepts Compose plain edge health without claiming business health', async () => {
  const original = globalThis.fetch, paths = [];
  const nginx = await readFile(new URL('../../deploy/compose/nginx.conf', import.meta.url), 'utf8');
  assert.match(nginx, /healthz[^\n]*return 200 'ok'/);
  globalThis.fetch = async raw => {
    paths.push(new URL(raw).pathname);
    if (raw.endsWith('/config.json')) return Response.json({ oidc_issuer: config.issuerURL, oidc_client_id: config.clientId });
    if (raw.endsWith('/.well-known/openid-configuration')) return Response.json({ issuer: config.issuerURL, authorization_endpoint: `${config.issuerURL}/protocol/openid-connect/auth`, code_challenge_methods_supported: ['S256'] });
    return new Response('ok', { headers: { 'content-type': 'text/plain' } });
  };
  try { const result = await preflight(config); assert.equal(result.status, 'pass'); assert.equal(result.business_api_requires_authenticated_case, true); assert.ok(result.checked.includes('gateway_edge_health_only')); assert.equal(paths.length, 3); }
  finally { globalThis.fetch = original; }
});
test('authorization verifier enforces genuine configured PKCE redirect and narrowed scopes', () => {
  verifyAuthorizationURL(authURL(), config, true);
  verifyAuthorizationURL(authURL({ scope: 'openid profile drive.read drive.write drive.delete drive.restore drive.convert drive.share.request drive.feedback' }), config);
  assert.throws(() => verifyAuthorizationURL(authURL({ scope: 'openid drive.read arbitrary-admin' }), config, true), AcceptanceError);
  for (const patch of [{ code_challenge_method: 'plain' }, { redirect_uri: 'https://other.example/callback' }, { client_id: 'wrong-client' }, { state: '' }, { scope: 'openid drive.read offline_access' }, { scope: 'openid drive.read drive.write' }]) assert.throws(() => verifyAuthorizationURL(authURL(patch), config, true), AcceptanceError);
});
test('safe browser context never asks for auth/video/HAR persistence or TLS bypass', async () => {
  let requested; await newSafeContext({ newContext: async options => { requested = options; return {}; } });
  assert.equal(requested.ignoreHTTPSErrors, false); assert.equal(requested.serviceWorkers, 'block');
  for (const field of ['storageState', 'recordHar', 'recordVideo', 'httpCredentials']) assert.equal(field in requested, false);
});
test('raw Playwright exceptions and spoofed arbitrary codes are withheld', () => {
  const raw = new Error('secret fill value https://idp.invalid/?code=SECRET'); raw.code = 'SECRET';
  assert.equal(failureCode(raw), 'driver_or_application_error_details_withheld');
  assert.equal(failureCode(new AcceptanceError('expected_fixture_failure')), 'expected_fixture_failure');
  assert.equal(failureCode(new AcceptanceError('Bearer SECRET')), 'driver_or_application_error_details_withheld');
});
test('case runner only marks executed actions and leaves later cases not_run', async () => {
  const evidence = createEvidence(['one', 'two', 'three']);
  await runCase(evidence, 'one', async () => {});
  await assert.rejects(runCase(evidence, 'two', async () => { throw new Error('SECRET'); }));
  assert.deepEqual(evidence.cases.map(entry => entry.status), ['pass', 'fail', 'not_run']);
  assert.equal(JSON.stringify(evidence).includes('SECRET'), false); await assert.rejects(runCase(evidence, 'missing', async () => {}));
});
test('evidence writes private JSON and does not turn prepared cases into passes', async () => withFixtureConfig(async (_, dir) => {
  const path = join(dir, 'result.json'), evidence = createEvidence(CASE_IDS); await writeEvidence(path, evidence);
  assert.equal((await stat(path)).mode & 0o777, 0o600); assert.equal(JSON.parse(await readFile(path)).status, 'not_run');
}));
test('synthetic PDF has real xref byte offsets and distinct version bytes', () => {
  const first = fixturePDF('version one'), second = fixturePDF('version two'); assert.notDeepEqual(first, second);
  const text = first.toString(); assert.match(text, /^%PDF-1.4/);
  const xref = Number(/startxref\n(\d+)/.exec(text)[1]); assert.equal(text.slice(xref, xref + 4), 'xref');
  for (let id = 1; id <= 5; id++) { const row = text.slice(xref).split('\n')[id + 2], offset = Number(row.slice(0, 10)); assert.equal(text.slice(offset, offset + 7), `${id} 0 obj`); }
  assert.throws(() => fixturePDF('not(safe)'), AcceptanceError);
});
test('authentic-session probe only sends to exact configured origin and clears on disposal', async () => {
  const listeners = new Map(), sent = [];
  const page = { on: (event, fn) => listeners.set(event, fn), off: (event) => listeners.delete(event), context: () => ({ request: { fetch: async (url, options) => { sent.push({ url, options }); return {}; } } }) };
  const session = observeSession(page, config);
  await assert.rejects(session.request('GET', '/v1/me'), AcceptanceError);
  listeners.get('request')({ url: () => `${config.publicURL}/v1/me`, headerValue: async () => 'Bearer UNIT-NOT-A-REAL-TOKEN' });
  await session.request('GET', '/v1/me'); assert.equal(sent[0].options.maxRedirects, 0); assert.equal(sent[0].url, `${config.publicURL}/v1/me`);
  await assert.rejects(session.request('GET', '//outside.example/v1/me'), AcceptanceError);
  session.dispose(); await assert.rejects(session.request('GET', '/v1/me'), AcceptanceError);
});
test('CLI missing config emits honest not_run evidence and exit 2 without browser', async () => withFixtureConfig(async (_, dir) => {
  const output = join(dir, 'missing.json');
  const result = spawnSync(process.execPath, ['scripts/browser-acceptance.mjs', '--check-only', '--output', output], { cwd: new URL('..', import.meta.url), env: { PATH: process.env.PATH }, encoding: 'utf8' });
  assert.equal(result.status, 2); const evidence = JSON.parse(await readFile(output)); assert.equal(evidence.execution, 'not_run'); assert.equal(evidence.cases.every(c => c.status === 'not_run'), true);
}));
test('CLI valid config exits 3 without launching even when executable is node, not Chromium', async () => withFixtureConfig(async (env, dir) => {
  const output = join(dir, 'ready.json');
  const result = spawnSync(process.execPath, ['scripts/browser-acceptance.mjs', '--check-only', '--output', output], { cwd: new URL('..', import.meta.url), env: { PATH: process.env.PATH, ...env }, encoding: 'utf8' });
  assert.equal(result.status, 3); const evidence = JSON.parse(await readFile(output)); assert.equal(evidence.status, 'not_run'); assert.equal(evidence.browser_started, false); assert.equal(evidence.preflight.network_used, false);
}));
test('acceptance sources contain no route mocks, storage injection, recording or insecure flags', async () => {
  for (const filename of ['browser-acceptance.mjs', 'browser-acceptance-lib.mjs']) {
    const source = await readFile(new URL(filename, import.meta.url), 'utf8');
    assert.doesNotMatch(source, /\.(?:route|addInitScript|storageState|screenshot)\s*\(/);
    assert.doesNotMatch(source, /(?:ignoreHTTPSErrors|ignoreDefaultArgs)\s*:\s*true/);
    assert.doesNotMatch(source, /(?:sessionStorage|localStorage)\s*\./);
  }
});

test('evidence exclusively creates new files and never overwrites credential aliases', async () => withFixtureConfig(async (env, dir) => {
  const original = await readFile(env.TEST_PASSWORD_FILE, 'utf8');
  const symbolic = join(dir, 'symbolic.json'), hard = join(dir, 'hard.json');
  await symlink(env.TEST_PASSWORD_FILE, symbolic); await link(env.TEST_PASSWORD_FILE, hard);
  for (const path of [env.TEST_PASSWORD_FILE, symbolic, hard, dir]) await assert.rejects(writeEvidence(path, createEvidence(CASE_IDS)), AcceptanceError);
  await assert.rejects(writeEvidence(join(dir, 'not-yet-created-password'), createEvidence(CASE_IDS), { protectedPaths: [join(dir, 'not-yet-created-password')] }), AcceptanceError);
  assert.equal(await readFile(env.TEST_PASSWORD_FILE, 'utf8'), original);
  const output = join(dir, 'exclusive.json'); await writeEvidence(output, createEvidence(CASE_IDS));
  const saved = await readFile(output, 'utf8'); await assert.rejects(writeEvidence(output, { status: 'pass' }), AcceptanceError); assert.equal(await readFile(output, 'utf8'), saved);
}));
test('CLI reserves evidence before configuration/browser work and refuses password overwrite', async () => withFixtureConfig(async (env) => {
  const before = await readFile(env.TEST_PASSWORD_FILE, 'utf8');
  const result = spawnSync(process.execPath, ['scripts/browser-acceptance.mjs', '--check-only', '--output', env.TEST_PASSWORD_FILE], { cwd: new URL('..', import.meta.url), env: { PATH: process.env.PATH, ...env }, encoding: 'utf8' });
  assert.equal(result.status, 2); assert.equal(await readFile(env.TEST_PASSWORD_FILE, 'utf8'), before);
  assert.equal((result.stdout + result.stderr).includes(before.trim()), false);
}));
test('anonymous links and every page resource are constrained to same-origin public rasters', () => {
  const raw = '/s/UNIT_PUBLIC_CAPABILITY_NOT_REAL', version = '12345678-1234-1234-1234-123456789abc';
  assert.equal(validateAnonymousLink(raw, config).pathname, raw);
  for (const value of ['https://outside.example/s/UNIT', '/v1/files/source/content', raw + '?token=UNIT', raw + '#fragment', 'https://user:secret@drive.localhost:8080/s/UNIT']) assert.throws(() => validateAnonymousLink(value, config), AcceptanceError);
  assert.equal(anonymousResourceKind(raw, config, raw), 'document');
  assert.equal(anonymousResourceKind(`${raw}/pages/1?version_id=${version}`, config, raw), 'raster_page');
  for (const value of [`${raw}/pages/1?version_id=${version}&download=true`, `${raw}/content`, '/internal/source/test.pdf', 'https://outside.example/image.png', '/v1/files/fixture/content']) assert.equal(anonymousResourceKind(value, config, raw), 'unexpected');
});
test('anonymous HTML excludes active content, original/download routes and internal URLs', () => {
  verifyAnonymousHTML('<title>CloudDrive preview</title><img src="/s/UNIT/pages/1?version_id=12345678-1234-1234-1234-123456789abc">');
  for (const html of ['<script>alert(1)</script>', '<a download href="/s/UNIT">save</a>', '<img src="/internal/source/x">', '<a href="/v1/files/x/content">原件</a>', '<img src="http://ocis:9200/dav/x">']) assert.throws(() => verifyAnonymousHTML(html), AcceptanceError);
});
test('anonymous declared cases cannot serialize raw capability URLs or page HTML to evidence', async () => {
  assert.ok(CASE_IDS.includes('anonymous_pdf_link_preview_dom_and_resource_isolation'));
  assert.ok(CASE_IDS.includes('anonymous_pdf_link_revocation_dom_and_backend'));
  const source = await readFile(new URL('browser-acceptance.mjs', import.meta.url), 'utf8');
  assert.doesNotMatch(source, /evidence[^\n]*=[^\n]*(?:anonymousLink|priorAnonymousImageURL|page\.content|grant\.(?:url|public_url))/);
  assert.doesNotMatch(source, /console\.[a-z]+\([^\n]*(?:anonymousLink|priorAnonymousImageURL|grant)/);
});
