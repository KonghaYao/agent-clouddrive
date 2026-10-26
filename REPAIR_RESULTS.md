# 后端修复执行记录

状态：实现完成，按真实依赖边界验收（2026-10-26）。本文件只记录实际运行的验证；未运行或被跳过的检查不记为通过。

- 审计 SHA：`2d3b4fff498f4a3b95922d323687977c5225c0d4`
- 实施起点 SHA：`2d3b4fff498f4a3b95922d323687977c5225c0d4`（main，工作区干净）
- 修复分支：`repair/backend-audit-2026-10-26`（主集成）
- 集成基线：`ae04324`（A/B/C/D/E/F 合并后；本文件与 `taskboard.json` 的文档提交在其后）
- 服务版本与测试环境：
  - Go `go1.25.0`（`GOTOOLCHAIN=go1.25.0`），sqlc v1.29.0
  - 原生 PostgreSQL 17.11（`127.0.0.1:55432`，测试使用随机 schema）
  - Python 3.12.8（`/tmp/repair-venv`，converter 测试依赖按 pin 安装）
  - Meilisearch v1.55.0、NATS（集成测试）
  - MinIO（真实 S3 兼容 Range/416 验证）
  - demo compose：oCIS 8.2.2 / Keycloak 26.8.1（E 的隔离 realm、B 的 oCIS 原子用例；未修改 demo 业务库）
- 共享设计：`/tmp/repair-work/SHARED_DESIGN.md`（四项共享契约，作过程材料保留，不进入产品代码）
- 前端：未检查、未修改；所有改动均在 Go 后端、迁移、`converters/` 与部署网关。

## 结论摘要

- B01–B22 全部有实现、迁移/回填方案与正确性测试；其中 **10 项 verified**（B03、B04、B07、B10、B13、B14、B15、B16、B20、B22），**12 项 implemented**（B01、B02、B05、B06、B08、B09、B11、B12、B17、B18、B19、B21）。
- `implemented` 不等于验收通过：这些项的核心缺陷已用原生 PG/生产 HTTP/adapter 子进程等验证，但仍有明确未跑的真实依赖（见“剩余阻塞”），因此不标 `verified`。
- 原审计测试的 PASS 表示“缺陷复现成功”，**没有**被当作修复通过。正式验收使用新增的正确性断言测试（`TestRepair*`、`ReviewAudit*`、`converters/tests/test_repair_pages.py`）。
- 所有新增迁移已在“真实基线库副本（000001–000008 + demo 数据）”上成功应用；未对生产/demo 业务库执行迁移。

## 逐项结果

| 编号 | 级别 | 主责 | 状态 | 提交/关键文件 | 正确性测试与日志 | 升级/历史状态恢复 | 剩余阻塞 |
|---|---|---|---|---|---|---|---|
| B01 | P1 | A | implemented | `4a11935`、`d603f82`；`upload.go`、`http.go`、`worker.go`、迁移 000009 | 原生 PG：挂起 PATCH→abandon 拒绝、并发准入、响应丢失不释放；本机 TCP 生产 tus handler 实测。`agent-A-*.log` | 000009 标记历史未决上传 `legacy_dispatch_unknown`，保持占用；不按年龄/404 猜终结 | 真实 oCIS 挂起 PATCH/响应丢失未重演；无终结证据的自动取消回收未实现（有意 fail-closed） |
| B02 | P1 | B | implemented | `2493199`、`a6b3adb`；`mutations.go`、`worker.go`、迁移 000010 | 原生 PG：目标父/祖先改名、删除、移动六种竞争；真实 oCIS MOVE/MKCOL/LookupChild。`agent-B-*.log` | 000010 回填 source/target/ancestor holds；历史错路径按当前 parent 链受控重锚或补偿 | 完整历史异常矩阵（多级祖先、重复对账、重启组合）未全跑 |
| B03 | P1 | B | verified | `2493199`、`a6b3adb`；`batch/batch.go`、`worker.go`、迁移 000010 | 原生 PG：N=1 单连接、并发批次+普通读取、租约接管/重放 | 无业务回填；遗留批次经 lease 过期可接管 | 无（本项范围） |
| B04 | P1 | A | verified | `4a11935`、`d603f82`；`worker.go`、迁移 000009 | 原生 PG：18 个混合失败任务、双 reconciler、租约过期；原子领取即退避 | 无专门迁移；停滞上传经 `retry_at`/lease 重新可领取 | OS 级 kill 未跑；重启用新 Service 实例验证 |
| B05 | P1 | A | implemented | `4a11935`、`d603f82`；`upload.go`、`worker.go`、迁移 000009 | 原生 PG+替身：完成证据绕过过期 tus HEAD；版本/配额/转换幂等 | 历史已完成但 tus HEAD 过期者按持久完成证据推进；无证据保持门禁 | 真实 oCIS 过期会话验收未跑 |
| B06 | P1 | C | implemented | `8d7da858`、`4420c87`；`worker_visibility.go`、`mutations.go`、迁移 000011 | 原生 PG+搜索替身：删除期间完成→延迟激活、恢复→重放、代次 CAS。`agent-C-*.log` | 000011 建 activation 队列并回填非 active 文件的已发布 artifact | 真实 Meilisearch 激活链路未单独跑 |
| B07 | P1 | D | verified | `7d5b99b`、`8b625d7`、`600e561`；`mcp.go`、`domain/mcp.go`、契约 JSON | 官方 MCP SDK+真实 PG：嵌套 mkdir 必须显式父 revision；缺失/过期/类型错误被拒；契约零差异 | 无迁移；旧客户端需补 `expected_parent_revision` | 真实 oCIS MKCOL/OIDC 为替身（不影响参数契约缺陷） |
| B08 | P1 | B | implemented | `2493199`、`a6b3adb`；`worker.go`、迁移 000010 | 原生 PG+真实 oCIS MKCOL 响应丢失恢复；并发首次删除、身份不符门禁 | 000010 为 retention_roots 增加 lease/retry/attempt；卡住任务可重领取 | 生产后台身份完整链未端到端验证 |
| B09 | P1 | E | implemented | `4c881bb`；`audit.go`、`access/content.go`、`ports.go`、审核能力 | 真实 vault+原生 PG：trash/祖先 trash 固定字节、授权撤销/过期、越权/并发撤销 | 无迁移；已有 grant 在下一次读取按新能力对象校验 | 真实 oCIS+Keycloak 离线授权整链未跑 |
| B10 | P1 | E | verified | `4c881bb`、`9634a8f`；Keycloak 事件适配器、`identity_events.go`、迁移 000013 | 真实 Keycloak 26.8.1 隔离 realm+PG：零访问期间 disable→enable、旧公开/用户/组分享永久撤销；重放/重启/清空/缺口 | 000013 增加 epoch/事件游标；首次缺口保守撤销旧分享，需人工重新确认 | 与真实 oCIS 资源鉴权组合未跑 |
| B11 | P1 | F | implemented | `d38677e`；`converters/adapter.py`、`test_repair_pages.py` | 真实多帧 TIFF：每帧一页、帧资产/预览；全量 converters 67 tests | 无迁移；旧多帧 TIFF 的 `total_pages=1` 不可反推，按扩展名受控重建 | 真实 PaddleOCR/kkFileView 未跑 |
| B12 | P1 | C | implemented | `8d7da858`、`4420c87`；`worker_pages.go`、`worker.go`、`converters` 适配、迁移 000011 | 原生 PG+fake 子进程超时：调用 1→2→2、成功页不重跑、partial 发布；真实 adapter PDF 每页事件含 `total_pages`/`engine` | 000011 建页工作单元；旧任务无页证据只能重跑，已落盘页可跳过 | 真实进程在最终 result 前被 kill 的端到端续跑未跑；F 元数据缺口已消除 |
| B13 | P2 | D | verified | `b02cf14`；`query.go`、`http.go`、`read_cursor.go`、迁移 000012 | 真实 PG：固定 artifact/版本、逐页重授权、篡改/跨主体/跨文件/过期/版本切换 | 000012 新表；旧十进制 cursor 失效（fail closed），客户端重新分页 | 无（TTL 1h；未设每主体行数上限） |
| B14 | P2 | D | verified | `b02cf14`、`5c5da6d`；`ports.go`、`artifactstore`、`preview.go` | 真实 MinIO：4 MiB 两页 `read=131078 delivered=131072 opens=2`；206/416；本地短读/EOF/字符边界 | 无迁移 | 云 S3 未测；MinIO S3 兼容端已验证 |
| B15 | P2 | D | verified | `4a053b9`、`8b625d7`；`preview.go`、`query.go`、`http.go` | 真实 PG+Hertz HTTP：v1/v2 固定 grant 分别预览、跨版本/撤销/匿名固定 grant 被拒 | 无迁移；历史预览按明确 `version_id` 重新解析 | 预览转换器为替身（版本授权缺陷已覆盖） |
| B16 | P2 | B | verified | `2493199`、`a6b3adb`；`mutations.go`、`access/*` | 原生 PG：restore-only 根/嵌套恢复、权限拒绝、父版本/同名冲突；未新增 write 能力 | 无迁移 | 无（本项范围） |
| B17 | P2 | C | implemented | `8d7da858`、`4420c87`；`worker_visibility.go`、迁移 000011 | 原生 PG+搜索替身：迟到 Upsert 周期复核、引用保护、重试；固定旧产物仍可读 | 000011 建 cleanup 队列并回填；无引用才删可重建索引文档 | 真实 Meilisearch 清理路径未跑 |
| B18 | P2 | F | implemented | `d38677e`；`adapter.py`、`test_repair_pages.py` | 真实 PDF 直线/路径检测；本地 vision HTTP 替身；图表页缺模型时 partial | 无迁移；旧图表页 `description=not_required` 需受控重建候选代次 | 外部视觉模型未配置 |
| B19 | P2 | F | implemented | `d38677e`；`adapter.py`、`test_repair_pages.py` | 本地 provider 替身捕获真实 payload；`engine.model/provider`；不可用→`blocked/model_unavailable` | 无迁移；旧 manifest 模型不一致无法反推，按部署时间窗受控重建 | 外部 provider 未连接 |
| B20 | P2 | F | verified | `d38677e`；`adapter.py`、`test_repair_pages.py` | 真实子进程超时/SIGKILL+真实文件系统：attempt 目录隔离、孤儿回收、退出清理 | 无迁移；旧版平铺临时文件需一次性人工清理（命令见报告） | 无（本项范围） |
| B21 | P2 | F | implemented | `d38677e`；`adapter.py`、`test_repair_pages.py` | 本地真实文件系统：输入/输出/临时分别限额；合法输入完整暂存；写失败清理残片 | 无迁移；旧截断残片由 B20 清理覆盖 | 真实 kkFileView/LibreOffice 与大文件全尺寸未跑 |
| B22 | P2 | E | verified | `9634a8f`；`deploy/gateway/nginx.conf`、`compose.yaml`、`http.go` | 真实 nginx+TLS+显式代理 CIDR：直连 TLS、可信代理、伪造 `X-Forwarded-Proto`、开发 HTTP 路径 | 部署需显式配置受信代理 CIDR；无数据迁移 | 完整浏览器登录链路未跑 |

## 运行记录（实际退出码）

| 日期 | 命令 | 退出码 | 环境/边界 | 日志 |
|---|---|---|---|---|
| 2026-10-26 | `GOTOOLCHAIN=go1.25.0 go vet ./...` | 0 | 基线，无 DB | `evidence/repair-2026-10-26/go-vet-baseline.log` |
| 2026-10-26 | `DRIVE_TEST_DATABASE_URL=… go test -race -count=1 -timeout=900s ./...` | 0 | 基线，原生 PG 17.11 | `evidence/repair-2026-10-26/logs/go-test-baseline-with-db.log` |
| 2026-10-26 | 同上 + `DRIVE_TEST_NATS_URL` + `DRIVE_TEST_MEILISEARCH_URL`，合并后全量 | 0 | 原生 PG+NATS+Meilisearch；所有包 `ok` | `evidence/repair-2026-10-26/logs/integration-all-fulltest.log` |
| 2026-10-26 | `GOTOOLCHAIN=go1.25.0 go vet ./...` | 0 | 合并后 | `evidence/repair-2026-10-26/logs/integration-all-vet.log` |
| 2026-10-26 | `~/go/bin/sqlc generate`；`git status` | 0 | 5 个合并迁移后零漂移 | 本记录 + `git status` |
| 2026-10-26 | `python -m unittest discover -s converters/tests` | 0 | 67 tests（65 pass，2 skip） | `evidence/repair-2026-10-26/logs/integration-all-python.log` |
| 2026-10-26 | `python -m unittest discover -s deploy/compose/tests` | 0 | 8 pass，1 个 RustFS 实跑 skip（E 的 nginx 环境） | `agent-E-*.log` |
| 2026-10-26 | 真实 MinIO `TestRepairS3MinIORangeIntegration` | 0 | 真实 S3 兼容 206/416/If-Match 416 | `agent-D-*.log` |
| 2026-10-26 | 真实 Keycloak `repair_keycloak_live.py` | 0 | 隔离 realm，未改 demo | `agent-E-*.log` |
| 2026-10-26 | 真实 nginx 安全测试 | 0 | TLS/代理 CIDR/伪造头 | `agent-E-*.log` |
| 2026-10-26 | 真实 oCIS MKCOL/MOVE/LookupChild 原子用例 | 0 | demo oCIS 8.2.2；未改业务库 | `agent-B-*.log` |
| 2026-10-26 | 真实 adapter 2 页 PDF `page_report` | 0 | 首事件 `total_pages=2`、`engine=pdf-page-pipeline`；5 行 JSONL | 本记录复跑 |
| 2026-10-26 | 基线库副本应用 000009–000013 | 0 | demo `clouddrive`（000008）克隆为 `repair_upgrade`；3 个未决操作全部有 holds、3 个 legacy 上传标记、6 张新表 | 见“迁移升级” |
| 2026-10-26 | 重复应用 000009 | 0 | `IF NOT EXISTS` 可重入 | 见“迁移升级” |

## 迁移升级与历史异常恢复

新增迁移（按合并顺序）：

1. `000009_upload_write_occupancy.up.sql`：PATCH attempt/epoch、abandon 请求、完成证据、未决上传 `legacy_dispatch_unknown`。
2. `000010_lifecycle_occupancy.up.sql`：`file_operation_holds`、批处理/retention 租约与 fencing，未决操作回填 holds。
3. `000011_artifact_visibility_pages.up.sql`：activation/cleanup 队列、页工作单元、非 active 文件已发布 artifact 的延迟激活、清理回填。
4. `000012_read_cursor_binding.up.sql`：不透明分页 cursor（只存摘要，绑定 principal/file/version/artifact/offset）。
5. `000013_identity_epoch_events.up.sql`：`principals.identity_epoch`、事件游标、share/audit grant 的 epoch 绑定。

真实升级验证：将 demo `clouddrive`（迁移到 `000008`，含真实数据）克隆到独立 `repair_upgrade`，依次应用 000009–000013，全部 `rc=0`。结果：

- 6 张新表存在；`file_operations` 中 3 个未决操作 → `file_operation_holds` 覆盖 3 个；
- 3 个未决上传被标记 `legacy_dispatch_unknown`（保持门禁，未释放）；
- 21 个 trashed/deleting 文件无已发布 artifact，activation/cleanup 回填为 0（符合数据）；
- `principals.identity_epoch` 默认 0；无历史 share/audit grant（demo 数据为空）；
- 000009 重复应用 `rc=0`（其余迁移按迁移器只前向执行一次）。
- 未对 demo/生产业务库执行迁移，未做永久清除。

历史坏状态恢复原则（不猜、不原地改不可变产物）：

- **在途/未知远端写**：保持占用与 `legacy_dispatch_unknown`，由 worker 在拿到原生终结证据后推进；不能按年龄/404 自动释放。
- **错路径/remote_confirmed**：按当前目标身份/parent 链重锚，或在原目标仍 active 时补偿回滚；无法安全收敛者保留门禁并要求人工/原生对账。
- **删除期间完成的 artifact**：迁移为 activation 任务，文件恢复后由 worker 索引确认→CAS 指针；不提前暴露。
- **迟到索引/落败代次**：迁移为 cleanup 任务，确认无任何文件引用后才删可重建的搜索文档；固定旧 artifact 不删。
- **旧分页 cursor**：全部 fail closed（客户端重新分页），不做十进制兼容。
- **旧分享**：epoch 迁移后，首次缺失事件游标的部署保守撤销旧分享，需人工重新确认；启用不复活。
- **多帧 TIFF/图表 PDF/模型不一致产物**：只提供受控重建候选（按扩展名、`total_pages=1`、矢量绘图、部署时间窗），不原地改 manifest/recipe。
- **旧版平铺临时文件**：不在新命名空间，需一次性人工清理（B20 报告给出命令）。

## 剩余阻塞（均为未跑的真实依赖，不影响已列出的正确性断言）

1. 真实 oCIS 故障注入：B01 挂起 PATCH/响应丢失、B02 完整历史矩阵、B05 过期会话、B08 生产后台身份、B09 真机授权整链。
2. 真实 Meilisearch：B06 激活链路、B17 清理链路（全量测试带 Meili URL 通过，但这两条专用路径未单独断言）。
3. 真实 OCR/预览/模型供应商：B11 PaddleOCR/kkFileView、B18 外部视觉模型、B19 外部 provider、B21 kkFileView/LibreOffice 与大文件全尺寸。
4. OS 级 SIGKILL：B01/B04/B12 用新 Service 实例与 fake 子进程验证恢复，未做内核 kill 重演；B12 真实进程在最终 result 前被 kill 的端到端续跑未跑。
5. 云 S3：B14 已在真实 MinIO 验证 Range/416，未连云厂商 S3。
6. B22 完整浏览器登录未跑（TLS/代理/Cookie 安全断言已跑）。

## 审计测试与验收口径

- 原始审计测试的 PASS = 缺陷复现成功，不作为修复验收。
- 正式测试断言修复后的正确行为；若把旧审计反例（断言缺陷仍存在）重新运行，预期会失败，这是修复生效的旁证，不是回归。
- 报告中 `implemented` 项的核心缺陷都有针对性正确性测试；但按上述真实依赖边界，未标记 `verified`。
- 所有证据、命令与边界见 `evidence/repair-2026-10-26/` 与各 `agent-*.md`；本记录不替代真实运行日志。
