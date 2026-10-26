import { Alert, App, Button, Descriptions, Drawer, Empty, Form, Input, Select, Space, Spin, Tabs, Timeline, Typography } from 'antd';
import { DownloadOutlined, ReloadOutlined, ShareAltOutlined } from '@ant-design/icons';
import { useEffect, useState } from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import rehypeSanitize from 'rehype-sanitize';
import type { API } from './api';
import { APIError, errorText, query } from './api';
import { bytes, controlledAsset, date, safeMarkdownURL } from './security';
import { downloadFile } from './download';
import { CompletenessNotice, ErrorNotice, OperationNotice, Status } from './components';
import type { DriveFile, MarkdownPage, Operation, Page, Version, SharedFile } from './types';
function ResourceImage({ api, src, alt }: { api: API; src: string; alt?: string }) {
  const [url, setURL] = useState(''), [failed, setFailed] = useState(false);
  useEffect(() => {
    setFailed(false); setURL('');
    const abort = new AbortController(); let objectURL = '';
    void api.binary(src, abort.signal).then(async response => {
      const type = response.headers.get('Content-Type') ?? '';
      if (!/^image\/(png|jpeg|webp|gif|avif)(?:;|$)/.test(type)) throw new Error('仅允许安全的光栅图片');
      const length = Number(response.headers.get('Content-Length'));
      if (!Number.isFinite(length) || length > 20 * 1024 * 1024) throw new Error('图片过大');
      const reader = response.body?.getReader(); if (!reader) throw new Error('空图片');
      const chunks: Uint8Array[] = []; let size = 0;
      while (true) { const part = await reader.read(); if (part.done) break; size += part.value.byteLength; if (size > 20 * 1024 * 1024) { await reader.cancel(); throw new Error('图片过大'); } chunks.push(part.value); }
      if (abort.signal.aborted) return;
      objectURL = URL.createObjectURL(new Blob(chunks as BlobPart[], { type })); setURL(objectURL);
    }).catch(() => { if (!abort.signal.aborted) setFailed(true); });
    return () => { abort.abort(); if (objectURL) URL.revokeObjectURL(objectURL); };
  }, [api, src]);
  return failed ? <Typography.Text type="secondary">[无法读取图片：{alt}]</Typography.Text> : url ? <img src={url} alt={alt ?? '文档图片'} className="document-image" loading="lazy" /> : <Spin size="small" />;
}
export function SafeMarkdown({ api, content }: { api: API; content: string }) {
  return <div className="markdown"><ReactMarkdown remarkPlugins={[remarkGfm]} rehypePlugins={[rehypeSanitize]} skipHtml urlTransform={safeMarkdownURL} components={{
    a: ({ href, children }) => href ? <a href={href} target="_blank" rel="noopener noreferrer" referrerPolicy="no-referrer">{children}</a> : <span>{children}</span>,
    img: ({ src, alt }) => typeof src === 'string' && controlledAsset(src) ? <ResourceImage api={api} src={src} alt={alt} /> : <span>[外部或不受控图片已阻止：{alt}]</span>,
  }}>{content}</ReactMarkdown></div>;
}
export function Details({ api, file: initial, onClose, canConvert, canShare, canFeedback, shared, onShare, initialVersion, initialArtifact }: { api: API; file: DriveFile; initialVersion?: string; initialArtifact?: string; onClose: () => void; canConvert: boolean; canShare: boolean; canFeedback: boolean; shared?: boolean; onShare: (file: DriveFile, version?: string) => void }) {
  const { message } = App.useApp();
  const [file, setFile] = useState(initial), [versions, setVersions] = useState<Version[]>([]), [versionCursor, setVersionCursor] = useState<string | null>(null);
  const sharedVersion = shared && (initial as SharedFile).shared_version_policy === 'fixed' ? (initial as SharedFile).shared_version_id : undefined;
  const [selected, setSelected] = useState<string | undefined>(initialVersion ?? sharedVersion ?? initial.current_version_id);
  const [markdown, setMarkdown] = useState<MarkdownPage | null>(null), [error, setError] = useState<unknown>(), [busy, setBusy] = useState(false);
  const [conversionJob, setConversionJob] = useState<{ id: string; state?: string; status?: string } | null>(null), [download, setDownload] = useState('');
  const [preview, setPreview] = useState<{ pages: { url: string; page?: number }[]; version_id?: string } | null>(null);
  const [previewError, setPreviewError] = useState<unknown>();
  const [feedbackForm] = Form.useForm();
  useEffect(() => {
    const abort = new AbortController(); setError(undefined);
    if (!shared) {
      void api.get<DriveFile>(`/v1/files/${encodeURIComponent(file.id)}`, abort.signal).then(setFile).catch(e => { if (!abort.signal.aborted) setError(e); });
      void api.get<Page<Version>>(`/v1/files/${encodeURIComponent(file.id)}/versions`, abort.signal).then(data => { setVersions(data.items ?? []); setVersionCursor(data.next_cursor ?? null); }).catch(e => { if (!abort.signal.aborted) setError(e); });
    }
    return () => abort.abort();
  }, [api, initial.id, shared]);
  useEffect(() => {
    const abort = new AbortController(); setMarkdown(null);
    if (shared || !selected) return;
    void api.get<MarkdownPage>(`/v1/files/${encodeURIComponent(file.id)}/markdown${query({ version_id: selected, artifact_set_id: selected === initialVersion ? initialArtifact : undefined })}`, abort.signal).then(setMarkdown).catch(e => { if (!abort.signal.aborted) setError(e); });
    return () => abort.abort();
  }, [api, file.id, selected, shared]);
  async function readMore() {
    if (!markdown?.next_cursor) return;
    setBusy(true);
    try {
      const next = await api.get<MarkdownPage>(`/v1/files/${encodeURIComponent(file.id)}/markdown${query({ version_id: selected, artifact_set_id: markdown.artifact_set_id, cursor: markdown.next_cursor })}`);
      setMarkdown(old => old ? { ...next, content: (old.content ?? old.markdown ?? '') + (next.content ?? next.markdown ?? '') } : next);
    } catch (e) { setError(e); } finally { setBusy(false); }
  }
  async function original() {
    try { await downloadFile(api, `/v1/files/${encodeURIComponent(file.id)}/content${query({ version_id: shared ? sharedVersion : selected })}`, file.name, versions.find(v => (v.version_id ?? v.id) === selected)?.size_bytes ?? file.size_bytes, setDownload); }
    catch (e) { if (e instanceof Error && e.name !== 'AbortError') void message.error(e.message); }
  }
  async function loadPreview() {
    setPreviewError(undefined);
    try { const result = await api.get<{ pages: { url: string; page?: number }[]; version_id?: string }>(`/v1/preview/${encodeURIComponent(file.id)}`); if (sharedVersion && result.version_id !== sharedVersion) throw new APIError(403, { code: 'ACCESS_DENIED' }); setPreview(result); }
    catch (e) { setPreviewError(e); }
  }
  async function convert() {
    if (!selected) return;
    setBusy(true); setError(undefined);
    try { setConversionJob(await api.post(`/v1/files/${encodeURIComponent(file.id)}/conversions`, { version_id: selected, idempotency_key: crypto.randomUUID() })); }
    catch (e) { setError(e); } finally { setBusy(false); }
  }
  async function feedback(values: { content: string; category: string }) {
    setBusy(true);
    try { await api.post('/v1/feedback', { file_id: file.id, version_id: selected ?? file.current_version_id, artifact_set_id: markdown?.artifact_set_id, category: values.category, body: values.content }); feedbackForm.resetFields(); void message.success('匿名反馈已提交'); }
    catch (e) { setError(e); } finally { setBusy(false); }
  }
  return <Drawer open title={file.name} size={720} onClose={onClose} extra={<Button icon={<DownloadOutlined />} onClick={() => void original()}>下载原件</Button>}>
    <Space orientation="vertical" className="full-width" size={16}>
      <Descriptions size="small" column={1} items={[{ key: 'path', label: '当前位置', children: file.path }, { key: 'size', label: '原件大小', children: bytes(file.size_bytes) }, { key: 'updated', label: '最后更新', children: date(file.updated_at) }, { key: 'id', label: '稳定文件 ID', children: <Typography.Text copyable>{file.id}</Typography.Text> }, { key: 'version', label: '返回源版本', children: markdown?.version_id ?? selected ?? file.current_version_id ?? '—' }]} />
      {file.conversion && <Status value={file.conversion.status} />}
      <CompletenessNotice value={file.conversion} />
      <ErrorNotice error={error} />
      {download && <Alert type="info" title={download} />}
      <>{conversionJob && <Alert type="info" title="转换任务已受理，原件仍可正常下载" description={`任务：${conversionJob.id} · ${conversionJob.state ?? conversionJob.status ?? 'queued'}。请稍后刷新文件列表查看处理结果。`} />}</>
      <Space wrap>{canConvert && !shared && <Button icon={<ReloadOutlined />} loading={busy} onClick={() => void convert()}>重新转换此版本</Button>}{canShare && !shared && <Button icon={<ShareAltOutlined />} onClick={() => onShare(file, selected !== file.current_version_id ? selected : undefined)}>请求分享{selected !== file.current_version_id ? '此历史版本' : ''}</Button>}</Space>
      <Tabs onChange={key => { if (key === 'preview' && !preview) void loadPreview(); }} items={[
        ...(!shared ? [{ key: 'markdown', label: '转换正文', children: <Space orientation="vertical" className="full-width" size={16}><CompletenessNotice value={markdown?.completeness} />{markdown?.version_id && markdown.version_id !== selected && <Alert type="warning" title="显示的是旧的可用转换版本，不是所选版本" description={`实际返回源版本：${markdown.version_id}。新版本可能仍在处理中。`} />}{markdown ? <><Typography.Text type="secondary">固定产物代次：{markdown.artifact_set_id}</Typography.Text><SafeMarkdown api={api} content={markdown.content ?? markdown.markdown ?? ''} />{markdown.next_cursor && <Button loading={busy} onClick={() => void readMore()}>继续读取正文</Button>}</> : <Empty description="此版本尚无可读取的转换正文。原件仍可下载。" image={Empty.PRESENTED_IMAGE_SIMPLE} />}</Space> }] : []),
        { key: 'preview', label: '原件预览', children: <Space orientation="vertical" className="full-width"><Alert type="info" title="预览由受控服务生成，不执行原件中的脚本或宏。" /><ErrorNotice error={previewError} retry={() => void loadPreview()} />{preview?.version_id && <Typography.Text type="secondary">预览源版本：{preview.version_id}</Typography.Text>}{preview?.version_id && preview.version_id !== selected && <Alert type="info" title="这里预览的是当前原件，不是所选历史版本。历史原件可通过下载按钮读取。" />}{preview?.pages?.map((page, i) => <ResourceImage key={page.url} api={api} src={page.url} alt={`第 ${page.page ?? i + 1} 页`} />)}{preview && !preview.pages?.length && <Empty description="当前格式没有可用的派生预览" />}{!preview && !previewError && <Button onClick={() => void loadPreview()}>加载受控预览</Button>}</Space> },
        ...(!shared ? [{ key: 'versions', label: '历史版本', children: <><Alert type="info" title="历史原件与引用长期保留。选择旧版本不会覆盖当前内容。" /><Timeline className="version-timeline" items={versions.map(v => ({ key: v.id, children: <Space orientation="vertical" size={4}><Button type="link" onClick={() => { setSelected(v.version_id ?? v.id); setError(undefined); }}>v{v.version_seq} · {date(v.created_at)}{(v.version_id ?? v.id) === selected ? ' · 当前选择' : ''}</Button><span>{bytes(v.size_bytes)} · {v.version_id ?? v.id}</span><Status value={v.conversion_status} /></Space> }))} />{versionCursor && <Button onClick={() => void api.get<Page<Version>>(`/v1/files/${encodeURIComponent(file.id)}/versions${query({ cursor: versionCursor })}`).then(data => { setVersions(old => [...old, ...data.items]); setVersionCursor(data.next_cursor ?? null); }).catch(setError)}>更多历史版本</Button>}</> }] : []),
        ...(canFeedback ? [{ key: 'feedback', label: '匿名反馈', children: <><Alert type="info" showIcon title="只用于后台排查，不进入搜索或 Agent 正文" description="请勿填写姓名、联系方式或其他身份信息。提交后不会保存你的身份或来源。" /><Form form={feedbackForm} layout="vertical" onFinish={feedback} className="form-gap"><Form.Item name="category" label="问题类型" initialValue="text"><Select options={[{ value: 'text', label: '文字转换错误' }, { value: 'missing_page', label: '缺失页面' }, { value: 'description', label: '图片或图表描述' }, { value: 'format', label: '格式问题' }, { value: 'other', label: '其他' }]} /></Form.Item><Form.Item name="content" label="问题描述" rules={[{ required: true, message: '请描述问题' }, { max: 4000 }]}><Input.TextArea rows={5} maxLength={4000} showCount placeholder="例如：第 12 页表格缺失。请勿填写身份信息。" /></Form.Item><Button type="primary" htmlType="submit" loading={busy}>提交匿名反馈</Button></Form></> }] : []),
      ]} />
    </Space>
  </Drawer>;
}
