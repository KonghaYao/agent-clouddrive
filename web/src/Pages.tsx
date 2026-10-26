import { Alert, Button, Card, Empty, Input, List, Space, Table, Tabs, Tag, Typography } from 'antd';
import { SearchOutlined } from '@ant-design/icons';
import { useEffect, useRef, useState } from 'react';
import type { API } from './api';
import type { AdminJob, Feedback, Page, SearchHit } from './types';
import { usePage } from './hooks';
import { CompletenessNotice, ErrorNotice, Status } from './components';
import { date } from './security';
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
export function AdminPage({ api, revision }: { api: API; revision: number }) {
  const [status, setStatus] = useState('');
  const jobs = usePage<AdminJob>(api, `/v1/admin/jobs${status ? `?status=${encodeURIComponent(status)}` : ''}`, revision);
  const [category, setCategory] = useState('');
  const feedback = usePage<Feedback>(api, `/v1/admin/feedback${category ? `?category=${encodeURIComponent(category)}` : ''}`, revision);
  return <Space orientation="vertical" size={20} className="full-width"><Alert type="info" showIcon title="处理状态与匿名反馈" description="后台身份不自动授予文件正文读取权。反馈记录不包含提交者、来源地址或关联追踪信息。" /><Tabs items={[
    { key: 'jobs', label: '处理任务', children: <Space orientation="vertical" className="full-width"><Input.Search placeholder="按状态筛选，例如 failed、blocked、partial" onSearch={setStatus} allowClear /><ErrorNotice error={jobs.error} retry={jobs.reload} /><Table rowKey="id" loading={jobs.loading} dataSource={jobs.items} pagination={false} columns={[{ title: '任务', dataIndex: 'id' }, { title: '状态', dataIndex: 'status', render: value => <Status value={value} /> }, { title: '分类错误', dataIndex: 'error_code' }, { title: '缺失文字页', dataIndex: 'missing_text_pages', render: (value: number[]) => value?.join('、') ?? '—' }, { title: '缺失描述页', dataIndex: 'missing_description_pages', render: (value: number[]) => value?.join('、') ?? '—' }, { title: '更新', dataIndex: 'updated_at', render: value => date(value) }]} />{jobs.next && <Button onClick={() => void jobs.more()}>更多任务</Button>}</Space> },
    { key: 'feedback', label: '匿名反馈', children: <Space orientation="vertical" className="full-width"><Input.Search placeholder="按分类筛选，例如 text、missing_page、description" onSearch={setCategory} allowClear /><ErrorNotice error={feedback.error} retry={feedback.reload} /><Table rowKey="id" loading={feedback.loading} dataSource={feedback.items} pagination={false} columns={[{ title: '文件 ID', dataIndex: 'file_id' }, { title: '源版本', dataIndex: 'version_id' }, { title: '分类', dataIndex: 'category' }, { title: '内容', dataIndex: 'content' }]} />{feedback.next && <Button onClick={() => void feedback.more()}>更多反馈</Button>}</Space> },
  ]} /></Space>;
}
