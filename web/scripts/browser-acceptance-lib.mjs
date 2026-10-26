// Real deployment helpers. Never mock routes, persist credentials, or relax Chromium/TLS security.
import { access, readFile, stat, mkdir, open, realpath } from 'node:fs/promises';
import { constants } from 'node:fs';
import { dirname, resolve } from 'node:path';

export class AcceptanceError extends Error {
  constructor(code) { super(code); this.name = 'AcceptanceError'; this.code = code; }
}
export function requireCondition(value, code) { if (!value) throw new AcceptanceError(code); }
export function failureCode(error) {
  // Playwright errors may contain credential input, full auth URLs, or response bodies.
  // Never serialize their messages, stack, call log, cause, or arbitrary code property.
  return error instanceof AcceptanceError && /^[a-z0-9_]+$/.test(error.code) ? error.code : 'driver_or_application_error_details_withheld';
}
export function validateURL(raw, originOnly = false) {
  let url; try { url = new URL(raw); } catch { throw new AcceptanceError('invalid_endpoint_url'); }
  requireCondition(['http:', 'https:'].includes(url.protocol) && !url.username && !url.password && !url.search && !url.hash, 'unsafe_endpoint_url');
  const loopback = ['localhost', '127.0.0.1', '[::1]'].includes(url.hostname) || url.hostname.endsWith('.localhost');
  requireCondition(url.protocol === 'https:' || loopback, 'http_requires_loopback_endpoint');
  requireCondition(!originOnly || url.pathname === '/', 'public_url_must_be_origin');
  return originOnly ? url.origin : url.href.replace(/\/$/, '');
}
async function secretFile(path) {
  requireCondition(Boolean(path), 'missing_fixture_password_file');
  let info; try { info = await stat(path); } catch { throw new AcceptanceError('fixture_password_file_unreadable'); }
  requireCondition(info.isFile() && info.size > 0 && info.size <= 32768, 'invalid_fixture_password_file');
  requireCondition((info.mode & 0o077) === 0, 'fixture_password_file_must_be_private');
  return readFile(path, 'utf8');
}
export async function loadConfig(env = process.env) {
  requireCondition(!env.DEBUG && !env.PWDEBUG && !env.NODE_DEBUG, 'disable_verbose_debug_to_protect_credentials');
  requireCondition(env.NODE_TLS_REJECT_UNAUTHORIZED !== '0', 'tls_verification_must_remain_enabled');
  requireCondition(env.TEST_PUBLIC_URL && env.TEST_OIDC_ISSUER, 'missing_test_endpoint_configuration');
  requireCondition(env.TEST_USERNAME && env.TEST_USERNAME_B && env.TEST_USERNAME !== env.TEST_USERNAME_B, 'two_distinct_fixture_users_required');
  requireCondition(env.ALLOW_DISPOSABLE_TEST_WRITES === 'yes', 'disposable_writes_not_approved');
  requireCondition(env.ALLOW_DISPOSABLE_TEST_OIDC_CONSENT === 'yes', 'fixture_oidc_consent_not_approved');
  let ownerPassword, recipientPassword;
  if (env.TEST_CREDENTIALS_FILE) {
    let doc; try { doc = JSON.parse(await secretFile(env.TEST_CREDENTIALS_FILE)); } catch (error) { if (error instanceof AcceptanceError) throw error; throw new AcceptanceError('fixture_credentials_json_invalid'); }
    ownerPassword = doc.user; recipientPassword = doc.user_b ?? doc.user;
  } else {
    ownerPassword = (await secretFile(env.TEST_PASSWORD_FILE)).replace(/\r?\n$/, '');
    recipientPassword = (await secretFile(env.TEST_PASSWORD_FILE_B)).replace(/\r?\n$/, '');
  }
  requireCondition(typeof ownerPassword === 'string' && ownerPassword.length > 0 && typeof recipientPassword === 'string' && recipientPassword.length > 0, 'fixture_password_empty');
  const timeoutMs = Number(env.TEST_BROWSER_TIMEOUT_MS ?? 120000);
  requireCondition(Number.isSafeInteger(timeoutMs) && timeoutMs >= 10000 && timeoutMs <= 900000, 'invalid_browser_timeout');
  const config = { publicURL: validateURL(env.TEST_PUBLIC_URL, true), issuerURL: validateURL(env.TEST_OIDC_ISSUER), clientId: env.TEST_WEB_CLIENT_ID ?? 'clouddrive-web', chromiumPath: env.CHROMIUM_PATH ?? '/usr/bin/chromium', timeoutMs, allowConsent: true, owner: { username: env.TEST_USERNAME, password: ownerPassword }, recipient: { username: env.TEST_USERNAME_B, password: recipientPassword } };
  requireCondition(/\/realms\/[^/]+$/.test(new URL(config.issuerURL).pathname), 'keycloak_realm_issuer_required');
  try { await access(config.chromiumPath, constants.X_OK); } catch { throw new AcceptanceError('installed_chromium_not_executable'); }
  return config;
}
export async function preflight(config, { network = true } = {}) {
  if (!network) return { status: 'not_run', configuration_valid: true, browser_started: false, network_used: false };
  const get = async (url) => {
    let response; try { response = await fetch(url, { redirect: 'error', signal: AbortSignal.timeout(config.timeoutMs), headers: { Accept: 'application/json' } }); } catch { throw new AcceptanceError('preflight_endpoint_unreachable_or_tls_rejected'); }
    requireCondition(response.ok, 'preflight_endpoint_not_successful');
    requireCondition(response.headers.get('content-type')?.includes('application/json'), 'preflight_expected_json');
    try { return await response.json(); } catch { throw new AcceptanceError('preflight_invalid_json'); }
  };
  const runtime = await get(`${config.publicURL}/config.json`);
  requireCondition(runtime.oidc_issuer?.replace(/\/$/, '') === config.issuerURL && runtime.oidc_client_id === config.clientId, 'deployed_identity_config_mismatch');
  const discovery = await get(`${config.issuerURL}/.well-known/openid-configuration`);
  requireCondition(discovery.issuer === config.issuerURL, 'discovery_issuer_mismatch');
  const auth = new URL(discovery.authorization_endpoint);
  requireCondition(auth.origin === new URL(config.issuerURL).origin && auth.pathname === `${new URL(config.issuerURL).pathname}/protocol/openid-connect/auth`, 'unexpected_authorization_endpoint');
  requireCondition(discovery.code_challenge_methods_supported?.includes('S256'), 'pkce_s256_not_advertised');
  let healthResponse;
  try { healthResponse = await fetch(`${config.publicURL}/healthz`, { redirect: 'error', signal: AbortSignal.timeout(config.timeoutMs) }); }
  catch { throw new AcceptanceError('preflight_health_endpoint_unreachable_or_tls_rejected'); }
  requireCondition(healthResponse.ok, 'gateway_health_not_ready');
  const healthText = (await healthResponse.text()).trim();
  let health; try { health = JSON.parse(healthText); } catch { /* Compose gateway returns plain 'ok'. */ }
  const businessHealth = health?.status === 'ok' && health?.service === 'agent-clouddrive';
  requireCondition(healthText === 'ok' || businessHealth, 'gateway_health_not_ready');
  return { status: 'pass', configuration_valid: true, browser_started: false, network_used: true, checked: ['runtime_identity_config', 'keycloak_discovery_s256', businessHealth ? 'business_database_health' : 'gateway_edge_health_only'], business_api_requires_authenticated_case: !businessHealth };
}
export async function newSafeContext(browser) {
  return browser.newContext({ viewport: { width: 1440, height: 1000 }, acceptDownloads: true, ignoreHTTPSErrors: false, serviceWorkers: 'block' });
}
export function apiResponse(page, config, pathname, method = 'GET') {
  return page.waitForResponse(response => {
    const url = new URL(response.url());
    return url.origin === config.publicURL && url.pathname === pathname && response.request().method() === method;
  }, { timeout: config.timeoutMs });
}
export async function checkedJSON(response, code = 'api_response_not_successful') {
  requireCondition(response.ok(), code);
  try { return await response.json(); } catch { throw new AcceptanceError('application_returned_invalid_json'); }
}
export function verifyAuthorizationURL(raw, config, readOnly = false) {
  const url = new URL(raw), issuer = new URL(config.issuerURL), q = url.searchParams;
  requireCondition(url.origin === issuer.origin && url.pathname === `${issuer.pathname}/protocol/openid-connect/auth`, 'unexpected_keycloak_authorization_target');
  requireCondition(q.get('client_id') === config.clientId && q.get('redirect_uri') === `${config.publicURL}/auth/callback`, 'unexpected_oidc_client_or_callback');
  requireCondition(q.get('response_type') === 'code' && q.get('code_challenge_method') === 'S256' && Boolean(q.get('code_challenge')) && Boolean(q.get('state')), 'missing_real_pkce_authorization');
  const scopes = q.get('scope')?.split(' ') ?? [];
  requireCondition(scopes.includes('openid') && scopes.includes('drive.read') && !scopes.includes('offline_access'), 'unexpected_browser_authorization_scope');
  const expected = readOnly ? ['drive.read'] : ['drive.read', 'drive.write', 'drive.delete', 'drive.restore', 'drive.convert', 'drive.share.request', 'drive.feedback'];
  requireCondition(expected.every(scope => scopes.includes(scope)) && scopes.every(scope => ['openid', 'profile', ...expected].includes(scope)), readOnly ? 'read_only_scope_not_requested' : 'unexpected_full_scope_request');
}
export async function login(page, config, account, { readOnly = false } = {}) {
  page.setDefaultTimeout(config.timeoutMs);
  await page.goto(config.publicURL, { waitUntil: 'domcontentloaded' });
  await page.getByRole('heading', { name: '连接你的个人空间', exact: true }).waitFor();
  const checks = page.getByRole('checkbox');
  requireCondition(await checks.count() === 7, 'scope_picker_does_not_offer_seven_scopes');
  for (const check of await checks.all()) requireCondition(await check.isChecked(), 'scope_picker_default_not_selected');
  if (readOnly) {
    await page.getByRole('button', { name: '取消全选', exact: true }).click();
    requireCondition(await page.getByRole('button', { name: '继续安全登录', exact: true }).isDisabled(), 'empty_scope_login_not_disabled');
    await page.getByRole('checkbox', { name: /读取与搜索/ }).check();
  }
  const mePromise = apiResponse(page, config, '/v1/me').then(checkedJSON);
  // Register rejection handling immediately: navigation can fail before /v1/me arrives.
  mePromise.catch(() => {});
  await page.getByRole('button', { name: '继续安全登录', exact: true }).click();
  await page.waitForURL(url => url.origin === new URL(config.issuerURL).origin && url.pathname.endsWith('/protocol/openid-connect/auth'));
  verifyAuthorizationURL(page.url(), config, readOnly);
  await page.locator('#username').fill(account.username);
  await page.locator('#password').fill(account.password);
  await page.locator('#kc-login').click();
  const deadline = Date.now() + config.timeoutMs;
  while (Date.now() < deadline) {
    if (await page.getByRole('heading', { name: '我的文件', exact: true }).isVisible()) break;
    const consent = page.locator('#kc-oauth');
    if (await consent.isVisible()) {
      const url = new URL(page.url()), issuer = new URL(config.issuerURL);
      requireCondition(config.allowConsent === true && url.origin === issuer.origin && url.pathname.startsWith(`${issuer.pathname}/login-actions/`), 'unexpected_consent_page');
      // Only the initial browser client's already-checked scope request is accepted.
      await consent.locator('[name="accept"]').click();
    }
    if (await page.locator('#input-error, #kc-error-message').isVisible()) throw new AcceptanceError('keycloak_login_or_required_action_failed');
    await page.waitForTimeout(200);
  }
  await page.getByRole('heading', { name: '我的文件', exact: true }).waitFor();
  const current = new URL(page.url());
  requireCondition(current.origin === config.publicURL && !current.searchParams.has('code') && !current.searchParams.has('state'), 'authorization_callback_not_scrubbed');
  const me = await mePromise;
  requireCondition(typeof me.id === 'string' && me.id.length > 0 && Array.isArray(me.scopes), 'invalid_authenticated_identity');
  if (readOnly) requireCondition(me.scopes.includes('drive.read') && !me.scopes.some(scope => scope.startsWith('drive.') && scope !== 'drive.read'), 'server_did_not_reduce_scopes');
  return me;
}
// Credentials below come only from the real application's requests after Keycloak login.
// Used for narrow adversarial backend assertions; never serialized or used for UI login.
export function observeSession(page, config) {
  let authorization = '', live = true, sequence = 0, appliedSequence = 0;
  const pending = new Set();
  const listener = request => {
    const url = new URL(request.url());
    if (url.origin === config.publicURL && url.pathname.startsWith('/v1/')) {
      // headers() can omit security-related values; request only the necessary header.
      const requestSequence = ++sequence;
      const read = request.headerValue('authorization').then(header => {
        if (live && requestSequence >= appliedSequence && header?.startsWith('Bearer ')) { authorization = header; appliedSequence = requestSequence; }
      }).catch(() => {});
      pending.add(read); read.finally(() => pending.delete(read));
    }
  };
  page.on('request', listener);
  return {
    async request(method, path, body) {
      await Promise.all(pending);
      requireCondition(live && authorization && /^\/v1\/[a-zA-Z0-9/?=&._%:-]+$/.test(path), 'unsafe_or_unauthenticated_probe');
      const target = new URL(path, config.publicURL);
      requireCondition(target.origin === config.publicURL && target.pathname.startsWith('/v1/'), 'unsafe_probe_target');
      return page.context().request.fetch(target.href, { method, headers: { Authorization: authorization, Origin: config.publicURL }, ...(body === undefined ? {} : { data: body }), timeout: config.timeoutMs, failOnStatusCode: false, maxRedirects: 0 });
    },
    dispose() { live = false; page.off('request', listener); authorization = ''; },
  };
}
export function validateAnonymousLink(raw, config) {
  let url; try { url = new URL(raw, config.publicURL); } catch { throw new AcceptanceError('invalid_anonymous_share_link'); }
  requireCondition(url.origin === config.publicURL && !url.username && !url.password && !url.search && !url.hash && /^\/s\/[A-Za-z0-9_-]+$/.test(url.pathname), 'uncontrolled_anonymous_share_link');
  return url;
}
export function anonymousResourceKind(raw, config, publicLinkPath) {
  let url; try { url = new URL(raw, config.publicURL); } catch { return 'unexpected'; }
  if (url.origin !== config.publicURL || url.username || url.password || url.hash) return 'unexpected';
  if (url.pathname === publicLinkPath && !url.search) return 'document';
  if (url.pathname === '/favicon.ico' && !url.search) return 'favicon';
  const suffix = url.pathname.slice(publicLinkPath.length);
  if (url.pathname.startsWith(publicLinkPath) && /^\/pages\/[1-9][0-9]*$/.test(suffix) && url.searchParams.size === 1 && /^[a-f0-9-]{36}$/.test(url.searchParams.get('version_id') ?? '')) return 'raster_page';
  return 'unexpected';
}
export function verifyAnonymousHTML(html) {
  requireCondition(typeof html === 'string' && !/(?:\/v1\/|\/internal\/|\/dav\/|\bdownload\b|<script\b|<iframe\b|<object\b|<embed\b|原件|(?:ocis|rustfs|kkfileview):[0-9])/i.test(html), 'anonymous_html_exposed_disallowed_resource_or_action');
}
export function createEvidence(ids, mode = 'check-only') {
  return { schema_version: 1, suite: 'browser', mode, execution: 'not_run', status: 'not_run', started_at: new Date().toISOString(), browser_started: false, cases: ids.map(id => ({ id, status: 'not_run' })), safety: { real_keycloak_required: true, mocks: false, sandbox_required: true, tls_verification: true, credentials_persisted: false, auth_recording: false }, retained_fixtures: [] };
}
export async function createEvidenceWriter(path, { protectedPaths = [] } = {}) {
  try {
    const target = resolve(path);
    for (const protectedPath of protectedPaths.filter(Boolean)) {
      requireCondition(target !== resolve(protectedPath), 'evidence_output_aliases_credential_input');
      // Existing same-inode/real-path aliases are also rejected by exclusive create below.
      const canonical = await realpath(protectedPath).catch(() => undefined);
      requireCondition(!canonical || target !== canonical, 'evidence_output_aliases_credential_input');
    }
    await mkdir(dirname(target), { recursive: true, mode: 0o700 });
    requireCondition(constants.O_NOFOLLOW !== undefined, 'evidence_no_follow_unavailable');
    const handle = await open(target, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW, 0o600);
    // Holding the descriptor prevents a pathname swap between creation, writing and chmod.
    let used = false;
    return async evidence => {
      requireCondition(!used, 'evidence_writer_already_used'); used = true;
      try { await handle.chmod(0o600); await handle.writeFile(JSON.stringify(evidence, null, 2) + '\n'); await handle.sync(); }
      catch { throw new AcceptanceError('evidence_output_write_failed'); }
      finally { await handle.close().catch(() => {}); }
    };
  } catch (error) {
    if (error instanceof AcceptanceError) throw error;
    throw new AcceptanceError('evidence_output_must_be_new_regular_file');
  }
}
export async function writeEvidence(path, evidence, options) {
  const write = await createEvidenceWriter(path, options); await write(evidence);
}
export async function runCase(evidence, id, action) {
  const entry = evidence.cases.find(item => item.id === id);
  requireCondition(entry && entry.status === 'not_run', 'undeclared_or_duplicate_test_case');
  const started = Date.now();
  try { await action(); entry.status = 'pass'; }
  catch (error) { entry.status = 'fail'; entry.failure_code = failureCode(error); throw error; }
  finally { entry.duration_ms = Date.now() - started; }
}
export function parseArgs(args) {
  let mode = 'check-only', output = 'test-results/browser-acceptance.json', selected = false;
  for (let i = 0; i < args.length; i++) {
    const arg = args[i];
    if (['--check-only', '--preflight', '--run'].includes(arg)) { requireCondition(!selected, 'only_one_execution_mode_allowed'); mode = arg.slice(2); selected = true; }
    else if (arg === '--output') { requireCondition(Boolean(args[i + 1]) && !args[i + 1].startsWith('--'), 'missing_evidence_output_path'); output = args[++i]; }
    else throw new AcceptanceError('unsupported_cli_argument');
  }
  return { mode, output };
}
