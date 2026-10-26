export type Scope = 'drive.read' | 'drive.write' | 'drive.delete' | 'drive.restore' | 'drive.convert' | 'drive.share.request' | 'drive.feedback';
export const SCOPES: { value: Scope; label: string; description: string }[] = [
  { value: 'drive.read', label: '读取与搜索', description: '查看、搜索和下载自己的文件及历史版本' },
  { value: 'drive.write', label: '上传与整理', description: '新建、覆盖、重命名和移动文件' },
  { value: 'drive.delete', label: '移入回收站', description: '隐藏文件，原件与历史长期保留' },
  { value: 'drive.restore', label: '恢复文件', description: '从业务回收站恢复文件' },
  { value: 'drive.convert', label: '重新转换', description: '为固定源版本生成新的转换代次' },
  { value: 'drive.share.request', label: '请求分享', description: '仅创建请求，每次分享仍需用户确认' },
  { value: 'drive.feedback', label: '匿名反馈', description: '反馈内容仅供后台排查' },
];
export interface Completeness { status?: string; total_pages?: number; missing_text_pages?: number[]; missing_description_pages?: number[]; warnings?: string[] }
export interface Conversion extends Completeness { status: string; error_code?: string; completeness?: Completeness }
export interface DriveFile { id: string; parent_id: string | null; name: string; path: string; kind: 'file' | 'directory'; metadata_revision: number; current_version_id?: string; size_bytes: number; updated_at: string; conversion?: Conversion; lifecycle?: string; trash_id?: string; deleted_at?: string; original_path?: string }
export interface Version { id: string; version_id?: string; version_seq: number; created_at: string; size_bytes: number; conversion_status?: string; artifact_set_id?: string }
export interface Page<T> { items: T[]; next_cursor?: string | null }
export interface Me { id: string; display_name?: string; scopes: string[]; quota: { used_bytes: number; reserved_bytes: number; limit_bytes: number }; permissions?: { admin?: boolean; audit?: boolean }; csrf_token?: string }
export interface Operation { id: string; operation_id?: string; state?: string; status?: string; file_id?: string; version_id?: string; error?: { code: string; message?: string }; next_action?: string }
export interface UploadSession { id: string; upload_url: string; operation_id: string; status?: string }
export interface SearchHit { file_id: string; path: string; returned_version_id: string; current_version_id: string; artifact_set_id: string; stale: boolean; newer_version_status?: string; completeness: Completeness; snippet: string; location?: { page?: number; block?: string } }
export interface MarkdownPage { content?: string; markdown?: string; version_id?: string; source_version_id?: string; artifact_set_id: string; completeness?: Completeness; next_cursor?: string | null }
export interface Share { id: string; file_id: string; file_name?: string; audience_type: 'user' | 'group' | 'link'; audience_id?: string; rights: string[]; version_policy: 'latest' | 'fixed'; version_id?: string; expires_at?: string; status?: string; disabled_reason?: string; public_url?: string }
export interface ShareRequest extends Share { request_id?: string; request_expires_at?: string }
export interface SharedFile extends DriveFile { share_id?: string; permissions?: string[]; owner_name?: string; shared_version_policy?: 'fixed' | 'latest'; shared_version_id?: string }
export interface AdminJob { id: string; file_id: string; status: string; error_code?: string; updated_at?: string; missing_text_pages?: number[]; missing_description_pages?: number[] }
export interface Feedback { id: string; file_id: string; version_id: string; artifact_set_id?: string; category?: string; content: string; created_at: string }
export interface BatchResult { items: { item_id: string; operation_id?: string; status: string; error?: { code: string; message?: string } }[] }
