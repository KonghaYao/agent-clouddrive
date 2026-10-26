// Pure evidence checks and bounded I/O for the real-browser driver. Never launches a browser.
import assert from 'node:assert/strict';
import { createHash, createHmac } from 'node:crypto';
import { createReadStream, constants } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { mkdir, open, readFile, stat } from 'node:fs/promises';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
export const TEN_GIB = 10_737_418_240;
export const CHUNK_BYTES = 32 * 1024 * 1024;
const exec = promisify(execFile);
export const sha256 = value => createHash('sha256').update(value).digest('hex');
export function assertSafeEnvironment(env) {
  assert.ok(!env.DEBUG && !env.PWDEBUG && !env.NODE_DEBUG, 'disable_verbose_protocol_debug');
  assert.notEqual(env.NODE_TLS_REJECT_UNAUTHORIZED, '0', 'tls_verification_must_remain_enabled');
}
export function positiveOffset(value) { const n = Number(value); return Number.isSafeInteger(n) && n > 0 ? n : null; }
export function expectedSize(smokeBytes) {
  if (smokeBytes === undefined) return TEN_GIB;
  const size = Number(smokeBytes);
  assert.ok(Number.isSafeInteger(size) && size >= 3 * CHUNK_BYTES && size <= 1024 ** 3, 'Smoke input must be 96 MiB–1 GiB; it never satisfies the 10 GiB gate');
  return size;
}
export function safeURL(value, { originOnly = false } = {}) {
  const u = new URL(value);
  assert.ok(['http:', 'https:'].includes(u.protocol) && !u.username && !u.password && !u.search && !u.hash, 'Expected an HTTP(S) URL without credentials, query, or fragment');
  if (originOnly) assert.equal(u.pathname, '/', 'Public URL must be an origin');
  return u.href.replace(/\/$/, '');
}
// This endpoint receives signed credentials from the host-side evidence client.
// Docker-internal native_endpoint is compare-only and is validated separately.
export function safeProbeURL(value) {
  const normalized = safeURL(value, { originOnly: true }), url = new URL(normalized);
  const loopback = ['localhost', '127.0.0.1', '[::1]'].includes(url.hostname);
  assert.ok(url.protocol === 'https:' || loopback, 'signed_s3_probe_requires_https_or_loopback');
  return normalized;
}
export async function hashFile(path) {
  const before = await stat(path); assert.ok(before.isFile(), 'Input is not a regular file');
  const hash = createHash('sha256'); let bytes = 0;
  for await (const chunk of createReadStream(path, { highWaterMark: 1024 * 1024 })) { bytes += chunk.length; hash.update(chunk); }
  const after = await stat(path);
  assert.equal(bytes, before.size); assert.equal(after.size, before.size); assert.equal(after.mtimeMs, before.mtimeMs); assert.equal(after.ino, before.ino);
  return { bytes, sha256: hash.digest('hex'), mtime_ms: before.mtimeMs, inode: before.ino, method: 'node_read_stream_1MiB' };
}
export function quotaSnapshot(me) {
  const q = me?.quota;
  for (const key of ['used_bytes', 'reserved_bytes', 'limit_bytes']) assert.ok(Number.isSafeInteger(q?.[key]) && q[key] >= 0, `Invalid /v1/me quota ${key}`);
  return { used_bytes: q.used_bytes, reserved_bytes: q.reserved_bytes, limit_bytes: q.limit_bytes };
}
export function assertQuotaOnce(before, held, committed, replayed, size) {
  assert.equal(before.reserved_bytes, 0, 'Dedicated test user must have no other held uploads');
  assert.equal(held.used_bytes, before.used_bytes); assert.equal(held.reserved_bytes, size);
  assert.equal(committed.used_bytes - before.used_bytes, size, 'Commit must add the new file once');
  assert.equal(committed.reserved_bytes, 0); assert.deepEqual(replayed, committed, 'Finalize replay changed quota');
}
export function assertResume(events, interruption, size, sessionID) {
  const tus = events.filter(e => ['HEAD', 'PATCH'].includes(e.method));
  assert.ok(tus.length > 0);
  assert.ok(tus.every(e => e.session_id === sessionID), 'A second tus session was used');
  const before = tus.filter(e => e.method === 'PATCH' && e.status === 204 && e.seq <= interruption.last_confirmed_seq);
  assert.ok(before.length > 0 && interruption.confirmed_offset > 0 && interruption.confirmed_offset < size, 'No positive partial offset before interruption');
  assert.equal(Math.max(...before.map(e => e.response_offset)), interruption.confirmed_offset);
  assert.ok(interruption.offline_completed_at && interruption.online_completed_at, 'Offline/online actions did not both complete');
  const after = tus.filter(e => e.seq > interruption.online_event_seq);
  const head = after.find(e => e.method === 'HEAD' && e.status >= 200 && e.status < 300 && e.response_offset >= interruption.confirmed_offset && e.response_offset < size);
  assert.ok(head, 'No successful post-reconnection HEAD of the same positive partial offset');
  const resumed = after.find(e => e.method === 'PATCH' && e.status === 204 && e.request_offset >= head.response_offset && e.seq > head.seq);
  assert.ok(resumed && resumed.request_offset > 0, 'No real successful PATCH after the resumed HEAD');
  assert.ok(!after.some(e => e.method === 'PATCH' && e.request_offset === 0), 'Upload restarted from zero after interruption');
  const successful = tus.filter(e => e.method === 'PATCH' && e.status === 204);
  assert.ok(successful.every(e => Number.isSafeInteger(e.request_offset) && Number.isSafeInteger(e.response_offset) && e.response_offset > e.request_offset && e.response_offset - e.request_offset <= CHUNK_BYTES && e.response_offset <= size), 'Invalid successful PATCH offsets');
  assert.equal(Math.max(...successful.map(e => e.response_offset - e.request_offset)), CHUNK_BYTES, 'No full real 32 MiB Uppy chunk observed');
  assert.equal(Math.max(...successful.map(e => e.response_offset)), size, 'No successful final PATCH acknowledging exact input length');
  return { pre_interruption_offset: interruption.confirmed_offset, resume_head_offset: head.response_offset, resumed_patch_offset: resumed.request_offset, successful_patch_count: successful.length };
}
export function assertNativeSession(before, after, replay, size) {
  for (const sample of [after, replay]) {
    for (const key of ['upload_id', 'operation_id', 'file_id', 'native_upload_ref_sha256', 'node_id']) assert.equal(sample[key], before[key], `Native/business mapping changed: ${key}`);
    assert.equal(sample.expected_size, size); assert.equal(sample.received_bytes, size);
    assert.equal(sample.upload_state, 'committed'); assert.equal(sample.operation_state, 'committed');
    assert.equal(sample.version_count, 1); assert.equal(sample.reservation_count, 1); assert.equal(sample.reservation_state, 'committed');
    assert.equal(sample.reservation_bytes, size); assert.equal(sample.reservation_old_size, 0);
    assert.ok(sample.result_version_id); assert.equal(sample.current_version_id, sample.result_version_id);
  }
  assert.equal(before.reservation_state, 'held'); assert.equal(before.expected_size, size);
  assert.equal(after.result_version_id, replay.result_version_id);
}
export function assertHashes(source, download, expected) {
  assert.equal(source.bytes, expected); assert.equal(download.bytes, expected);
  assert.match(source.sha256, /^[a-f0-9]{64}$/); assert.equal(download.sha256, source.sha256);
  assert.equal(download.status, 200); assert.equal(download.redirected, false);
}
export async function nativeSnapshot(databaseURL, uploadID) {
  assert.match(uploadID, /^[a-f0-9-]{36}$/i, 'Invalid upload UUID');
  // Read only transaction + SELECT. DSN goes in child environment, not argv/evidence.
  const sql = `BEGIN READ ONLY; SELECT json_build_object('upload_id',u.id,'operation_id',u.operation_id,'file_id',o.file_id,'native_upload_ref',u.native_upload_ref,'node_id',u.node_id,'expected_size',u.expected_size,'received_bytes',u.received_bytes,'upload_state',u.state,'operation_state',o.state,'result_version_id',o.result_version_id,'current_version_id',f.current_version_id,'version_count',(SELECT count(*) FROM file_versions v WHERE v.file_id=f.id),'reservation_count',(SELECT count(*) FROM quota_reservations r WHERE r.upload_id=u.id),'reservation_state',q.state,'reservation_bytes',q.bytes,'reservation_old_size',q.old_size) FROM upload_sessions u JOIN file_operations o ON o.id=u.operation_id JOIN files f ON f.id=o.file_id JOIN quota_reservations q ON q.upload_id=u.id WHERE u.id='${uploadID}'::uuid; COMMIT;`;
  let stdout;
  try { ({ stdout } = await exec('psql', ['-X', '-q', '-A', '-t', '-v', 'ON_ERROR_STOP=1', '-c', sql], { env: { ...process.env, PGDATABASE: databaseURL, PGOPTIONS: '-c default_transaction_read_only=on -c statement_timeout=15000' }, timeout: 30_000, maxBuffer: 1024 * 1024 })); }
  catch { throw new Error('Read-only psql native upload/quota inspection failed; database details suppressed'); }
  const rows = stdout.trim().split('\n').filter(v => v.startsWith('{')); assert.equal(rows.length, 1, 'Exactly one native session row required');
  const row = JSON.parse(rows[0]); assert.ok(row.native_upload_ref, 'Native upload mapping is empty');
  row.native_upload_ref_sha256 = sha256(row.native_upload_ref); delete row.native_upload_ref;
  return row;
}
const ENV_KEYS = ['DRIVE_STORAGE_MODE','DRIVE_UPLOAD_MAX_BYTES','DRIVE_CONVERT_MAX_BYTES','DRIVE_PUBLIC_URL','STORAGE_USERS_DRIVER','STORAGE_USERS_S3NG_ENDPOINT','STORAGE_USERS_S3NG_REGION','STORAGE_USERS_S3NG_BUCKET','STORAGE_USERS_S3NG_PUT_OBJECT_DISABLE_MULTIPART','STORAGE_USERS_S3NG_PUT_OBJECT_PART_SIZE','FRONTEND_UPLOAD_MAX_CHUNK_SIZE','STORAGE_USERS_UPLOAD_EXPIRATION'];
export function sanitizeInspection(row) {
  const environment = {};
  for (const item of row.Config?.Env ?? []) { const i = item.indexOf('='); if (ENV_KEYS.includes(item.slice(0, i))) environment[item.slice(0, i)] = item.slice(i + 1); }
  return { id: row.Id, image: row.Config?.Image, image_id: row.Image, running: row.State?.Running, started_at: row.State?.StartedAt, project: row.Config?.Labels?.['com.docker.compose.project'], service: row.Config?.Labels?.['com.docker.compose.service'], environment };
}
export function assertDeployment(declaration, containers) {
  assert.equal(declaration.disposable, true); assert.equal(declaration.storage_mode, 's3'); assert.equal(declaration.single_node_compose, true);
  assert.ok(declaration.compose_project && declaration.public_url && declaration.s3?.probe_endpoint && declaration.s3?.bucket);
  for (const service of ['api', 'ocis', 'rustfs', 'gateway']) {
    const row = containers[service]; assert.ok(row?.running, `${service} is not running`);
    assert.equal(row.project, declaration.compose_project, 'Different Compose project'); assert.equal(row.service, service, 'Unexpected Compose service');
    assert.ok(row.image && row.image_id && row.started_at, 'Missing runtime image identity');
  }
  assert.equal(containers.api.environment.DRIVE_STORAGE_MODE, 's3');
  assert.equal(safeURL(containers.api.environment.DRIVE_PUBLIC_URL, { originOnly: true }), safeURL(declaration.public_url, { originOnly: true }));
  const o = containers.ocis.environment;
  assert.equal(o.STORAGE_USERS_DRIVER, 's3ng'); assert.equal(o.STORAGE_USERS_S3NG_BUCKET, declaration.s3.bucket);
  assert.equal(o.STORAGE_USERS_S3NG_ENDPOINT, declaration.s3.native_endpoint);
  assert.equal(o.STORAGE_USERS_S3NG_PUT_OBJECT_DISABLE_MULTIPART, 'false');
  assert.ok(Number(o.STORAGE_USERS_S3NG_PUT_OBJECT_PART_SIZE) > 0);
}
export async function inspectDeployment(declaration) {
  const containers = {};
  for (const service of ['api', 'ocis', 'rustfs', 'gateway']) {
    const id = declaration.containers?.[service]; assert.ok(typeof id === 'string' && /^[a-zA-Z0-9_.-]+$/.test(id), `Missing/invalid ${service} container ID`);
    let stdout; try { ({ stdout } = await exec('docker', ['inspect', '--type', 'container', id], { timeout: 30_000, maxBuffer: 4 * 1024 * 1024 })); } catch { throw new Error(`Live Docker inspection failed for ${service}; no default-S3 acceptance claim possible`); }
    const rows = JSON.parse(stdout); assert.equal(rows.length, 1); containers[service] = sanitizeInspection(rows[0]);
  }
  assertDeployment(declaration, containers);
  let config;
  try { const result = await exec('docker', ['exec', declaration.containers.gateway, 'nginx', '-T'], { timeout: 30_000, maxBuffer: 4 * 1024 * 1024 }); config = result.stdout; } catch { throw new Error('Read-only gateway nginx -T failed'); }
  const fields = [...config.matchAll(/^\s*(client_max_body_size|client_body_timeout|proxy_read_timeout|proxy_send_timeout|proxy_request_buffering|proxy_buffering)\s+([^;]+);/gm)].map(m => ({ directive: m[1], value: m[2] }));
  const v1 = config.match(/location\s+(?:\^~\s+)?\/v1\/\s*\{([^}]+)\}/)?.[1] ?? '';
  assert.ok(/proxy_request_buffering\s+off;/.test(v1), 'Formal /v1/ gateway request streaming configuration not verified');
  assert.ok(/proxy_buffering\s+off;/.test(v1), 'Formal /v1/ gateway response streaming configuration not verified');
  return { containers, gateway_directives: fields, gateway_config_sha256: sha256(config), source: 'live_docker_inspect_and_nginx_T' };
}
// Minimal AWS Signature V4 for read-only ListObjectsV2/HEAD. No SDK mocks and no writes.
const awsEncode = value => encodeURIComponent(value).replace(/[!'()*]/g, c => `%${c.charCodeAt(0).toString(16).toUpperCase()}`);
const hmac = (key, data) => createHmac('sha256', key).update(data).digest();
export function signedS3Request({ endpoint, bucket, key, query = {}, region = 'us-east-1', accessKey, secretKey, method = 'GET', now = new Date() }) {
  assert.ok(['GET', 'HEAD'].includes(method), 'S3 evidence client is read only');
  const base = new URL(safeProbeURL(endpoint));
  const path = '/' + [bucket, ...(key === undefined ? [] : key.split('/'))].map(awsEncode).join('/');
  const queryString = Object.entries(query).map(([k,v]) => [awsEncode(k), awsEncode(String(v))]).sort(([a],[b]) => a.localeCompare(b)).map(([k,v]) => `${k}=${v}`).join('&');
  const stamp = now.toISOString().replace(/[:-]|\.\d{3}/g, ''), day = stamp.slice(0, 8), payload = sha256('');
  const headers = { host: base.host, 'x-amz-content-sha256': payload, 'x-amz-date': stamp };
  const names = Object.keys(headers).sort(), canonical = [method, path, queryString, names.map(k => `${k}:${headers[k]}\n`).join(''), names.join(';'), payload].join('\n');
  const scope = `${day}/${region}/s3/aws4_request`;
  const signingKey = hmac(hmac(hmac(hmac('AWS4' + secretKey, day), region), 's3'), 'aws4_request');
  const signature = createHmac('sha256', signingKey).update(`AWS4-HMAC-SHA256\n${stamp}\n${scope}\n${sha256(canonical)}`).digest('hex');
  headers.Authorization = `AWS4-HMAC-SHA256 Credential=${accessKey}/${scope}, SignedHeaders=${names.join(';')}, Signature=${signature}`;
  return { url: base.origin + path + (queryString ? '?' + queryString : ''), headers, method };
}
async function boundedMetadataText(response, limit) {
  assert.ok(response.body, 'Missing metadata response body');
  const reader = response.body.getReader(), chunks = []; let bytes = 0;
  try {
    while (true) { const { done, value } = await reader.read(); if (done) break; bytes += value.byteLength; if (bytes > limit) { await reader.cancel(); throw new Error('metadata_response_exceeds_bound'); } chunks.push(value); }
  } finally { reader.releaseLock(); }
  return Buffer.concat(chunks, bytes).toString('utf8');
}
const xmlUnescape = value => value.replace(/&lt;/g,'<').replace(/&gt;/g,'>').replace(/&quot;/g,'"').replace(/&apos;/g,"'").replace(/&amp;/g,'&');
function xmlTag(value, name) { return xmlUnescape(value.match(new RegExp(`<${name}>([\\s\\S]*?)</${name}>`))?.[1] ?? ''); }
export async function listS3Objects(credentials, declaration) {
  const objects = []; let continuation;
  do {
    const request = signedS3Request({ ...credentials, endpoint: declaration.s3.probe_endpoint, bucket: declaration.s3.bucket, region: declaration.s3.region, query: { 'list-type': 2, 'max-keys': 1000, ...(continuation ? { 'continuation-token': continuation } : {}) } });
    const response = await fetch(request.url, { ...request, redirect: 'error', signal: AbortSignal.timeout(30_000) }); assert.equal(response.status, 200, 'Read-only S3 list failed');
    const xml = await boundedMetadataText(response, 8 * 1024 * 1024);
    for (const match of xml.matchAll(/<Contents>([\s\S]*?)<\/Contents>/g)) objects.push({ key: xmlTag(match[1], 'Key'), size: Number(xmlTag(match[1], 'Size')), etag: xmlTag(match[1], 'ETag') });
    assert.ok(objects.length < 100_000, 'Use a dedicated disposable S3 bucket; evidence scan is bounded');
    continuation = xmlTag(xml, 'NextContinuationToken');
    assert.ok(xmlTag(xml, 'IsTruncated') !== 'true' || continuation, 'Truncated S3 listing has no continuation token');
  } while (continuation);
  return objects;
}
export async function proveMultipart(before, after, credentials, declaration, size) {
  const old = new Set(before.map(o => `${o.key}|${o.etag}|${o.size}`));
  const candidates = after.filter(o => o.size === size && !old.has(`${o.key}|${o.etag}|${o.size}`));
  assert.equal(candidates.length, 1, 'Dedicated bucket must expose exactly one new exact-size object');
  const object = candidates[0]; const request = signedS3Request({ ...credentials, endpoint: declaration.s3.probe_endpoint, bucket: declaration.s3.bucket, region: declaration.s3.region, key: object.key, method: 'HEAD' });
  const response = await fetch(request.url, { ...request, redirect: 'error', signal: AbortSignal.timeout(30_000) });
  assert.equal(response.status, 200); assert.equal(Number(response.headers.get('content-length')), size);
  const etag = response.headers.get('etag'); assert.equal(etag, object.etag);
  const parts = Number(etag?.match(/-([0-9]+)"?$/)?.[1]); assert.ok(parts > 1, 'S3 multipart ETag not observed; configured multipart is insufficient proof');
  return { bucket: declaration.s3.bucket, key_sha256: sha256(object.key), size, etag, observed_part_count: parts, source: 'live_s3_list_objects_v2_then_head' };
}
export async function streamDownload(url, headers, size, timeoutMs) {
  const start = Date.now(), response = await fetch(url, { headers, redirect: 'error', signal: AbortSignal.timeout(timeoutMs) });
  assert.equal(response.status, 200); assert.ok(response.body); assert.equal(response.redirected, false);
  const length = response.headers.get('content-length'); if (length !== null) assert.equal(Number(length), size);
  const hash = createHash('sha256'); let bytes = 0, maxChunk = 0;
  for await (const chunk of response.body) { bytes += chunk.length; assert.ok(bytes <= size, 'Download exceeds source length'); maxChunk = Math.max(maxChunk, chunk.length); hash.update(chunk); }
  return { bytes, sha256: hash.digest('hex'), status: response.status, redirected: response.redirected, declared_content_length: length, max_chunk_bytes: maxChunk, elapsed_ms: Date.now() - start, method: 'formal_content_endpoint_node_fetch_stream_no_whole_file_buffer', runner_max_rss_kib: process.resourceUsage().maxRSS };
}
export async function readSecret(path) { const info = await stat(path); assert.ok(info.isFile() && info.size > 0 && info.size <= 32768, 'Invalid credential file'); assert.equal(info.mode & 0o077, 0, 'Credential files must be private (chmod 600)'); const value = (await readFile(path, 'utf8')).replace(/\r?\n$/, ''); assert.ok(value, 'Credential file is empty'); return value; }

// Reserve a new report once. Never replace an existing file or follow a leaf symlink.
export async function createEvidenceWriter(path, protectedPaths = []) {
  const target = resolve(path);
  assert.ok(!protectedPaths.filter(Boolean).some(input => resolve(input) === target), 'evidence_output_collides_with_input');
  await mkdir(dirname(target), { recursive: true, mode: 0o700 });
  const file = await open(target, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW, 0o600);
  return {
    async write(value) {
      const bytes = Buffer.from(JSON.stringify(value, null, 2) + '\n'); let offset = 0;
      while (offset < bytes.length) { const result = await file.write(bytes, offset, bytes.length - offset, offset); assert.ok(result.bytesWritten > 0); offset += result.bytesWritten; }
      await file.truncate(bytes.length); await file.sync();
    },
    async close() { await file.close(); },
  };
}
