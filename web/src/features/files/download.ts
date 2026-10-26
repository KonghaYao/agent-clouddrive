import type { API } from '../../shared/api';
import { bytes } from '../../shared/security';
interface FileHandle { createWritable(): Promise<WritableStream<Uint8Array>> }
export async function downloadFile(api: API, path: string, name: string, size: number, onProgress: (value: string) => void): Promise<void> {
  const picker = (window as unknown as { showSaveFilePicker?: (o: { suggestedName: string }) => Promise<FileHandle> }).showSaveFilePicker;
  // The chooser must run synchronously from a user gesture, before asynchronous token work.
  const handle = picker ? await picker({ suggestedName: name }) : null;
  if (!handle && size > 100 * 1024 * 1024) throw new Error('大文件下载需要支持流式保存的 Chromium 浏览器；不会把大文件整体读入内存。');
  const response = await api.binary(path);
  if (!response.body) throw new Error('下载响应没有内容');
  let received = 0;
  if (handle) {
    const progress = new TransformStream<Uint8Array, Uint8Array>({ transform(chunk, target) { received += chunk.byteLength; onProgress(`${bytes(received)} / ${bytes(size)}`); target.enqueue(chunk); } });
    await response.body.pipeThrough(progress).pipeTo(await handle.createWritable());
  } else {
    const declared = Number(response.headers.get('Content-Length') ?? size);
    if (declared > 100 * 1024 * 1024) { await response.body.cancel(); throw new Error('大文件需要使用支持流式保存的浏览器。'); }
    const chunks: Uint8Array[] = [];
    const reader = response.body.getReader();
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      received += value.byteLength;
      if (received > 100 * 1024 * 1024) { await reader.cancel(); throw new Error('下载超过浏览器内存保护上限，请使用 Chromium 流式下载。'); }
      chunks.push(value); onProgress(`${bytes(received)} / ${bytes(size)}`);
    }
    const blob = new Blob(chunks as BlobPart[], { type: 'application/octet-stream' });
    const url = URL.createObjectURL(blob), anchor = document.createElement('a');
    anchor.href = url; anchor.download = name; anchor.rel = 'noopener'; anchor.click();
    setTimeout(() => URL.revokeObjectURL(url), 30000);
  }
  onProgress('下载已完成');
}
