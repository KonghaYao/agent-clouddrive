// Unit tests only. These manufactured evidence objects are NOT browser/S3 acceptance proof.
import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, readFile, stat, symlink, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createEvidenceWriter, assertSafeEnvironment, TEN_GIB, CHUNK_BYTES, expectedSize, positiveOffset, hashFile, quotaSnapshot, assertQuotaOnce, assertResume, assertNativeSession, assertHashes, sanitizeInspection, assertDeployment, signedS3Request, sha256, safeURL, safeProbeURL } from './large-upload-lib.mjs';
const clone = value => structuredClone(value);
test('host-side signed S3 probes require verified HTTPS or exact loopback', () => {
  for (const endpoint of ['https://s3.example.test', 'http://localhost:9000', 'http://127.0.0.1:9000', 'http://[::1]:9000']) {
    assert.equal(safeProbeURL(endpoint), endpoint);
    assert.ok(signedS3Request({ endpoint, bucket: 'fixture', accessKey: 'synthetic', secretKey: 'synthetic' }).headers.Authorization);
  }
  for (const endpoint of ['http://s3.example.test', 'http://rustfs:9000', 'http://127.0.0.1.attacker.test', 'http://192.168.1.2:9000', 'https://user:secret@s3.example.test', 'https://s3.example.test?token=secret']) {
    assert.throws(() => safeProbeURL(endpoint));
    assert.throws(() => signedS3Request({ endpoint, bucket: 'fixture', accessKey: 'synthetic', secretKey: 'synthetic' }));
  }
  assert.equal(safeURL('http://rustfs:9000'), 'http://rustfs:9000', 'compare-only native Docker URL stays supported');
});
const interruption = { confirmed_offset: CHUNK_BYTES, last_confirmed_seq: 1, offline_completed_at: '2026-01-01T00:00:01Z', online_completed_at: '2026-01-01T00:00:10Z', online_event_seq: 2 };
const resumeEvents = [
  { seq: 1, method: 'PATCH', status: 204, session_id: 'u', request_offset: 0, response_offset: CHUNK_BYTES },
  { seq: 2, method: 'PATCH', status: null, session_id: 'u', request_offset: CHUNK_BYTES },
  { seq: 3, method: 'HEAD', status: 200, session_id: 'u', response_offset: CHUNK_BYTES },
  ...Array.from({ length: TEN_GIB / CHUNK_BYTES - 1 }, (_, i) => ({ seq: i + 4, method: 'PATCH', status: 204, session_id: 'u', request_offset: (i + 1) * CHUNK_BYTES, response_offset: (i + 2) * CHUNK_BYTES })),
];
test('10 GiB means exactly 10,737,418,240 bytes; smoke is explicitly bounded', () => {
  assert.equal(expectedSize(), 10737418240); assert.equal(expectedSize(String(3 * CHUNK_BYTES)), 100663296);
  for (const n of ['10000000000', 'NaN', '1', '-1', '96.1']) assert.throws(() => expectedSize(n));
});
test('positive offsets reject malformed, zero and unsafe values', () => {
  assert.equal(positiveOffset('33554432'), CHUNK_BYTES);
  for (const v of [0, '', null, -1, 'NaN', 1.5, Number.MAX_SAFE_INTEGER + 1]) assert.equal(positiveOffset(v), null);
});
test('positive same-session resumed HEAD/PATCH is required', () => {
  assert.equal(assertResume(resumeEvents, interruption, TEN_GIB, 'u').resume_head_offset, CHUNK_BYTES);
});
test('no resuming from zero, new session, missing HEAD, or missing final bytes', () => {
  const newSession = clone(resumeEvents); newSession[3].session_id = 'other';
  const zero = clone(resumeEvents); zero[3].request_offset = 0;
  const incomplete = clone(resumeEvents); incomplete.at(-1).response_offset = TEN_GIB - 1;
  const noOffline = { ...interruption, offline_completed_at: null };
  for (const events of [newSession, zero, incomplete, resumeEvents.filter(e => e.method !== 'HEAD')]) assert.throws(() => assertResume(events, interruption, TEN_GIB, 'u'));
  assert.throws(() => assertResume(resumeEvents, noOffline, TEN_GIB, 'u'));
});
test('failed PATCH and a synthetic transfer event cannot replace successful HTTP evidence', () => {
  const failed = resumeEvents.map(e => e.method === 'PATCH' ? { ...e, status: 500 } : e);
  assert.throws(() => assertResume(failed, interruption, TEN_GIB, 'u'));
  assert.throws(() => assertResume([{ method: 'mock_upload', bytes: TEN_GIB }], interruption, TEN_GIB, 'u'));
});
const beforeQuota = { used_bytes: 4, reserved_bytes: 0, limit_bytes: 20 * 1024 ** 3 };
const heldQuota = { ...beforeQuota, reserved_bytes: TEN_GIB };
const committedQuota = { ...beforeQuota, used_bytes: TEN_GIB + 4 };
test('quota asserts exactly one reservation and one settlement, not just final size', () => {
  assertQuotaOnce(beforeQuota, heldQuota, committedQuota, clone(committedQuota), TEN_GIB);
  assert.throws(() => assertQuotaOnce(beforeQuota, heldQuota, committedQuota, { ...committedQuota, used_bytes: committedQuota.used_bytes + TEN_GIB }, TEN_GIB));
  assert.throws(() => assertQuotaOnce(beforeQuota, { ...heldQuota, reserved_bytes: 2 * TEN_GIB }, committedQuota, committedQuota, TEN_GIB));
  assert.throws(() => quotaSnapshot({ quota: { ...beforeQuota, used_bytes: '4' } }));
});
const nativeBefore = { upload_id: 'u', operation_id: 'o', file_id: 'f', native_upload_ref_sha256: sha256('native'), node_id: 'n', expected_size: TEN_GIB, reservation_state: 'held' };
const nativeAfter = { ...nativeBefore, received_bytes: TEN_GIB, upload_state: 'committed', operation_state: 'committed', version_count: 1, reservation_count: 1, reservation_state: 'committed', reservation_bytes: TEN_GIB, reservation_old_size: 0, result_version_id: 'v', current_version_id: 'v' };
test('native mapping, receiver, one retained version and one reservation are stable', () => {
  assertNativeSession(nativeBefore, nativeAfter, clone(nativeAfter), TEN_GIB);
  for (const patch of [{ native_upload_ref_sha256: 'different' }, { version_count: 2 }, { reservation_count: 2 }, { node_id: 'new-node' }, { result_version_id: 'new-version' }, { upload_state: 'uploading' }]) assert.throws(() => assertNativeSession(nativeBefore, nativeAfter, { ...nativeAfter, ...patch }, TEN_GIB));
});
test('integrity rejects decimal 10GB, wrong hash and redirects', () => {
  const source = { bytes: TEN_GIB, sha256: 'a'.repeat(64) }, download = { ...source, status: 200, redirected: false };
  assertHashes(source, download, TEN_GIB);
  for (const patch of [{ bytes: 10_000_000_000 }, { sha256: 'b'.repeat(64) }, { redirected: true }, { status: 206 }]) assert.throws(() => assertHashes(source, { ...download, ...patch }, TEN_GIB));
});
test('source hashing uses the real disk stream', async () => {
  const directory = await mkdtemp(join(tmpdir(), 'large-upload-unit-'));
  try { const path = join(directory, 'tiny-unit-input.bin'), data = Buffer.from('unit input, not acceptance'); await writeFile(path, data); const digest = await hashFile(path); assert.equal(digest.bytes, data.length); assert.equal(digest.sha256, sha256(data)); assert.equal(digest.method, 'node_read_stream_1MiB'); }
  finally { await rm(directory, { recursive: true, force: true }); }
});
const declaration = { disposable: true, storage_mode: 's3', single_node_compose: true, compose_project: 'test', public_url: 'http://localhost:8080', s3: { bucket: 'originals', native_endpoint: 'http://rustfs:9000', probe_endpoint: 'http://localhost:9000' } };
const containers = Object.fromEntries(['api','ocis','rustfs','gateway'].map(service => [service, { running: true, project: 'test', service, image: service + ':1', image_id: 'sha256:example', started_at: 'now', environment: {} }]));
containers.api.environment = { DRIVE_STORAGE_MODE: 's3', DRIVE_PUBLIC_URL: declaration.public_url };
containers.ocis.environment = { STORAGE_USERS_DRIVER: 's3ng', STORAGE_USERS_S3NG_BUCKET: 'originals', STORAGE_USERS_S3NG_ENDPOINT: 'http://rustfs:9000', STORAGE_USERS_S3NG_PUT_OBJECT_DISABLE_MULTIPART: 'false', STORAGE_USERS_S3NG_PUT_OBJECT_PART_SIZE: '16777216' };
test('default S3 cannot pass based only on declaration or local driver', () => {
  assertDeployment(declaration, containers);
  const local = clone(containers); local.ocis.environment.STORAGE_USERS_DRIVER = 'ocis'; assert.throws(() => assertDeployment(declaration, local));
  assert.throws(() => assertDeployment(declaration, {}));
  const wrongProject = clone(containers); wrongProject.api.project = 'production'; assert.throws(() => assertDeployment(declaration, wrongProject));
});
test('Docker evidence whitelist never retains arbitrary environment credentials', () => {
  const output = sanitizeInspection({ Config: { Env: ['SECRET_KEY=do-not-output', 'DRIVE_STORAGE_MODE=s3', 'DRIVE_DATABASE_URL=private'], Labels: {} }, State: {} });
  assert.deepEqual(output.environment, { DRIVE_STORAGE_MODE: 's3' }); assert.ok(!JSON.stringify(output).includes('private'));
});
test('S3 signing is deterministic, read only and has canonical escaped paths', () => {
  const request = signedS3Request({ endpoint: 'http://localhost:9000', bucket: 'originals', key: 'a b/中.bin', accessKey: 'fixture', secretKey: 'unit-only', method: 'HEAD', now: new Date('2026-01-01T00:00:00Z') });
  assert.equal(request.url, 'http://localhost:9000/originals/a%20b/%E4%B8%AD.bin'); assert.equal(request.method, 'HEAD'); assert.match(request.headers.Authorization, /Credential=fixture\/20260101\/us-east-1\/s3\/aws4_request/);
  assert.throws(() => signedS3Request({ endpoint: 'http://localhost', bucket: 'x', method: 'PUT' }));
  assert.throws(() => safeURL('http://name:password@localhost'));
});

test('debugging and disabled Node TLS verification are rejected before credentials', () => {
  assertSafeEnvironment({});
  for (const env of [{ NODE_TLS_REJECT_UNAUTHORIZED: '0' }, { DEBUG: '*' }, { PWDEBUG: '1' }, { NODE_DEBUG: 'http' }]) assert.throws(() => assertSafeEnvironment(env));
});

test('evidence writer protects inputs, existing files, symlinks and restrictive permissions', async () => {
  const directory = await mkdtemp(join(tmpdir(), 'large-upload-output-unit-'));
  try {
    const input = join(directory, 'source'), link = join(directory, 'link'), output = join(directory, 'evidence.json');
    await writeFile(input, 'preserve'); await symlink(input, link);
    await assert.rejects(createEvidenceWriter(input, [input]));
    await assert.rejects(createEvidenceWriter(input));
    await assert.rejects(createEvidenceWriter(link));
    assert.equal(await readFile(input, 'utf8'), 'preserve');
    const writer = await createEvidenceWriter(output, [input]);
    try { await writer.write({ long_value: 'longer first report' }); await writer.write({ status: 'not_run' }); } finally { await writer.close(); }
    assert.deepEqual(JSON.parse(await readFile(output, 'utf8')), { status: 'not_run' });
    assert.equal((await stat(output)).mode & 0o777, 0o600);
    await assert.rejects(createEvidenceWriter(output));
  } finally { await rm(directory, { recursive: true, force: true }); }
});
