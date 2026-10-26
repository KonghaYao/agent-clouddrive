import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { User } from 'oidc-client-ts';
import {
  RECOVERY_MARKER_KEY, RECOVERY_MARKER_TTL_MS, SILENT_IFRAME_TIMEOUT_SECONDS, clearRecoveryMarker,
  createAttemptGate, grantedBusinessScopes, readRecoveryMarker, recoverSession, recoveryFailureReason,
  scopesForRecovery, writeRecoveryMarker, type SilentSigninManager,
} from '../session-recovery';
const issuer = 'http://drive.localhost:8080/identity/realms/clouddrive';
const clientId = 'clouddrive-web';
const user = { access_token: 'secret-access-token', id_token: 'secret-id-token', refresh_token: 'secret-refresh-token', token_type: 'Bearer', scope: 'openid profile drive.read', profile: { iss: issuer, sub: 'subject' } } as unknown as User;
beforeEach(() => { sessionStorage.clear(); localStorage.clear(); });
describe('session recovery marker', () => {
  it('stores only the granted non-credential hint and never a token', () => {
    writeRecoveryMarker({ issuer, clientId, scopes: ['openid', 'profile', 'drive.read', 'drive.write', 'unknown.scope'] });
    const marker = readRecoveryMarker();
    expect(marker).toMatchObject({ version: 1, issuer, client_id: clientId, scopes: ['drive.read', 'drive.write'] });
    const raw = sessionStorage.getItem(RECOVERY_MARKER_KEY)!;
    expect(Object.keys(JSON.parse(raw)).sort()).toEqual(['client_id', 'issuer', 'saved_at', 'scopes', 'version']);
    expect(raw).not.toMatch(/access_token|refresh_token|id_token|csrf|code=/);
    expect(localStorage.length).toBe(0);
    expect(sessionStorage.length).toBe(1);
  });
  it('requires the marker to match issuer, client and a known scope subset', () => {
    writeRecoveryMarker({ issuer, clientId, scopes: ['drive.read'] });
    expect(scopesForRecovery(readRecoveryMarker(), issuer, clientId)).toEqual(['drive.read']);
    expect(scopesForRecovery(readRecoveryMarker(), issuer, 'another-client')).toBeNull();
    expect(scopesForRecovery(readRecoveryMarker(), 'http://another-issuer', clientId)).toBeNull();
    sessionStorage.setItem(RECOVERY_MARKER_KEY, JSON.stringify({ version: 1, issuer, client_id: clientId, scopes: ['drive.read', 'drive.admin'], saved_at: Date.now() }));
    expect(readRecoveryMarker()).toBeNull();
  });
  it('expires stale markers and rejects malformed values', () => {
    writeRecoveryMarker({ issuer, clientId, scopes: ['drive.read'], now: 1000 });
    expect(readRecoveryMarker(1000 + RECOVERY_MARKER_TTL_MS + 1)).toBeNull();
    sessionStorage.setItem(RECOVERY_MARKER_KEY, '{not json');
    expect(readRecoveryMarker()).toBeNull();
  });
  it('logout clears the marker so the next refresh cannot auto-restore', () => {
    writeRecoveryMarker({ issuer, clientId, scopes: ['drive.read'] });
    clearRecoveryMarker();
    expect(readRecoveryMarker()).toBeNull();
    expect(scopesForRecovery(readRecoveryMarker(), issuer, clientId)).toBeNull();
  });
  it('maps only known business scopes and deduplicates them', () => {
    expect(grantedBusinessScopes(['openid', 'profile', 'drive.read', 'drive.read', 'drive.admin'])).toEqual(['drive.read']);
  });
  it('allows one silent attempt per page load', () => {
    const claim = createAttemptGate();
    expect(claim()).toBe(true);
    expect(claim()).toBe(false);
    expect(claim()).toBe(false);
  });
});
describe('bounded silent recovery', () => {
  it('does not call the identity service without a marker', async () => {
    const signinSilent = vi.fn();
    const result = await recoverSession({ manager: { signinSilent } as unknown as SilentSigninManager, marker: null, issuer, clientId });
    expect(result).toEqual({ status: 'skipped' });
    expect(signinSilent).not.toHaveBeenCalled();
  });
  it('recovers with exactly one prompt=none request limited to the stored scopes', async () => {
    writeRecoveryMarker({ issuer, clientId, scopes: ['drive.read', 'drive.write'] });
    const signinSilent = vi.fn().mockResolvedValue(user);
    const result = await recoverSession({ manager: { signinSilent } as unknown as SilentSigninManager, marker: readRecoveryMarker(), issuer, clientId });
    expect(result).toEqual({ status: 'recovered', user });
    expect(signinSilent).toHaveBeenCalledTimes(1);
    expect(signinSilent).toHaveBeenCalledWith({ scope: 'openid profile drive.read drive.write', silentRequestTimeoutInSeconds: SILENT_IFRAME_TIMEOUT_SECONDS });
    expect(sessionStorage.getItem(RECOVERY_MARKER_KEY)).not.toContain('secret-');
    expect(localStorage.length).toBe(0);
  });
  it('reports an interaction fallback after one failed attempt', async () => {
    writeRecoveryMarker({ issuer, clientId, scopes: ['drive.read'] });
    const signinSilent = vi.fn().mockRejectedValue({ error: 'login_required' });
    const result = await recoverSession({ manager: { signinSilent } as unknown as SilentSigninManager, marker: readRecoveryMarker(), issuer, clientId });
    expect(result).toEqual({ status: 'failed', reason: 'interaction_required' });
    expect(signinSilent).toHaveBeenCalledTimes(1);
    expect(recoveryFailureReason({ error: 'consent_required' })).toBe('interaction_required');
    expect(recoveryFailureReason(new Error('network'))).toBe('error');
  });
  it('bounds a hanging silent request with a timeout instead of retrying', async () => {
    writeRecoveryMarker({ issuer, clientId, scopes: ['drive.read'] });
    const signinSilent = vi.fn().mockReturnValue(new Promise(() => {}));
    const result = await recoverSession({ manager: { signinSilent } as unknown as SilentSigninManager, marker: readRecoveryMarker(), issuer, clientId, timeoutMs: 10 });
    expect(result).toEqual({ status: 'failed', reason: 'timeout' });
    expect(signinSilent).toHaveBeenCalledTimes(1);
  });
  it('ignores a late silent result after cancellation', async () => {
    writeRecoveryMarker({ issuer, clientId, scopes: ['drive.read'] });
    let cancelled = false;
    let resolve!: (value: User | null) => void;
    const signinSilent = vi.fn().mockReturnValue(new Promise<User | null>(done => { resolve = done; }));
    const pending = recoverSession({ manager: { signinSilent } as unknown as SilentSigninManager, marker: readRecoveryMarker(), issuer, clientId, isCancelled: () => cancelled });
    cancelled = true;
    resolve(user);
    expect(await pending).toEqual({ status: 'skipped' });
    expect(signinSilent).toHaveBeenCalledTimes(1);
  });
});
