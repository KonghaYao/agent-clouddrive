import { describe, expect, it, vi } from 'vitest';
import type { TusOptions, TusBody } from '@uppy/tus';
import { API } from '../shared/api';
// Select the real browser distribution: Vitest otherwise resolves tus's Node Buffer reader.
// @ts-expect-error The browser distribution shares its declarations with the package root.
vi.mock('tus-js-client', () => import('tus-js-client/lib.esm/browser/index.js'));
import { createControlledUploadClient, ensureUploadNonce, preflightUpload, UploadPreflightError, uploadConflictFromFailure, uploadFailurePayload, uploadFailureText, uploadIdempotencyKey, uploadNonce } from '../features/upload/upload-client';
type Stack = NonNullable<TusOptions<Record<string, unknown>, TusBody>['httpStack']>;
function protocolFixture(length: number, initialOffset = 0, failureStatus?: number) {
  let offset = initialOffset;
  const calls: { method: string; offset?: number; length: number; auth?: string }[] = [];
  const stack: Stack = {
    getName: () => 'deterministic-tus-protocol-fixture',
    createRequest(method, url) {
      const headers = new Map<string, string>();
      let progress: (bytes: number) => void = () => {};
      return {
        getMethod: () => method, getURL: () => url,
        setHeader: (key, value) => { headers.set(key.toLowerCase(), value); }, getHeader: key => headers.get(key.toLowerCase()),
        setProgressHandler: callback => { progress = callback; }, abort: async () => {}, getUnderlyingObject: () => ({}),
        send: async (body: Blob | undefined) => {
          const sent = body?.size ?? 0, sentOffset = headers.has('upload-offset') ? Number(headers.get('upload-offset')) : undefined;
          calls.push({ method, offset: sentOffset, length: sent, auth: headers.get('authorization') });
          let status = method === 'HEAD' ? 200 : 204;
          if (method === 'PATCH') {
            if (failureStatus) status = failureStatus;
            else if (sentOffset !== offset) status = 409;
            else { offset += sent; progress(sent); }
          }
          return { getStatus: () => status, getHeader: key => ({ 'tus-resumable': '1.0.0', 'upload-length': String(length), 'upload-offset': String(offset) })[key.toLowerCase() as 'upload-offset'], getBody: () => '', getUnderlyingObject: () => ({ status }) };
        },
      };
    },
  };
  return { stack, calls, offset: () => offset };
}
describe('actual Uppy + tus client with deterministic protocol transport', () => {
  it('HEADs a controlled existing session and resumes from server offset, without POSTing native storage', async () => {
    const fixture = protocolFixture(100, 40), api = new API(async () => 'fixture-access-token');
    const client = createControlledUploadClient(api, 1, fixture.stack);
    const id = client.addFile({ name: 'test.bin', type: 'application/octet-stream', data: new Blob([new Uint8Array(100)]) });
    client.setFileState(id, { tus: { uploadUrl: `${location.origin}/v1/tus/session1` } });
    try {
      const result = await client.upload();
      expect(result?.successful).toHaveLength(1); expect(result?.failed).toHaveLength(0);
      expect(fixture.calls.map(c => c.method)).toEqual(['HEAD', 'PATCH']); expect(fixture.calls[1].offset).toBe(40); expect(fixture.calls[1].length).toBe(60); expect(fixture.offset()).toBe(100);
      expect(fixture.calls.every(c => c.auth === 'Bearer fixture-access-token')).toBe(true);
    } finally { client.destroy(); }
  });
  it('does not retry forbidden sessions or report a completed upload', async () => {
    const fixture = protocolFixture(30, 0, 403), client = createControlledUploadClient(new API(async () => 'token'), 1, fixture.stack);
    const id = client.addFile({ name: 'blocked.bin', type: 'application/octet-stream', data: new Blob([new Uint8Array(30)]) });
    client.setFileState(id, { tus: { uploadUrl: `${location.origin}/v1/tus/blocked` } });
    try { const result = await client.upload(); expect(result?.successful).toHaveLength(0); expect(result?.failed).toHaveLength(1); expect(fixture.calls.filter(c => c.method === 'PATCH')).toHaveLength(1); expect(fixture.offset()).toBe(0); }
    finally { client.destroy(); }
  });
  it('rejects external upload URLs before exposing authorization', async () => {
    const fixture = protocolFixture(10), token = vi.fn().mockResolvedValue('token'), client = createControlledUploadClient(new API(token), 1, fixture.stack);
    const id = client.addFile({ name: 'private.bin', data: new Blob([new Uint8Array(10)]) });
    client.setFileState(id, { tus: { uploadUrl: 'https://native-storage.example/v1/tus/leak', retryDelays: [] } });
    try { const result = await client.upload(); expect(result?.failed).toHaveLength(1); expect(fixture.calls).toHaveLength(0); expect(token).not.toHaveBeenCalled(); }
    finally { client.destroy(); }
  });
});
describe('per-file upload idempotency nonce', () => {
  it('reuses the persisted nonce and builds a compact key from name, MIME and relative path free content', () => {
    const client = createControlledUploadClient(new API(async () => 'token'), 1);
    try {
      const id = client.addFile({
        name: 'test_智能办公助手.docx', type: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
        data: new Blob(['x']), meta: { relativePath: '资料/测试/test_智能办公助手.docx' },
      });
      const first = ensureUploadNonce(client, id);
      expect(ensureUploadNonce(client, id)).toBe(first);
      expect(client.getFile(id)?.meta.upload_nonce).toBe(first);
      const key = uploadIdempotencyKey(first);
      expect(key).toBe(`upload:${first}`);
      expect(key).toHaveLength(43);
      expect(key).toMatch(/^upload:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);
      expect(key).not.toContain(id);
      expect(key).not.toContain('docx');
      expect(key).not.toContain('application/');
    } finally { client.destroy(); }
  });
  it('creates a nonce only when the file has none and keeps an existing value unchanged', () => {
    const client = createControlledUploadClient(new API(async () => 'token'), 2);
    try {
      const generated = uploadNonce({});
      expect(uploadIdempotencyKey(generated)).toHaveLength(43);
      expect(uploadNonce({ upload_nonce: 'kept' })).toBe('kept');
      expect(uploadNonce({ upload_nonce: 42 })).not.toBe('42');
      const fresh = client.addFile({ name: 'fresh.bin', data: new Blob(['x']) });
      const nonce = ensureUploadNonce(client, fresh);
      expect(client.getFile(fresh)?.meta.upload_nonce).toBe(nonce);
      const kept = client.addFile({ name: 'kept.bin', data: new Blob(['x']), meta: { upload_nonce: 'kept' } });
      expect(ensureUploadNonce(client, kept)).toBe('kept');
      expect(client.getFile(kept)?.meta.upload_nonce).toBe('kept');
    } finally { client.destroy(); }
  });
});
describe('failed tus transfer diagnostics', () => {
  const conflictBody = JSON.stringify({ error: { code: 'PATH_CONFLICT', message: 'target path is occupied', details: { reason: 'incomplete_upload', upload_id: 'upload-9', expected_size: 2194 } } });
  it('parses the 409 body wrapped by @uppy/tus into an actionable conflict', () => {
    expect(uploadConflictFromFailure({ message: 'failed' }, { status: 409, body: { xhr: { responseText: conflictBody } } }))
      .toEqual({ reason: 'incomplete_upload', uploadId: 'upload-9', expectedSize: 2194 });
  });
  it('parses the 409 body exposed by tus-js-client originalResponse', () => {
    const error = { originalResponse: { getStatus: () => 409, getBody: () => conflictBody } };
    expect(uploadConflictFromFailure(error)).toEqual({ reason: 'incomplete_upload', uploadId: 'upload-9', expectedSize: 2194 });
    expect(uploadConflictFromFailure(error, { status: 409 })).toEqual({ reason: 'incomplete_upload', uploadId: 'upload-9', expectedSize: 2194 });
  });
  it('reports the real status and code for a 422 without inventing a conflict', () => {
    const error = { originalResponse: { getStatus: () => 422, getBody: () => JSON.stringify({ error: { code: 'INVALID_ARGUMENT' } }) } };
    expect(uploadConflictFromFailure(error)).toBeNull();
    expect(uploadFailurePayload(error)).toEqual({ error: { code: 'INVALID_ARGUMENT' } });
    expect(uploadFailureText(422, 'INVALID_ARGUMENT')).toContain('HTTP 422');
    expect(uploadFailureText(422, 'INVALID_ARGUMENT')).toContain('INVALID_ARGUMENT');
  });
  it('falls back to the status-only text when the body is not JSON', () => {
    expect(uploadFailurePayload({ originalResponse: { getBody: () => '<html>502</html>' } })).toBeUndefined();
    expect(uploadFailureText(502, undefined)).toContain('HTTP 502');
    expect(uploadFailureText(undefined, undefined)).toBe('传输暂未完成。可重试或继续同一会话；接收节点失效或会话过期时需重新选择文件。');
  });
});
describe('browser tus stack and HEAD preflight', () => {
  it('keeps tus-js-client browser defaults when no test stack is injected and has no create fallback', () => {
    const client = createControlledUploadClient(new API(async () => 'token'), 1);
    try {
      const opts = (client.getPlugin('Tus') as unknown as { opts: Record<string, unknown> }).opts;
      // An own `httpStack: undefined` key replaces the browser XHR stack and breaks every upload.
      expect(Object.prototype.hasOwnProperty.call(opts, 'httpStack')).toBe(false);
      expect(opts.endpoint).toBe('');
    } finally { client.destroy(); }
  });
  it('surfaces a non-200 HEAD status without any create-endpoint fallback', async () => {
    const calls: { method?: string; url: string }[] = [];
    const request = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ method: init?.method, url: String(input) });
      return new Response(null, { status: 503 });
    }) as unknown as typeof fetch;
    await expect(preflightUpload(new API(async () => 'token'), `${location.origin}/v1/tus/session-503`, request)).rejects.toBeInstanceOf(UploadPreflightError);
    expect(calls).toEqual([{ method: 'HEAD', url: `${location.origin}/v1/tus/session-503` }]);
    expect(calls.some(call => call.method === 'POST' && new URL(call.url).pathname === '/v1/tus')).toBe(false);
  });
  it('sends Bearer + Tus-Resumable and reports the server offsets on 200', async () => {
    let headers: Record<string, string> = {};
    const request = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
      headers = init?.headers as Record<string, string>;
      return new Response(null, { status: 200, headers: { 'Upload-Offset': '4', 'Upload-Length': '10' } });
    }) as unknown as typeof fetch;
    await expect(preflightUpload(new API(async () => 'token'), `${location.origin}/v1/tus/session-200`, request)).resolves.toEqual({ offset: 4, length: 10 });
    expect(headers.Authorization).toBe('Bearer token');
    expect(headers['Tus-Resumable']).toBe('1.0.0');
  });
});
