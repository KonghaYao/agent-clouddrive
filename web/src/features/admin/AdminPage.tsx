import { Alert, Button, Input, Space, Table, Tabs } from 'antd';
import { useState } from 'react';
import type { API } from '../../shared/api';
import type { AdminJob, Feedback } from '../../shared/types';
import { usePage } from '../../shared/hooks';
import { ErrorNotice, Status } from '../../shared/components';
import { date } from '../../shared/security';
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
