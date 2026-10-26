# Agent C 报告

分支/HEAD：`repair/C` / `4420c87d88ecaf76c73cabab8e113fb5df0e39b6`

工作目录：`/Users/konghayao/code/ai/agent-clouddrive-C`。未检查/修改前端，未修改 `converters/adapter.py`，未修改上传 reconcile、retention 对账；未切分支或合并。报告与日志按授权写入主工作区。

提交：
- `8d7da8583d23b68c9f101902e74bbb57a472e6f0` `fix(B06,B12,B17): artifact activation, page work units, and index cleanup`。提交时检查发现该实现已被父会话收录到 repair/C；没有重写或重复提交。
- `4420c87d88ecaf76c73cabab8e113fb5df0e39b6` `test(B06 B12 B17): reconstruct worker fixtures without copying locks`，本人提交测试修正，消除 go vet 的复制锁警告。

## 逐项

### B06：implemented（原生 PostgreSQL + 受控搜索替身正确性 verified；真实 Meilisearch not_run）

- 根因/触发：旧 publish_artifact 在非 active 状态完成 job/artifact，却既不激活 pointer 也不创建恢复任务；restore 只重建已有 pointer。
- 实现：
  - `db/migrations/000011_artifact_visibility_pages.up.sql`：持久 `artifact_activation_tasks`，带 state/attempt/lease_owner/token/until/retry_at/last_error；`activate_artifact` 检查已发布/已索引、current_version、祖先 active、主体状态、generation/attempt 单调性，再 CAS 可见指针。
  - 已发布产物不可变，与激活责任拆分；非 active/落败时写任务，不静默丢失恢复责任。激活前重建索引并 Wait，成功后才 CAS；旧 pointer 保留为合法回退。
  - restore 使用同迁移的生命周期 AFTER UPDATE trigger，事务内为恢复根及整个子树选择当前源版本的最新已发布/索引确认产物。无需改 `mutations.go`，降低与 B 冲突。
  - `internal/application/worker_visibility.go`：maintenance 独立 lane，租约 + token + 行锁 + owner advisory lock；inactive 延后，失败持久重试；索引副作用与初次发布/清理共用锁。
- 正确性测试（最终专项 race 全 PASS）：
  - `TestRepairWorkerCompletionDuringTrashAndRestore`：before_delete/during_trash/during_restore，走真实 Mutate delete/restore，恢复默认 Markdown/搜索一致；激活不重新读原件（删除/恢复本身的历史验证会读原件，单独计数）。
  - `TestRepairWorkerActivationFailureRestartAndDeleteRace`：索引 Wait 失败不提前激活，构造新 Service 表示重启，再次删除竞态不激活，后续恢复收敛。
  - `TestRepairWorkerAncestorRestoreWithOldPointer`：祖先删除、保留旧 pointer，恢复激活新代；固定旧产物仍可读。
  - `TestRepairWorkerLoserCleanupRestartAndLateUpsert` 同时验证旧代晚到不回滚 pointer。
- 数据库迁移/历史恢复：000011 在 fixture 全量迁移后执行；`TestRepairWorkerMigrationReentrantAndPageFence` 再执行同迁移成功。安全 backfill 只对 current_version 的 published/index succeeded 候选创建激活任务；不猜未完成索引结果、不删除历史。
- 真实依赖 vs 替身：原生 PostgreSQL 真实事务/触发器/SQL，真实本地不可变 artifact store；身份、oCIS、搜索为受控替身。未连接真实 Meilisearch，故不按全依赖 verified 关闭。
- 剩余风险/复核：跨进程 owner 锁在本轮以 SQL/受控任务覆盖，未跑真实 Meili 响应丢失。主 Agent 应复核与 B 的目录恢复生命周期投影兼容。

### B12：blocked（后端 implemented，原生 PG + fake 子进程超时续跑 verified；F 真实正常 JSONL/PDF verified；F 首事件缺元数据阻塞真实超时恢复）

- 根因/触发：整篇 Execute 返回前页结果只在内存，进程死亡丢进度；原保存路径会用最终 manifest 引用覆盖页 bytes。
- 实现：
  - `internal/ports/ports.go`：可选 PageConverter/ExecutePages 回调，不破坏旧 Converter 替身；请求新增 page_report/resume_pages，支持与 F 当前 recipe 下字段兼容；保存实际 engine.model/provider，model 顶层兼容字符串/对象。
  - `internal/adapters/converters/http.go`、`pages.go`：HTTP 和本地 Process 均增量消费，json.Decoder 连续读 JSON 值；严格验证 page/stage/state/error；旧单 JSON、紧凑 JSONL、pretty-print 多行最终 result 均兼容；整体字节预算，事件/结果顺序检查。HTTP 不再 ReadAll 等待整篇才回调。
  - `internal/application/worker_pages.go:35,110,187,200`：每页/阶段 bytes、状态、计数立即写 PG；job lease_token/attempt fencing；恢复读取累计快照，成功页排除于 resume_pages，只重试失败/缺失页；阶段增量 markdown/asset 累计，不把描述片段误当整页；实际模型不符不发布 ready。
  - 000011 加 job page_count/pages_ready/pages_failed/completeness/actual_engine，页 recorded_at，页 trigger 锁住 job 再 fencing，artifact attempt 标识不可变。
  - partial 正常索引/Wait 后可见，job 仍 partial；claim 支持 partial 定时续跑直至 max_attempts。每个 attempt 新不可变 artifact，旧 partial 固定引用保持可读。
  - `internal/application/query.go:212` 仅增加同代 attempt DESC 排序，避免显式 version 读取随机选旧 partial；未改其它查询/权限逻辑。
- 正确性测试：
  - `TestRepairWorkerPageTimeoutPersistsPartialThenResumesAfterRestart`：Go test 生成 Python JSONL fake converter，真实启动/超时杀进程；首次 partial，总页2/成功1/失败1，默认 Markdown/搜索可用；构造新 Service 重启，第二次仅跑页2，调用记录 `1\n2\n2\n`，完成 ready，旧 partial 不变。
  - `TestRepairWorkerMigrationReentrantAndPageFence`：lease 过期重领，旧 attempt 迟到被 DB 拒绝；终结后写页也被拒绝。
  - `TestRepairConversionProcessTimeoutKillAndBounds`：真实子进程 timeout/异常退出后确认已收页，非法页/缺 error/结果后事件/输出预算均拒绝。
  - `TestRepairConversionHTTPStreamsPagesBeforeResult`：httptest handler flush 首事件，最终 result 尚未发送时回调已确认，验证 HTTP 客户端是真流式，不是仅 CLI。
  - `TestRepairWorkerRealFPageProtocolPersistence`：直接运行 F worktree adapter，真实合成两页 reportlab PDF，实际 pypdf/PDFium 输出4个阶段事件，PG 存4条，正常结果 ready、正文完整合并。
  - `TestRepairConversionRealAdapterSyntheticPDF`：同 F 真实 CLI 两页文本 PDF、超页数拒绝；捕获 F 实际页事件后拼接同次 result 的 pretty-print 形式，41行连续 JSON 消费成功。对应日志见 targeted-race。
- **仍阻塞的精确问题**：F `d38677e` 的 result_ref 只有阶段 markdown/assets/pages，`completeness.total_pages`、engine 在最终 result 才出现。进程在 final 前被杀，后端不知道真实总页数，不能枚举失败/缺失页，更不能以最大已收页号猜总页数。后端保存已收 bytes，但在 `worker_pages.go:211,261` 返回 blocked/page_metadata_missing，绝不从头重跑已成功页来伪装修复。请 F/主 Agent 在首个及后续事件 result_ref 注入真实 `total_pages` 和 `engine:{name,version,model?,provider?}`；现有 fake 超时测试含该元数据，正常 F 集成结果证明字段/增量解析兼容，但不证明真实 F 超时闭环。
- 数据库/历史恢复：已发布 partial 不改写，新 attempt 留新 artifact；原基线没有持久化的成功页 bytes 无法追溯恢复，不能猜测已计费历史。blocked/page_metadata_missing 的 job 应在 producer 补元数据并可靠补齐真实总页数后受控恢复，不应直接重新全量执行。
- 未验证：真实 OCR/视觉质量、真实 F 后页超时续跑、真实 HTTP F 服务器端到端（客户端有流式替身测试）、版本切换与取消的完整应用调度组合未穷举。原生 PG fencing 与固定 job/source/recipe 绑定已验证，不能扩大成全部组合已验收。

### B17：implemented（原生 PG + 受控搜索替身正确性 verified；真实 Meilisearch not_run）

- 根因/触发：旧代码只给 winner 的 previous pointer 发清理事件，从未成为 pointer 的旧代晚到写块永久残留。
- 实现：
  - 000011 新 `artifact_cleanup_tasks`，artifact 唯一、持久租约/重试；backfill 所有非 pending 索引产物。
  - `worker.go` indexAndPublish 在第一次外部 Upsert 之前持久登记清理责任，覆盖远端响应未知及 publish 失败。
  - `worker_visibility.go` 清理前重读任何 files.search_artifact_set_id 引用；当前产物不删，未发布且仍在处理的产物等待；只删搜索块，不删任何不可变 artifact。
  - 初次 Upsert/激活/维护/清理共用 owner advisory lock。对响应未知的迟到 Upsert，不把一次清理当终结：清理任务每30秒持久复核，保证最终收敛；这不是即时远端 fencing，也不是零残留窗口承诺。
- 正确性测试：`TestRepairWorkerLoserCleanupRestartAndLateUpsert`：新代先完成、旧代晚到，pointer 不倒退；模拟删除失败，重启并重试后落败块清空，winner 仍可搜索；人为把旧块在清理后重新 Upsert，下一持久复核再次清空；固定旧 artifact Markdown 仍可读。
- 数据库/历史修复：迁移只创建重建型清理任务，不删除历史 artifacts；现有落败 indexed/published 产物自动进入补偿责任。
- 真实依赖 vs 替身：原生 PG + 本地 artifact 真实，搜索调用受控 fake；真实 Meili 异步 task 行为/大规模排序遮挡 not_run，未写成实测。
- 剩余风险：保留30秒低频复核增加长期索引清理负担，尚无大规模成本实测；真实远端已派发操作可能短暂再次出现，但 PostgreSQL pointer 二次过滤仍有效，复核最终删除。

## 最终验证命令与日志

环境每个 Go 命令：`GOTOOLCHAIN=go1.25.0`；PG 使用 brief 指定的原生本地测试数据库，测试随机 schema；真实 F 联调附加：
`DRIVE_TEST_CONVERTER_ADAPTER=/Users/konghayao/code/ai/agent-clouddrive-F/converters/adapter.py`。

1. `go test -race -count=1 -run 'RepairWorker|RepairConversion' -v ./internal/application ./internal/adapters/converters ./internal/adapters/search` → **exit 0**。7个 application 顶层测试（含3个时序子场景），3个 converter 顶层测试；search 本轮筛选无匹配测试。日志：`evidence/repair-2026-10-26/logs/agent-C-targeted-race.log`。
2. `go test -count=1 ./internal/application/... ./internal/adapters/converters ./internal/adapters/search ./internal/ports` → **exit 0**。日志 `agent-C-affected-full.log`。依赖 opt-in 测试可能 skip，不把 skip 当真实外部服务通过。
3. `go vet ./...` → **最终 exit 0**，`agent-C-vet.log` 为空即无诊断。初次 vet 发现新测试复制 Service 内 Mutex，已在4420c87修正，没有提高超时或放宽断言。
4. `~/go/bin/sqlc generate && git diff --exit-code -- internal/dbgen` → **exit 0**，生成物零漂移，日志 `agent-C-sqlc-drift.log`。
5. `git diff --check` → **exit 0**；最终测试修正 commit 完成后工作树干净。

真实运行说明：测试实际启动 /tmp/repair-venv/bin/python 子进程，运行合成 PDF 的真实 F adapter（正常 + 页数上限），实际超时 kill fake JSONL producer，实际访问原生 PG，非静态方案。旧 C adapter 直接 CLI 在 macOS RLIMIT_AS 会报 ValueError；默认兼容测试通过 import 真实 execute 只验证内容，使用 F env 的最终联调直接跑 F CLI（F 已有 macOS 限制兼容）。不以内容测试声称沙箱/生产部署验收。

## 与 Agent F 协议对齐 / 共享文件变更建议

- 请求顶层 page_report/resume_pages，并兼容 recipe 同名字段（F 当前实际使用 recipe）。recipe 快照不可变，调度参数只在请求副本设置。
- 全局1-based页号；render/text/ocr/description；ready/failed/not_required；failed 必须 error.code；失败页 partial，不允许超页数截断 ready。
- result_ref 是阶段新增片段，PG 存累计页快照；最终完整 result 可以单行或多行，均由 json.Decoder 解析；非法 page 事件不降级接受。
- model 顶层可以对象或字符串，engine.model/provider 是实际模型元数据；recipe.model 不一致 blocked/model_unavailable。
- **F 必补首事件元数据 total_pages + engine**，否则 B12 真实超时恢复仍 blocked。正常输出联调及 pretty-print 回归通过不解除此阻塞。
- completeness.not_processed_pages 由后端已持久成功页与本次返回 pages 合并重算缺页，不把只处理某些页的结果错误标整篇完成。

## 合并注意事项

- 主 Agent 若已合入8d7da858，只需追加4420c87的测试修正，不要重复 cherry-pick 实现。
- 000011 编号已按共享设计保留；合并 A/B/D/E 后统一 sqlc generate，生成物冲突以重新生成为准。
- query.go 只一处 attempt DESC 排序，与 D 的读取修复做最小文本合并。
- 没有修改 mutations.go；restore 激活任务由数据库 trigger 与恢复事务绑定，目录恢复对子树入队。
- B06/B17 不以真实 Meili 未跑结果标全依赖 verified；B12 不应在 F 元数据补齐并重跑真实超时恢复前关闭。
