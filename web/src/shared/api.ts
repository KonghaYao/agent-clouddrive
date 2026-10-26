import { controlledURL } from './security';
import { normalizeResponse } from '../features/files/normalize';
export interface APIErrorBody { code?: string; message?: string; operation_id?: string; current_version_id?: string; current_metadata_revision?: number; required_scope?: string; details?: { reason?: string; upload_id?: string; expected_size?: number; expires_at?: string } }
export class APIError extends Error {
  constructor(public status: number, public detail: APIErrorBody, public retryAfter?: string | null) { super(detail.message || `请求失败 (${status})`); }
  get code() { return this.detail.code ?? 'UNKNOWN_ERROR'; }
}
const messages: Record<string, string> = {
  UNAUTHENTICATED: '登录已过期，请重新授权。',
  INSUFFICIENT_SCOPE: '当前授权不包含这个操作。请重新登录并选择所需权限。',
  ACCESS_DENIED: '没有访问权限，或账号已停用。',
  NOT_FOUND: '资源不存在或不可访问。',
  VERSION_CONFLICT: '文件已被其他操作更新。请刷新后重新修改，不会覆盖新内容。',
  PATH_CONFLICT: '目标位置已有同名项目。请选择其他名称或目录，不会覆盖已有文件。',
  OPERATION_PENDING: '上一次操作结果仍在确认中，请查看操作状态，不要重复上传。',
  IDEMPOTENCY_CONFLICT: '这个重试标识已用于不同的请求，请刷新后重试。',
  UPLOAD_TOO_LARGE: '文件超过原件上传上限，尚未上传。',
  QUOTA_EXCEEDED: '个人逻辑配额不足。回收站仍计入配额，删除不会释放额度。',
  INVALID_PATH: '路径不合法。请使用有效目录和不含斜线的文件名。',
  RATE_LIMITED: '请求过于频繁，请稍后重试。',
  DEPENDENCY_UNAVAILABLE: '依赖服务暂时不可用。已提交的原件不会因此丢失，请稍后刷新。',
  INTERACTIVE_SESSION_REQUIRED: '分享批准需要当前网页的交互式登录，请重新登录。',
  CONFIRMATION_REQUIRED: '分享批准需要当前网页的安全会话与明确确认。请重新登录。',
  CONFIRMATION_EXPIRED: '此分享确认请求已过期，请重新创建。',
  INVALID_ARGUMENT: '请求参数无效，请检查名称、权限和目标位置。',
  CSRF_INVALID: '安全会话已失效，请重新登录后确认。',
};
export function errorText(error: unknown): string {
  if (error instanceof APIError) {
    const message = messages[error.code] ?? `操作失败（${error.code}）`;
    return `${message}${error.detail.required_scope ? ` 所需权限：${error.detail.required_scope}` : ''}${error.status === 429 && error.retryAfter ? ` 建议等待 ${error.retryAfter} 秒。` : ''}`;
  }
  if (error instanceof Error && error.name === 'AbortError') return '请求已取消。';
  return '网络请求失败，请检查连接后重试。';
}
export class API {
  csrfToken = '';
  constructor(private token: () => Promise<string>) {}
  async request<T>(path: string, options: RequestInit = {}): Promise<T> {
    if (!path.startsWith('/v1/') || path.startsWith('//')) throw new Error('无效 API 路径');
    const headers = new Headers(options.headers);
    headers.set('Authorization', `Bearer ${await this.token()}`);
    if (options.body) headers.set('Content-Type', 'application/json');
    if (this.csrfToken && options.method && !['GET', 'HEAD'].includes(options.method)) headers.set('X-CSRF-Token', this.csrfToken);
    const response = await fetch(path, { ...options, headers, credentials: 'include', cache: 'no-store', referrerPolicy: 'no-referrer' });
    if (!response.ok) {
      const body = await response.json().catch(() => ({}));
      throw new APIError(response.status, body.error ?? body, response.headers.get('Retry-After'));
    }
    if (response.status === 204) return undefined as T;
    return normalizeResponse(path, await response.json()) as T;
  }
  get<T>(path: string, signal?: AbortSignal) { return this.request<T>(path, { signal }); }
  post<T>(path: string, value: unknown = {}, signal?: AbortSignal) { return this.request<T>(path, { method: 'POST', body: JSON.stringify(value), signal }); }
  patch<T>(path: string, value: unknown) { return this.request<T>(path, { method: 'PATCH', body: JSON.stringify(value) }); }
  delete<T>(path: string, value: unknown) { return this.request<T>(path, { method: 'DELETE', body: JSON.stringify(value) }); }
  async binary(path: string, signal?: AbortSignal): Promise<Response> {
    const url = controlledURL(path);
    const result = await fetch(url, { headers: { Authorization: `Bearer ${await this.token()}` }, credentials: 'include', referrerPolicy: 'no-referrer', cache: 'no-store', signal });
    if (!result.ok) {
      const body = await result.json().catch(() => ({}));
      throw new APIError(result.status, body.error ?? body);
    }
    return result;
  }
  async authHeader(): Promise<Record<string, string>> { return { Authorization: `Bearer ${await this.token()}` }; }
}
export function query(params: Record<string, string | number | null | undefined>): string {
  const p = new URLSearchParams();
  Object.entries(params).forEach(([key, value]) => { if (value !== undefined && value !== null && value !== '') p.set(key, String(value)); });
  return p.size ? `?${p}` : '';
}
