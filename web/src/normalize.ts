// The API intentionally returns database projections for nested trash and share records.
// Normalize only presentation names; authorization and all decisions remain server-side.
type Row = Record<string, unknown>;
function row(value: unknown): Row { return value && typeof value === 'object' && !Array.isArray(value) ? value as Row : {}; }
export function normalizeFile(input: unknown): Row {
  const file = row(input);
  return { ...file, size_bytes: file.size_bytes ?? file.size ?? 0 };
}
export function normalizeShare(input: unknown): Row {
  const share = row(input), body = row(share.request_body);
  if (share.request_body) return { ...share, ...body, id: share.id, status: share.state ?? share.status, request_expires_at: share.expires_at, expires_at: body.expires_at };
  return { ...share, status: share.state ?? share.status, public_url: share.public_url ?? share.url };
}
export function normalizeResponse(path: string, input: unknown): unknown {
  const data = row(input), endpoint = path.split('?')[0];
  const mapItems = (fn: (value: unknown) => unknown) => ({ ...data, items: Array.isArray(data.items) ? data.items.map(fn) : [] });
  if (endpoint === '/v1/files') return mapItems(normalizeFile);
  if (/^\/v1\/files\/[^/]+$/.test(endpoint)) return normalizeFile(input);
  if (endpoint.endsWith('/versions')) return mapItems(value => { const v = row(value); return { ...v, size_bytes: v.size_bytes ?? v.size ?? 0, created_at: v.created_at ?? v.committed_at }; });
  if (endpoint === '/v1/trash') return mapItems(value => { const t = row(value); return t.file ? { ...normalizeFile(t.file), trash_id: t.id, original_path: t.original_path, original_name: t.original_name, name: t.original_name ?? row(t.file).name, deleted_at: t.deleted_at } : normalizeFile(t); });
  if (endpoint === '/v1/shared-with-me') return mapItems(value => { const g = row(value); return g.file ? { ...normalizeFile(g.file), share_id: g.id, permissions: g.rights, shared_version_policy: g.version_policy, shared_version_id: g.version_id } : normalizeFile(g); });
  if (endpoint === '/v1/share-requests' || endpoint === '/v1/shares') return Array.isArray(data.items) ? mapItems(normalizeShare) : normalizeShare(input);
  if (/^\/v1\/share-requests\/[^/]+(?:\/approve)?$/.test(endpoint)) return normalizeShare(input);
  if (endpoint === '/v1/admin/feedback') return mapItems(value => { const f = row(value); return { ...f, content: f.body ?? f.content }; });
  return input;
}
