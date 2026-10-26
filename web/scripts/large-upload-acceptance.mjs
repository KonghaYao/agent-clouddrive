#!/usr/bin/env node
// Explicit opt-in destructive test against a disposable single-node S3 Compose deployment.
// Chromium is NEVER launched by --check-only. No route mocks, browser flags, injected tokens,
// hand-written tus client, or Python/native substitute for the actual Uppy browser upload.
import assert from 'node:assert/strict';
import { readFile, stat } from 'node:fs/promises';
import { basename, resolve } from 'node:path';
import { parseArgs } from 'node:util';
import { chromium } from 'playwright-core';
import { AcceptanceError } from './browser-acceptance-lib.mjs';
import { createEvidenceWriter, assertSafeEnvironment, TEN_GIB, CHUNK_BYTES, expectedSize, safeURL, safeProbeURL, hashFile, quotaSnapshot, assertQuotaOnce, assertResume, assertNativeSession, assertHashes, nativeSnapshot, inspectDeployment, listS3Objects, proveMultipart, streamDownload, readSecret } from './large-upload-lib.mjs';
const { values: args } = parseArgs({ options: { run: { type: 'boolean' }, 'check-only': { type: 'boolean' }, output: { type: 'string', default: 'test-results/uppy-10gib.json' }, 'smoke-bytes': { type: 'string' } }, strict: true });
const CASES = [
  ['deployment', 'Live default S3 single-node Compose and gateway configuration'],
  ['source', 'Exact on-disk input and streaming source SHA256'],
  ['login', 'Real browser OIDC PKCE login'],
  ['ui_upload', 'Real Uppy chooser and one business upload session'],
  ['interruption_resume', 'Actual context offline, resumed positive HEAD/PATCH offsets'],
  ['native_session', 'Same native/business upload and receiver node before/after interruption'],
  ['commit_once', 'One source version and idempotent finalize replay'],
  ['quota_once', 'One held reservation and one quota settlement'],
  ['download_integrity', 'Formal streamed download byte count and SHA256'],
  ['s3_multipart', 'Live new exact-size S3 object and multipart HEAD evidence'],
  ['processing_state', 'Original saved and explicit over-limit conversion state'],
  ['receiver_survived', 'Same browser process and uninterrupted receiver container'],
];
const report = { schema_version: 1, suite: args['smoke-bytes'] === undefined ? 'uppy-10gib' : 'uppy-small-smoke', execution: 'not_run', status: 'not_run', mode: args['smoke-bytes'] === undefined ? 'acceptance_10gib' : 'small_smoke_not_10gib', accepts_10gib: false, started_at: new Date().toISOString(), cases: CASES.map(([id, description]) => ({ id, description, status: 'not_run' })), missing: [], evidence: { events: [] } };
const output = resolve(args.output), env = process.env;
let evidenceWriter, evidenceWriteFailed = false, browser, context, activeCase, credentials = {}, config, declaration;
const failCase = (id, data = {}) => Object.assign(report.cases.find(c => c.id === id), { status: 'fail', ...data });
const passCase = (id, data = {}) => Object.assign(report.cases.find(c => c.id === id), { status: 'pass', ...data });
const runCase = id => { activeCase = id; };
// Never serialize Playwright errors, assertion diffs, stacks or arbitrary remote text.
const safeError = error => error instanceof AcceptanceError && /^[a-z0-9_]+$/.test(error.code)
  ? error.code : activeCase ? `${activeCase}_assertion_or_execution_failed` : `${report.configuration_check || 'configuration'}_invalid`;

async function save() {
  if (evidenceWriteFailed) throw new Error('evidence_output_unavailable');
  try {
    evidenceWriter ??= await createEvidenceWriter(output, ['TEST_PASSWORD_FILE','TEST_PASSWORD_FILE_B','TEST_CREDENTIALS_FILE','CHROMIUM_PATH','LARGE_UPLOAD_SOURCE_FILE','LARGE_UPLOAD_DEPLOYMENT_FILE','LARGE_UPLOAD_S3_ACCESS_KEY_FILE','LARGE_UPLOAD_S3_SECRET_KEY_FILE'].map(key => env[key]));
    await evidenceWriter.write(report);
  } catch { evidenceWriteFailed = true; throw new Error('evidence_output_unavailable'); }
}

async function waitFor(check, timeoutMs, label) {
  const end = Date.now() + timeoutMs;
  while (Date.now() < end) { const result = await check(); if (result) return result; await new Promise(r => setTimeout(r, 250)); }
  throw new Error(`Timed out waiting for ${label}`);
}
async function configure() {
  report.configuration_check = 'safe_runtime_environment'; assertSafeEnvironment(env);
  const required = ['TEST_PUBLIC_URL','TEST_OIDC_ISSUER','TEST_USERNAME','TEST_PASSWORD_FILE','CHROMIUM_PATH','LARGE_UPLOAD_SOURCE_FILE','LARGE_UPLOAD_DEPLOYMENT_FILE','LARGE_UPLOAD_DATABASE_URL','LARGE_UPLOAD_S3_ACCESS_KEY_FILE','LARGE_UPLOAD_S3_SECRET_KEY_FILE'];
  for (const name of required) if (!env[name]) report.missing.push(name);
  if (env.ALLOW_DISPOSABLE_TEST_WRITES !== 'yes') report.missing.push('ALLOW_DISPOSABLE_TEST_WRITES=yes');
  if (env.ALLOW_DISPOSABLE_TEST_OIDC_CONSENT !== 'yes') report.missing.push('ALLOW_DISPOSABLE_TEST_OIDC_CONSENT=yes');
  if (report.missing.length) return;
  report.configuration_check = 'input_mode_and_endpoint_urls';
  const size = expectedSize(args['smoke-bytes']);
  const { validateURL } = await import('./browser-acceptance-lib.mjs');
  validateURL(env.TEST_PUBLIC_URL, true); validateURL(env.TEST_OIDC_ISSUER);
  config = { publicURL: safeURL(env.TEST_PUBLIC_URL, { originOnly: true }), issuerURL: safeURL(env.TEST_OIDC_ISSUER), clientId: env.TEST_WEB_CLIENT_ID || 'clouddrive-web', timeoutMs: 120_000, allowConsent: true, size, source: resolve(env.LARGE_UPLOAD_SOURCE_FILE), transferTimeoutMs: Number(env.LARGE_UPLOAD_TIMEOUT_MS || 7_200_000), offlineMs: Number(env.LARGE_UPLOAD_OFFLINE_MS || 8000) };
  assert.ok(Number.isSafeInteger(config.transferTimeoutMs) && config.transferTimeoutMs >= 60_000 && config.transferTimeoutMs <= 86_400_000, 'Timeout must be 1 minute–24 hours');
  assert.ok(Number.isSafeInteger(config.offlineMs) && config.offlineMs >= 5000 && config.offlineMs <= 30_000, 'Offline interval must be 5–30 seconds');
  report.configuration_check = 'source_file_exact_size';
  assert.equal((await stat(config.source)).size, size, 'Input file is not the required exact size');
  report.configuration_check = 'chromium_binary';
  assert.ok((await stat(env.CHROMIUM_PATH)).isFile(), 'CHROMIUM_PATH is not a file');
  report.configuration_check = 'deployment_declaration';
  declaration = JSON.parse(await readFile(env.LARGE_UPLOAD_DEPLOYMENT_FILE, 'utf8'));
  assert.equal(declaration.disposable, true); assert.equal(declaration.single_node_compose, true); assert.equal(declaration.storage_mode, 's3');
  assert.equal(safeURL(declaration.public_url, { originOnly: true }), config.publicURL);
  assert.ok(declaration.compose_project && declaration.containers && declaration.s3 && declaration.components, 'Deployment declaration incomplete');
  for (const name of ['api', 'ocis', 'rustfs', 'gateway']) assert.ok(declaration.containers[name], `Missing container ${name}`);
  for (const name of ['uppy_core', 'uppy_tus', 'ocis', 'rustfs']) assert.ok(declaration.components[name], `Missing declared component version ${name}`);
  for (const name of ['probe_endpoint', 'native_endpoint', 'bucket', 'region']) assert.ok(declaration.s3[name], `Missing S3 ${name}`);
  safeProbeURL(declaration.s3.probe_endpoint); safeURL(declaration.s3.native_endpoint);
  assert.ok(Number.isSafeInteger(declaration.conversion_max_bytes) && declaration.conversion_max_bytes > 0, 'Declare live automatic-conversion maximum bytes');
  report.configuration_check = 'private_credential_files';
  credentials = { accessKey: await readSecret(env.LARGE_UPLOAD_S3_ACCESS_KEY_FILE), secretKey: await readSecret(env.LARGE_UPLOAD_S3_SECRET_KEY_FILE) };
  config.password = await readSecret(env.TEST_PASSWORD_FILE);
  report.configuration_check = 'valid';
  report.configuration = { public_url: config.publicURL, source_name: basename(config.source), expected_bytes: size, expected_uppy_chunk_bytes: CHUNK_BYTES, offline_ms: config.offlineMs, timeout_ms: config.transferTimeoutMs, compose_project: declaration.compose_project, component_versions: declaration.components, component_version_provenance: 'operator declaration; actual runtime image IDs and browser version are separately recorded', required_backend: 's3ng_on_rustfs', write_opt_in: true, consent_opt_in: true, small_smoke_does_not_satisfy_10gib: report.mode !== 'acceptance_10gib' };
}
try {
  assert.ok(Boolean(args.run) !== Boolean(args['check-only']), 'Choose exactly one of --run or --check-only');
  await configure();
  // Refuse an unsafe/existing output before any network or browser execution.
  await save();
  if (args['check-only']) {
    report.reason = report.missing.length ? 'Configuration is incomplete. No network checks or browser execution occurred.' : 'Local configuration is valid only. Deployment/authentication/S3/browser/upload have NOT run.';
    process.exitCode = report.missing.length ? 2 : 3;
  } else {
    assert.equal(report.missing.length, 0, `Missing configuration: ${report.missing.join(', ')}`);
    report.execution = 'executed';
    runCase('deployment');
    const deployment = await inspectDeployment(declaration); report.evidence.deployment_before = deployment;
    assert.ok(Number(deployment.containers.api.environment.DRIVE_UPLOAD_MAX_BYTES || 20 * 1024 ** 3) >= config.size, 'Live API upload limit cannot admit exact input');
    assert.equal(Number(deployment.containers.api.environment.DRIVE_CONVERT_MAX_BYTES || 100 * 1024 ** 2), declaration.conversion_max_bytes, 'Declared conversion maximum does not match live API configuration');
    const s3Before = await listS3Objects(credentials, declaration); report.evidence.s3_before_object_count = s3Before.length;
    passCase('deployment', { source: 'live read-only Docker inspect/nginx -T and authenticated S3 ListObjectsV2' });
    runCase('source');
    const source = await hashFile(config.source); assert.equal(source.bytes, config.size); report.evidence.source = source; passCase('source'); await save();
    runCase('login');
    const { login, newSafeContext } = await import('./browser-acceptance-lib.mjs');
    browser = await chromium.launch({ executablePath: env.CHROMIUM_PATH, headless: env.LARGE_UPLOAD_HEADED !== 'yes', chromiumSandbox: true });
    context = await newSafeContext(browser);
    const page = await context.newPage(); page.setDefaultTimeout(120_000);
    report.evidence.browser = { version: browser.version(), sandbox: true, browser_flags_added: [], page_created_at: new Date().toISOString() };
    let auth = '', csrf = '', authRequestSequence = 0, observedAuthSequence = 0, sequence = 0, fatal, interruptionPromise, interruption, latestUIFinalize, committedUIOperation, retryClicked = false;
    const events = report.evidence.events, sessions = [], finalizes = [], pageErrors = [], authPending = new Set();
    page.on('pageerror', error => pageErrors.push(safeError(error)));
    page.on('request', request => { const pending = (async () => {
      const url = new URL(request.url()); if (url.origin !== config.publicURL || !url.pathname.startsWith('/v1/')) return;
      const requestSequence = ++authRequestSequence;
      const authorization = await request.headerValue('authorization');
      const token = await request.headerValue('x-csrf-token');
      if (authorization && requestSequence >= observedAuthSequence) { auth = authorization; observedAuthSequence = requestSequence; }
      if (token) csrf = token;
    })(); authPending.add(pending); pending.catch(error => { fatal = error; }).finally(() => authPending.delete(pending)); });
    page.on('requestfailed', request => {
      const url = new URL(request.url()); if (url.origin !== config.publicURL || !url.pathname.startsWith('/v1/tus/')) return;
      events.push({ seq: ++sequence, at: new Date().toISOString(), method: request.method(), session_id: url.pathname.split('/').pop(), status: null, error: /^net::ERR_[A-Z_]+$/.test(request.failure()?.errorText ?? '') ? request.failure().errorText : 'browser_request_failed', request_offset: Number(request.headers()['upload-offset'] ?? NaN) });
    });
    async function api(path, { method = 'GET', body } = {}) {
      assert.ok(path.startsWith('/v1/') && !path.includes('://'), 'Only formal same-origin REST endpoints allowed');
      assert.ok(auth.startsWith('Bearer '), 'No genuine authenticated UI request observed');
      const response = await context.request.fetch(config.publicURL + path, { method, headers: { Authorization: auth, ...(csrf ? { 'X-CSRF-Token': csrf } : {}), ...(body ? { 'Content-Type': 'application/json' } : {}) }, ...(body ? { data: body } : {}), maxRedirects: 0, timeout: config.transferTimeoutMs });
      assert.ok(response.ok(), `Formal API ${method} ${path.split('?')[0]} returned HTTP ${response.status()}`);
      const result = await response.json(); await response.dispose(); return result;
    }
    async function listAllFiles() {
      const items = []; let cursor; const seen = new Set();
      do { const result = await api('/v1/files' + (cursor ? '?cursor=' + encodeURIComponent(cursor) : '')); assert.ok(Array.isArray(result.items)); items.push(...result.items); assert.ok(items.length < 10000, 'Use a dedicated fixture user with fewer than 10000 files'); cursor = result.next_cursor; if (cursor) { assert.ok(!seen.has(cursor), 'File cursor repeated'); seen.add(cursor); } } while (cursor);
      return items;
    }
    async function refreshBrowserCredentials() {
      const meResponse = page.waitForResponse(r => new URL(r.url()).origin === config.publicURL && new URL(r.url()).pathname === '/v1/me' && r.request().method() === 'GET');
      await page.getByRole('button', { name: '刷新', exact: true }).click(); const response = await meResponse; assert.ok(response.ok(), 'Real UI refresh did not renew authenticated access'); await Promise.all([...authPending]);
    }
    page.on('response', response => { void (async () => {
      const request = response.request(), url = new URL(response.url());
      if (url.origin !== config.publicURL) return;
      if (url.pathname === '/v1/session' && response.ok()) { csrf = (await response.json()).csrf_token ?? csrf; return; }
      if (url.pathname === '/v1/uploads' && request.method() === 'POST') {
        const data = response.ok() ? await response.json() : {};
        sessions.push({ status: response.status(), id: data.id, operation_id: data.operation_id, upload_path: data.upload_url ? new URL(data.upload_url, config.publicURL).pathname : null }); return;
      }
      if (/^\/v1\/uploads\/[^/]+\/finalize$/.test(url.pathname) && request.method() === 'POST') {
        const raw = response.ok() ? await response.json() : null;
        const body = raw ? Object.fromEntries(['file_id','version_id','operation_id','status','state','reason'].filter(key => typeof raw[key] === 'string').map(key => [key, raw[key]])) : null;
        const item = { seq: ++sequence, status: response.status(), session_id: url.pathname.split('/')[3], result: body, source: 'actual_ui' };
        finalizes.push(item); latestUIFinalize = item; return;
      }
      if (/^\/v1\/operations\/[^/]+$/.test(url.pathname) && response.ok()) {
        const op = await response.json(); if (op.id === sessions[0]?.operation_id && op.state === 'committed') committedUIOperation = { file_id: op.file_id, version_id: op.result_version_id, status: 'committed', operation_id: op.id }; return;
      }
      if (!url.pathname.startsWith('/v1/tus/') || !['HEAD','PATCH'].includes(request.method())) return;
      const headers = await response.allHeaders();
      const event = { seq: ++sequence, at: new Date().toISOString(), method: request.method(), session_id: url.pathname.split('/').pop(), status: response.status(), request_offset: Number(request.headers()['upload-offset'] ?? NaN), response_offset: Number(headers['upload-offset'] ?? NaN), response_length: Number(headers['upload-length'] ?? NaN) };
      events.push(event);
      if (!interruptionPromise && event.method === 'PATCH' && event.status === 204 && event.response_offset > 0 && event.response_offset < config.size) {
        interruption = { confirmed_offset: event.response_offset, last_confirmed_seq: event.seq, offline_requested_at: new Date().toISOString() }; report.evidence.interruption = interruption;
        interruptionPromise = (async () => {
          await context.setOffline(true); interruption.offline_completed_at = new Date().toISOString();
          // Drain response events already acknowledged before the offline transition completed.
          await new Promise(r => setTimeout(r, 250));
          const acknowledged = events.filter(e => e.method === 'PATCH' && e.status === 204);
          const last = acknowledged.reduce((a, b) => b.response_offset > a.response_offset ? b : a, event);
          interruption.confirmed_offset = last.response_offset; interruption.last_confirmed_seq = last.seq;
          assert.ok(interruption.confirmed_offset < config.size, 'Transfer finished before network interruption');
          // Read only out-of-browser inspection cannot transfer file bytes or replace browser upload.
          await waitFor(() => sessions.length, 10_000, 'UI-created business upload session');
          report.evidence.native_during_interruption = await nativeSnapshot(env.LARGE_UPLOAD_DATABASE_URL, sessions[0].id);
          report.evidence.quota_held = quotaSnapshot(await api('/v1/me'));
          await new Promise(r => setTimeout(r, config.offlineMs));
          interruption.online_event_seq = sequence;
          await context.setOffline(false); interruption.online_completed_at = new Date().toISOString();
        })();
        interruptionPromise.catch(error => { fatal = error; });
      }
    })().catch(error => { fatal = error; }); });
    const me = await login(page, config, { username: env.TEST_USERNAME, password: config.password });
    await waitFor(() => { if (fatal) throw fatal; return auth.startsWith('Bearer '); }, 10_000, 'genuine authenticated UI request');
    report.evidence.quota_before = quotaSnapshot(me);
    assert.equal(report.evidence.quota_before.reserved_bytes, 0, 'Use an idle dedicated test user');
    assert.ok(report.evidence.quota_before.limit_bytes - report.evidence.quota_before.used_bytes >= config.size, 'Insufficient business quota for exact-size upload');
    passCase('login', { source: 'genuine browser OIDC authorization code + PKCE, actual /v1/me' });
    runCase('ui_upload');
    const listBefore = await listAllFiles(); assert.ok(!listBefore.some(file => file.name === basename(config.source)), 'Input basename already exists; use a fresh disposable source filename');
    await page.getByRole('button', { name: '上传文件', exact: true }).click();
    await page.locator('.uppy-Dashboard').waitFor();
    const [chooser] = await Promise.all([page.waitForEvent('filechooser'), page.locator('.uppy-Dashboard-browse').first().click()]);
    await chooser.setFiles(config.source); // A real disk file selected by the rendered Uppy chooser.
    await page.locator('.uppy-StatusBar-actionBtn--upload').click();
    await waitFor(() => { if (fatal) throw fatal; return interruptionPromise; }, config.transferTimeoutMs, 'first successful partial browser PATCH');
    await interruptionPromise; if (fatal) throw fatal;
    assert.equal(sessions.length, 1); assert.ok(sessions[0].status >= 200 && sessions[0].status < 300);
    assert.equal(sessions[0].upload_path, `/v1/tus/${sessions[0].id}`);
    report.evidence.business_sessions = sessions;
    passCase('ui_upload', { input: 'actual Uppy dashboard filechooser, disk path', upload_transport: 'actual Uppy tus browser HTTP' });
    runCase('interruption_resume');
    await waitFor(async () => {
      if (fatal) throw fatal;
      if (latestUIFinalize?.status >= 400) throw new Error(`UI finalize failed with HTTP ${latestUIFinalize.status}`);
      if (committedUIOperation) return true;
      if (latestUIFinalize?.result && ['saved','committed','completed','succeeded'].includes(latestUIFinalize.result.status ?? latestUIFinalize.result.state)) return true;
      const retry = page.locator('.uppy-StatusBar-actionBtn--retry');
      if (!retryClicked && await retry.isVisible()) { await retry.click(); retryClicked = true; }
      return false;
    }, config.transferTimeoutMs, 'actual UI transfer and successful finalize');
    assert.equal(sessions.length, 1, 'Uppy created a second business session after resume');
    assert.ok(events.some(e => e.status === null && e.at >= interruption.offline_completed_at && e.at <= interruption.online_completed_at), 'No failed browser tus request observed during offline interval');
    assert.ok(events.filter(e => e.status >= 200 && e.status < 300).every(e => e.response_length === config.size), 'Successful tus responses must report exact total Upload-Length');
    report.evidence.resume = assertResume(events, interruption, config.size, sessions[0].id);
    passCase('interruption_resume');
    runCase('commit_once');
    assert.equal(finalizes.length, 1, 'Expected exactly one UI finalize; replay is separately recorded');
    await page.getByText('原件已保存，转换另行处理', { exact: true }).waitFor();
    // The upload Modal blocks the background refresh button. Close only after saved.
    await page.locator('.ant-modal:visible').filter({ has: page.locator('.uppy-Dashboard') }).locator('.ant-modal-close').click();
    await page.getByRole('button', { name: '关闭面板', exact: true }).click();
    await page.locator('.uppy-Dashboard').waitFor({ state: 'detached' });
    report.evidence.upload_panel_closed_after_saved = true;
    await refreshBrowserCredentials();
    const commit = committedUIOperation ?? latestUIFinalize.result; assert.ok(commit.file_id && commit.version_id);
    report.evidence.ui_finalize = finalizes; if (committedUIOperation) report.evidence.ui_committed_operation = committedUIOperation;
    const versionPath = `/v1/files/${encodeURIComponent(commit.file_id)}/versions`;
    const versions = await api(versionPath); assert.equal(versions.items.length, 1); assert.equal(versions.items[0].id, commit.version_id);
    assert.equal(Number(versions.items[0].size ?? versions.items[0].size_bytes), config.size); assert.equal(versions.items[0].sha256, source.sha256);
    const files = await listAllFiles(); const matches = files.filter(file => file.name === basename(config.source)); assert.equal(matches.length, 1); assert.equal(matches[0].id, commit.file_id);
    report.evidence.quota_committed = quotaSnapshot(await api('/v1/me'));
    report.evidence.native_committed = await nativeSnapshot(env.LARGE_UPLOAD_DATABASE_URL, sessions[0].id);
    const replay = await api(`/v1/uploads/${sessions[0].id}/finalize`, { method: 'POST', body: { idempotency_key: `finalize:${sessions[0].id}` } });
    assert.equal(replay.file_id, commit.file_id); assert.equal(replay.version_id, commit.version_id); assert.ok(['saved','committed'].includes(replay.status));
    const replayVersions = await api(versionPath); assert.equal(replayVersions.items.length, 1); assert.equal(replayVersions.items[0].id, commit.version_id);
    report.evidence.finalize_replay = { file_id: replay.file_id, version_id: replay.version_id, status: replay.status };
    report.evidence.source_version = { file_id: commit.file_id, version_id: commit.version_id, size_bytes: config.size, sha256: versions.items[0].sha256, count_before_replay: versions.items.length, count_after_replay: replayVersions.items.length };
    passCase('commit_once');
    runCase('native_session');
    report.evidence.native_replayed = await nativeSnapshot(env.LARGE_UPLOAD_DATABASE_URL, sessions[0].id);
    assertNativeSession(report.evidence.native_during_interruption, report.evidence.native_committed, report.evidence.native_replayed, config.size); passCase('native_session');
    runCase('quota_once');
    report.evidence.quota_replayed = quotaSnapshot(await api('/v1/me'));
    assertQuotaOnce(report.evidence.quota_before, report.evidence.quota_held, report.evidence.quota_committed, report.evidence.quota_replayed, config.size); passCase('quota_once'); await save();
    runCase('download_integrity');
    await refreshBrowserCredentials();
    const contentURL = `${config.publicURL}/v1/files/${encodeURIComponent(commit.file_id)}/content?version_id=${encodeURIComponent(commit.version_id)}`;
    const cookies = await context.cookies(contentURL); const downloadHeaders = { Authorization: auth, 'Accept-Encoding': 'identity', Cookie: cookies.map(c => `${c.name}=${c.value}`).join(';') };
    const download = await streamDownload(contentURL, downloadHeaders, config.size, config.transferTimeoutMs);
    report.evidence.download = { ...download, route: '/v1/files/:id/content?version_id=:id' }; assertHashes(source, download, config.size); passCase('download_integrity');
    const sourceAfter = await stat(config.source); assert.equal(sourceAfter.size, source.bytes); assert.equal(sourceAfter.mtimeMs, source.mtime_ms); assert.equal(sourceAfter.ino, source.inode);
    runCase('s3_multipart');
    const s3After = await listS3Objects(credentials, declaration); report.evidence.s3_multipart = await proveMultipart(s3Before, s3After, credentials, declaration, config.size); passCase('s3_multipart');
    runCase('processing_state');
    await refreshBrowserCredentials();
    let file = await api(`/v1/files/${commit.file_id}`);
    if (config.size > declaration.conversion_max_bytes) {
      file = await waitFor(async () => { const f = await api(`/v1/files/${commit.file_id}`); return ['not_converted','blocked','failed'].includes(f.conversion?.status) ? f : false; }, 180_000, 'explicit over-limit conversion result');
      assert.equal(file.conversion?.status, 'not_converted');
      assert.equal(file.conversion?.error_code, 'limit_exceeded', 'Above-limit original must have explicit limit_exceeded, not unrelated processing failure');
    }
    report.evidence.processing = { original_saved: true, conversion: file.conversion ?? null, declared_conversion_max_bytes: declaration.conversion_max_bytes }; passCase('processing_state');
    runCase('receiver_survived');
    const deploymentAfter = await inspectDeployment(declaration);
    for (const service of ['api','ocis','rustfs','gateway']) for (const key of ['id','started_at']) assert.equal(deploymentAfter.containers[service][key], deployment.containers[service][key], `${service} restarted/replaced during this network-only test`);
    assert.equal(browser.isConnected(), true); assert.equal(page.isClosed(), false); assert.deepEqual(pageErrors, []);
    report.evidence.deployment_after = deploymentAfter; passCase('receiver_survived');
    assert.ok(report.cases.every(c => c.status === 'pass'));
    report.status = 'pass'; report.accepts_10gib = config.size === TEN_GIB && report.mode === 'acceptance_10gib';
    report.summary = report.accepts_10gib ? 'Exact 10 GiB actual-browser S3 Compose interrupted/resumed transfer assertions passed.' : 'Small smoke passed ONLY. The mandatory 10 GiB acceptance remains NOT RUN.';
  }
} catch (error) {
  report.error = safeError(error); report.failure_code = report.error;
  if (report.execution === 'executed') { report.status = 'fail'; if (activeCase) failCase(activeCase, { error: report.error }); process.exitCode = 1; }
  else { report.status = 'not_run'; report.reason = 'Invalid or incomplete local configuration. No browser acceptance ran.'; process.exitCode = 2; }
} finally {
  if (context) await context.setOffline(false).catch(() => {});
  await browser?.close().catch(() => {});
  report.finished_at = new Date().toISOString();
  report.retention = 'No automatic cleanup: originals/history are intentionally retained. Use only disposable infrastructure; this driver never purges user data.';
  try { await save(); } catch { process.exitCode = 2; report.status = report.execution === 'executed' ? 'fail' : 'not_run'; report.accepts_10gib = false; report.error = 'evidence_output_unavailable_existing_or_protected_path'; }
  await evidenceWriter?.close().catch(() => {});
  console.log(JSON.stringify({ suite: report.suite, mode: report.mode, execution: report.execution, status: report.status, accepts_10gib: report.accepts_10gib, output, missing: report.missing, error: report.error, reason: report.reason }));
}
