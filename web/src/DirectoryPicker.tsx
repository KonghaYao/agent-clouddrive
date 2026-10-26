import { Breadcrumb, Button, Empty, List, Space, Typography } from 'antd';
import { FolderOutlined, HomeOutlined, RightOutlined } from '@ant-design/icons';
import { useState } from 'react';
import type { API } from './api';
import { query } from './api';
import { usePage } from './hooks';
import { ErrorNotice } from './components';
import type { DriveFile } from './types';
export function DirectoryPicker({ api, onChange, excludedId }: { api: API; onChange: (folder: DriveFile | null) => void; excludedId?: string }) {
  const [path, setPath] = useState<DriveFile[]>([]);
  const current = path.at(-1) ?? null;
  const data = usePage<DriveFile>(api, '/v1/files' + query({ parent_id: current?.id }));
  const navigate = (next: DriveFile[]) => { setPath(next); onChange(next.at(-1) ?? null); };
  const directories = data.items.filter(file => file.kind === 'directory' && file.id !== excludedId);
  return <div className="directory-picker"><Breadcrumb items={[{ title: <a onClick={() => navigate([])}><HomeOutlined /> 个人空间</a> }, ...path.map((folder, index) => ({ title: <a onClick={() => navigate(path.slice(0, index + 1))}>{folder.name}</a> }))]} />
    <ErrorNotice error={data.error} retry={data.reload} />
    <List loading={data.loading} dataSource={directories} locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="这个位置没有子目录" /> }} renderItem={folder => <List.Item role="button" tabIndex={0} onKeyDown={event => { if (event.key === 'Enter' || event.key === ' ') navigate([...path, folder]); }} onClick={() => navigate([...path, folder])} className="folder-option"><Space><FolderOutlined />{folder.name}</Space><RightOutlined /></List.Item>} />
    {data.next && <Button onClick={() => void data.more()} loading={data.loading}>加载更多目录</Button>}
    <Typography.Text type="secondary">目标目录：{current?.path ?? '/'}</Typography.Text>
  </div>;
}
