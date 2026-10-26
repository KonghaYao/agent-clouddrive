import { Button, Card, Empty, Input, List, Space, Tag, Typography } from 'antd';
import { SearchOutlined } from '@ant-design/icons';
import { useEffect, useRef, useState } from 'react';
import type { API } from '../../shared/api';
import type { Page, SearchHit } from '../../shared/types';
import { CompletenessNotice, ErrorNotice } from '../../shared/components';
export function SearchPage({ api, onOpen }: { api: API; onOpen: (hit: SearchHit) => void }) {
  const [draft, setDraft] = useState(''), [term, setTerm] = useState('');
  const [results, setResults] = useState<SearchHit[]>([]), [cursor, setCursor] = useState<string | null>(null), [busy, setBusy] = useState(false), [error, setError] = useState<unknown>();
  const generation = useRef(0);
  useEffect(() => {
    const sequence = ++generation.current, abort = new AbortController();
    setResults([]); setCursor(null); setError(undefined);
    if (!term) return;
    setBusy(true);
    void api.post<Page<SearchHit>>('/v1/search', { query: term, limit: 20 }, abort.signal).then(data => { if (sequence === generation.current) { setResults(data.items ?? []); setCursor(data.next_cursor ?? null); } }).catch(e => { if (!abort.signal.aborted) setError(e); }).finally(() => { if (!abort.signal.aborted) setBusy(false); });
    return () => { abort.abort(); generation.current++; };
  }, [api, term]);
  async function more() {
    if (!cursor) return;
    const sequence = generation.current; setBusy(true);
    try { const data = await api.post<Page<SearchHit>>('/v1/search', { query: term, cursor, limit: 20 }); if (sequence === generation.current) { setResults(old => [...old, ...data.items]); setCursor(data.next_cursor ?? null); } }
    catch (e) { if (sequence === generation.current) setError(e); } finally { if (sequence === generation.current) setBusy(false); }
  }
  return <Space orientation="vertical" className="full-width" size={20}>
    <Input.Search size="large" prefix={<SearchOutlined />} placeholder="输入关键词，搜索自己的文件正文" value={draft} onChange={e => setDraft(e.target.value)} onSearch={value => setTerm(value.trim())} enterButton="搜索" loading={busy} allowClear />
    <Typography.Text type="secondary">仅检索自己的文件。收到的分享不会混入搜索或 Agent 读取范围。</Typography.Text>
    <ErrorNotice error={error} />
    <List loading={busy} dataSource={results} locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={term ? '没有找到可访问的结果，试试其他关键词' : '搜索正文、OCR 文字和已完成的图片描述'} /> }} renderItem={(hit, index) => <List.Item key={`${hit.file_id}-${hit.artifact_set_id}-${index}`}><Card className="search-result"><Space orientation="vertical" size={10} className="full-width"><Typography.Link className="search-title" onClick={() => onOpen(hit)}>{hit.path}</Typography.Link><Typography.Paragraph className="search-snippet">{hit.snippet}</Typography.Paragraph><Space wrap><Tag>源版本：{hit.returned_version_id}</Tag>{hit.location?.page && <Tag>第 {hit.location.page} 页</Tag>}{hit.stale && <Tag color="orange">返回旧的可用版本 · 新版{hit.newer_version_status === 'processing' ? '处理中' : hit.newer_version_status}</Tag>}</Space><CompletenessNotice value={hit.completeness} /><Typography.Text type="secondary" copyable={{ text: JSON.stringify({ file_id: hit.file_id, version_id: hit.returned_version_id, artifact_set_id: hit.artifact_set_id, location: hit.location }) }}>复制固定版本引用</Typography.Text></Space></Card></List.Item>} />
    {cursor && <Button onClick={() => void more()} loading={busy}>加载更多结果</Button>}
  </Space>;
}
