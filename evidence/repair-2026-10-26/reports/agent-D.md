# Agent D 报告

分支/HEAD：`repair/D` / `5c5da6df6b1c5b48ea99186b5c047a531ca4b40d`
工作区：`/Users/konghayao/code/ai/agent-clouddrive-D`
基线：`2d3b4fff498f4a3b95922d323687977c5225c0d4`

提交：

- `4a053b9` fix(B15): select an authorized historical preview version
- `7d5b99b` fix(B07): expose expected_parent_revision on the MCP mkdir contract
- `8b625d7` docs(B07,B15): publish mkdir parent revision and preview version parameters
- `b02cf14` fix(B13,B14): pin Markdown cursors and read bounded artifact ranges
- `600e561` test(B07): include parent revision type validation in MCP repair runs
- `5c5da6d` test(B14): fail closed on truncated object range reads

## 逐项

### B07：verified

- 根因/触发（当前代码）：非根 mkdir 在后端要求 `expected_parent_revision`（`internal/application/mutations.go:172,253`），但 `ToolSchema` 未声明该字段且设置 `additionalProperties=false`，MCP handler 又显式拒绝未列出参数，导致 Agent 无法在已有目录下创建子目录。根目录调用不需要该字段。
- 实现：
  - `internal/transport/mcp.go:35` 更新 `drive_mkdir` 描述，明确非根父目录需要父目录 `metadata_revision`。
  - `internal/transport/mcp.go:59` 在工具 schema 中开放可选的 `expected_parent_revision`（`integer`，`minimum: 1`）。
  - `internal/application/query.go:96-105` 将 mkdir 从通用变更分支拆出；`internal/application/query.go:150-183` 在 REST/MCP/batch 共用的 `Execute` 边界校验显式传入值的类型与下界。缺字段仍交给原有后端逻辑报“required”，不放宽乐观并发校验。
  - 未修改 `internal/application/mutations.go`（任务书禁止）；后端原有父版本比较逻辑保持不变。
  - 生成契约：`docs/reference/mcp-tools.json`（`cmd/contracts` 重新生成）、`docs/reference/openapi.json`（`scripts/generate-openapi.py` 重新生成，`CreateDirectory.expected_parent_revision`）。
- 正确性测试：
  - `TestRepairMCPMkdirNestedContract`（`internal/transport/repair_mcp_mkdir_test.go:121`，官方 `modelcontextprotocol/go-sdk` Streamable HTTP 客户端 + 真实 PostgreSQL）：根 mkdir → `drive_stat` 取 revision=2 → 子目录 mkdir → `drive_list` 看到 `reports`；缺 revision 返回 `INVALID_ARGUMENT`，过期 revision 返回 `VERSION_CONFLICT`；字符串类型被 schema/handler 拒绝；第二个根目录无 revision 仍成功。
  - `TestRepairMCPMkdirParentRevisionTypeValidation`（`internal/application/repair_reads_test.go:367`）：整数、`json.Number` 与 batch 解码产生的整值 `float64` 通过；字符串、布尔、0、负数、小数、非数字拒绝；缺字段由该边界放行并由后端按根/非根处理。
  - 关键日志：`official SDK root mkdir -> stat(revision=2) -> nested mkdir -> list closed loop; missing and stale revisions rejected`。
  - 命令与退出码：见文末统一命令记录，`EXIT:0`。
- 数据库迁移/历史状态修复：无迁移。历史状态无需修复（纯契约/参数可达性缺陷）。
- 真实依赖 vs 替身边界：真实 PostgreSQL、真实应用授权与 mutation 状态机、真实 MCP 协议 handler/SDK 客户端；OIDC 与原生 oCIS `MKCOL` 是测试替身。未验证真实 oCIS/Peri 客户端。
- 剩余风险/阻塞：
  - 现有 `ToolSchema` 生成器对所有工具共享同一属性集合，因此 `expected_parent_revision` 也会出现在其他工具 schema 上（既有模式，handler 仍按业务忽略）。若需要严格按工具裁剪，应重构生成器，属于共享文件改动。
  - JSON Schema 未表达“仅非根目录必填”的条件；缺失时由后端返回明确参数错误，这一语义已写入工具描述。

### B13：verified

- 根因/触发（当前代码）：`markdown` 的 `cursor` 只是十进制字节偏移；续页时重新按 `files.search_artifact_set_id` 或该源版本的最新 generation 选 artifact。两页之间发布新代次会拼接不同文档；旧 cursor 也不绑定主体/文件/版本/产物。
- 实现：
  - 迁移 `db/migrations/000012_read_cursor_binding.up.sql:6` 新增 `read_cursors`：`token_hash`（仅存 SHA-256 摘要）、`format_version`、`principal_id`、`file_id`、`source_version_id`、`artifact_set_id`、`read_scope`、`byte_offset`、`expires_at`，并用复合外键保证 file/version/artifact 关系。已加入 `sqlc.yaml:13`，`internal/dbgen/models.go` 重新生成。
  - `internal/application/read_cursor.go:35` 加载并校验 cursor（格式版本、主体、文件、版本、产物、偏移、过期）；`read_cursor.go:56` 生成 256 位随机不透明 token（库中只存摘要），TTL 1 小时，并做最多 64 行的过期清理。
  - `internal/application/query.go:244-350` 重写 `markdown`：cursor 存在时只按绑定的 artifact/version/offset 续读；显式 `artifact_set_id`/`version_id`/`offset` 必须与 cursor 一致，否则 `INVALID_ARGUMENT`；显式 `artifact_set_id` 与 `version_id` 互相不一致也拒绝。每页重新执行 `Owned` 授权，并按创建时的读取类别（current=`ActionReadMarkdown`，history=`ActionReadHistory`）重新检查 scope、账号状态与生命周期。旧十进制 cursor、篡改、跨主体、跨文件、过期 cursor 均明确失败，策略为 fail closed（客户端重新开始读取）。
- 正确性测试：
  - `TestRepairReadsCursorPinsPublishedGeneration`（`internal/application/repair_reads_test.go:79`）：默认指针路径与显式源版本路径各一遍；发布/切换代次 B 后，cursor 仍读 A；三页拼接恰好是 A；显式重复绑定一致的字段成功。
  - `TestRepairReadsCursorRejectsForeignTamperedExpiredAndLegacy`（同文件 `:136`）：篡改 token、另一主体、另一文件、artifact 冲突、version 冲突、offset 冲突、旧十进制 cursor、数据库中将过期时间改为过去，全部 `INVALID_ARGUMENT`。
  - `TestRepairReadsCursorReauthorizesEveryPage`（同文件 `:188`）：撤销 `drive.read` → `INSUFFICIENT_SCOPE`；账号停用 → `ACCESS_DENIED`；当前版本读取在文件进入 trashed 后 → `NOT_FOUND`；显式历史读取 cursor 在 trashed 后仍可读绑定产物（与交付 §10.3 owner 历史读取能力一致，未放宽当前版本读取）。
  - 命令与退出码：`go test -race ... -run 'RepairReads|RepairMCP|RepairPreview'`，`EXIT:0`。
- 数据库迁移/历史状态修复：
  - 新表，无历史数据回填。空库安装由测试中完整迁移集覆盖；从基线升级（`000001..000008` 后应用 `000012`，重复应用一次）实测通过，见 `agent-D-migration-check.log`。
  - 历史“坏状态”：修复前已下发给客户端的十进制 cursor 不再被解释为偏移；明确返回 `INVALID_ARGUMENT`（“start a new read”），不会静默换代。无需修复数据库行。
  - 运维清理（可选）：`DELETE FROM read_cursors WHERE expires_at < now();`；代码在创建新 cursor 时已做有界机会式清理（每次最多 64 行）。
- 真实依赖 vs 替身边界：真实 PostgreSQL 与真实本地 artifact store；OIDC 为替身；未使用 PGlite。S3 不参与 cursor 存储。
- 剩余风险/阻塞：
  - cursor TTL 固定 1 小时，未做配置项；超长阅读会话需重新开始（显式失败）。
  - cursor 行在 TTL 内会累积；每个非末页创建一行，未做每主体行数上限/限流（沿用 API 现有认证与网关限流），当前清理有界但有写放大，若生产流量大建议加周期任务。
  - 这是有意的客户端契约变更：cursor 必须视为不透明字符串。

### B14：verified

- 根因/触发（当前代码）：每次分页都用 `Open(key, nil)` + `readBounded(64MiB)` 读取完整 artifact 后切片，4 MiB 文档两页实际读 8 MiB（64 倍放大），完整阅读成本平方增长。
- 实现：
  - `internal/application/read_cursor.go:79` 新增 `readMarkdownPage`：用 `ports.ByteRange{Start: offset, End: offset+64KiB+2}` 有界读取（最多 65539 字节），页面最多 65536 字节；在页尾向前回退到 UTF-8 字符起始，续读 offset 正好从被跳过的字符开始，不丢字/不重复。
  - `read_cursor.go:132` 仅在范围 open 返回 `ports.ErrInvalid`（offset 在 EOF 或空对象）时，用 1 字节范围探测总长度（空对象再用无范围 metadata open），因此 EOF/空文件/越界都能明确处理，且不会退化为整篇下载。
  - `internal/adapters/artifactstore/s3.go:165` 将 HTTP 416 映射为 `ports.ErrInvalid`，与本地适配器的 EOF 语义一致。Local/S3 的 Range 能力原本已存在，本次直接复用。
- 正确性测试：
  - `TestRepairReadsPaginationIsBounded`（`internal/application/repair_reads_test.go:255`）：4 MiB 正文、两页、用计量 store 统计；实测 `object bytes read=131078 delivered=131072 opens=2`（每页仅多读 3 字节 UTF-8 余量），并校验内容未变。
  - `TestRepairReadsUTF8AndEOF`（同文件 `:281`）：3 字节中文全文走读拼接一致；4 字节 emoji 恰好跨 65536 边界时第一页 65535 字节、第二页从 emoji 起始、拼接等于原文；恰好 65536、空正文、10 字节短正文、offset==EOF、offset 越界、offset 落在多字节字符中间均按预期。
  - `TestRepairReadsShortObjectReadFailsClosed`（`internal/application/repair_reads_test.go` 末尾）：store 返回短于声明范围的字节时必须 `DEPENDENCY_UNAVAILABLE`，不得把截断页当完整页返回。
  - `TestRepairReadsRangeEOFAndEmptyObject` / `TestRepairReadsS3UnsatisfiableRangeIsInvalid`（`internal/adapters/artifactstore/repair_range_test.go:17,65`）：本地 EOF/空对象 Range 语义、S3 416→`ErrInvalid`。
- 数据库迁移/历史状态修复：无迁移。
- 真实依赖 vs 替身边界：真实本地 store 与真实 PostgreSQL；S3 适配器只做了 416 映射的单元测试，未连接真实 S3/对象存储。PGlite 未使用。
- 剩余风险/阻塞：
  - 真实 S3/MinIO Range 路径未在本环境实测；其既有 Range 实现与本地适配器契约一致，但应作为合并后集成项复核。
  - 页面按“向前回退到字符边界”保持 ≤64 KiB，最多少 3 字节；不会超过契约页大小。

### B15：verified

- 根因/触发（当前代码）：`PreviewInfo` 没有 version 参数，`previewFile` 总调用 `SharedFile(..., "", ...)` 选第一个可用 grant；`PreviewPage` 先取默认 grant 再要求请求版本等于该 grant 的版本，导致同一文件 v1/v2 都有合法 fixed 授权时只有一个版本可预览。HTTP `GET /v1/preview/{id}` 也丢弃 `version_id`。
- 实现：
  - `internal/application/preview.go:61` `previewFile(ctx,a,fid,vid)`：默认（vid 为空）保持 current/默认 grant；显式 vid 时 owner 走 `ActionReadHistory` 并校验该版本确属此文件，接收者走 `SharedFile(..., vid, ActionPreview)`，按目标版本选择授权。
  - `internal/application/preview.go:160` `PreviewInfo(..., vid)` 将 vid 同时传给 owner 路径和匿名 link 路径；`preview.go:188` `PreviewPage` 直接用 vid 重新授权并打开该版本的 raster。
  - `internal/transport/http.go:424` 登录态 preview 读取 `?version_id=`；`http.go:451` 匿名 `/s/{token}` 同样支持（fixed/latest grant 仍在 `domain.Authorize` 中限制版本）；`http.go:443` 页面入口原本已传递该参数。
  - `docs/reference/openapi.json` 与生成脚本为 `/v1/preview/*`、`/s/*` 声明可选 `version_id` query。
- 正确性测试：
  - `TestRepairPreviewSelectsAuthorizedHistoricalVersion`（`internal/application/repair_preview_test.go:43`）：同一文件 v1/v2 各一条 fixed grant；默认预览落到某个合法版本且 URL 绑定版本；分别显式预览 v1/v2 成功且页面内容来自对应版本；owner 历史预览成功；第三版本无授权 → `NOT_FOUND`；跨文件 version → `NOT_FOUND`/页面拒绝；撤销 v1 后 v1 拒绝而 v2 正常；fixed 匿名链接只能读绑定版本，传另一版本被拒。
  - `TestRepairPreviewLatestGrantRejectsHistoricalOverride`（同文件 `:155`）：latest grant 默认返回 current，显式旧版本被拒。
  - `TestRepairPreviewManifestVersionMatchesAssetVersion`（同文件 `:186`）：manifest 的 version 与打开的页面资产版本一致。
  - `TestRepairPreviewHTTPVersionSelection`（`internal/transport/repair_preview_http_test.go:58`）：启动真实 Hertz 服务（127.0.0.1 临时端口）+ 真实 PostgreSQL/local store；`GET /v1/preview/{id}?version_id=...` 与 `/pages/1?version_id=...` 对两个历史版本分别返回正确 manifest/PNG；默认调用、未授权版本 404 均覆盖。
- 数据库迁移/历史状态修复：无迁移；现有 grant/资产数据无需回填。
- 真实依赖 vs 替身边界：真实 PostgreSQL、真实 Hertz HTTP 路由、真实本地 artifact store；OIDC、预览转换器与原生 oCIS 原件读取为测试替身（测试预先发布 manifest/page，避免依赖外部渲染器）。未验证真实 kkFileView/预览适配器。
- 剩余风险/阻塞：
  - owner 显式历史预览使用 `ActionReadHistory`，因此文件进入业务回收站后 owner 仍可预览其历史版本（与显式历史下载能力一致）；当前版本默认预览仍受 active 生命周期限制。
  - Web 前端未修改（按任务边界）；默认行为保持兼容，页面 URL 原本就带 `version_id`。
  - 未覆盖真实转换器重新渲染历史版本的端到端链路，仅覆盖授权/版本选择与资产读取。

## 统一验证命令与退出码

日志目录：`/Users/konghayao/code/ai/agent-clouddrive/evidence/repair-2026-10-26/logs/`

1. 专项（race，含 B07/B13/B14/B15）
   ```
   cd /Users/konghayao/code/ai/agent-clouddrive-D
   GOTOOLCHAIN=go1.25.0 DRIVE_TEST_DATABASE_URL='postgres://repair:repair@127.0.0.1:55432/repair?sslmode=disable' \
     go test -race -count=1 -run 'RepairReads|RepairMCP|RepairPreview' -v \
     ./internal/application ./internal/transport ./internal/adapters/artifactstore
   ```
   退出码 0；日志 `agent-D-repair-race.log`；无 DATA RACE。
2. 全部 Go 包
   ```
   GOTOOLCHAIN=go1.25.0 DRIVE_TEST_DATABASE_URL='...' go test -count=1 ./...
   ```
   退出码 0；日志 `agent-D-fulltest.log`（`EXIT:0`）。
3. 静态检查
   ```
   GOTOOLCHAIN=go1.25.0 go vet -buildvcs=false ./...
   ```
   退出码 0；日志 `agent-D-contracts-vet.log`。
4. sqlc / 契约零漂移
   ```
   ~/go/bin/sqlc generate          # exit 0，git status 干净
   go run -buildvcs=false ./cmd/contracts > /tmp/mcp-tools.json
   diff docs/reference/mcp-tools.json /tmp/mcp-tools.json      # exit 0
   python3 scripts/generate-openapi.py
   diff 生成前后 docs/reference/openapi.json                    # exit 0
   ```
   日志 `agent-D-contracts-vet.log`。
5. 迁移从基线升级 + 幂等
   ```
   python3 /tmp/agentD_migration_check.py
   ```
   退出码 0；日志 `agent-D-migration-check.log`：基线 8 个迁移后应用 `000012` 两次成功，表/索引/约束齐全。
6. 环境说明：本 worktree 是 linked git worktree，`go build ./...` 会因 VCS stamping 报 `exit status 128`；使用 `-buildvcs=false` 或 `go test`（不 stamp 主包）即可。这不是代码缺陷。

## 原审计反例对照（修复后预期反转）

将审计反例临时复制进对应包运行（仅把 `previewFile` 调用补上新的 `vid` 参数，不改断言），运行后立即删除，工作区保持干净。日志：`agent-D-audit-counterexamples.log`。

- `TestAuditReadsMkdirSchemaRejectsRequiredParentRevision`：FAIL，“expected current schema to reject ... got <nil>”，即 schema 已接受该字段。
- `TestAuditReadsMkdirBackendRequiresSchemaForbiddenField`：PASS，后端仍要求非根父版本，乐观并发校验未被放宽（该反例本身只证明“后端要求该字段”）。
- `TestAuditReadsCursorSwitchesPublishedGeneration`：FAIL，两页返回同一 `artifact_set_id`（默认与显式源版本两个子测试均如此），cursor 不再跟随可见指针换代。
- `TestAuditReadsPaginationRereadsWholeArtifact`：FAIL，“read=131078 delivered=131072”，不再是 8,388,608 字节的整篇重读。
- `TestAuditReadsPreviewCannotSelectBothAuthorizedHistoricalGrants`：FAIL，“expected the current implementation to reject the second explicitly authorized historical preview”，第二个合法历史版本现在可以预览。

这些 FAIL 是修复后的正确反转，不是正式验收；正式验收见上文各 Agent D 正确性测试。

## 共享文件变更建议（若未直接修改）

- `docs/architecture/engineering-handoff.md` 建议补充：`GET /v1/files/{id}/markdown` 的 `next_cursor` 是绑定 principal/file/source version/artifact/offset 的不透明值（1 小时有效）；`GET /v1/preview/{id}` 支持可选 `version_id`，fixed/latest 分享仍按各自版本策略限制。
- 可选配置：将 cursor TTL 提为 `DRIVE_READ_CURSOR_TTL`；当前为常量 1 小时。
- 若生产读取量较大，建议为 `read_cursors` 增加周期清理任务（当前每次创建 cursor 最多清理 64 条过期行）。
- `internal/application/mutations.go` 未修改；B07 依赖其现有父版本校验，未放宽。

## 合并注意事项

- 迁移顺序：`000012` 应在 C 的 `000011` 之后；本表只依赖 `000001` 的 `principals/files/file_versions/artifact_sets`，与 9/10/11 无依赖，但主 Agent 合并后必须重跑 `~/go/bin/sqlc generate`，以重新生成为准。`db/migrations/000012_read_cursor_binding.up.sql` 使用 `IF NOT EXISTS`，可重复应用。
- 契约文件 `docs/reference/mcp-tools.json`、`docs/reference/openapi.json` 不要手工合并：合并后分别运行 `go run -buildvcs=false ./cmd/contracts > docs/reference/mcp-tools.json` 与 `python3 scripts/generate-openapi.py`。
- 客户端契约变更：`markdown.next_cursor` 从十进制偏移变为不透明 token（1 小时有效），旧 cursor 会被明确拒绝（`INVALID_ARGUMENT`）。部署顺序必须是先迁移 `read_cursors`，再滚动 API；否则新代码在旧库上会返回依赖错误。
- B15 内部函数签名 `PreviewInfo`/`previewFile` 已变化；仓库内调用点仅 `internal/transport/http.go` 与本 Agent 测试，已全部更新。`PreviewPage` 仍要求显式 `version_id`。
- B13 的再授权语义：默认/current cursor 在文件 trashed 后拒绝；显式历史 cursor 仍允许 owner 读取绑定产物（与既有显式历史读取一致）。如果产品要求更严格，应单独决策，不能通过放宽 `ActionReadMarkdown` 实现。
- 测试使用真实 PostgreSQL 随机 schema；`TestRepairPreviewHTTPVersionSelection` 会在 127.0.0.1 启动临时 Hertz 监听并在结束时关闭（若 CI 禁止 loopback 监听需调整）。MCP/预览测试的 OIDC 与原生 oCIS/转换器是替身，不能当作真实部署验收。
- 未修改前端（`web/`），未部署，未合并远端 main。
