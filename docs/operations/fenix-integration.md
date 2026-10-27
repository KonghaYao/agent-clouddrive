# Fenix 对接 CloudDrive 指南

> 适用范围：Fenix 后端作为受信任调用方，通过 CloudDrive 的 MCP 接口，为每个 Fenix 用户提供独立的个人云盘空间。
>
> 设计原则：CloudDrive 不包含 Fenix 专用代码。本文只使用通用能力：master key 认证、external issuer、space provisioner、只读/可写 service credential。任何外部 IdP 都可以按同样方式接入。

## 0. 一页结论

- Fenix **后端**调用 `POST /mcp`，不经过 CloudDrive 自带的 Web UI。
- 认证使用 **静态 master key** + `X-On-Behalf-Of: <Fenix 用户 ID>`。Fenix 前端、浏览器和终端用户都拿不到 master key。
- CloudDrive 按 `(issuer, subject)` 分区：Fenix 用户第一次写入时，自动 provision 一个独立的 oCIS project drive。
- 文本读写需要 `drive.read`、`drive.write`；二进制上传使用 `drive_begin_upload` → TUS `PATCH /v1/tus/<id>` → `drive_finish_upload`。
- 关键配置：`DRIVE_MASTER_KEY_ALLOWED_CIDRS` 不能为空；service credential 的 scopes 必须包含 `openid`，否则 oCIS 的 userinfo 校验会返回 403。
- 跨用户隔离由 CloudDrive 的 `owner_id`/space 映射保证，不由 oCIS ACL 保证；不要把 oCIS 直接暴露给终端用户。

## 1. 架构与信任边界

```text
Fenix 前端/Agent
      │  只调用 Fenix 自己的 /mcp/drive
      ▼
Fenix 后端（受信任）
      │  Authorization: Bearer <master key>
      │  X-On-Behalf-Of: <Fenix 用户稳定 ID>
      ▼
CloudDrive /mcp  ──►  principal / quota / per-user space
      │
      ├── 文本与元数据：CloudDrive DB
      └── 原件与上传：oCIS project drive（service credential）

oCIS 的 drive owner 是 provisioner 的 service account；
用户之间的隔离由 CloudDrive 鉴权与空间映射保证。
```

信任边界：

- master key 只存在于 Fenix 后端和 CloudDrive 部署配置中。不要下发给浏览器、移动端、Agent 运行时或日志系统。
- Fenix 必须先完成用户认证，再决定 `X-On-Behalf-Of`；不要接受客户端直接传入的任意 subject。
- CloudDrive 的 `/mcp` 会把调用者当作 **Agent actor**，限制在其自己的个人空间内；不能使用接收共享等 Web/REST-only 能力。
- CloudDrive 自带 Web UI 不作为 Fenix 用户的人类入口。

## 2. 请求协议

### 2.1 认证头

每次请求（包括 TUS 上传）都需要：

```http
Authorization: Bearer <master key>
X-On-Behalf-Of: <Fenix 用户稳定 ID>
X-Key-Id: <可选，多 key 轮换时指定 key id>
Content-Type: application/json
Accept: application/json, text/event-stream
```

规则：

- `X-On-Behalf-Of` 必须是稳定、不可变的用户标识（建议用 Fenix 内部 user ID），不要用邮箱或可修改的昵称。
- `X-Key-Id` 不传时使用 `DRIVE_MASTER_KEY_DEFAULT_ID`；多 key 轮换时显式传入。
- master key 错误 → HTTP 401；`X-On-Behalf-Of` 缺失或格式非法 → HTTP 400；来源 IP 不在允许 CIDR 内 → HTTP 403。
- 配置了状态检查端点时，状态检查失败会 fail-closed（拒绝请求）。

### 2.2 错误语义

MCP 业务错误通常返回 **HTTP 200**，错误在 JSON-RPC 结果内部：

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "content": [
      {
        "type": "text",
        "text": "{\"error\":{\"code\":\"ACCESS_DENIED\",\"message\":\"access denied\"}}"
      }
    ],
    "isError": true
  }
}
```

调用方必须同时检查 `result.isError`，并二次解析 `result.content[0].text`。认证/协议层错误可能是 HTTP 4xx + 普通 JSON，不要只按 HTTP 状态判断业务结果。

常见 code：

| code | 含义 | 是否可重试 |
|---|---|---|
| `ACCESS_DENIED` | scope 不足、外部写未启用、issuer 不在允许列表、principal 非活跃 | 否，先修配置 |
| `NOT_FOUND` | 文件不存在或无权访问（用于下载等接口） | 否 |
| `PATH_CONFLICT` | 同目录同名目标已存在 | 否，改用覆盖或改名 |
| `VERSION_CONFLICT` | `expected_version_id` / `expected_metadata_revision` 不匹配 | 否，重新读取元数据 |
| `IDEMPOTENCY_CONFLICT` | 同一个 idempotency key 被用于不同请求 | 否，换 key |
| `INVALID_ARGUMENT` | 参数缺失或格式错误 | 否 |
| `SPACE_PROVISIONER_UNAVAILABLE` | 空间 provisioner 未配置/未验证/调用失败 | 视情况，先查 provisioner 配置 |
| `DEPENDENCY_UNAVAILABLE` | oCIS/IdP/数据库等依赖不可用 | 可退避重试，但要先排除配置问题 |

### 2.3 最小调用示例

```bash
curl -sS https://drive.example.com/mcp \
  -H "Authorization: Bearer $DRIVE_MASTER_KEY" \
  -H "X-On-Behalf-Of: $FENIX_USER_ID" \
  -H "X-Key-Id: fenix-prod" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{
    "jsonrpc": "2.0",
    "id": 1,
    "method": "tools/call",
    "params": {
      "name": "drive_create_text",
      "arguments": {
        "name": "hello.md",
        "content": "# hello\n",
        "idempotency_key": "fenix-20261027-0001"
      }
    }
  }'
```

## 3. 推荐调用流程

### 3.1 文本创建与覆盖

- 创建：`drive_create_text`，必填 `name`、`content`、`idempotency_key`；单文件上限 1 MiB。
- 目录：先 `drive_mkdir`（根目录不需要 revision），再用 `parent_id` 在目录下创建。
- 覆盖：必须传 `file_id`、`name`、`expected_version_id`、`expected_metadata_revision`；`parent_id` 可选，省略时从 `file_id` 推导（根目录和子目录都适用），显式传入时必须与文件当前父目录一致。CloudDrive **不会**因为同名而静默覆盖；同名创建返回 `PATH_CONFLICT`。
- 覆盖前先 `drive_stat` 读取 `current_version_id` 与 `metadata_revision`；两者都要作为期望值传入。

### 3.2 二进制上传（TUS）

1. `drive_begin_upload`：

```json
{"name":"report.pdf","size_bytes":12345,"idempotency_key":"fenix-upload-0001"}
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
Authorization: Bearer <master key>
X-On-Behalf-Of: <Fenix 用户 ID>
```

请求体是该分片的原始字节。重复 HEAD/PATCH 直到 `Upload-Offset == size_bytes`。
4. `drive_finish_upload`，参数 `{"upload_id":"<upload-id>"}`。CloudDrive 会做原生校验后才提交版本。

注意：

- TUS 端点同样需要 master key 与 `X-On-Behalf-Of`，不要只把 URL 交给没有凭证的客户端。
- 分片大小受 `size_bytes` 预留长度约束；不要超过剩余长度。
- 上传会话默认 24 小时过期；失败后用同一 `idempotency_key` 重试 `drive_begin_upload` 可恢复同一会话。

### 3.3 读取与下载

- `drive_list`：列出自己的文件；返回 `id`、`name`、`path`、`size`、`space_id`、`current_version_id`、`lifecycle`、`conversion` 等。
- `drive_stat`：读取单个文件的元数据与转换状态；对不存在或不属于当前 `(issuer, subject)` 的 `file_id` 返回 `NOT_FOUND`（`/mcp` 的 Agent actor 不查询共享）。
- `drive_search`：关键词搜索自己的文件；索引是最终一致的，刚写入的文件可能短暂搜不到。
- `drive_read_markdown`：读取已固定的 Markdown artifact（分页）。
- `drive_get_download`：返回 5 分钟有效的下载 URL；URL 本身是受控票据，按秘密处理，不要写入日志。
- `drive_versions`：读取源文件历史版本。

### 3.4 其他常用工具

| 工具 | scope | 说明 |
|---|---|---|
| `drive_mkdir` | `drive.write` | 创建目录 |
| `drive_move` / `drive_rename` | `drive.write` | 移动/改名，需要 `expected_metadata_revision` |
| `drive_delete` | `drive.delete` | 进入业务回收站，不会物理删除 |
| `drive_trash_list` | `drive.read` | 列出回收站 |
| `drive_restore` | `drive.restore` | 恢复 |
| `drive_reconvert` | `drive.convert` | 请求新的转换 generation |
| `drive_batch` | 视子操作 | 独立子操作可部分成功；依赖失败会被阻塞 |
| `drive_operation_status` | 无 | 查询操作状态 |

`drive_request_share` 只发起共享申请；审批必须在 CloudDrive Web/REST 完成。Fenix Agent 流程不应依赖该能力。

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
DRIVE_MASTER_KEY_SCOPES='drive.read,drive.write,drive.delete,drive.restore,drive.convert,drive.feedback'
# 可选：状态检查
DRIVE_MASTER_KEY_STATUS_URL='https://fenix.example.com/internal/drive-status'
DRIVE_MASTER_KEY_STATUS_TOKEN='<status token>'
DRIVE_MASTER_KEY_STATUS_TTL_SECONDS=60
```

注意：

- `DRIVE_MASTER_KEY_ALLOWED_CIDRS` 为空时**拒绝所有来源**；必须显式配置。
- 如果 CloudDrive 前面有网关/反代，`DRIVE_MASTER_KEY_ALLOWED_CIDRS` 必须填**真实客户端**的 CIDR，同时把网关网段填进 `DRIVE_TRUSTED_PROXY_CIDRS`，并确保网关设置/清洗 `X-Forwarded-For`。参考 nginx 已改为 `X-Forwarded-For $remote_addr`。直连 `api:8081`/`:8080` 时不要配置 `DRIVE_TRUSTED_PROXY_CIDRS`。
- 空 `DRIVE_MASTER_KEY_SCOPES` 会回退到默认 scopes（含读写、删除、恢复、转换、反馈），生产环境应显式配置最小权限，不要依赖默认值。
- 默认 scopes 已包含读写、删除、恢复、转换、反馈；按最小权限裁剪。
- `DRIVE_MASTER_KEY_ISSUER` 必须与 Fenix 请求的身份域一致；不要与 `DRIVE_OIDC_ISSUER` 相同。

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

- `DRIVE_DELEGATION_KEY_FILE` 是 delegation vault 的加密密钥文件；没有它，外部读写凭证不会被签发。
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
- A 用户的 `drive_list`、`drive_stat`、`drive_search`、`drive_get_download` 都只看到自己的文件；B 用户看不到 A 的文件。
- 配额来自 `DRIVE_DEFAULT_QUOTA_BYTES`（按 principal 计）。
- 原生 drive 由 provisioner 服务账号拥有；**隔离由 CloudDrive 应用层保证**。不要把 oCIS DAV/Graph 直接暴露给 Fenix 用户，否则会绕过 CloudDrive 的 owner 检查。

## 6. Fenix 侧实现清单

1. master key 存在 secret manager；只在后端出站请求中使用；不写日志、不返回前端。
2. 用户认证完成后，从服务端会话解析稳定 user ID，填入 `X-On-Behalf-Of`；不要信任客户端传入的 subject。
3. 明确 peri 到 CloudDrive 的网络路径：经网关时，在 CloudDrive 配置 `DRIVE_TRUSTED_PROXY_CIDRS` 为网关网段，`DRIVE_MASTER_KEY_ALLOWED_CIDRS` 配 peri 的真实出口 CIDR；直连 `api:8081` 时，`DRIVE_TRUSTED_PROXY_CIDRS` 留空，CIDR 直接配 peri 的网段。Fenix 出口 IP 变更时同步更新 CloudDrive。
4. 为每次写操作生成全局唯一的 `idempotency_key`；同一 key 的请求体必须完全一致，否则会得到 `IDEMPOTENCY_CONFLICT`。
5. 读操作可重试；`DEPENDENCY_UNAVAILABLE` 用指数退避重试；`ACCESS_DENIED`、`PATH_CONFLICT`、`VERSION_CONFLICT` 不要盲目重试。
6. 覆盖写前先 `drive_stat`，同时带上 `expected_version_id` 和 `expected_metadata_revision`；`parent_id` 可省略，显式传入时必须与现有父目录一致。
7. 二进制上传：由 Fenix 后端代理 TUS，或把 upload URL + 认证头封装在受控上传会话中；不要把 master key 交给浏览器。
8. 解析响应时同时检查 HTTP 状态、`result.isError` 和 `result.content[0].text` 内的 `error.code`。
9. 日志记录 `key_id`、`on_behalf_of`、`operation_id`、`idempotency_key`；绝不记录 master key、Authorization 头、上传 token、下载票据。
10. 监控 401/403、`ACCESS_DENIED`、`SPACE_PROVISIONER_UNAVAILABLE`、`DEPENDENCY_UNAVAILABLE` 的比例。
11. 轮换：在 `DRIVE_MASTER_KEYS` 中先加入新 key，Fenix 切换到新 `X-Key-Id`，确认后再移除旧 key。

## 7. 验证与排障

### 7.1 最小 smoke test

```bash
# 1) 列表
curl -sS https://drive.example.com/mcp \
  -H "Authorization: Bearer $KEY" -H "X-On-Behalf-Of: $USER_A" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"drive_list","arguments":{}}}'

# 2) 创建
# 3) 用 USER_B 再 list，确认看不到 USER_A 的文件
# 4) 用 USER_B stat USER_A 的 file_id，确认被拒绝
```

### 7.2 常见错误

| 现象 | 优先检查 |
|---|---|
| HTTP 401 `authentication denied` | master key 值、`X-Key-Id`、`DRIVE_MASTER_KEYS` 是否加载 |
| HTTP 400 `X-On-Behalf-Of` | 请求头是否缺失/含非法字符 |
| HTTP 403 | `DRIVE_MASTER_KEY_ALLOWED_CIDRS` 是否包含真实客户端 IP；经网关时 `DRIVE_TRUSTED_PROXY_CIDRS` 是否含网关网段、网关是否转发 `X-Forwarded-For` |
| `ACCESS_DENIED` | `DRIVE_MASTER_KEY_SCOPES` 是否含 `drive.write`；`DRIVE_EXTERNAL_WRITE_ENABLED=true`；issuer 在 `DRIVE_EXTERNAL_WRITE_ISSUERS` 中；principal 是否活跃 |
| `SPACE_PROVISIONER_UNAVAILABLE` | `DRIVE_SPACE_PROVISIONER_ENABLED/VERIFIED`；token URL/client；provisioner 是否有建盘权限；oCIS 是否 403/409；查 `operation=space_provision` 日志 |
| `DEPENDENCY_UNAVAILABLE` | service credential 是否含 `openid`；`DRIVE_DELEGATION_KEY_FILE`、`DRIVE_OCIS_INTEGRATION_VERIFIED=true`、`DRIVE_DELEGATION_VERIFIED=true` 是否齐全；oCIS userinfo 是否 403；数据库迁移是否完整；oCIS/IdP 是否 5xx |
| 上传 403 / userinfo 403 | `DRIVE_OCIS_WRITE_SERVICE_SCOPES` 是否含 `openid`；TUS 请求是否同时带 master key 与 `X-On-Behalf-Of` |
| `PATH_CONFLICT` | 同名文件已存在；改用 `file_id` 覆盖或先改名 |
| `VERSION_CONFLICT` | 重新 `drive_stat`，同时更新 `expected_version_id` 与 `expected_metadata_revision` |
| oCIS `user is missing a displayName` | IdP 服务账号缺少 `name`/`email` profile |

### 7.3 运维检查

- 部署前确保数据库迁移已执行到当前版本。该能力复用现有 `spaces`/`principals`/`upload_sessions` 等表，没有为 Fenix 新增专用迁移。
- 查看 provisioner 审计：日志中 `operation=space_provision`，含 `issuer`、`subject`、`drive_id`、`status`。
- 查看写凭证审计：`credential_kind=write_service`、`operation`、`issuer`、`subject`；不会记录 token。
- oCIS 的 `/dav`、`/graph` 只对 CloudDrive 服务网络开放，不对 Fenix 用户网络开放。

## 8. 已知限制

1. **`drive_stat` 的错误码已统一**：对不存在或无权 `file_id` 返回 `NOT_FOUND`；`/mcp` 的 Agent actor 不再查询共享。REST 的共享路径仍只对交互式用户开放。
2. **覆盖写需要完整 revision**：`file_id` + `name` + `expected_version_id` + `expected_metadata_revision`；`parent_id` 可省略（从 `file_id` 推导），显式传入时必须与现有父目录一致。缺少 `expected_metadata_revision` 时仍可能返回 `VERSION_CONFLICT`，错误提示不够精确。
3. **同名创建不隐式覆盖**：返回 `PATH_CONFLICT`，这是安全默认，但错误码更像路径问题。
4. **业务错误 HTTP 200**：必须检查 `result.isError` 并解析内部 JSON。
5. **搜索最终一致**：刚写入的文件可能短暂搜不到。
6. **provisioner 与读写账号不同时的权限缺口**：当前不会自动授予 project drive manager 权限；需要复用同一 oCIS 身份或在 oCIS 侧补权限。
7. **孤儿 drive**：provisioner 建盘成功但数据库写入失败时，可能留下没有映射的 drive；审计日志会记录 drive ID，需要人工清理。
8. **master key 泄漏的影响面**：可冒充允许 issuer 下任意 subject 并执行已配置 scopes。缓解手段是 CIDR allowlist、状态检查、独立写凭证、短 TTL、审计与轮换。
9. **个人云盘范围**：当前只做个人空间，不做团队空间；Agent actor 不能使用接收共享。
10. **oCIS 服务账号的 drive owner 语义**：原生 drive 的 owner 是 provisioner 服务账号，不是 Fenix 用户；这是为了支持 provisioner 建盘，不能据此认为 oCIS ACL 已经按用户隔离。
11. **CIDR 依赖网络拓扑**：经网关时必须在 CloudDrive 配置 `DRIVE_TRUSTED_PROXY_CIDRS`，并确保网关覆盖/清洗 `X-Forwarded-For`；直连时不要配置该变量。否则 allowlist 看到的是网关 IP，可能误拒或失去限制意义。

## 9. 参考

- MCP 工具 schema：[`docs/reference/mcp-tools.json`](../reference/mcp-tools.json)
- REST OpenAPI：[`docs/reference/openapi.json`](../reference/openapi.json)
- 部署说明：[`docs/operations/deployment.md`](deployment.md)
- 已知限制：[`docs/operations/known-limitations.md`](known-limitations.md)
- 安全评审：[`docs/security/security-review.md`](../security/security-review.md)
