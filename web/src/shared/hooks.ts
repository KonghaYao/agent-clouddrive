import { useEffect, useRef, useState } from 'react';
import type { API } from './api';
import type { Page } from './types';
export function usePage<T>(api: API, path: string, revision = 0, enabled = true) {
  const [items, setItems] = useState<T[]>([]), [next, setNext] = useState<string | null>(null);
  const [loading, setLoading] = useState(false), [error, setError] = useState<unknown>();
  const [retry, setRetry] = useState(0);
  const generation = useRef(0);
  useEffect(() => {
    const current = ++generation.current, abort = new AbortController();
    setItems([]); setNext(null); setLoading(enabled); setError(undefined);
    if (!enabled) return () => { abort.abort(); generation.current++; };
    void api.get<Page<T>>(path, abort.signal).then(result => {
      if (generation.current === current && !abort.signal.aborted) { setItems(result.items ?? []); setNext(result.next_cursor ?? null); }
    }).catch(e => { if (!abort.signal.aborted) setError(e); }).finally(() => { if (!abort.signal.aborted) setLoading(false); });
    return () => { abort.abort(); generation.current++; };
  }, [api, path, revision, retry, enabled]);
  async function more() {
    if (!next || loading) return;
    const current = generation.current; setLoading(true); setError(undefined);
    try {
      const data = await api.get<Page<T>>(`${path}${path.includes('?') ? '&' : '?'}cursor=${encodeURIComponent(next)}`);
      if (current === generation.current) { setItems(old => [...old, ...(data.items ?? [])]); setNext(data.next_cursor ?? null); }
    } catch (e) { if (current === generation.current) setError(e); }
    finally { if (current === generation.current) setLoading(false); }
  }
  return { items, next, loading, error, more, reload: () => setRetry(x => x + 1) };
}
