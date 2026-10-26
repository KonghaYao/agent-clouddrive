import { createContext, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { InMemoryWebStorage, UserManager, WebStorageStateStore, type User } from 'oidc-client-ts';
import { API } from '../../shared/api';
import { SCOPES, type Me, type Scope } from '../../shared/types';
import {
  SILENT_IFRAME_TIMEOUT_SECONDS, claimSilentRecovery, clearRecoveryMarker, grantedBusinessScopes,
  readRecoveryMarker, recoverSession, scopesForRecovery, writeRecoveryMarker,
} from './session-recovery';
interface RuntimeConfig { oidc_issuer: string; oidc_client_id: string; oidc_audience?: string; oidc_resource?: string }
interface AuthValue { user: User | null; me: Me | null; api: API | null; loading: boolean; recovering: boolean; error: string; configured: boolean; login: (scopes: Scope[]) => Promise<void>; logout: () => Promise<void>; refreshMe: () => Promise<void>; startBackgroundAccess: () => Promise<void> }
const Context = createContext<AuthValue>(null!);
async function getConfig(): Promise<RuntimeConfig> {
  const defaults = { oidc_issuer: import.meta.env.VITE_OIDC_ISSUER ?? '', oidc_client_id: import.meta.env.VITE_OIDC_CLIENT_ID ?? 'clouddrive-web', oidc_audience: import.meta.env.VITE_OIDC_AUDIENCE ?? 'clouddrive-api' };
  try {
    const response = await fetch('/config.json', { cache: 'no-store' });
    if (response.ok && response.headers.get('content-type')?.includes('application/json')) return { ...defaults, ...await response.json() };
  } catch { /* Build-time configuration remains available. */ }
  return defaults;
}
function createUserManager(config: RuntimeConfig): UserManager {
  return new UserManager({
    authority: config.oidc_issuer, client_id: config.oidc_client_id,
    redirect_uri: `${location.origin}/auth/callback`, post_logout_redirect_uri: location.origin,
    // Reuse the registered callback for iframe prompt=none recovery. Only the
    // main.tsx branch decides whether this page mounts the app or answers the parent.
    silent_redirect_uri: `${location.origin}/auth/callback`,
    iframeNotifyParentOrigin: location.origin,
    silentRequestTimeoutInSeconds: SILENT_IFRAME_TIMEOUT_SECONDS,
    response_type: 'code', disablePKCE: false, scope: `openid profile ${SCOPES.map(s => s.value).join(' ')}`,
    automaticSilentRenew: true, loadUserInfo: false,
    userStore: new WebStorageStateStore({ store: new InMemoryWebStorage() }),
    stateStore: new WebStorageStateStore({ store: window.sessionStorage }),
    extraQueryParams: { ...(config.oidc_audience ? { audience: config.oidc_audience } : {}), ...(config.oidc_resource ? { resource: config.oidc_resource } : {}) },
  });
}
/** True only inside a same-tab iframe that is handling the silent-auth callback. */
export function isSilentCallbackPage(): boolean {
  return window.self !== window.top && location.pathname === '/auth/callback';
}
/** Answer the parent window; the parent validates state, PKCE and the response. */
export async function completeSilentCallback(): Promise<void> {
  try {
    const config = await getConfig();
    if (!config.oidc_issuer) return;
    await createUserManager(config).signinSilentCallback();
  } catch {
    // The parent owns the bounded timeout and falls back to the manual login page.
  }
}
export function AuthProvider({ children }: { children: ReactNode }) {
  const [manager, setManager] = useState<UserManager | null>(null);
  const [user, setUser] = useState<User | null>(null);
  const [me, setMe] = useState<Me | null>(null);
  const [loading, setLoading] = useState(true);
  const [recovering, setRecovering] = useState(false);
  const [error, setError] = useState('');
  const [configured, setConfigured] = useState(false);
  const loggedOut = useRef(false);
  const recoveryAbandoned = useRef(false);
  const api = useMemo(() => manager ? new API(async () => {
    let current = await manager.getUser();
    if (current?.expired) current = await manager.signinSilent();
    if (!current || current.expired) throw new Error('登录已失效');
    return current.access_token;
  }) : null, [manager]);
  useEffect(() => {
    let cancelled = false;
    let dispose = () => {};
    void (async () => {
      try {
        const config = await getConfig();
        if (!config.oidc_issuer) return;
        const instance = createUserManager(config);
        const loaded = (next: User) => { if (!cancelled && !loggedOut.current && !recoveryAbandoned.current) setUser(next); };
        const removed = () => { if (!cancelled) { setUser(null); setMe(null); } };
        instance.events.addUserLoaded(loaded);
        instance.events.addUserUnloaded(removed);
        instance.events.addAccessTokenExpired(removed);
        dispose = () => { instance.events.removeUserLoaded(loaded); instance.events.removeUserUnloaded(removed); instance.events.removeAccessTokenExpired(removed); instance.stopSilentRenew(); };
        if (cancelled) { dispose(); return; }
        setManager(instance); setConfigured(true);
        await instance.clearStaleState();
        if (location.pathname === '/auth/callback') {
          const result = await instance.signinRedirectCallback();
          // Remove authorization codes from browser history immediately.
          const returnTo = (result.state as { returnTo?: string } | undefined)?.returnTo;
          history.replaceState(null, '', returnTo && /^\/\?confirm_share=[a-zA-Z0-9-]+$/.test(returnTo) ? returnTo : '/');
          if (!cancelled) {
            const scopes = grantedBusinessScopes(result.scopes);
            // Only the actually granted scope subset becomes a non-credential recovery hint.
            if (scopes.length) writeRecoveryMarker({ issuer: config.oidc_issuer, clientId: config.oidc_client_id, scopes });
            else clearRecoveryMarker();
            setUser(result);
          }
          return;
        }
        const existing = await instance.getUser();
        if (existing && !existing.expired) { if (!cancelled) setUser(existing); return; }
        const marker = readRecoveryMarker();
        if (!scopesForRecovery(marker, config.oidc_issuer, config.oidc_client_id) || !claimSilentRecovery()) return;
        if (!cancelled) setRecovering(true);
        const outcome = await recoverSession({
          manager: instance, marker, issuer: config.oidc_issuer, clientId: config.oidc_client_id,
          isCancelled: () => cancelled || loggedOut.current,
        });
        if (cancelled || loggedOut.current) return;
        if (outcome.status === 'recovered') { setUser(outcome.user); return; }
        if (outcome.status === 'failed' && outcome.reason === 'timeout') recoveryAbandoned.current = true;
        if (outcome.status === 'failed' && outcome.reason === 'interaction_required') clearRecoveryMarker();
        setRecovering(false);
      } catch { if (!cancelled) { setError('身份验证未完成。请检查身份服务配置后重新登录。'); history.replaceState(null, '', '/'); } }
      finally { if (!cancelled) setLoading(false); }
    })();
    return () => { cancelled = true; dispose(); };
  }, []);
  const refreshMe = async () => {
    if (!api || !user) return;
    const value = await api.get<Me>('/v1/me');
    if (value.csrf_token) api.csrfToken = value.csrf_token;
    setMe(value);
  };
  const subjectKey = user ? `${user.profile.iss}|${user.profile.sub}` : '';
  useEffect(() => {
    if (!api || !user) return;
    let live = true;
    setLoading(true);
    void (async () => {
      try {
        const session = await api.post<{ csrf_token: string }>('/v1/session');
        api.csrfToken = session.csrf_token;
        const value = await api.get<Me>('/v1/me');
        if (live) { setMe(value); setError(''); }
      } catch { if (live) setError('无法建立业务安全会话。请检查服务状态或重新登录。'); }
      finally { if (live) { setLoading(false); setRecovering(false); } }
    })();
    return () => { live = false; };
  }, [api, subjectKey]);
  const value: AuthValue = { user, me, api, loading, recovering, error, configured, refreshMe,
    startBackgroundAccess: async () => {
      if (!api || !manager) throw new Error('需要先登录');
      const result = await api.post<{ authorization_url: string }>('/v1/delegation/start');
      const expected = new URL(await manager.metadataService.getAuthorizationEndpoint());
      const target = new URL(result.authorization_url);
      if (target.origin !== expected.origin || target.pathname !== expected.pathname || target.username || target.password) throw new Error('后台授权地址与身份服务不匹配');
      location.assign(target.href);
    },
    login: async (scopes) => {
      if (!manager) return;
      recoveryAbandoned.current = false;
      setError('');
      await manager.signinRedirect({ scope: `openid profile ${scopes.join(' ')}`, state: { returnTo: location.pathname + location.search } });
    },
    logout: async () => {
      if (!manager) return;
      // Invalidate a late silent result before clearing local state, then make
      // sure this tab can never silently restore the logged-out session.
      loggedOut.current = true;
      recoveryAbandoned.current = true;
      clearRecoveryMarker();
      manager.stopSilentRenew();
      setMe(null); setUser(null); setRecovering(false); setLoading(false); setError('');
      if (api) api.csrfToken = '';
      try { await manager.signoutRedirect(); }
      catch {
        await manager.removeUser().catch(() => {});
        setError('已清除本地会话，但身份服务退出未完成。');
      }
    },
  };
  return <Context.Provider value={value}>{children}</Context.Provider>;
}
export const useAuth = () => useContext(Context);
