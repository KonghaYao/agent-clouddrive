# Fenix 对接 CloudDrive 指南

> 适用范围：Fenix 后端作为受信任调用方，通过 CloudDrive **REST API** 为每个 Fenix 用户提供独立的个人云盘空间。
>
> 正式对接面只有 REST。MCP 当前为 **beta/内部能力**，不属于 Fenix 正式契约，也不在公网暴露。
>
> 设计原则：CloudDrive 不包含 Fenix 专用代码。本文只使用通用能力：master key 认证、external issuer、space provisioner、只读/可写 service credential。任何外部 IdP 都可以按同样方式接入。

## 0. 一页结论

- Fenix **后端**调用 CloudDrive REST：`/v1/*`，上传数据走 `/v1/tus/<id>`。Fenix 前端和浏览器不直接持有 master key。
- 认证使用 **静态 master key** + `X-On-Behalf-Of: <Fenix 用户 ID>`。master key 只存在于 Fenix 后端和 CloudDrive 部署配置中。
- CloudDrive 按 `(issuer, subject)` 分区：Fenix 用户第一次写入时，自动 provision 一个独立的 oCIS project drive。
- REST 没有 `create_text` 便捷接口；**文本和二进制都走统一上传流程**：`POST /v1/uploads` → TUS `HEAD/PATCH /v1/tus/<id>` → `POST /v1/uploads/<id>/finalize`。
- MCP 是 beta/内部能力：参考网关不代理 `/mcp`，只在 `api:8081/mcp` 内网可用；不要把它作为 Fenix 的生产依赖。
- 关键配置：`DRIVE_MASTER_KEY_ALLOWED_CIDRS` 不能为空；service credential 的 scopes 必须包含 `openid`，否则 oCIS 的 userinfo 校验会返回 403；读路径还需要 `DRIVE_DELEGATION_KEY_FILE`、`DRIVE_OCIS_INTEGRATION_VERIFIED=true`、`DRIVE_DELEGATION_VERIFIED=true`。
- 跨用户隔离由 CloudDrive 的 `owner_id`/space 映射保证，不由 oCIS ACL 保证；不要把 oCIS 直接暴露给终端用户。

## 1. 架构与信任边界

```text
Fenix UI / 用户操作
      │  Fenix 自己的会话与 API
      ▼
Fenix 后端（受信任）
      │  Authorization: Bearer <master key>
      │  X-On-Behalf-Of: <Fenix 用户稳定 ID>
      ▼
CloudDrive REST /v1/*  ──►  principal / quota / per-user space
      │
      ├── 文本、元数据：CloudDrive DB + oCIS 原件
      └── 上传数据面：TUS /v1/tus/<id>（service credential）

Fenix Agent（beta，仅内网）
      │  MCP /mcp（api:8081）
      ▼
CloudDrive MCP（beta，非公开）
```

信任边界：

- master key 只存在于 Fenix 后端和 CloudDrive 部署配置中。不要下发给浏览器、移动端、Agent 运行时或日志系统。
- Fenix 必须先完成用户认证，再决定 `X-On-Behalf-Of`；不要接受客户端直接传入的任意 subject。
- REST 的 master key 调用默认按 **ActorUser** 处理，具备完整用户面能力（文件、目录、回收站、上传、分享等）。因此它只能在后端使用，不能出现在前端或公网客户端。
- MCP 的调用者固定按 **ActorAgent** 处理，只允许操作自己的个人空间；不能使用接收共享、分享审批等 Web/REST-only 能力。
- CloudDrive 自带 Web UI 不作为 Fenix 用户的人类入口。
- 不要把 oCIS DAV/Graph 直接暴露给 Fenix 用户或 Agent，否则会绕过 CloudDrive 的 owner 检查。

## 2. REST 协议

完整的机器可读契约见 [OpenAPI](../reference/openapi.json)。本节只列 Fenix 对接必须掌握的部分。

### 2.1 认证头

每次 REST 请求都需要：

```http
Authorization: Bearer <master key>
X-On-Behalf-Of: <Fenix 用户稳定 ID>
X-Key-Id: <可选，多 key 轮换时指定 key id>
Content-Type: application/json
```

TUS `PATCH` 的 `Content-Type` 是 `application/offset+octet-stream`，认证头相同。

规则：

- `X-On-Behalf-Of` 必须是稳定、不可变的用户标识（建议用 Fenix 内部 user ID），不要用邮箱或可修改的昵称。
- `X-Key-Id` 不传时使用 `DRIVE_MASTER_KEY_DEFAULT_ID`；多 key 轮换时显式传入。
- master key 错误 → HTTP 401；`X-On-Behalf-Of` 缺失或格式非法 → HTTP 400；来源 IP 不在允许 CIDR 内 → HTTP 403。
- 配置了状态检查端点时，状态检查失败会 fail-closed（拒绝请求）。

### 2.2 错误语义

REST 使用 **HTTP 状态码 + JSON error**，不是 MCP 的 JSON-RPC 200 包装：

```json
{
  "error": {
    "code": "ACCESS_DENIED",
    "message": "access denied",
    "details": {}
  }
}
```

调用方先看 HTTP 状态，再解析 `error.code`。OpenAPI 列出的业务状态包括 401、403、404、409、413、422、429、503。

常见 code：

| code | 含义 | 是否可重试 |
|---|---|---|
| `ACCESS_DENIED` | scope 不足、外部写未启用、issuer 不在允许列表、principal 非活跃 | 否，先修配置 |
| `INSUFFICIENT_SCOPE` | master key 的 scope 不包含该操作 | 否，补 scope |
| `NOT_FOUND` | 文件不存在或不属于当前用户 | 否 |
| `PATH_CONFLICT` | 同目录同名目标已存在 | 否，改用覆盖或改名 |
| `VERSION_CONFLICT` | `expected_version_id` / `expected_metadata_revision` 不匹配 | 否，重新读取元数据 |
| `IDEMPOTENCY_CONFLICT` | 同一个 idempotency key 被用于不同请求 | 否，换 key |
| `INVALID_ARGUMENT` | 参数缺失或格式错误 | 否 |
| `QUOTA_EXCEEDED` | 配额不足 | 否，先清理或扩容 |
| `UPLOAD_TOO_LARGE` | 超过单文件/请求限制 | 否 |
| `OPERATION_PENDING` | 操作仍在进行 | 是，按 `operation_id` 轮询 |
| `SPACE_PROVISIONER_UNAVAILABLE` | 空间 provisioner 未配置/未验证/调用失败 | 视情况，先查 provisioner 配置 |
| `DEPENDENCY_UNAVAILABLE` | oCIS/IdP/数据库等依赖不可用 | 可退避重试，但要先排除配置问题 |

### 2.3 幂等、版本与覆盖

- 每个写操作都生成全局唯一的 `idempotency_key`；同一 key 的请求体必须完全一致，否则返回 `IDEMPOTENCY_CONFLICT`。
- 覆盖写必须传 `file_id`、`name`、`expected_version_id`、`expected_metadata_revision`。`parent_id` 可选，省略时从 `file_id` 推导；显式传入时必须与文件当前父目录一致。
- CloudDrive **不会**因为同名而静默覆盖；同名创建返回 `PATH_CONFLICT`。
- 覆盖前先 `GET /v1/files/{id}` 读取 `current_version_id` 与 `metadata_revision`，两者都作为期望值传入。

### 2.4 最小调用示例

```bash
curl -sS https://drive.example.com/v1/files \
  -H "Authorization: Bearer $DRIVE_MASTER_KEY" \
  -H "X-On-Behalf-Of: $FENIX_USER_ID" \
  -H "X-Key-Id: fenix-prod"
```

## 3. REST 调用流程

### 3.1 列表、元数据与目录

| 操作 | 方法 | 路径 | 说明 |
|---|---|---|---|
| 当前主体 | GET | `/v1/me` | 主体、配额、空间等 |
| 列出文件 | GET | `/v1/files` | 支持 `parent_id`、`cursor` |
| 文件元数据 | GET | `/v1/files/{id}` | 不存在或无权访问返回 `NOT_FOUND` |
| 创建目录 | POST | `/v1/directories` | 字段见 OpenAPI；根目录不需要父 revision |
| 移动/改名 | PATCH | `/v1/files/{id}/location` | 需要 `expected_metadata_revision` |
| 删除 | DELETE | `/v1/files/{id}` | 进入业务回收站，不物理删除 |
| 回收站 | GET | `/v1/trash` | 列出已删除条目 |
| 恢复 | POST | `/v1/trash/{id}/restore` | 字段见 OpenAPI |
| 版本 | GET | `/v1/files/{id}/versions` | 返回历史版本 |
| 原文 | GET | `/v1/files/{id}/content` | 流式返回；支持单个 `Range` |
| Markdown | GET | `/v1/files/{id}/markdown` | 已固定的转换产物 |
| 搜索 | POST | `/v1/search` | 最终一致；刚写入的文件可能短暂搜不到 |
| 重新转换 | POST | `/v1/files/{id}/conversions` | 需要 `drive.convert` |
| 批处理 | POST | `/v1/batches` | 子操作可部分成功；依赖失败会被阻塞 |
| 操作状态 | GET | `/v1/operations/{id}` | 轮询长时间运行的操作 |

请求/响应字段以 OpenAPI 为准；REST 与 MCP 共享同一业务用例，但 REST 直接返回 JSON/字节，不包 JSON-RPC envelope。

### 3.2 上传：文本和二进制统一走 TUS

REST **没有** `create_text`。文本也作为 UTF-8 字节走同一上传流程；1 MiB 上限只适用于 MCP beta 的 `drive_create_text` 便捷工具。

1. 预留上传会话：

```http
POST /v1/uploads
Content-Type: application/json

{
  "name": "report.pdf",
  "size_bytes": 12345,
  "content_type": "application/pdf",
  "idempotency_key": "fenix-upload-0001"
}
```

返回：

```json
{
  "id": "<upload-id>",
  "file_id": "<file-id>",
  "operation_id": "<operation-id>",
  "status": "uploading",
  "upload_url": "https://drive.example.com/v1/tus/<upload-id>",
  "expires_at": "..."
}
```

2. 对 `upload_url` 发 `HEAD`，读取 `Upload-Offset`。
3. 对 `upload_url` 发 `PATCH`，携带：

```http
Tus-Resumable: 1.0.0
Upload-Offset: <当前偏移>
Content-Type: application/offset+octet-stream
Content-Length: <本片字节数>
Authorization: Bearer <master key>
X-On-Behalf-Of: <Fenix 用户 ID>
```

请求体是该分片的原始字节。重复 HEAD/PATCH 直到 `Upload-Offset == size_bytes`。`Content-Length` 必须存在；chunked transfer 会被拒绝。

4. 提交：

```http
POST /v1/uploads/<upload-id>/finalize
```

CloudDrive 会在原生校验通过后才提交版本。失败或放弃时调用：

```http
POST /v1/uploads/<upload-id>/abandon
```

注意：

- TUS 端点与 REST 使用同一 master key 认证。master key 只能由 Fenix 后端持有；浏览器上传必须由后端代理 TUS，或由后端签发受控上传会话。不要把 master key 交给浏览器。
- 分片大小受 `size_bytes` 预留长度约束；不要超过剩余长度。
- 上传会话带 `expires_at`；服务端清理策略以部署为准，不要把它当作安全边界。
- 覆盖：在 `POST /v1/uploads` 中带 `file_id`、`expected_version_id`、`expected_metadata_revision`；`parent_id` 可省略，显式传入时必须与现有父目录一致。

### 3.3 读取、版本与下载

- 原文/固定版本：`GET /v1/files/{id}/content`，可带 `version_id`；支持单个 `Range`，返回 `Content-Disposition: attachment`。
- 元数据：`GET /v1/files/{id}`；对不存在或不属于当前用户的 `file_id` 返回 `NOT_FOUND`。
- 版本：`GET /v1/files/{id}/versions`。
- Markdown：`GET /v1/files/{id}/markdown`。
- 下载票据 `/v1/download/{token}` 是受控票据的消费端点，Fenix 不需要自己签发；优先使用带认证的 `content` 端点。
- 不要记录 `Authorization`、master key、下载票据或带签名的 URL。

### 3.4 搜索、转换与批处理

- 搜索是最终一致的；写入后立即搜索可能短暂无结果。
- 转换是异步的；`GET /v1/files/{id}` 和 `GET /v1/operations/{id}` 返回转换/操作状态。需要新 generation 时调用 `POST /v1/files/{id}/conversions`。
- 批处理返回每个子操作的结果；不要因为部分成功而整体重试，重试时复用同一个 idempotency key。

### 3.5 分享与审批（REST-only）

以下能力只属于交互式用户面，不属于 Agent/MCP：

- `POST /v1/share-requests`、`GET /v1/share-requests`、`GET /v1/share-requests/{id}`
- `POST /v1/share-requests/{id}/approve`（需要交互式会话和 CSRF 确认）
- `GET /v1/shares`、`POST /v1/shares/{id}/revoke`
- `GET /v1/shared-with-me`

分享审批必须有人类确认，不能由 Fenix 后端或 Agent 自动批准。

## 4. 配置清单

以下变量均为 CloudDrive 侧配置。密钥类变量支持 `_FILE` 形式，生产环境优先用文件或 secret manager，不要把明文写进 Compose/Git。

### 4.1 Master key

```bash
DRIVE_MASTER_KEYS='{"fenix-prod":"<至少 32 字节的随机值>"}'
DRIVE_MASTER_KEY_DEFAULT_ID=fenix-prod
DRIVE_MASTER_KEY_ISSUER=fenix
DRIVE_MASTER_KEY_ALLOWED_CIDRS='10.0.0.0/8,192.168.0.0/16'
# 仅当请求经过受信网关/反代时配置；填网关所在网段
DRIVE_TRUSTED_PROXY_CIDRS='10.10.0.0/16'
DRIVE_MASTER_KEY_SCOPES='drive.read,drive.write,drive.delete,drive.restore,drive.convert,drive.share.request,drive.feedback'
# 可选：状态检查
DRIVE_MASTER_KEY_STATUS_URL='https://fenix.example.com/internal/drive-status'
DRIVE_MASTER_KEY_STATUS_TOKEN='<status token>'
DRIVE_MASTER_KEY_STATUS_TTL_SECONDS=60
```

注意：

- `DRIVE_MASTER_KEY_ALLOWED_CIDRS` 为空时**拒绝所有来源**；必须显式配置。
- 如果 CloudDrive 前面有网关/反代，`DRIVE_MASTER_KEY_ALLOWED_CIDRS` 必须填**真实客户端**的 CIDR，同时把网关网段填进 `DRIVE_TRUSTED_PROXY_CIDRS`，并确保网关设置/清洗 `X-Forwarded-For`。参考 nginx 已改为 `X-Forwarded-For $remote_addr`。直连 `api:8080` 时不要配置 `DRIVE_TRUSTED_PROXY_CIDRS`。
- 从旧版本升级时，`generated/nginx.conf` 不会随镜像自动更新。必须用与首次部署相同的 `--mode`、`--public-url` 和 `--trusted-proxy-cidr` 重新执行 `setup.py`；`up.sh` 会校验模板哈希，配置过期时拒绝启动。新模板不再把 `/mcp` 代理到公网。
- 空 `DRIVE_MASTER_KEY_SCOPES` 会回退到默认 scopes（含读写、删除、恢复、转换、分享请求、反馈），生产环境应显式配置最小权限，不要依赖默认值。
- `DRIVE_MASTER_KEY_ISSUER` 必须与 Fenix 请求的身份域一致；不要与 `DRIVE_OIDC_ISSUER` 相同。
- REST 的 master key 调用默认按 `ActorUser` 处理；`DRIVE_MASTER_KEY_INTERACTIVE_CLIENT_ID` 默认等于 WebClientID，通常不需要改。不要把这个 key 放进浏览器或公开客户端。

### 4.2 外部空间的 provisioner

```bash
DRIVE_OIDC_ISSUER='https://idp.example.com/realms/clouddrive'
DRIVE_EXTERNAL_SPACE_ISSUERS='fenix'

DRIVE_SPACE_PROVISIONER_ENABLED=true
DRIVE_SPACE_PROVISIONER_VERIFIED=true
DRIVE_SPACE_PROVISIONER_TOKEN_URL='https://idp.example.com/realms/clouddrive/protocol/openid-connect/token'
DRIVE_SPACE_PROVISIONER_CLIENT_ID='fenix-space-provisioner'
DRIVE_SPACE_PROVISIONER_CLIENT_SECRET='<secret>'
DRIVE_SPACE_PROVISIONER_AUDIENCE='ocis'
DRIVE_SPACE_PROVISIONER_SCOPES='openid'
DRIVE_SPACE_PROVISIONER_NATIVE_INSTANCE='ocis'
```

- `DRIVE_OIDC_ISSUER` 是 CloudDrive 自己的主 OIDC issuer。Fenix 的 issuer（`fenix`）与它不同，才会被识别为 external issuer。
- provisioner 的服务账号需要创建 project drive 的权限（demo 中为 `ocisAdmin`）。生产环境应使用专用的最小权限账号。
- 服务账号在 IdP 中需要有 `name`/`email` 等 profile 信息，否则 oCIS 自动建用户可能失败（例如 `user is missing a displayName`）。
- provisioner 创建的是 **project drive**，owner 是 provisioner 服务账号；不要让终端用户直接访问 oCIS。

### 4.3 可写 service credential

```bash
DRIVE_EXTERNAL_WRITE_ENABLED=true
DRIVE_EXTERNAL_WRITE_ISSUERS='fenix'

DRIVE_OCIS_WRITE_SERVICE_TOKEN_URL='https://idp.example.com/realms/clouddrive/protocol/openid-connect/token'
DRIVE_OCIS_WRITE_SERVICE_CLIENT_ID='fenix-drive-write'
DRIVE_OCIS_WRITE_SERVICE_CLIENT_SECRET='<secret>'
DRIVE_OCIS_WRITE_SERVICE_AUDIENCE='ocis'
DRIVE_OCIS_WRITE_SERVICE_SCOPES='openid,drive.read,drive.write'
DRIVE_OCIS_WRITE_SERVICE_TTL_SECONDS=300
DRIVE_OCIS_WRITE_SERVICE_VERIFIED=true
```

- `openid` **必须**包含：oCIS 的 OIDC authenticator 会用 userinfo 校验 bearer token；缺少 `openid` 时 Keycloak 返回 403。
- 写路径位于 delegation vault 内，需要配置 `DRIVE_DELEGATION_KEY_FILE`；否则外部写会 fail-closed。
- 写路径还要求 `DRIVE_OCIS_INTEGRATION_VERIFIED=true` 与 `DRIVE_DELEGATION_VERIFIED=true`（见 4.5）。
- 生产环境不要给写账号 `ocisAdmin`；它只需要目标空间的文件读写权限。

### 4.4 只读 / 后台 service credential

```bash
DRIVE_JOB_CREDENTIAL_MODE=service
DRIVE_OCIS_SERVICE_TOKEN_URL='https://idp.example.com/realms/clouddrive/protocol/openid-connect/token'
DRIVE_OCIS_SERVICE_CLIENT_ID='fenix-drive-read'
DRIVE_OCIS_SERVICE_CLIENT_SECRET='<secret>'
DRIVE_OCIS_SERVICE_AUDIENCE='ocis'
DRIVE_OCIS_SERVICE_SCOPES='openid,drive.read'
DRIVE_JOB_CREDENTIAL_TTL_SECONDS=300
```

只读凭证允许 `openid` 与 `drive.read`，必须包含 `drive.read`，且禁止 `drive.write`。worker、转换、保留策略仍保持只读。

### 4.5 部署门禁（读路径必填）

以下变量控制 delegation vault 与原生读/移动能力。缺少任一项时，下载、版本读取、move/rename、reconcile 会 fail-closed 为 `DEPENDENCY_UNAVAILABLE`：

```bash
DRIVE_DELEGATION_KEY_FILE='/run/secrets/delegation_key'
DRIVE_OCIS_INTEGRATION_VERIFIED=true
DRIVE_DELEGATION_VERIFIED=true
```

- `DRIVE_DELEGATION_KEY_FILE` 是 delegation vault 的加密密钥文件；没有它，外部读写凭证不会被签发。启用 `DRIVE_MASTER_KEYS` 或 `DRIVE_EXTERNAL_WRITE_ENABLED` 时若该变量为空，进程启动会直接失败（fail-fast），不会等到首次下载才返回 503。
- 只读 service credential 的 `Verified` 由 `DRIVE_OCIS_INTEGRATION_VERIFIED` 与 `DRIVE_DELEGATION_VERIFIED` 共同决定。
- 写路径还需要 `DRIVE_OCIS_WRITE_SERVICE_VERIFIED=true`（见 4.3）。
- 启动日志会在 master key 或外部写开启、但上述门禁未全部为 true 时打印 `WARN`；生产环境应把它当作配置错误处理。

### 4.6 生产环境的分账号建议

demo 为了简化使用了同一个 Keycloak client 同时做 provisioner 和文件读写，且带 `ocisAdmin`。生产环境建议拆成三个身份：

1. **provisioner**：创建 project drive，具备最小化的空间管理权限。
2. **write service**：只对目标空间有文件写权限，不具备管理权限。
3. **read service**：只读。

如果 provisioner 与读写 service 是**不同的 oCIS 用户**，当前实现不会自动给读写账号授予 project drive 的 manager 权限。此时需要：

- 让三者复用同一个 oCIS 服务身份；或
- 在 oCIS/Graph 侧为读写账号补上目标 drive 的权限（这部分不在 CloudDrive 当前代码内）。

## 5. 空间、隔离与配额

- 第一次写入时，CloudDrive 为 `(issuer, subject)` 创建 `spaces` 行，并调用 provisioner 创建原生 project drive。
- 空间名是确定性的：`drive-<sanitized subject>-<sha256(issuer + NUL + subject) 前 8 位>`。重试会复用同一空间，不会重复建盘。
- 同一个原生 drive 冲突时返回 `PATH_CONFLICT`；已有 owner 映射时会做 reconcile。
- A 用户的 `GET /v1/files`、`GET /v1/files/{id}`、`POST /v1/search` 都只看到自己的文件；B 用户看不到 A 的文件。
- 配额来自 `DRIVE_DEFAULT_QUOTA_BYTES`（按 principal 计）。
- 原生 drive 由 provisioner 服务账号拥有；**隔离由 CloudDrive 应用层保证**。不要把 oCIS DAV/Graph 直接暴露给 Fenix 用户，否则会绕过 CloudDrive 的 owner 检查。

## 6. Fenix 侧实现清单

1. master key 存在 secret manager；只在后端出站请求中使用；不写日志、不返回前端。
2. 用户认证完成后，从服务端会话解析稳定 user ID，填入 `X-On-Behalf-Of`；不要信任客户端传入的 subject。
3. 明确 Fenix 到 CloudDrive 的网络路径：经网关时，在 CloudDrive 配置 `DRIVE_TRUSTED_PROXY_CIDRS` 为网关网段，`DRIVE_MASTER_KEY_ALLOWED_CIDRS` 配 Fenix 的真实出口 CIDR；直连 `api:8080` 时，`DRIVE_TRUSTED_PROXY_CIDRS` 留空，CIDR 直接配 Fenix 的网段。
4. 为每次写操作生成全局唯一的 `idempotency_key`；同一 key 的请求体必须完全一致。
5. 读操作可重试；`DEPENDENCY_UNAVAILABLE` 用指数退避重试；`ACCESS_DENIED`、`PATH_CONFLICT`、`VERSION_CONFLICT` 不要盲目重试。
6. 覆盖写前先 `GET /v1/files/{id}`，同时带上 `expected_version_id` 和 `expected_metadata_revision`；`parent_id` 可省略，显式传入时必须与现有父目录一致。
7. 上传：由 Fenix 后端代理 TUS，或把 upload URL + 认证头封装在受控上传会话中；不要把 master key 交给浏览器。文本也走同一上传流程。
8. 下载使用 `GET /v1/files/{id}/content`；解析响应时检查 HTTP 状态和 `error.code`，不要按 HTTP 200 就认为成功。
9. 日志记录 `key_id`、`on_behalf_of`、`operation_id`、`idempotency_key`；绝不记录 master key、Authorization 头、上传 token、下载票据。
10. 监控 401/403、`ACCESS_DENIED`、`SPACE_PROVISIONER_UNAVAILABLE`、`DEPENDENCY_UNAVAILABLE` 的比例。
11. 轮换：在 `DRIVE_MASTER_KEYS` 中先加入新 key，Fenix 切换到新 `X-Key-Id`，确认后再移除旧 key。
12. MCP 只用于内网 beta 的 Agent 试验；不要把它作为 Fenix 生产依赖，也不要通过公网网关暴露。

## 7. 验证与排障

### 7.1 最小 smoke test

```bash
# 0) 当前主体
curl -sS https://drive.example.com/v1/me \
  -H "Authorization: Bearer $DRIVE_MASTER_KEY" \
  -H "X-On-Behalf-Of: $USER_A"

# 1) 列表
curl -sS https://drive.example.com/v1/files \
  -H "Authorization: Bearer $DRIVE_MASTER_KEY" \
  -H "X-On-Behalf-Of: $USER_A"

# 2) 创建文本上传会话
curl -sS -X POST https://drive.example.com/v1/uploads \
  -H "Authorization: Bearer $DRIVE_MASTER_KEY" \
  -H "X-On-Behalf-Of: $USER_A" \
  -H 'Content-Type: application/json' \
  -d '{"name":"hello.md","size_bytes":8,"content_type":"text/markdown; charset=utf-8","idempotency_key":"fenix-20261027-0001"}'

# 3) HEAD/PATCH 返回的 upload_url，最后 finalize
# 4) 用 USER_B 再 list，确认看不到 USER_A 的文件
# 5) 用 USER_B GET USER_A 的 file_id，确认返回 NOT_FOUND
```

### 7.2 常见错误

| 现象 | 优先检查 |
|---|---|
| 401 | master key 是否正确、`X-Key-Id` 是否指向存在的 key |
| 400 | `X-On-Behalf-Of` 是否缺失/非法 |
| 403 | 来源 CIDR、scope、外部写/issuer 配置 |
| 404 | `file_id` 是否属于当前用户；MCP beta 下 Agent 不查询共享 |
| 409 | `PATH_CONFLICT` / `VERSION_CONFLICT` / `IDEMPOTENCY_CONFLICT` |
| 413 | `size_bytes`、分片长度或配额 |
| 429 | 限流或配额 |
| 503 | provisioner、oCIS、IdP、delegation key 等依赖 |

### 7.3 运维检查

- 查看 principal/space：`SELECT id, owner_id, native_instance, native_space_id FROM spaces;`
- 查看 provisioner 审计：日志中 `operation=space_provision`，含 `issuer`、`subject`、`drive_id`、`status`。
- 检查 `generated/nginx.conf` 是否来自当前模板；`up.sh` 会在模板过期时拒绝启动。

## 8. 已知限制

- REST 没有 `create_text`；文本必须走上传流程。1 MiB 上限只适用于 MCP beta 的便捷工具。
- MCP 是 beta/内部能力；参考网关不代理 `/mcp`，不提供稳定性承诺，不作为 Fenix 生产契约。
- 上传会话带 `expires_at`，但服务端清理策略需要单独确认；不要把它当作安全边界。
- TUS `PATCH` 必须带 `Content-Length`；chunked transfer 会被拒绝。
- 当前没有 scoped upload token；浏览器/外部 Agent 直传必须由 Fenix 后端代理，或后续补一次性上传令牌。
- `drive_stat`/`drive_versions` 对不存在或不属于当前主体的 `file_id` 返回 `NOT_FOUND`（MCP beta 的 Agent actor）；REST `ActorUser` 仍可能查询共享，按分享规则处理。
- provisioner 与读写 service 如果是不同 oCIS 用户，当前不会自动授予 project drive manager 权限。
- 原生 drive owner 是 provisioner 服务账号；不能据此认为 oCIS ACL 已按用户隔离。
- 同名创建不静默覆盖；覆盖写需要版本 + metadata revision。
- 搜索是最终一致的；转换是异步的。
- master key 泄露等于可冒充允许 issuer 下的任意 subject；用 CIDR、状态检查、轮换、审计和最小 scopes 降低风险。

## 9. MCP Beta（非公开）

MCP 仍在代码中，但定位为 beta/内部能力：

- **不在公网暴露**：参考网关的 `/mcp` 直接返回 404；不要把它加回公网 ingress。
- **内网入口**：仅 `api:8081/mcp`，不发布宿主端口。需要 beta 访问时使用独立内网入口/VPN。
- **认证**：与 REST 相同的 master key + `X-On-Behalf-Of`；调用者固定按 `ActorAgent` 处理，只能操作自己的空间。
- **能力边界**：不能接收共享、不能审批分享、不能执行 Web/REST-only 的交互式操作。
- **协议**：JSON-RPC；业务错误通常 HTTP 200，错误在 `result.isError` 和 `result.content[0].text` 中。
- **工具清单**：[21 tool schemas](../reference/mcp-tools.json)；工具名和行为可能变化，不提供稳定性承诺。
- **定位**：只用于内部 Agent 试验；Fenix 的正式集成面是 REST。

## 10. 参考

- [REST OpenAPI](../reference/openapi.json)
- [MCP tool schemas（beta）](../reference/mcp-tools.json)
- [部署说明](deployment.md)
- [已知限制](known-limitations.md)
- [安全审查](../security/security-review.md)
