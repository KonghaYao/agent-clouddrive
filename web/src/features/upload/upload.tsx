import { createControlledUploadClient, ensureUploadNonce, preflightUpload, resetUploadNonce, UploadPreflightError, uploadConflict, uploadConflictFromFailure, uploadFailurePayload, uploadFailureText, uploadIdempotencyKey, uploadSessionWritable, type UploadConflict, type UploadFailureResponse } from './upload-client';
import Dashboard from '@uppy/react/dashboard';
import { Alert, Button, Card, Space, Tag } from 'antd';
import { useEffect, useRef, useState } from 'react';
import type { API } from '../../shared/api';
import { errorText } from '../../shared/api';
import { controlledURL, operationDone, operationFailed, operationState } from '../../shared/security';
import type { DriveFile, Operation, UploadSession } from '../../shared/types';
import '@uppy/core/css/style.min.css';
import '@uppy/dashboard/css/style.min.css';
interface CommitState { session: UploadSession; name: string; status: string; error?: string }
export interface PendingUploadConflict extends UploadConflict { fileId: string; name: string }
export function uploadConflictText(conflict: PendingUploadConflict): string {
  if (conflict.reason === 'incomplete_upload') return `“${conflict.name}”上次传输在收到任何字节之前中断，因此不会出现在文件列表中，但仍占用这个名称。取消这份未完成上传后会使用新的重试标识重新开始；也可以重命名后上传。已有文件不会被覆盖。`;
  return `目标位置已有同名项目“${conflict.name}”，它可能尚未出现在列表中。请重命名后上传；已有文件不会被覆盖。`;
}
export function UploadConflictAlert({ conflict, acting, onAbandon, onRename }: { conflict: PendingUploadConflict; acting: boolean; onAbandon: () => void; onRename: () => void }) {
  return <Alert type="warning" showIcon title={conflict.reason === 'incomplete_upload' ? '目标位置有一份未完成的上传' : '目标位置已有同名项目'}
    description={<Space orientation="vertical" size={8}>
      <span>{uploadConflictText(conflict)}</span>
      <Space wrap>
        {conflict.reason === 'incomplete_upload' && <Button type="primary" loading={acting} onClick={onAbandon}>取消未完成上传并重试</Button>}
        <Button onClick={onRename}>重命名后重试</Button>
      </Space>
    </Space>} />;
}
type UploadClient = ReturnType<typeof createControlledUploadClient>;
export interface UploadFailureOutcome { conflict: PendingUploadConflict | null; text: string; specific: boolean }
/** The upload-error channel must expose the real tus status/code instead of an information-free banner. */
export function uploadFailureOutcome(file: { id: string; name?: string } | undefined, error: unknown, response?: UploadFailureResponse): UploadFailureOutcome {
  const conflict = uploadConflictFromFailure(error, response);
  if (conflict && file) return { conflict: { ...conflict, fileId: file.id, name: file.name ?? '' }, text: '', specific: true };
  const status = response?.status ?? (error as { originalResponse?: { getStatus?: () => number } })?.originalResponse?.getStatus?.();
  const payload = uploadFailurePayload(error, response) as { error?: { code?: string }; code?: string } | undefined;
  const code = payload?.error?.code ?? payload?.code;
  return { conflict: null, text: uploadFailureText(status, code), specific: Boolean(status || code) };
}
/** A specific failure replaces the generic banner; the generic banner never replaces a specific one. */
export function mergeUploadError(current: string, outcome: UploadFailureOutcome): string {
  if (outcome.conflict) return '';
  return outcome.specific ? outcome.text : (current || outcome.text);
}
export interface PrepareUploadOptions {
  api: API; uppy: UploadClient; sessions: Map<string, UploadSession>;
  parent: DriveFile | null; overwrite?: DriveFile; signal: AbortSignal;
  showConflict: (conflict: PendingUploadConflict | null) => void;
  clearConflictFor: (id: string) => void;
  setError: (message: string) => void;
  onSession?: (id: string, session: UploadSession, name: string) => void;
  preflight?: (session: UploadSession) => Promise<unknown>;
}
/** Create or reuse a writable session, then probe it exactly as the uploader will. */
export async function prepareUploads(ids: string[], o: PrepareUploadOptions): Promise<void> {
  o.setError('');
  for (const id of ids) {
    const file = o.uppy.getFile(id);
    if (!file) continue;
    let session = o.sessions.get(id);
    if (!session || !uploadSessionWritable(session)) {
      o.sessions.delete(id);
      const nonce = ensureUploadNonce(o.uppy, id);
      try {
        session = await o.api.post<UploadSession>('/v1/uploads', {
          parent_id: o.overwrite?.parent_id ?? o.parent?.id ?? null, name: o.overwrite?.name ?? file.name,
          size_bytes: file.size, content_type: file.type || 'application/octet-stream',
          file_id: o.overwrite?.id, expected_version_id: o.overwrite?.current_version_id,
          expected_metadata_revision: o.overwrite?.metadata_revision ?? o.parent?.metadata_revision,
          idempotency_key: uploadIdempotencyKey(nonce),
        }, o.signal);
      } catch (e) {
        const details = uploadConflict(e);
        if (details) {
          o.showConflict({ ...details, fileId: id, name: file.name ?? '' });
          o.setError('');
        } else o.setError(errorText(e));
        throw new Error(errorText(e));
      }
      if (!uploadSessionWritable(session)) {
        o.setError(`上次上传会话已结束（${session.status ?? '未知状态'}），不会继续传输。请重命名后重新上传。`);
        throw new Error('upload session is not writable');
      }
      const url = controlledURL(session.upload_url);
      o.sessions.set(id, session);
      o.clearConflictFor(id);
      o.uppy.setFileState(id, { tus: { uploadUrl: url } });
      o.onSession?.(id, session, file.name ?? '');
    }
    try {
      await (o.preflight ?? (current => preflightUpload(o.api, current.upload_url)))(session);
    } catch (e) {
      o.sessions.delete(id);
      const status = e instanceof UploadPreflightError ? e.status : undefined;
      o.setError(status ? `上传地址预检失败（HTTP ${status}）。请重试；若持续出现，请重新选择文件。` : errorText(e));
      throw new Error('upload preflight failed');
    }
  }
}
export function UploadPanel({ api, parent, overwrite, onSaved }: { api: API; parent: DriveFile | null; overwrite?: DriveFile; onSaved: () => void }) {
  const [commits, setCommits] = useState<Record<string, CommitState>>({});
  const [error, setError] = useState('');
  const [conflict, setConflict] = useState<PendingUploadConflict | null>(null);
  const [acting, setActing] = useState(false);
  const conflictRef = useRef<PendingUploadConflict | null>(null);
  const callback = useRef(onSaved); callback.current = onSaved;
  const controller = useRef(new AbortController());
  const sessions = useRef(new Map<string, UploadSession>());
  const polling = useRef(new Map<string, ReturnType<typeof setTimeout>>());
  const [uppy] = useState(() => createControlledUploadClient(api, overwrite ? 1 : 20));
  function showConflict(next: PendingUploadConflict | null) { conflictRef.current = next; setConflict(next); }
  function setCommit(id: string, patch: Partial<CommitState>) { setCommits(old => ({ ...old, [id]: { ...old[id], ...patch } })); }
  async function poll(id: string, operationId: string) {
    try {
      const operation = await api.get<Operation>(`/v1/operations/${encodeURIComponent(operationId)}`, controller.current.signal);
      setCommit(id, { status: operationState(operation), error: operation.error?.code });
      if (operationDone(operation)) { callback.current(); return; }
      if (operationFailed(operation)) return;
    } catch (e) {
      if (controller.current.signal.aborted) return;
      setCommit(id, { status: 'outcome_unknown', error: errorText(e) });
    }
    polling.current.set(id, setTimeout(() => void poll(id, operationId), 3000));
  }
  async function finalize(session: UploadSession) {
    setCommit(session.id, { status: 'confirming', error: undefined });
    try {
      const result = await api.post<Operation>(`/v1/uploads/${encodeURIComponent(session.id)}/finalize`, { idempotency_key: `finalize:${session.id}` }, controller.current.signal);
      setCommit(session.id, { status: operationState(result) });
      if (operationDone(result)) callback.current();
      else if (!operationFailed(result)) void poll(session.id, result.operation_id ?? result.id ?? session.operation_id);
    } catch (e) {
      if (controller.current.signal.aborted) return;
      setCommit(session.id, { status: 'outcome_unknown', error: errorText(e) });
      void poll(session.id, session.operation_id);
    }
  }
  async function abandonAndRetry() {
    const active = conflictRef.current;
    if (!active?.uploadId) return;
    setActing(true);
    try {
      await api.post(`/v1/uploads/${encodeURIComponent(active.uploadId)}/abandon`, {}, controller.current.signal);
      sessions.current.delete(active.fileId);
      resetUploadNonce(uppy, active.fileId);
      showConflict(null); setError(''); setActing(false);
      void uppy.retryUpload(active.fileId).catch(() => setError('重试未能启动，请重命名或重新选择文件。'));
    } catch (e) {
      if (controller.current.signal.aborted) return;
      setError(errorText(e)); setActing(false);
    }
  }
  function renameAndRetry() {
    const active = conflictRef.current;
    if (!active) return;
    const next = typeof window.prompt === 'function' ? window.prompt('新的文件名（不会覆盖已有项目）', active.name)?.trim() : undefined;
    if (!next) return;
    if (next === active.name || next.includes('/') || next.includes('\\')) { setError('文件名需与原名不同，且不能包含斜线。'); return; }
    uppy.setFileState(active.fileId, { name: next });
    sessions.current.delete(active.fileId);
    resetUploadNonce(uppy, active.fileId);
    showConflict(null); setError('');
    void uppy.retryUpload(active.fileId).catch(() => setError('重试未能启动，请重新选择文件。'));
  }
  useEffect(() => {
    const prepare = async (ids: string[]) => {
      await prepareUploads(ids, {
        api, uppy, sessions: sessions.current, parent, overwrite, signal: controller.current.signal,
        showConflict,
        clearConflictFor: id => { if (conflictRef.current?.fileId === id) showConflict(null); },
        setError,
        onSession: (id, session, name) => setCommit(session.id, { session, name, status: 'uploading' }),
      });
    };
    uppy.addPreProcessor(prepare);
    const added = (file: ReturnType<typeof uppy.getFile>) => { ensureUploadNonce(uppy, file.id); };
    const success = (file: ReturnType<typeof uppy.getFile> | undefined) => { if (file) { const session = sessions.current.get(file.id); if (session) void finalize(session); } };
    const failure = (file: { id: string; name?: string } | undefined, error: unknown, response?: UploadFailureResponse) => {
      const outcome = uploadFailureOutcome(file, error, response);
      if (outcome.conflict) showConflict(outcome.conflict);
      setError(current => mergeUploadError(current, outcome));
    };
    const retry = (file: { id: string }) => { sessions.current.delete(file.id); };
    const retryAll = (files: Array<{ id: string }>) => { files.forEach(file => sessions.current.delete(file.id)); };
    uppy.on('file-added', added); uppy.on('upload-success', success); uppy.on('upload-error', failure);
    uppy.on('upload-retry', retry); uppy.on('retry-all', retryAll);
    const beforeUnload = (event: BeforeUnloadEvent) => { if (uppy.getFiles().some(f => !f.progress.uploadComplete)) event.preventDefault(); };
    window.addEventListener('beforeunload', beforeUnload);
    return () => { controller.current.abort(); polling.current.forEach(clearTimeout); uppy.removePreProcessor(prepare); window.removeEventListener('beforeunload', beforeUnload); uppy.off('upload-retry', retry); uppy.off('retry-all', retryAll); uppy.destroy(); };
  }, [uppy]);
  return <Space orientation="vertical" size={16} className="full-width">
    <Alert type="info" showIcon title={overwrite ? `覆盖 ${overwrite.name}，将创建新源版本` : `上传到 ${parent?.path ?? '/'}`} description="原件传输完成后仍需服务器确认保存。转换在后台独立执行；请保持此页面打开以便断网续传。" />
    {conflict && <UploadConflictAlert conflict={conflict} acting={acting} onAbandon={() => void abandonAndRetry()} onRename={renameAndRetry} />}
    {error && <Alert type="error" showIcon title={error} />}
    <Dashboard uppy={uppy} width="100%" height={330} proudlyDisplayPoweredByUppy={false} hideProgressDetails={false} note="tus 断点续传 · 每块 32 MiB · 原件和转换状态独立" />
    {Object.values(commits).map(c => <Card size="small" key={c.session.id}><Space wrap><strong>{c.name}</strong><Tag color={operationDone({ status: c.status }) ? 'green' : 'blue'}>{c.status === 'uploading' ? '正在传输原件' : operationDone({ status: c.status }) ? '原件已保存，转换另行处理' : c.status === 'confirming' ? '传输结束，正在确认保存' : c.status}</Tag>{c.error && <span>{c.error}</span>}{c.status === 'outcome_unknown' && <Button size="small" onClick={() => void finalize(c.session)}>查询并确认</Button>}</Space></Card>)}
  </Space>;
}
