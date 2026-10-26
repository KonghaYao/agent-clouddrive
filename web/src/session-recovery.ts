import type { User } from 'oidc-client-ts';
import { SCOPES, type Scope } from './types';

/**
 * Tab-scoped, non-credential hint that this tab completed a login before.
 * It never contains tokens, the OIDC User, CSRF values or profile claims, and
 * it is only an untrusted input for whether to try one silent SSO request.
 */
export const RECOVERY_MARKER_KEY = 'agent-clouddrive.recovery.v1';
export const RECOVERY_MARKER_TTL_MS = 12 * 60 * 60 * 1000;
export const SILENT_RECOVERY_TIMEOUT_MS = 9000;
export const SILENT_IFRAME_TIMEOUT_SECONDS = 8;

const knownScopes = new Set<string>(SCOPES.map(scope => scope.value));
const interactionErrors = new Set(['login_required', 'consent_required', 'interaction_required', 'account_selection_required', 'session_selection_required', 'access_denied']);

export interface RecoveryMarker {
  version: 1;
  issuer: string;
  client_id: string;
  scopes: Scope[];
  saved_at: number;
}

export type RecoveryFailure = 'interaction_required' | 'timeout' | 'error';
export type RecoveryOutcome = { status: 'recovered'; user: User } | { status: 'skipped' } | { status: 'failed'; reason: RecoveryFailure };

export interface SilentSigninManager {
  signinSilent(args: { scope: string; silentRequestTimeoutInSeconds: number }): Promise<User | null>;
}

function sessionStore(): Storage | null {
  try { return window.sessionStorage; } catch { return null; }
}

export function grantedBusinessScopes(values: readonly string[]): Scope[] {
  return [...new Set(values.filter((value): value is Scope => knownScopes.has(value)))];
}

export function writeRecoveryMarker(input: { issuer: string; clientId: string; scopes: readonly string[]; now?: number }): void {
  const store = sessionStore();
  if (!store || !input.issuer || !input.clientId) return;
  const marker: RecoveryMarker = {
    version: 1,
    issuer: input.issuer,
    client_id: input.clientId,
    scopes: grantedBusinessScopes(input.scopes),
    saved_at: input.now ?? Date.now(),
  };
  try { store.setItem(RECOVERY_MARKER_KEY, JSON.stringify(marker)); } catch { /* Recovery is optional when storage is unavailable. */ }
}

export function readRecoveryMarker(now = Date.now()): RecoveryMarker | null {
  const store = sessionStore();
  if (!store) return null;
  let raw: string | null;
  try { raw = store.getItem(RECOVERY_MARKER_KEY); } catch { return null; }
  if (!raw) return null;
  let parsed: unknown;
  try { parsed = JSON.parse(raw); } catch { return null; }
  if (!parsed || typeof parsed !== 'object') return null;
  const value = parsed as Record<string, unknown>;
  if (value.version !== 1 || typeof value.issuer !== 'string' || !value.issuer) return null;
  if (typeof value.client_id !== 'string' || !value.client_id) return null;
  if (typeof value.saved_at !== 'number' || !Number.isFinite(value.saved_at)) return null;
  if (value.saved_at > now + 60_000 || now - value.saved_at > RECOVERY_MARKER_TTL_MS) return null;
  if (!Array.isArray(value.scopes) || value.scopes.some(scope => typeof scope !== 'string' || !knownScopes.has(scope))) return null;
  return { version: 1, issuer: value.issuer, client_id: value.client_id, scopes: grantedBusinessScopes(value.scopes as string[]), saved_at: value.saved_at };
}

export function clearRecoveryMarker(): void {
  try { sessionStore()?.removeItem(RECOVERY_MARKER_KEY); } catch { /* Nothing else is persisted. */ }
}

/** The marker is an untrusted hint; it may only narrow the original grant and never expand it. */
export function scopesForRecovery(marker: RecoveryMarker | null, issuer: string, clientId: string): Scope[] | null {
  if (!marker || !issuer || !clientId) return null;
  if (marker.issuer !== issuer || marker.client_id !== clientId) return null;
  return marker.scopes.length ? marker.scopes : null;
}

/** One silent attempt per page load, even if the provider effect is mounted twice. */
export function createAttemptGate(): () => boolean {
  let used = false;
  return () => { if (used) return false; used = true; return true; };
}
export const claimSilentRecovery = createAttemptGate();

class RecoveryTimeoutError extends Error {
  constructor() { super('silent recovery timed out'); this.name = 'RecoveryTimeoutError'; }
}

export function recoveryFailureReason(error: unknown): RecoveryFailure {
  if (error instanceof RecoveryTimeoutError) return 'timeout';
  const code = error && typeof error === 'object' && typeof (error as { error?: unknown }).error === 'string' ? (error as { error: string }).error : '';
  return interactionErrors.has(code) ? 'interaction_required' : 'error';
}

export async function recoverSession(input: {
  manager: SilentSigninManager;
  marker: RecoveryMarker | null;
  issuer: string;
  clientId: string;
  isCancelled?: () => boolean;
  timeoutMs?: number;
}): Promise<RecoveryOutcome> {
  const scopes = scopesForRecovery(input.marker, input.issuer, input.clientId);
  if (!scopes || input.isCancelled?.()) return { status: 'skipped' };
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    const timeout = new Promise<never>((_, reject) => { timer = setTimeout(() => reject(new RecoveryTimeoutError()), input.timeoutMs ?? SILENT_RECOVERY_TIMEOUT_MS); });
    const user = await Promise.race([
      input.manager.signinSilent({ scope: `openid profile ${scopes.join(' ')}`, silentRequestTimeoutInSeconds: SILENT_IFRAME_TIMEOUT_SECONDS }),
      timeout,
    ]);
    if (input.isCancelled?.()) return { status: 'skipped' };
    return user ? { status: 'recovered', user } : { status: 'failed', reason: 'error' };
  } catch (error) {
    if (input.isCancelled?.()) return { status: 'skipped' };
    return { status: 'failed', reason: recoveryFailureReason(error) };
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
}
