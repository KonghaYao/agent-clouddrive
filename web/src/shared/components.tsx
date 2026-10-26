import { Alert, Button, Space, Tag, Typography } from 'antd';
import { useEffect, useRef, useState } from 'react';
import { API, APIError, errorText } from './api';
import { operationDone, operationFailed, operationState } from './security';
import type { Completeness, Conversion, Operation } from './types';
const stateLabels: Record<string, [string, string]> = { ready: ['完整可用', 'green'], partial: ['部分可用', 'gold'], queued: ['等待转换', 'blue'], processing: ['正在转换', 'processing'], failed: ['转换失败', 'red'], blocked: ['转换受阻', 'orange'], not_converted: ['原件已保存 · 未转换', 'default'], committed: ['原件已保存', 'green'], outcome_unknown: ['结果待确认', 'gold'], pending: ['正在执行', 'blue'] };
export function Status({ value }: { value?: string }) { const [label, color] = stateLabels[value ?? ''] ?? [value || '尚未转换', 'default']; return <Tag color={color}>{label}</Tag>; }
export function CompletenessNotice({ value }: { value?: Completeness | Conversion }) {
  if (!value) return null;
  const data = 'completeness' in value && value.completeness ? value.completeness : value;
  const missingText = data.missing_text_pages ?? [], missingImages = data.missing_description_pages ?? [];
  if (data.status !== 'partial' && !missingText.length && !missingImages.length && !data.warnings?.length) return null;
  return <Alert type="warning" showIcon title="转换内容不完整" description={<Space orientation="vertical" size={2}>
    {!!missingText.length && <span>缺失文字页：{missingText.join('、')}</span>}
    {!!missingImages.length && <span>缺失图片／图表描述页：{missingImages.join('、')}</span>}
    {!!data.warnings?.length && <span>提示：{data.warnings.join('，')}</span>}
    <span>仅已完成部分可用于检索和引用，请同时核对原件。</span>
  </Space>} />;
}
export function ErrorNotice({ error, retry }: { error: unknown; retry?: () => void }) {
  if (!error) return null;
  return <Alert type="error" showIcon title={errorText(error)} description={error instanceof APIError && error.detail.operation_id ? `待确认操作：${error.detail.operation_id}` : undefined} action={retry && <Button size="small" onClick={retry}>重试</Button>} />;
}
export function OperationNotice({ api, operation, onComplete }: { api: API; operation: Operation | null; onComplete?: () => void }) {
  const [value, setValue] = useState(operation);
  const [error, setError] = useState<unknown>();
  const doneRef = useRef(onComplete); doneRef.current = onComplete;
  useEffect(() => {
    setValue(operation); setError(undefined);
    if (!operation) return;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      try {
        const next = await api.get<Operation>(`/v1/operations/${encodeURIComponent(operation.operation_id ?? operation.id)}`, controller.signal);
        if (controller.signal.aborted) return;
        setValue(next); setError(undefined);
        if (operationDone(next)) { doneRef.current?.(); return; }
        if (operationFailed(next)) return;
      } catch (e) { if (controller.signal.aborted) return; setError(e); }
      timer = setTimeout(poll, 3000);
    };
    if (!operationDone(operation) && !operationFailed(operation)) timer = setTimeout(poll, 1500);
    else if (operationDone(operation)) doneRef.current?.();
    return () => { controller.abort(); clearTimeout(timer); };
  }, [api, operation]);
  if (!value) return null;
  return <Alert type={operationFailed(value) ? 'error' : operationDone(value) ? 'success' : 'info'} showIcon
    title={operationDone(value) ? '操作已确认完成' : operationFailed(value) ? '操作未完成' : '正在确认操作结果'}
    description={<Space orientation="vertical" size={2}><Status value={operationState(value)} /><Typography.Text type="secondary">操作编号：{value.operation_id ?? value.id}</Typography.Text>{value.error && <span>{value.error.code}</span>}{error ? <span>查询暂不可用，将继续确认；请勿重复提交。</span> : null}</Space>} />;
}
