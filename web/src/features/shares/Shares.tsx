import { Alert, App, Button, Checkbox, Descriptions, Empty, Form, Input, InputNumber, Modal, Select, Space, Table, Tabs, Tag, Typography } from 'antd';
import { useEffect, useState } from 'react';
import type { API } from '../../shared/api';
import { errorText } from '../../shared/api';
import type { DriveFile, Share, ShareRequest } from '../../shared/types';
import { date } from '../../shared/security';
import { usePage } from '../../shared/hooks';
import { ErrorNotice } from '../../shared/components';
export function ShareRequestModal({ api, file, version, onClose, onCreated }: { api: API; file: DriveFile; version?: string; onClose: () => void; onCreated: (request: ShareRequest) => void }) {
  const [form] = Form.useForm(), recipient = Form.useWatch('audience_type', form);
  const [busy, setBusy] = useState(false), [error, setError] = useState<unknown>();
  async function submit(values: { audience_type: 'user' | 'group' | 'link'; audience_id?: string; days: number }) {
    setBusy(true); setError(undefined);
    try {
      const result = await api.post<ShareRequest>('/v1/share-requests', { file_id: file.id, audience_type: values.audience_type, audience_id: values.audience_type === 'link' ? undefined : values.audience_id?.trim(), rights: values.audience_type === 'link' ? ['preview'] : ['read', 'download'], version_policy: version ? 'fixed' : 'latest', version_id: version, expires_at: new Date(Date.now() + values.days * 86400000).toISOString(), idempotency_key: crypto.randomUUID() });
      onCreated(result); onClose();
    } catch (e) { setError(e); } finally { setBusy(false); }
  }
  return <Modal open title="创建分享待确认请求" onCancel={onClose} onOk={() => form.submit()} okText="创建待确认请求" confirmLoading={busy}>
    <Space orientation="vertical" className="full-width" size={16}><Alert type="info" showIcon title="创建请求不会开放访问" description="每次分享都必须由你在网页中再次确认。Agent 没有批准能力。" /><ErrorNotice error={error} /><Form form={form} layout="vertical" onFinish={submit} initialValues={{ audience_type: 'user', days: 7 }}>
      <Form.Item label="文件"><Typography.Text>{file.path}</Typography.Text></Form.Item>
      <Form.Item label="版本策略"><Tag color={version ? 'orange' : 'blue'}>{version ? `固定历史版本：${version}` : '仅当前最新版，不包含历史版本'}</Tag></Form.Item>
      <Form.Item name="audience_type" label="分享给" rules={[{ required: true }]}><Select options={[{ value: 'user', label: '指定系统用户' }, { value: 'group', label: '指定系统用户组' }, { value: 'link', label: '未登录链接访问者' }]} /></Form.Item>
      {recipient !== 'link' && <Form.Item name="audience_id" label={recipient === 'group' ? '系统用户组 ID' : '系统用户 ID'} rules={[{ required: true, whitespace: true, message: '请填写精确的系统 ID，不使用显示名猜测接收者' }]}><Input placeholder="精确的系统用户／组 ID" autoComplete="off" /></Form.Item>}
      <Form.Item label="访问权限"><Typography.Text>{recipient === 'link' ? '仅受控原件预览，不提供原件下载' : '只读与原件下载，不允许编辑'}</Typography.Text></Form.Item>
      <Form.Item name="days" label="有效天数" rules={[{ required: true }]}><InputNumber min={1} max={365} precision={0} /></Form.Item>
    </Form></Space>
  </Modal>;
}
export function ApproveShareModal({ api, requestId, onClose, onApproved }: { api: API; requestId: string; onClose: () => void; onApproved: () => void }) {
  const [request, setRequest] = useState<ShareRequest | null>(null), [error, setError] = useState<unknown>();
  const [confirmed, setConfirmed] = useState(false), [busy, setBusy] = useState(false);
  const [granted, setGranted] = useState<Share | null>(null);
  useEffect(() => { const abort = new AbortController(); void api.get<ShareRequest>(`/v1/share-requests/${encodeURIComponent(requestId)}`, abort.signal).then(setRequest).catch(e => { if (!abort.signal.aborted) setError(e); }); return () => abort.abort(); }, [api, requestId]);
  async function approve() {
    if (!confirmed || !request) return;
    setBusy(true); setError(undefined);
    try { const result = await api.post<Share>(`/v1/share-requests/${encodeURIComponent(requestId)}/approve`, {}); onApproved(); if (result.public_url) setGranted(result); else onClose(); }
    catch (e) { setError(e); } finally { setBusy(false); }
  }
  const expired = !!request?.request_expires_at && new Date(request.request_expires_at).valueOf() <= Date.now();
  if (granted) {
    const url = new URL(granted.public_url!, location.origin);
    const allowed = url.origin === location.origin && url.pathname.startsWith('/s/');
    return <Modal open title="分享已开放" onCancel={onClose} onOk={onClose} okText="已保存链接" cancelButtonProps={{ style: { display: 'none' } }}><Space orientation="vertical" className="full-width" size={16}><Alert type="success" showIcon title="请现在复制分享链接" description="安全起见，服务器仅保存链接摘要，关闭后不能再次取回原始链接。可以撤销后重新确认生成新链接。" />{allowed ? <Typography.Paragraph copyable>{url.href}</Typography.Paragraph> : <Alert type="error" title="分享已创建，但服务器返回了非本站链接，已阻止显示。" />}</Space></Modal>;
  }
  return <Modal open title="确认开放文件访问" onCancel={onClose} onOk={() => void approve()} okText="确认并开放访问" okButtonProps={{ disabled: !confirmed || !request || expired }} confirmLoading={busy}>
    <Space orientation="vertical" className="full-width" size={16}><Alert type="warning" showIcon title="请核对接收对象、权限和版本" description="删除文件、停用账号或撤销分享后会失效。恢复文件不会重新开放分享。" /><ErrorNotice error={error} />
      {request && <><Descriptions bordered size="small" column={1} items={[{ key: 'file', label: '文件', children: request.file_name ?? request.file_id }, { key: 'recipient', label: '接收对象', children: request.audience_type === 'link' ? '持有链接的未登录访问者' : `${request.audience_type === 'group' ? '用户组' : '用户'}：${request.audience_id}` }, { key: 'permission', label: '权限', children: request.rights?.join('、') }, { key: 'version', label: '版本策略', children: request.version_policy === 'fixed' ? `仅历史版本 ${request.version_id}` : '仅当前最新版，不包含历史内容' }, { key: 'expire', label: '分享有效期', children: date(request.expires_at) }, { key: 'request-expire', label: '请求到期', children: date(request.request_expires_at) }]} />{expired ? <Alert type="error" title="此确认请求已过期，请新建请求。" /> : <Checkbox checked={confirmed} onChange={event => setConfirmed(event.target.checked)}>我已核对上述信息，同意按这些固定条件开放访问</Checkbox>}</>}
    </Space>
  </Modal>;
}
export function SharesPage({ api, revision, onApprove }: { api: API; revision: number; onApprove: (id: string) => void }) {
  const { modal, message } = App.useApp();
  const requests = usePage<ShareRequest>(api, '/v1/share-requests', revision), shares = usePage<Share>(api, '/v1/shares', revision);
  async function revoke(share: Share) {
    modal.confirm({ title: '撤销此分享？', content: '之后的预览、图片与下载请求将立即重新检查权限。', okText: '撤销分享', okButtonProps: { danger: true }, onOk: async () => {
      try { await api.post(`/v1/shares/${encodeURIComponent(share.id)}/revoke`); shares.reload(); void message.success('分享已撤销'); }
      catch (e) { void message.error(errorText(e)); throw e; }
    } });
  }
  function publicLink(raw?: string) {
    if (!raw) return null;
    try { const url = new URL(raw, location.origin); if (url.origin !== location.origin || !url.pathname.startsWith('/s/')) return null; return <Typography.Link href={url.href} target="_blank" rel="noopener noreferrer">打开受控预览</Typography.Link>; } catch { return null; }
  }
  const common = [{ title: '文件', dataIndex: 'file_name', render: (_: unknown, row: Share) => row.file_name ?? row.file_id }, { title: '接收对象', key: 'recipient', render: (_: unknown, row: Share) => row.audience_type === 'link' ? '持有链接的访问者' : `${row.audience_type === 'group' ? '用户组' : '用户'}：${row.audience_id}` }, { title: '版本', dataIndex: 'version_policy', render: (value: string) => value === 'fixed' ? <Tag color="orange">固定历史版本</Tag> : <Tag>仅最新版</Tag> }];
  return <><Alert className="section-alert" type="info" showIcon title="分享仅限单个文件；每次授权都需要你的明确确认。" /><Tabs items={[
    { key: 'requests', label: '待确认请求', children: <><ErrorNotice error={requests.error} retry={requests.reload} /><Table rowKey="id" loading={requests.loading} dataSource={requests.items} pagination={false} locale={{ emptyText: <Empty description="没有待确认的分享请求" /> }} columns={[...common, { title: '请求状态', dataIndex: 'status', render: value => <Tag>{value ?? 'pending'}</Tag> }, { title: '', key: 'action', render: (_, row) => <Button onClick={() => onApprove(row.request_id ?? row.id)} disabled={!!row.status && row.status !== 'pending'}>核对并确认</Button> }]} />{requests.next && <Button onClick={() => void requests.more()}>加载更多</Button>}</> },
    { key: 'shares', label: '已有分享', children: <><ErrorNotice error={shares.error} retry={shares.reload} /><Table rowKey="id" loading={shares.loading} dataSource={shares.items} pagination={false} locale={{ emptyText: <Empty description="还没有开放任何分享" /> }} columns={[...common, { title: '到期时间', dataIndex: 'expires_at', render: value => date(value) }, { title: '状态', key: 'status', render: (_, row) => <Space orientation="vertical" size={0}><Tag color={row.status === 'active' ? 'green' : 'default'}>{row.status ?? 'active'}</Tag>{row.disabled_reason && <Typography.Text type="secondary">{row.disabled_reason}</Typography.Text>}{publicLink(row.public_url)}</Space> }, { title: '', key: 'action', render: (_, row) => <Button danger disabled={!!row.status && row.status !== 'active'} onClick={() => void revoke(row)}>撤销</Button> }]} />{shares.next && <Button onClick={() => void shares.more()}>加载更多</Button>}</> },
  ]} /></>;
}
