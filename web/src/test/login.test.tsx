import { App as AntApp } from 'antd';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { SCOPES } from '../types';
const auth = vi.hoisted(() => ({ configured: true, error: '', login: vi.fn().mockResolvedValue(undefined) }));
vi.mock('../auth', () => ({ useAuth: () => auth }));
import { Login } from '../App';
describe('OAuth authorization scope selection', () => {
  it('defaults every scope selected and lets the user reduce operations before PKCE redirect', async () => {
    render(<AntApp><Login /></AntApp>);
    const checks = screen.getAllByRole('checkbox'); expect(checks).toHaveLength(7); checks.forEach(check => expect(check).toBeChecked());
    fireEvent.click(checks[2]); expect(checks[2]).not.toBeChecked(); fireEvent.click(screen.getByRole('button', { name: '继续安全登录' }));
    await waitFor(() => expect(auth.login).toHaveBeenCalledWith(SCOPES.map(s => s.value).filter(s => s !== 'drive.delete')));
  });
  it('blocks empty authorization and can restore all permissions', () => {
    render(<AntApp><Login /></AntApp>); fireEvent.click(screen.getByRole('button', { name: '取消全选' })); expect(screen.getByRole('button', { name: '继续安全登录' })).toBeDisabled(); fireEvent.click(screen.getByRole('button', { name: '全部选择' })); expect(screen.getByRole('button', { name: '继续安全登录' })).toBeEnabled();
  });
});
