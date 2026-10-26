import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
vi.mock('@uppy/react/dashboard', () => ({ default: () => null }));
import { API, APIError } from '../api';
import { mergeUploadError, prepareUploads, UploadConflictAlert, uploadFailureOutcome, type PendingUploadConflict } from '../upload';
import { createControlledUploadClient, ensureUploadNonce, resetUploadNonce, uploadConflict, uploadIdempotencyKey, uploadSessionWritable } from '../upload-client';
import type { UploadSession } from '../types';

const incomplete = new APIError(409, { code: 'PATH_CONFLICT', message: 'target path is occupied', details: { reason: 'incomplete_upload', upload_id: 'upload-1', expected_size: 2194, expires_at: '2026-10-27T03:06:12Z' } });
const nameTaken = new APIError(409, { code: 'PATH_CONFLICT', message: 'target path is occupied', details: { reason: 'name_taken' } });

describe('upload path conflict details', () => {
  it('turns an incomplete owner upload into an actionable abandon-and-retry alert', () => {
    const parsed = uploadConflict(incomplete);
    expect(parsed).toEqual({ reason: 'incomplete_upload', uploadId: 'upload-1', expectedSize: 2194, expiresAt: '2026-10-27T03:06:12Z' });
    const conflict: PendingUploadConflict = { ...parsed!, fileId: 'file-1', name: 'smart_assistant.tex' };
    const abandon = vi.fn(), rename = vi.fn();
    render(<UploadConflictAlert conflict={conflict} acting={false} onAbandon={abandon} onRename={rename} />);
    expect(screen.getByText(/不会出现在文件列表中，但仍占用这个名称/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: '取消未完成上传并重试' }));
    expect(abandon).toHaveBeenCalledOnce();
    fireEvent.click(screen.getByRole('button', { name: '重命名后重试' }));
    expect(rename).toHaveBeenCalledOnce();
    expect(screen.queryByRole('button', { name: /覆盖/ })).toBeNull();
  });

  it('shows only a rename message for a taken name and never offers overwrite', () => {
    const parsed = uploadConflict(nameTaken);
    expect(parsed).toEqual({ reason: 'name_taken' });
    render(<UploadConflictAlert conflict={{ ...parsed!, fileId: 'file-2', name: 'visible.txt' }} acting={false} onAbandon={vi.fn()} onRename={vi.fn()} />);
    expect(screen.getByText(/请重命名后上传/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '取消未完成上传并重试' })).toBeNull();
    expect(screen.getByRole('button', { name: '重命名后重试' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /覆盖/ })).toBeNull();
  });

  it('treats an older conflict without details as a name conflict and ignores non-path errors', () => {
    expect(uploadConflict(new APIError(409, { code: 'PATH_CONFLICT' }))).toEqual({ reason: 'name_taken' });
    expect(uploadConflict(new APIError(409, { code: 'IDEMPOTENCY_CONFLICT', details: { reason: 'incomplete_upload', upload_id: 'x' } }))).toBeNull();
    expect(uploadConflict(new Error('network'))).toBeNull();
  });
});

describe('retry after abandon', () => {
  it('resets the nonce so the retry uses a new idempotency key', () => {
    const client = createControlledUploadClient(new API(async () => 'token'), 1);
    try {
      const id = client.addFile({ name: 'smart_assistant.tex', data: new Blob(['x']) });
      const before = ensureUploadNonce(client, id);
      resetUploadNonce(client, id);
      const after = ensureUploadNonce(client, id);
      expect(after).not.toBe(before);
      expect(uploadIdempotencyKey(after)).not.toBe(uploadIdempotencyKey(before));
      expect(uploadIdempotencyKey(after)).toHaveLength(43);
      expect(client.getFile(id)?.meta.upload_nonce).toBe(after);
    } finally { client.destroy(); }
  });

  it('does not hand a terminal begin-upload session to tus', () => {
    expect(uploadSessionWritable({ status: 'uploading' })).toBe(true);
    expect(uploadSessionWritable({ status: 'failed_confirmed' })).toBe(false);
    expect(uploadSessionWritable({ status: 'committed' })).toBe(false);
    expect(uploadSessionWritable({})).toBe(false);
  });
});

const conflictBody = JSON.stringify({ error: { code: 'PATH_CONFLICT', message: 'target path is occupied', details: { reason: 'incomplete_upload', upload_id: 'upload-9', expected_size: 2194 } } });
const file = { id: 'file-1', name: 'smart_assistant.tex' };

describe('upload-error diagnostics', () => {
  it('shows the conflict panel for a 409 from the @uppy/tus response shape and clears the banner', () => {
    const outcome = uploadFailureOutcome(file, { message: 'failed' }, { status: 409, body: { xhr: { responseText: conflictBody } } });
    expect(outcome.conflict).toEqual({ reason: 'incomplete_upload', uploadId: 'upload-9', expectedSize: 2194, fileId: 'file-1', name: 'smart_assistant.tex' });
    expect(mergeUploadError('传输暂未完成。', outcome)).toBe('');
  });

  it('shows the conflict panel for a 409 from the tus-js-client originalResponse shape', () => {
    const outcome = uploadFailureOutcome(file, { originalResponse: { getStatus: () => 409, getBody: () => conflictBody } });
    expect(outcome.conflict).toEqual({ reason: 'incomplete_upload', uploadId: 'upload-9', expectedSize: 2194, fileId: 'file-1', name: 'smart_assistant.tex' });
    expect(mergeUploadError('', outcome)).toBe('');
  });

  it('reports the real HTTP status and code for a non-conflict failure', () => {
    const outcome = uploadFailureOutcome(file, { originalResponse: { getStatus: () => 422, getBody: () => JSON.stringify({ error: { code: 'INVALID_ARGUMENT' } }) } });
    expect(outcome.conflict).toBeNull();
    expect(outcome.specific).toBe(true);
    expect(outcome.text).toContain('HTTP 422');
    expect(outcome.text).toContain('INVALID_ARGUMENT');
    expect(mergeUploadError('', outcome)).toBe(outcome.text);
  });

  it('never replaces a specific failure with the old generic banner', () => {
    const specific = uploadFailureOutcome(file, { originalResponse: { getStatus: () => 422, getBody: () => JSON.stringify({ error: { code: 'INVALID_ARGUMENT' } }) } });
    const generic = uploadFailureOutcome(file, new Error('network'));
    expect(generic.specific).toBe(false);
    expect(mergeUploadError(specific.text, generic)).toBe(specific.text);
    expect(mergeUploadError('', generic)).toContain('传输暂未完成');
  });
});

describe('prepareUploads retry and preflight', () => {
  const uploaded = { id: 'session-new', file_id: 'file-new', operation_id: 'op-new', status: 'uploading', upload_url: `${location.origin}/v1/tus/session-new` };
  const json = (value: unknown) => new Response(JSON.stringify(value), { status: 200, headers: { 'Content-Type': 'application/json' } });

  it('re-POSTs when the cached session is no longer writable', async () => {
    const client = createControlledUploadClient(new API(async () => 'token'), 1);
    const id = client.addFile({ name: 'retry.txt', type: 'text/plain', data: new Blob(['x']) });
    const sessions = new Map<string, UploadSession>([[id, { id: 'session-old', operation_id: 'op-old', status: 'failed_confirmed', upload_url: `${location.origin}/v1/tus/session-old` }]]);
    const fetchMock = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => init?.method === 'HEAD' ? new Response(null, { status: 200 }) : json(uploaded));
    vi.stubGlobal('fetch', fetchMock);
    try {
      await prepareUploads([id], { api: new API(async () => 'token'), uppy: client, sessions, parent: null, signal: new AbortController().signal, showConflict: vi.fn(), clearConflictFor: vi.fn(), setError: vi.fn() });
      const posts = fetchMock.mock.calls.filter(call => (call[1] as RequestInit | undefined)?.method === 'POST');
      expect(posts).toHaveLength(1);
      expect(posts[0][0]).toBe('/v1/uploads');
      expect(sessions.get(id)?.id).toBe('session-new');
      expect(client.getFile(id)?.tus?.uploadUrl).toBe(`${location.origin}/v1/tus/session-new`);
    } finally { client.destroy(); }
  });

  it('stops when the HEAD preflight is non-200 and never starts a create request', async () => {
    const client = createControlledUploadClient(new API(async () => 'token'), 1);
    const id = client.addFile({ name: 'head.txt', type: 'text/plain', data: new Blob(['x']) });
    const sessions = new Map<string, UploadSession>();
    const calls: { method?: string; url: string }[] = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ method: init?.method, url: String(input) });
      if (init?.method === 'HEAD') return new Response(null, { status: 503 });
      return json(uploaded);
    });
    vi.stubGlobal('fetch', fetchMock);
    const setError = vi.fn();
    try {
      await expect(prepareUploads([id], { api: new API(async () => 'token'), uppy: client, sessions, parent: null, signal: new AbortController().signal, showConflict: vi.fn(), clearConflictFor: vi.fn(), setError })).rejects.toThrow('upload preflight failed');
      expect(setError).toHaveBeenCalledWith(expect.stringContaining('HTTP 503'));
      expect(sessions.has(id)).toBe(false);
      expect(calls.map(call => call.method)).toEqual(['POST', 'HEAD']);
      expect(calls.some(call => call.method === 'POST' && new URL(call.url, location.origin).pathname === '/v1/tus')).toBe(false);
    } finally { client.destroy(); }
  });

  it('does not clear the conflict panel before the POST and clears it after success', async () => {
    const client = createControlledUploadClient(new API(async () => 'token'), 2);
    const id = client.addFile({ name: 'conflict.txt', type: 'text/plain', data: new Blob(['x']) });
    const sessions = new Map<string, UploadSession>();
    const showConflict = vi.fn(), clearConflictFor = vi.fn();
    const fetchMock = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => init?.method === 'HEAD' ? new Response(null, { status: 200 }) : json(uploaded));
    vi.stubGlobal('fetch', fetchMock);
    try {
      await prepareUploads([id], { api: new API(async () => 'token'), uppy: client, sessions, parent: null, signal: new AbortController().signal, showConflict, clearConflictFor, setError: vi.fn() });
      expect(showConflict).not.toHaveBeenCalled();
      expect(clearConflictFor).toHaveBeenCalledWith(id);
      // A failed POST leaves the existing panel untouched for the user to act on.
      const conflict = new Response(JSON.stringify({ error: { code: 'PATH_CONFLICT', message: 'x', details: { reason: 'incomplete_upload', upload_id: 'upload-1' } } }), { status: 409, headers: { 'Content-Type': 'application/json' } });
      const failing = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => init?.method === 'HEAD' ? new Response(null, { status: 200 }) : conflict);
      vi.stubGlobal('fetch', failing);
      const showAgain = vi.fn(), clearAgain = vi.fn();
      const other = client.addFile({ name: 'conflict-2.txt', type: 'text/plain', data: new Blob(['x']) });
      await expect(prepareUploads([other], { api: new API(async () => 'token'), uppy: client, sessions, parent: null, signal: new AbortController().signal, showConflict: showAgain, clearConflictFor: clearAgain, setError: vi.fn() })).rejects.toBeTruthy();
      expect(clearAgain).not.toHaveBeenCalled();
      expect(showAgain).toHaveBeenCalledWith(expect.objectContaining({ reason: 'incomplete_upload', uploadId: 'upload-1', fileId: other }));
    } finally { client.destroy(); }
  });
});
