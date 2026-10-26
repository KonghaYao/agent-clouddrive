import { describe, expect, it } from 'vitest';
import { bytes, controlledAsset, controlledURL, operationDone, operationFailed, safeMarkdownURL, validName } from '../shared/security';
import { APIError, errorText } from '../shared/api';
describe('business gateway boundary', () => {
  const origin = 'https://drive.example';
  it.each(['/v1/files/f/content', '/v1/tus/s', '/v1/artifacts/a/assets/p.png', '/v1/preview/f/pages/1'])('accepts controlled resource %s', path => expect(controlledURL(path, origin)).toBe(origin + path));
  it.each(['https://ocis.example/file', '//ocis.example/file', 'javascript:alert(1)', 'data:text/html,secret', '/api/admin', '/v1/files/../../admin', 'https://user:secret@drive.example/v1/files/f'])('rejects unsafe URL %s', value => expect(() => controlledURL(value, origin)).toThrow());
  it.each(['javascript:alert(1)', 'data:text/html;base64,abc', 'vbscript:msgbox(1)', 'file:///etc/passwd'])('strips unsafe Markdown URL %s', value => expect(safeMarkdownURL(value)).toBe(''));
  it('allows ordinary links and anchors', () => { expect(safeMarkdownURL('https://example.com')).toBe('https://example.com'); expect(safeMarkdownURL('#chapter')).toBe('#chapter'); });
  it('restricts embedded images to controlled artifact routes', () => { expect(controlledAsset('/v1/artifacts/a/assets/page.png')).toBe(true); for (const value of ['https://tracking.example/pixel', '/v1/artifacts/a/assets/../x', '/v1/artifacts/a/assets/%2e%2e/x', '//evil/v1/artifacts/a/assets/x']) expect(controlledAsset(value)).toBe(false); });
});
describe('names, sizes, statuses and errors', () => {
  it.each(['', '.', '..', ' hello', 'a/b', 'a\\b', 'a\u0000b', 'a'.repeat(256)])('rejects invalid file name %s', value => expect(validName(value)).toBe(false));
  it('validates Unicode byte length and does not ban ordinary Unicode', () => { expect(validName('报告（最终版）.pdf')).toBe(true); expect(validName('字'.repeat(86))).toBe(false); });
  it('renders exact 10GiB input without decimal confusion', () => expect(bytes(10737418240)).toBe('10.0 GiB'));
  it('never calls an unknown remote outcome completed', () => { expect(operationDone({ state: 'outcome_unknown' })).toBe(false); expect(operationFailed({ state: 'outcome_unknown' })).toBe(false); expect(operationDone({ status: 'saved' })).toBe(true); expect(operationFailed({ state: 'failed_confirmed' })).toBe(true); });
  it('explains concurrency without offering overwrite', () => expect(errorText(new APIError(409, { code: 'VERSION_CONFLICT' }))).toContain('不会覆盖'));
  it('explains trash quota and Retry-After', () => { expect(errorText(new APIError(422, { code: 'QUOTA_EXCEEDED' }))).toContain('删除不会释放'); expect(errorText(new APIError(429, { code: 'RATE_LIMITED' }, '30'))).toContain('30 秒'); });
});
