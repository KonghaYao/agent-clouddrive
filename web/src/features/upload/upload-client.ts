import Uppy from '@uppy/core';
import Tus, { type TusBody, type TusOptions } from '@uppy/tus';
import { APIError, type API, type APIErrorBody } from '../../shared/api';
import { controlledURL } from '../../shared/security';
type Meta = Record<string, unknown>;
export interface UploadConflict { reason: 'incomplete_upload' | 'name_taken'; uploadId?: string; expectedSize?: number; expiresAt?: string }
/** Only an owner-scoped, zero-byte incomplete upload is actionable; all other path conflicts are name conflicts. */
export function uploadConflict(error: unknown): UploadConflict | null {
  if (!(error instanceof APIError) || error.status !== 409 || error.code !== 'PATH_CONFLICT') return null;
  const details = error.detail.details;
  if (details?.reason === 'incomplete_upload' && details.upload_id) return { reason: 'incomplete_upload', uploadId: details.upload_id, expectedSize: details.expected_size, expiresAt: details.expires_at };
  return { reason: 'name_taken' };
}
/** A session that is no longer uploading must not be handed to tus. */
export function uploadSessionWritable(session: { status?: string }): boolean { return session.status === 'uploading'; }
export interface UploadFailureResponse { status?: number; body?: unknown }
/** @uppy/tus wraps the failed response as {body:{xhr}}; tus-js-client exposes originalResponse. */
export function uploadFailurePayload(error: unknown, response?: UploadFailureResponse): unknown {
  const body = response?.body as { xhr?: { responseText?: string } } | string | undefined;
  const raw = (typeof body === 'object' ? body?.xhr?.responseText : body)
    ?? (error as { originalResponse?: { getBody?: () => string | undefined } })?.originalResponse?.getBody?.();
  if (!raw) return undefined;
  try { return JSON.parse(raw); } catch { return undefined; }
}
/** Failed tus PATCHes carry the same PATH_CONFLICT details as the begin-upload response. */
export function uploadConflictFromFailure(error: unknown, response?: UploadFailureResponse): UploadConflict | null {
  const status = response?.status ?? (error as { originalResponse?: { getStatus?: () => number } })?.originalResponse?.getStatus?.();
  if (status !== 409) return null;
  const payload = uploadFailurePayload(error, response) as (APIErrorBody & { error?: APIErrorBody }) | undefined;
  return uploadConflict(new APIError(status, payload?.error ?? payload ?? {}));
}
/** Failed transfers must never surface as an information-free banner. */
export function uploadFailureText(status?: number, code?: string): string {
  const detail = status ? `HTTP ${status}${code ? ` · ${code}` : ''}` : '';
  return detail
    ? `传输未完成（${detail}）。可重试或继续同一会话；接收节点失效或会话过期时需重新选择文件。`
    : '传输暂未完成。可重试或继续同一会话；接收节点失效或会话过期时需重新选择文件。';
}
/** Retrying after abandon or rename must use a fresh nonce and therefore a fresh idempotency key. */
export function resetUploadNonce(uppy: Uppy<Meta, TusBody>, id: string): string {
  const nonce = crypto.randomUUID();
  uppy.setFileMeta(id, { upload_nonce: nonce });
  return nonce;
}
/** A per-file nonce keeps retries idempotent without encoding names, MIME types or paths in the key. */
export function uploadNonce(meta: Meta): string {
  const existing = meta.upload_nonce;
  return typeof existing === 'string' && existing !== '' ? existing : crypto.randomUUID();
}
export function uploadIdempotencyKey(nonce: string): string { return `upload:${nonce}`; }
/** Reuse the nonce stored on the file; only create and persist it when missing. */
export function ensureUploadNonce(uppy: Uppy<Meta, TusBody>, id: string): string {
  const file = uppy.getFile(id);
  const nonce = uploadNonce(file.meta);
  if (file.meta.upload_nonce !== nonce) uppy.setFileMeta(id, { upload_nonce: nonce });
  return nonce;
}
/** Injection of the HTTP stack allows deterministic protocol tests, not production data simulation. */
export function createControlledUploadClient(api: API, maxFiles = 20, httpStack?: TusOptions<Meta, TusBody>['httpStack']) {
  return new Uppy<Meta, TusBody>({ autoProceed: false, restrictions: { maxNumberOfFiles: maxFiles }, logger: { debug: () => {}, warn: () => {}, error: () => {} } }).use(Tus, {
    // Sessions are always created by POST /v1/uploads first, so an empty endpoint is correct:
    // tus-js-client would otherwise POST a non-existent /v1/tus after any failed HEAD and hide the real status.
    endpoint: '', chunkSize: 32 * 1024 * 1024,
    withCredentials: true, retryDelays: [0, 1000, 3000, 5000, 10000, 30000],
    storeFingerprintForResuming: false, removeFingerprintOnSuccess: true, allowedMetaFields: [],
    // An explicit `httpStack: undefined` overrides tus-js-client's browser default and every upload
    // dies before the first request with "Cannot read properties of undefined (reading 'createRequest')".
    ...(httpStack !== undefined ? { httpStack } : {}),
    onBeforeRequest: async request => {
      controlledURL(request.getURL());
      const headers = await api.authHeader();
      Object.entries(headers).forEach(([key, value]) => request.setHeader(key, value));
    },
    onShouldRetry: (failure, _attempt, _options, next) => {
      const status = failure.originalResponse?.getStatus();
      return status === 401 || status === 403 || status === 404 || status === 409 || status === 410 ? false : next(failure);
    },
  });
}

/** A non-200 HEAD means the reserved session cannot be written; Uppy must not start tus at all. */
export class UploadPreflightError extends Error {
  constructor(public status: number) { super(`上传地址预检失败（HTTP ${status}）`); this.name = 'UploadPreflightError'; }
}
/** Probe the reserved upload URL with the same origin, bearer token and tus header the uploader will use. */
export async function preflightUpload(api: API, rawURL: string, request: typeof fetch = fetch): Promise<{ offset: number; length: number }> {
  const url = controlledURL(rawURL);
  const headers = { ...(await api.authHeader()), 'Tus-Resumable': '1.0.0' };
  const response = await request(url, { method: 'HEAD', headers, credentials: 'include', cache: 'no-store', referrerPolicy: 'no-referrer' });
  if (response.status !== 200) throw new UploadPreflightError(response.status);
  return { offset: Number(response.headers.get('Upload-Offset') ?? 0), length: Number(response.headers.get('Upload-Length') ?? 0) };
}
