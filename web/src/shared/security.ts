/** Keep every binary transfer on the business gateway. Never pass native oCIS URLs through. */
export function controlledURL(raw: string, origin = window.location.origin): string {
  const url = new URL(raw, origin);
  if (url.origin !== origin || url.username || url.password || !/^\/v1\/(?:uploads|tus|files|artifacts|preview)(?:\/|$)/.test(url.pathname)) {
    throw new Error('拒绝访问业务网关以外的资源');
  }
  return url.href;
}
export function safeMarkdownURL(raw: string): string {
  if (raw.startsWith('#')) return raw;
  try {
    const url = new URL(raw, 'https://clouddrive.invalid');
    if (['https:', 'http:', 'mailto:'].includes(url.protocol)) return raw;
  } catch { /* Invalid URLs are not rendered. */ }
  return '';
}
export function controlledAsset(raw: string): boolean {
  return /^\/v1\/artifacts\/[^/?#]+\/assets\/[^?#]+(?:\?[^#]*)?$/.test(raw) && !raw.includes('..') && !/%2e|%2f|%5c/i.test(raw);
}
export function validName(name: string): boolean {
  return !!name.trim() && name === name.trim() && name !== '.' && name !== '..' && !/[\x00-\x1f/\\]/.test(name) && new TextEncoder().encode(name).length <= 255;
}
export function bytes(value = 0): string {
  if (!Number.isFinite(value) || value < 0) return '—';
  if (value === 0) return '0 B';
  const unit = Math.min(Math.floor(Math.log(value) / Math.log(1024)), 4);
  return `${(value / 1024 ** unit).toFixed(unit > 0 ? 1 : 0)} ${['B', 'KiB', 'MiB', 'GiB', 'TiB'][unit]}`;
}
export function date(value?: string): string {
  if (!value) return '—';
  const d = new Date(value);
  return Number.isNaN(d.valueOf()) ? '—' : d.toLocaleString('zh-CN', { hour12: false });
}
export function operationState(value: { state?: string; status?: string }): string { return value.state ?? value.status ?? 'pending'; }
export function operationDone(value: { state?: string; status?: string }): boolean { return ['committed', 'completed', 'succeeded', 'saved'].includes(operationState(value)); }
export function operationFailed(value: { state?: string; status?: string }): boolean { return ['failed_confirmed', 'failed', 'cancelled'].includes(operationState(value)); }
