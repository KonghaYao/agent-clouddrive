import { App as AntApp, ConfigProvider } from 'antd';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';
import { API } from '../api';
import { ApproveShareModal } from '../Shares';
import { SafeMarkdown } from '../Details';
import { DirectoryPicker } from '../DirectoryPicker';
import { usePage } from '../hooks';
const Wrapper = ({ children }: { children: ReactNode }) => <ConfigProvider theme={{ token: { motion: false } }}><AntApp>{children}</AntApp></ConfigProvider>;
const request = { id: 'request1', file_id: 'file1', file_name: '合同.pdf', audience_type: 'user', audience_id: 'exact-user-id', rights: ['read', 'download'], version_policy: 'latest', expires_at: '2099-12-31T00:00:00Z', request_expires_at: '2099-10-24T00:00:00Z', status: 'pending' };
function fakeAPI(get: ReturnType<typeof vi.fn>, post = vi.fn()) { return { get, post } as unknown as API; }
describe('interactive sharing confirmation', () => {
  it('does not approve before explicit checkbox and click, sends no mutable grant fields', async () => {
    const get = vi.fn().mockResolvedValue(request), post = vi.fn().mockResolvedValue({ id: 'grant1' }), close = vi.fn(), approved = vi.fn();
    render(<ApproveShareModal api={fakeAPI(get, post)} requestId="request1" onClose={close} onApproved={approved} />, { wrapper: Wrapper });
    await screen.findByText('exact-user-id', { exact: false });
    const button = screen.getByRole('button', { name: '确认并开放访问' }); expect(button).toBeDisabled(); expect(post).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('checkbox')); expect(button).toBeEnabled(); fireEvent.click(button);
    await waitFor(() => expect(post).toHaveBeenCalledWith('/v1/share-requests/request1/approve', {}));
    expect(approved).toHaveBeenCalledOnce(); expect(close).toHaveBeenCalledOnce();
  });
  it('refuses expired requests even after the user tries to check confirmation', async () => {
    const post = vi.fn(); render(<ApproveShareModal api={fakeAPI(vi.fn().mockResolvedValue({ ...request, request_expires_at: '2000-01-01T00:00:00Z' }), post)} requestId="expired" onClose={vi.fn()} onApproved={vi.fn()} />, { wrapper: Wrapper });
    await screen.findByText('此确认请求已过期，请新建请求。'); expect(screen.getByRole('button', { name: '确认并开放访问' })).toBeDisabled(); expect(post).not.toHaveBeenCalled();
  });
  it('cancel does not send approval', async () => { const close = vi.fn(), post = vi.fn(); render(<ApproveShareModal api={fakeAPI(vi.fn().mockResolvedValue(request), post)} requestId="r" onClose={close} onApproved={vi.fn()} />, { wrapper: Wrapper }); await screen.findByText('合同.pdf'); fireEvent.click(screen.getByRole('button', { name: 'Cancel' })); expect(close).toHaveBeenCalled(); expect(post).not.toHaveBeenCalled(); });
});
describe('untrusted rendered document', () => {
  it('renders safe Markdown and blocks executable HTML, links and remote tracking images', () => {
    const api = fakeAPI(vi.fn());
    const { container } = render(<SafeMarkdown api={api} content={'# Report\n<script>window.pwned=true</script>\n[bad](javascript:alert%281%29)\n![tracking](https://external.example/pixel.png)\n[good](https://example.com)\n\n**bold**'} />);
    expect(screen.getByRole('heading', { name: 'Report' })).toBeInTheDocument(); expect(container.querySelector('script')).toBeNull(); expect(container.querySelector('img')).toBeNull(); expect(container.querySelector('a[href^="javascript"]')).toBeNull();
    expect(screen.getByRole('link', { name: 'good' })).toHaveAttribute('rel', 'noopener noreferrer'); expect(screen.getByText('bold')).toBeInTheDocument();
  });
});
describe('directory selection and asynchronous navigation', () => {
  it('navigates real directory metadata and reports chosen stable ID', async () => {
    const folder = { id: 'folder1', name: '项目 A', kind: 'directory', path: '/项目 A', metadata_revision: 3 };
    const get = vi.fn().mockResolvedValueOnce({ items: [folder, { id: 'document', name: 'not-folder', kind: 'file' }] }).mockResolvedValue({ items: [] }), chosen = vi.fn();
    render(<DirectoryPicker api={fakeAPI(get)} onChange={chosen} />, { wrapper: Wrapper });
    fireEvent.click(await screen.findByRole('button', { name: /项目 A/ })); expect(chosen).toHaveBeenCalledWith(folder); await waitFor(() => expect(get).toHaveBeenCalledWith('/v1/files?parent_id=folder1', expect.anything()));
    expect(screen.queryByText('not-folder')).toBeNull(); fireEvent.click(screen.getByText('个人空间')); expect(chosen).toHaveBeenLastCalledWith(null);
  });
  it('ignores a slower previous folder response after navigation', async () => {
    let resolveA!: (value: unknown) => void;
    const a = new Promise(resolve => { resolveA = resolve; });
    const get = vi.fn().mockImplementation((path: string) => path === '/v1/files?parent_id=a' ? a : Promise.resolve({ items: [{ id: 'b', name: 'new directory' }] }));
    const api = fakeAPI(get);
    function Harness({ path }: { path: string }) { const page = usePage<{ id: string; name: string }>(api, path); return <>{page.items.map(x => <span key={x.id}>{x.name}</span>)}</>; }
    const { rerender } = render(<Harness path="/v1/files?parent_id=a" />); rerender(<Harness path="/v1/files?parent_id=b" />); await screen.findByText('new directory');
    await act(async () => { resolveA({ items: [{ id: 'a', name: 'stale directory' }] }); await a; }); expect(screen.queryByText('stale directory')).toBeNull();
  });
});
