import { describe, expect, it, vi } from 'vitest';
import { API, APIError, query } from '../shared/api';
import { normalizeResponse } from '../features/files/normalize';
describe('authenticated API', () => {
  it('sends bearer and CSRF only in headers, with secure browser cookie', async () => {
    const fetch = vi.fn().mockResolvedValue(new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } })); vi.stubGlobal('fetch', fetch);
    const api = new API(async () => 'test-access-secret'); api.csrfToken = 'csrf-proof';
    await api.post('/v1/share-requests/r/approve', {});
    const [path, init] = fetch.mock.calls[0];
    expect(path).toBe('/v1/share-requests/r/approve'); expect(path).not.toContain('secret');
    expect(init.headers.get('Authorization')).toBe('Bearer test-access-secret'); expect(init.headers.get('X-CSRF-Token')).toBe('csrf-proof'); expect(init.credentials).toBe('include'); expect(init.referrerPolicy).toBe('no-referrer');
  });
  it('rejects external API routes before sending tokens', async () => { const fetch = vi.fn(); vi.stubGlobal('fetch', fetch); const api = new API(async () => 'secret'); await expect(api.get('https://evil.example/v1/files')).rejects.toThrow(); expect(fetch).not.toHaveBeenCalled(); });
  it('retains actionable conflict metadata', async () => { vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: { code: 'VERSION_CONFLICT', current_version_id: 'v2' } }), { status: 409 }))); const api = new API(async () => 'token'); await expect(api.post('/v1/uploads', {})).rejects.toMatchObject({ status: 409, code: 'VERSION_CONFLICT', detail: { current_version_id: 'v2' } }); });
  it('handles empty successful responses and non-JSON failures', async () => { const fetch = vi.fn().mockResolvedValueOnce(new Response(null, { status: 204 })).mockResolvedValueOnce(new Response('upstream failed', { status: 503 })); vi.stubGlobal('fetch', fetch); const api = new API(async () => 'token'); expect(await api.post('/v1/feedback', {})).toBeUndefined(); await expect(api.get('/v1/files')).rejects.toBeInstanceOf(APIError); });
  it('encodes cursors and excludes undefined IDs', () => expect(query({ cursor: 'a+b&c', parent_id: undefined })).toBe('?cursor=a%2Bb%26c'));
});
describe('real REST projections', () => {
  it('normalizes nested trash but preserves stable file and trash IDs', () => expect(normalizeResponse('/v1/trash', { items: [{ id: 'trash-id', original_name: 'report', original_path: '/old/report', file: { id: 'file-id', name: 'retained', size: 42 } }] })).toMatchObject({ items: [{ id: 'file-id', trash_id: 'trash-id', name: 'report', size_bytes: 42 }] }));
  it('separates request expiry from grant expiry', () => expect(normalizeResponse('/v1/share-requests/r', { id: 'r', state: 'pending', expires_at: 'request-time', request_body: { file_id: 'f', expires_at: 'grant-time', rights: ['preview'] } })).toMatchObject({ id: 'r', status: 'pending', request_expires_at: 'request-time', expires_at: 'grant-time', rights: ['preview'] }));
  it('keeps shared access read-only and history policy explicit', () => expect(normalizeResponse('/v1/shared-with-me', { items: [{ id: 'share-id', rights: ['read', 'download'], version_policy: 'fixed', version_id: 'v1', file: { id: 'f', size: 8 } }] })).toMatchObject({ items: [{ id: 'f', share_id: 'share-id', shared_version_policy: 'fixed', shared_version_id: 'v1', permissions: ['read', 'download'] }] }));
  it('maps version timestamps and byte counts without inventing versions', () => expect(normalizeResponse('/v1/files/f/versions', { items: [{ id: 'v1', committed_at: '2026-10-23', size: 30 }] })).toMatchObject({ items: [{ id: 'v1', created_at: '2026-10-23', size_bytes: 30 }] }));
});
