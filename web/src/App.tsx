import { Alert, App as AntApp, Avatar, Breadcrumb, Button, Card, Checkbox, Dropdown, Empty, Form, Input, Layout, Menu, Modal, Progress, Space, Spin, Table, Tag, Tooltip, Typography } from 'antd';
import { ApiOutlined, CloudOutlined, DeleteOutlined, EllipsisOutlined, FileOutlined, FileTextOutlined, FolderAddOutlined, FolderOutlined, HomeOutlined, LogoutOutlined, ReloadOutlined, SafetyCertificateOutlined, SearchOutlined, SettingOutlined, ShareAltOutlined, TeamOutlined, UploadOutlined } from '@ant-design/icons';
import { lazy, Suspense, useEffect, useState, type Key } from 'react';
import { useAuth } from './auth';
import { APIError, errorText, query } from './api';
import { SCOPES, type BatchResult, type DriveFile, type Operation, type Scope, type SharedFile, type SearchHit } from './types';
import { bytes, date, validName } from './security';
import { usePage } from './hooks';
import { ErrorNotice, OperationNotice, Status } from './components';
import { DirectoryPicker } from './DirectoryPicker';
const UploadPanel = lazy(() => import('./upload').then(module => ({ default: module.UploadPanel })));
const Details = lazy(() => import('./Details').then(module => ({ default: module.Details })));
import { ApproveShareModal, ShareRequestModal, SharesPage } from './Shares';
import { AdminPage, SearchPage } from './Pages';
type Section = 'files' | 'search' | 'shared' | 'shares' | 'trash' | 'admin';
type Action = { type: 'create' | 'rename' | 'move' | 'restore'; file?: DriveFile };
const titles: Record<Section, [string, string]> = { files: ['我的文件', '原件安全保存，知识随时可用'], search: ['搜索个人空间', '从内容出发，回溯每一个固定源版本'], shared: ['分享给我', '独立的只读空间，不对 Agent 开放'], shares: ['分享管理', '每一次开放，都由你确认'], trash: ['回收站', '隐藏不等于销毁，原件与历史长期保留'], admin: ['后台处理', '观察处理状态，定位转换问题'] };
export function Login() {
  const auth = useAuth(), { message } = AntApp.useApp();
  const [scopes, setScopes] = useState<Scope[]>(SCOPES.map(s => s.value)), [busy, setBusy] = useState(false);
  return <div className="login-page"><section className="login-story"><div className="brand"><div className="brand-icon"><CloudOutlined /></div><span>Agent CloudDrive</span></div><div className="login-copy"><div className="eyebrow">YOUR FILES. YOUR CONTROL.</div><h1>文件有归处，<br />知识可追溯。</h1><p>保存原件、检索正文，让 Agent 在你授权的范围内工作。每个版本、每次分享，都清晰可控。</p><Space orientation="vertical" size={18}><span><SafetyCertificateOutlined /> 默认私有，按操作授权</span><span><FileTextOutlined /> 原件、转换与引用一一对应</span><span><ApiOutlined /> 为 Agent 准备的个人知识空间</span></Space></div><div className="login-footer">关键词检索 · 固定版本引用 · 长期历史保留</div></section><section className="login-form"><Card className="auth-card" variant="borderless"><Typography.Title level={2}>连接你的个人空间</Typography.Title><Typography.Paragraph type="secondary">通过外部身份服务安全登录。授权始终覆盖你的整个自有空间，你可以缩小允许的操作。</Typography.Paragraph>{auth.error && <Alert className="section-alert" type="error" title={auth.error} />}{!auth.configured && <Alert className="section-alert" type="warning" title="尚未配置身份服务" description="部署时设置 /config.json 中的 oidc_issuer、oidc_client_id 和 oidc_audience，或对应的 Vite 环境变量。不会创建演示账号或虚假文件。" />}<div className="scope-heading"><strong>允许的操作</strong><Button type="link" size="small" onClick={() => setScopes(scopes.length === SCOPES.length ? [] : SCOPES.map(s => s.value))}>{scopes.length === SCOPES.length ? '取消全选' : '全部选择'}</Button></div><Checkbox.Group value={scopes} onChange={values => setScopes(values as Scope[])} className="scope-list">{SCOPES.map(scope => <Checkbox value={scope.value} key={scope.value}><span>{scope.label}</span><small>{scope.description}</small></Checkbox>)}</Checkbox.Group><Button className="sign-in" type="primary" size="large" block disabled={!auth.configured || !scopes.length} loading={busy} onClick={() => { setBusy(true); void auth.login(scopes).catch(() => { setBusy(false); void message.error('无法打开身份服务，请稍后重试。'); }); }}>继续安全登录</Button><Typography.Paragraph className="auth-note" type="secondary">分享请求不等于分享批准。即使选择全部权限，Agent 也无法代替你确认分享。</Typography.Paragraph></Card></section></div>;
}
export default function App() {
  const auth = useAuth();
  if (auth.loading || auth.recovering) return <div className="loading-screen"><Spin size="large" /><p>{auth.recovering ? '正在恢复会话…' : '正在建立安全连接…'}</p></div>;
  if (!auth.user || !auth.me || !auth.api) return <Login />;
  return <Drive />;
}
function Drive() {
  const { api: providedAPI, me: providedMe, logout, refreshMe, startBackgroundAccess } = useAuth(), api = providedAPI!, me = providedMe!;
  const { message, modal } = AntApp.useApp();
  const [section, setSection] = useState<Section>('files'), [path, setPath] = useState<DriveFile[]>([]), [revision, setRevision] = useState(0);
  const [selected, setSelected] = useState<Key[]>([]), [detail, setDetail] = useState<{ file: DriveFile; shared: boolean; version?: string; artifact?: string } | null>(null);
  const [action, setAction] = useState<Action | null>(null), [target, setTarget] = useState<DriveFile | null>(null), [actionError, setActionError] = useState<unknown>(), [acting, setActing] = useState(false);
  const [upload, setUpload] = useState<{ overwrite?: DriveFile } | null>(null), [share, setShare] = useState<{ file: DriveFile; version?: string } | null>(null), [approval, setApproval] = useState<string | null>(null);
  const [operation, setOperation] = useState<Operation | null>(null), [batch, setBatch] = useState<BatchResult | null>(null), [batchRequest, setBatchRequest] = useState<Record<string, unknown>[]>([]), [globalError, setGlobalError] = useState<unknown>();
  const [form] = Form.useForm();
  const [delegation, setDelegation] = useState<boolean | null>(null), [delegating, setDelegating] = useState(false);
  useEffect(() => { const abort = new AbortController(); void api.get<{ enabled: boolean }>('/v1/delegation/status', abort.signal).then(value => setDelegation(value.enabled)).catch(() => {}); return () => abort.abort(); }, [api]);
  const parent = path.at(-1) ?? null;
  const endpoint = section === 'trash' ? '/v1/trash' : section === 'shared' ? '/v1/shared-with-me' : '/v1/files' + query({ parent_id: parent?.id });
  const listing = usePage<SharedFile>(api, endpoint, revision, ['files', 'trash', 'shared'].includes(section));
  const has = (scope: Scope) => me.scopes.includes(scope);
  const refresh = () => { setRevision(v => v + 1); void refreshMe().catch(setGlobalError); };
  useEffect(() => { setSelected([]); }, [section, parent?.id]);
  useEffect(() => {
    const acceptHash = () => { const match = /^#\/share-requests\/([^/?#]+)$/.exec(location.hash); const requested = new URLSearchParams(location.search).get('confirm_share'); if (match) setApproval(decodeURIComponent(match[1])); else if (requested) setApproval(requested); };
    acceptHash(); window.addEventListener('hashchange', acceptHash); return () => window.removeEventListener('hashchange', acceptHash);
  }, []);
  function beginAction(next: Action) { setActionError(undefined); setTarget(null); form.resetFields(); form.setFieldsValue({ name: next.file?.name ?? '' }); setAction(next); }
  async function submitAction(values: { name: string }) {
    if (!action) return;
    setActing(true); setActionError(undefined);
    try {
      const file = action.file;
      let result: Operation | DriveFile;
      if (action.type === 'create') result = await api.post('/v1/directories', { parent_id: parent?.id ?? null, name: values.name, expected_parent_revision: parent?.metadata_revision, idempotency_key: crypto.randomUUID() });
      else if (action.type === 'restore') result = await api.post(`/v1/trash/${encodeURIComponent(file!.trash_id ?? file!.id)}/restore`, { parent_id: target?.id ?? null, name: values.name, expected_metadata_revision: file!.metadata_revision, idempotency_key: crypto.randomUUID() });
      else result = await api.patch(`/v1/files/${encodeURIComponent(file!.id)}/location`, { parent_id: action.type === 'rename' ? file!.parent_id : target?.id ?? null, name: values.name, expected_metadata_revision: file!.metadata_revision, idempotency_key: crypto.randomUUID() });
      if ('operation_id' in result || 'state' in result || 'status' in result) setOperation(result as Operation);
      else refresh();
      setAction(null);
    } catch (e) { setActionError(e); } finally { setActing(false); }
  }
  function remove(file: DriveFile) {
    modal.confirm({ title: `将“${file.name}”移入回收站？`, content: '文件将退出列表和搜索，所有分享立即失效。原件和历史长期保留，配额不会释放。', okText: '移入回收站', okButtonProps: { danger: true }, onOk: async () => {
      try { const result = await api.delete<Operation>(`/v1/files/${encodeURIComponent(file.id)}`, { expected_metadata_revision: file.metadata_revision, idempotency_key: crypto.randomUUID() }); setOperation(result); refresh(); }
      catch (e) { setGlobalError(e); throw e; }
    } });
  }
  async function submitBatch(items: Record<string, unknown>[]) {
    try { const result = await api.post<BatchResult>('/v1/batches', { idempotency_key: crypto.randomUUID(), items }); setBatch(result); setBatchRequest(items); refresh(); }
    catch (e) { setGlobalError(e); }
  }
  function removeSelected() {
    modal.confirm({ title: `将选中的 ${selected.length} 项移入回收站？`, content: '每个项目独立执行。冲突项不会阻止其他项目，分享会被停用，历史继续保留。', okText: '确认移入', okButtonProps: { danger: true }, onOk: async () => {
      const items = listing.items.filter(f => selected.includes(f.id)).map(file => ({ item_id: file.id, command: 'delete', input: { file_id: file.id, expected_metadata_revision: file.metadata_revision, idempotency_key: crypto.randomUUID() }, depends_on: [] }));
      await submitBatch(items); setSelected([]);
    } });
  }
  async function openFile(file: DriveFile, shared = false) { if (file.kind === 'directory') { setPath(old => [...old, file]); setSelected([]); } else setDetail({ file, shared }); }
  async function openID(hit: SearchHit) { try { setDetail({ file: await api.get(`/v1/files/${encodeURIComponent(hit.file_id)}`), shared: false, version: hit.returned_version_id, artifact: hit.artifact_set_id }); } catch (e) { setGlobalError(e); } }
  const menu = (file: DriveFile) => ({ items: [
    { key: 'rename', label: '重命名', disabled: !has('drive.write') }, { key: 'move', label: '移动到…', disabled: !has('drive.write') },
    ...(file.kind === 'file' ? [{ key: 'overwrite', label: '上传新版本', disabled: !has('drive.write') }, { key: 'share', label: '请求分享', disabled: !has('drive.share.request') }] : []),
    { key: 'delete', label: '移入回收站', danger: true, disabled: !has('drive.delete') },
  ], onClick: ({ key }: { key: string }) => { if (key === 'delete') remove(file); else if (key === 'overwrite') setUpload({ overwrite: file }); else if (key === 'share') setShare({ file }); else beginAction({ type: key as 'move' | 'rename', file }); } });
  const used = me.quota.used_bytes + me.quota.reserved_bytes, percent = me.quota.limit_bytes ? Math.min(100, Math.round(used / me.quota.limit_bytes * 100)) : 0;
  return <><Layout className="drive-layout"><Layout.Sider className="sidebar" width={244} breakpoint="lg" collapsedWidth={72} theme="light"><div className="brand"><div className="brand-icon"><CloudOutlined /></div><span>CloudDrive<small>个人知识空间</small></span></div><div className="workspace-label">WORKSPACE</div><Menu mode="inline" selectedKeys={[section]} onClick={({ key }) => { setSection(key as Section); setGlobalError(undefined); }} items={[{ key: 'files', icon: <FolderOutlined />, label: '我的文件' }, { key: 'search', icon: <SearchOutlined />, label: '搜索内容' }, { key: 'shared', icon: <TeamOutlined />, label: '分享给我' }, { key: 'shares', icon: <ShareAltOutlined />, label: '分享管理' }, { type: 'divider' }, { key: 'trash', icon: <DeleteOutlined />, label: '回收站' }, ...(me.permissions?.admin ? [{ key: 'admin', icon: <SettingOutlined />, label: '后台处理' }] : [])]} /><div className="sidebar-bottom"><div className="privacy-chip"><SafetyCertificateOutlined /> 私有空间 · 默认仅你可见</div><div className="quota"><Space className="quota-title"><CloudOutlined /><strong>个人逻辑配额</strong></Space><Progress percent={percent} showInfo={false} size="small" strokeColor="#4f7be8" /><div>{bytes(me.quota.used_bytes)} / {bytes(me.quota.limit_bytes)}</div>{me.quota.reserved_bytes > 0 && <small>上传预留 {bytes(me.quota.reserved_bytes)}</small>}<small>仅计算最新原件，包含回收站</small></div></div></Layout.Sider><Layout><Layout.Header className="topbar"><Space><div className="topbar-dot" /><span>Agent CloudDrive</span><Tag bordered={false}>个人空间</Tag></Space><Space size={14}><Tooltip title="OAuth 只缩小操作，不改变自有文件范围"><Tag icon={<SafetyCertificateOutlined />} color="blue">{me.scopes.length} 项操作授权</Tag></Tooltip><Avatar size={34} style={{ backgroundColor: '#e9efff', color: '#2c62e5' }}>{(me.display_name ?? '我').slice(0, 1)}</Avatar><span>{me.display_name ?? '个人账号'}</span><Button type="text" icon={<LogoutOutlined />} onClick={() => void logout().catch(() => message.error('退出失败，请重试'))} aria-label="退出登录" /></Space></Layout.Header><Layout.Content className="content"><header className="page-heading"><div><div className="eyebrow">YOUR PERSONAL DRIVE</div><h1>{titles[section][0]}</h1><p>{titles[section][1]}</p></div><Space><Button icon={<ReloadOutlined />} onClick={refresh}>刷新</Button>{section === 'files' && <><Button icon={<FolderAddOutlined />} disabled={!has('drive.write')} onClick={() => beginAction({ type: 'create' })}>新建文件夹</Button><Button type="primary" icon={<UploadOutlined />} disabled={!has('drive.write')} onClick={() => setUpload({})}>上传文件</Button></>}</Space></header>
    <Space orientation="vertical" size={16} className="full-width">{delegation === false && <Alert type="info" showIcon title="启用离线后台转换" description="允许受控后台服务在你关闭网页后继续转换固定源版本。需要在身份服务中单独确认离线访问；浏览器不会保存刷新凭据。原件上传不受影响。" action={<Button loading={delegating} onClick={() => { setDelegating(true); void startBackgroundAccess().catch(e => { setGlobalError(e); setDelegating(false); }); }}>前往授权</Button>} />}<ErrorNotice error={globalError} retry={() => { setGlobalError(undefined); refresh(); }} /><OperationNotice api={api} operation={operation} onComplete={refresh} />
    {batch && <Card size="small" title="批量操作逐项结果" extra={<Button type="text" onClick={() => setBatch(null)}>收起</Button>}><Space orientation="vertical" className="full-width">{batch.items.map(item => <Space key={item.item_id}><Typography.Text>{item.item_id}</Typography.Text><Tag color={['committed', 'completed', 'succeeded'].includes(item.status) ? 'green' : 'orange'}>{item.status}</Tag>{item.error && <span>{item.error.code}</span>}{item.operation_id && <Button size="small" onClick={() => setOperation({ id: item.operation_id!, status: item.status })}>查看操作</Button>}</Space>)}{batch.items.some(item => ['failed', 'failed_confirmed', 'blocked_dependency'].includes(item.status)) && <Button onClick={() => void submitBatch(batchRequest.filter(item => batch.items.some(result => result.item_id === item.item_id && ['failed', 'failed_confirmed', 'blocked_dependency'].includes(result.status))))}>仅重试失败项目</Button>}</Space></Card>}
    {['files', 'trash', 'shared'].includes(section) && <Card className="file-card" variant="borderless"><div className="file-toolbar">{section === 'files' ? <Breadcrumb items={[{ title: <a onClick={() => setPath([])}><HomeOutlined /> 个人空间</a> }, ...path.map((folder, index) => ({ title: <a onClick={() => setPath(path.slice(0, index + 1))}>{folder.name}</a> }))]} /> : <Typography.Text type="secondary">{section === 'trash' ? '文件恢复时可指定新的目录与名称，同名冲突不会覆盖。' : '仅有只读与下载权限，不能编辑或加入 Agent 搜索。'}</Typography.Text>}{selected.length > 0 ? <Space><span>已选 {selected.length} 项</span><Button danger size="small" onClick={removeSelected} disabled={!has('drive.delete')}>移入回收站</Button></Space> : <Tag bordered={false}>{listing.items.length} 项{listing.next ? '，还有更多' : ''}</Tag>}</div><ErrorNotice error={listing.error} retry={listing.reload} /><Table<SharedFile> rowKey="id" loading={listing.loading} dataSource={listing.items} pagination={false} rowSelection={section === 'files' ? { selectedRowKeys: selected, onChange: setSelected } : undefined} scroll={{ x: 680 }} locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={section === 'files' ? <Space orientation="vertical"><strong>这个目录还没有文件</strong><span>上传原件后，即可等待转换并搜索内容。</span>{has('drive.write') && <Button type="primary" onClick={() => setUpload({})}>上传第一个文件</Button>}</Space> : section === 'trash' ? '回收站为空' : '还没有收到文件分享'} /> }} columns={[
      { title: section === 'trash' ? '文件／原始位置' : '文件名称', key: 'name', render: (_, file) => <Space size={12}><span className={file.kind === 'directory' ? 'file-icon folder' : 'file-icon'}>{file.kind === 'directory' ? <FolderOutlined /> : <FileOutlined />}</span><Space orientation="vertical" size={0}>{section !== 'trash' ? <Button className="file-name" type="link" onClick={() => void openFile(file, section === 'shared')}>{file.name}</Button> : <strong>{file.name}</strong>}<Typography.Text type="secondary" className="file-secondary">{section === 'trash' ? file.original_path ?? file.path : file.kind === 'directory' ? '文件夹' : file.current_version_id ? `版本 ${file.current_version_id.slice(0, 8)}` : '原件'}</Typography.Text></Space></Space> },
      { title: '大小', dataIndex: 'size_bytes', width: 110, render: (size, file) => file.kind === 'directory' ? '—' : bytes(size) },
      { title: section === 'trash' ? '删除时间' : '更新时间', key: 'date', width: 185, render: (_, file) => date(section === 'trash' ? file.deleted_at : file.updated_at) },
      { title: '处理状态', key: 'conversion', width: 175, render: (_, file) => file.kind === 'directory' ? '—' : <Status value={file.conversion?.status} /> },
      { title: '', key: 'actions', width: 70, render: (_, file) => section === 'trash' ? <Button type="link" disabled={!has('drive.restore')} onClick={() => beginAction({ type: 'restore', file })}>恢复</Button> : section === 'shared' ? <Button type="link" onClick={() => void openFile(file, true)}>查看</Button> : <Dropdown menu={menu(file)} trigger={['click']}><Button type="text" icon={<EllipsisOutlined />} aria-label={`${file.name} 的操作`} /></Dropdown> },
    ]} />{listing.next && <div className="load-more"><Button loading={listing.loading} onClick={() => void listing.more()}>加载更多</Button></div>}</Card>}
    {section === 'search' && <Card variant="borderless"><SearchPage api={api} onOpen={hit => void openID(hit)} /></Card>}
    {section === 'shares' && <Card variant="borderless"><SharesPage api={api} revision={revision} onApprove={setApproval} /></Card>}
    {section === 'admin' && me.permissions?.admin && <Card variant="borderless"><AdminPage api={api} revision={revision} /></Card>}
    </Space><footer className="content-footer"><SafetyCertificateOutlined /> 原件优先保存 · 历史版本长期保留 · 分享须明确确认</footer>
    </Layout.Content></Layout></Layout>
    {detail && <Suspense fallback={<Spin />}><Details key={`${detail.file.id}:${detail.shared}`} api={api} file={detail.file} initialVersion={detail.version} initialArtifact={detail.artifact} shared={detail.shared} onClose={() => setDetail(null)} canConvert={has('drive.convert')} canShare={has('drive.share.request')} canFeedback={has('drive.feedback')} onShare={(file, version) => setShare({ file, version })} /></Suspense>}
    {action && <Modal open title={{ create: '新建文件夹', rename: '重命名', move: '移动文件', restore: '恢复文件' }[action.type]} onCancel={() => setAction(null)} onOk={() => form.submit()} confirmLoading={acting} okText="确认" destroyOnHidden><ErrorNotice error={actionError} /><Form className="form-gap" form={form} layout="vertical" onFinish={submitAction}><Form.Item name="name" label="名称" rules={[{ required: true, message: '请输入名称' }, { validator: (_, value) => validName(value ?? '') ? Promise.resolve() : Promise.reject(new Error('名称不能为空、包含斜线或控制字符，且不能超过 255 字节')) }]}><Input autoFocus maxLength={255} /></Form.Item>{['move', 'restore'].includes(action.type) && <Form.Item label="选择目标目录"><DirectoryPicker api={api} onChange={setTarget} excludedId={action.file?.id} /></Form.Item>}{action.type === 'restore' && <Alert type="info" title="历史版本保持不变，原有分享不会自动恢复。" />}</Form></Modal>}
    {upload && <Modal open title={upload.overwrite ? '上传新版本' : '上传文件'} width={780} footer={null} onCancel={() => modal.confirm({ title: '关闭上传面板？', content: '关闭会结束当前页面的传输会话，重新上传可能需要等待服务器释放预留。若只需暂停，请使用上传面板的暂停按钮并保持面板打开；已保存的原件不会丢失。', okText: '关闭面板', onOk: () => setUpload(null) })}><Suspense fallback={<Spin />}><UploadPanel api={api} parent={parent} overwrite={upload.overwrite} onSaved={refresh} /></Suspense></Modal>}
    {share && <ShareRequestModal api={api} file={share.file} version={share.version} onClose={() => setShare(null)} onCreated={request => { setApproval(request.request_id ?? request.id); refresh(); }} />}
    {approval && <ApproveShareModal api={api} requestId={approval} onClose={() => { setApproval(null); if (location.hash.startsWith('#/share-requests/') || new URLSearchParams(location.search).has('confirm_share')) history.replaceState(null, '', location.pathname); }} onApproved={refresh} />}
  </>;
}
