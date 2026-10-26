# Agent B 报告

分支/HEAD：`repair/B` / `a6b3adb025f1b3542443e5e7456090fd3e7346fb`

工作区：`/Users/konghayao/code/ai/agent-clouddrive-B`。未检查或修改 `web/`，未切换分支、merge、push 或替换既有 API 部署。主工作区仅写本报告和授权证据日志。

提交：
- `2493199` — `fix(backend): repair B02 B03 B08 B16 lifecycle ownership and recovery`
- `a6b3adb` — `test(backend): verify B02 B03 B08 B16 with PostgreSQL and native oCIS`

## 验证命令与结果

以下命令均先 `cd /Users/konghayao/code/ai/agent-clouddrive-B`；Go 使用 `GOTOOLCHAIN=go1.25.0`，原生 PostgreSQL 使用 brief 给定的 `DRIVE_TEST_DATABASE_URL`，测试逐个创建随机 schema 并应用全部迁移。数据库实测为 PostgreSQL 17.11（aarch64），不是 PGlite。

日志目录均为主工作区 `evidence/repair-2026-10-26/logs/`。

| 检查 | 命令（环境变量说明见下） | 退出码 | 日志 |
|---|---|---:|---|
| 最终正确性 | `go test -race -count=1 -run 'RepairLifecycle\|RepairBatch\|RepairRetention' -v ./internal/application/...` | 0 | `agent-B-final-correctness.log` |
| 原生存储＋正确性 | 设置 `REPAIR_NATIVE_WRITES=yes REPAIR_NATIVE_CREDENTIAL_FILE=/tmp/agent-B-native-private/credential.json REPAIR_NATIVE_OCIS_URL=http://127.0.0.1:59212` 后运行上述命令 | 0 | `agent-B-targeted-native-final.log` |
| 受影响包完整测试（PG；未提供 NATS/Meili） | `go test -race -count=1 -v ./internal/application/... ./internal/adapters/delegation` | 0 | `agent-B-affected-full.log` |
| 扩展完整测试（PG＋NATS＋Meili） | `DRIVE_TEST_NATS_URL=nats://127.0.0.1:14222 DRIVE_TEST_MEILI_URL=http://127.0.0.1:17700 go test -race -count=1 -v ./internal/application/... ./internal/adapters/delegation` | 1 | `agent-B-affected-final.log` |
| 最终静态检查 | `go vet ./...` | 0 | `agent-B-vet-delivery.log` |
| 实际编译 | `go build -buildvcs=false ./cmd/...` | 0 | `agent-B-build-no-vcs.log` |
| 普通编译 | `go build ./cmd/...`，随后 `go build -x ./cmd/...` 定位 | 1 | `agent-B-build-final.log`、`agent-B-build-diagnostic.log` |
| SQL 生成 | `~/go/bin/sqlc generate` | 0 | 生成物已提交 |
| patch 格式 | `git diff --check` | 0 | 最终命令回执 |

最终正确性执行了 12 个修复测试入口及其子用例，全部通过；本轮未设置显式原生写开关，因此 `TestRepairLifecycleNativeOCIS` 在此条命令中正常 skip，但已在独立真实运行日志中成功执行（不是以 skip 代替验收）。最后新增的 `TestRepairBatchConcurrentReplayAndFence` 也已通过。此前原生运行包含其余所有修复用例，之后没有修改生产实现。

扩展完整测试唯一观察到的失败是现有 `TestWorkerPostgresNATSMeiliIntegration`：20 秒后 `conversion did not publish queued context deadline exceeded`。其余受影响包测试、vault 测试通过。未修改 Agent C 的 conversion/index/publish 链路；该扩展集成整体为 **blocked**，不声称全套绿。

普通 build 失败原因由 `-x` 证据确认：Go 的 VCS 探测在 `/Users/konghayao/code` 执行 `git status --porcelain` 并返回 128；指定 worktree 的 git status/log 正常。未修改 git config，用 `-buildvcs=false` 验证编译成功。最早一轮 vet 发现新增测试复制含 Mutex 的 Service，已改为显式新建 Service，最终 vet 通过。

## 逐项

### B02：implemented（核心竞争与收敛路径已验证，完整历史异常矩阵尚未关闭）

- 根因：源文件 active gate 不覆盖远端 MOVE 的目标父目录；事务外 I/O 期间目标树可变；提交使用旧 target_path。
- 实现：
  - `db/migrations/000010_lifecycle_occupancy.up.sql:2` 新增 `file_operation_holds`、复合主键和按 file 索引。插入 operation 时在数据库触发器内以 parent_id 递归检查源子树未决 holds，记录 source/target_parent/ancestor；终态 holds 保留审计但不阻塞。对历史未决记录安全 backfill。
  - `internal/application/mutations.go` 在空间事务锁后重读父目录及当前 path，再构造 intent；提交 `commitMetadataMutationFenced`（约第 373 行）验证父 owner/space/native ID，按当前父路径重算，检测同名冲突。
  - `internal/application/lifecycle_convergence.go:16` / `:81` 为历史失效目标提供补偿 MOVE，先持久 compensation_plan、占用旧父 ancestry、预约补偿路径；重启先 LookupChild 验证稳定资源 ID，再决定是否 MOVE；写回 fenced。返回显式 `compensated`，不把补偿伪装成原请求成功移动。
- 正确性测试：
  - `TestRepairLifecycleInboundHolds`：挂起 MOVE，目标父/祖先分别 rename、move、delete，六种竞争返回 OPERATION_PENDING；源 MOVE 响应丢失后以新 Service 对账两次，parent_id/path/原生树一致，仅一个 MOVE。
  - `TestRepairLifecycleHistoricalTargetConvergence`：模拟升级前目标变更；改名按身份重算，已删除目标补偿到旧位置；补偿响应也丢失后再次对账，不重复 MOVE，不滞留 remote_confirmed。
  - `TestRepairLifecycleNativeOCIS`：真实 MOVE 前挂起，父 rename 明确拒绝；真实 LookupChild 检查资源身份。
  - 命令与退出码：最终正确性与原生运行均 0，见上表。
- 数据库迁移/历史状态：000010 backfill 不释放未知操作；已完成移动按稳定身份收敛，失效目标按旧位置补偿。原旧位置也冲突/失效时保持门禁并要求人工消歧，不能猜结果。
- 真实/替身：原生 PG 并发、约束和迁移均真实；六种确定性交错和历史坏状态用明确的内存原生树替身。真实 oCIS 覆盖父 rename 竞争和正常移动，并非全部历史删除组合。
- 剩余风险：restore 历史失效目标的补偿分支已实现，但没有专项测试；旧位置也失效/重名时仍需人工受控恢复；没有执行真实 SIGKILL 整个 backend 进程，重启由新 Service＋持久状态模拟。需主 Agent 交叉复核补偿计划在进一步异常目标变化下的可恢复性，不将整项标 verified。

### B03：verified（本项连接池、幂等与租约验收范围）

- 根因：长持 session advisory-lock 连接，循环与 Executor 又向同一池借连接，形成 N/N+1 自等待。
- 实现：`internal/application/batch/batch.go:29`、`:106` 删除 Acquire/session lock，短 UPDATE CAS 领取 batch lease；90 秒预算短于两分钟租约；执行期间不持连接；item claim、blocked/result、完成状态均带 lease_token/lease_until 条件，过期结果明确 LeaseLost。取消后保留项目状态，稳定 item 幂等键用于接管；defer 只释放自己持有的租约，崩溃则等租约到期。
- 迁移：000010 增加 `batch_operations.lease_owner/lease_until/lease_token`；`db/queries/batches.sql` 加 ClaimBatchLease 并生成 sqlc。
- 测试：
  - `TestRepairBatchSingleConnection`：pool_max_conns=1，五秒预算内完成并重放一次，只有一个原生 MKCOL。
  - `TestRepairBatchConcurrentBatchesExhaustPool`：pool 容量 2，两批同步停在 Executor，普通 SELECT 仍可完成，然后两批均完成。这是原审计未运行并发场景的原生 PG 正确性版本。
  - `TestRepairBatchConcurrentReplayAndFence`：同批并发只执行一次；人为升 token 后旧 holder 不得写回结果。
  - `TestRepairBatchCancellationAndLeaseTakeover`：副作用完成但保存结果前取消，模拟崩溃租约；未到期仅返回 snapshot，到期接管通过稳定 key 不重复副作用。
  - `TestRepairLifecycleNativeOCIS`：真实 MKCOL 的单连接 batch 完成。
  - 最终正确性命令退出码 0，真实存储命令退出码 0。
- 历史恢复：旧 running 批次 lease_until 默认 NULL 可由相同请求接管；保留原 batch/item/key 与成功结果。当前恢复入口是客户端同批重放，不新增独立 batch 调度器。
- 边界/风险：远端调用须尊重 context，幂等由已有文件 operation 状态机承担；没有用扩池修复，也未执行 OS 级 kill。验证并发所有权与接管是原生 PG，不是 PGlite。

### B08：implemented（恢复行为及真实 oCIS 运行通过，生产后台身份完整链尚未端到端验证）

- 根因：retention_roots 的 executing/outcome_unknown 没有恢复消费者，导致用户全部删除卡住。
- 实现：
  - 000010 为 retention_roots 增加 lease/fencing、retry_at/attempt/last_error、固定 directory_name/native_parent_id。
  - `internal/application/retention.go:12` 领取租约；`:22` 根据固定空间根及保留名称 LookupChild，验证 directory/space/resource；不存在才在持久 executing 后重试 MKCOL，冲突再查询，身份不符保留 unknown。失败推进 retry，保存无敏感信息的诊断码；重复对账幂等。
  - `internal/application/retention.go:85` 新增 maintenance 恢复入口；`internal/application/worker.go` 任务列表仅增加该函数，未触及上传/转换逻辑。
  - `internal/adapters/delegation/vault.go:176` 增加严格绑定 owner、retention operation、有效租约、space 和固定 native root 的内部任务授权；不授权任意文件或普通读取绕过。
- 测试：`TestRepairRetentionCrashAndLostResponse`（调用前崩溃/真实副作用后响应丢失/新 Service 重启，删除继续且仅一个保留根）、`TestRepairRetentionConcurrentFirstDeletes`、`TestRepairRetentionIdentityMismatchAndFence`（错误原生对象维持门禁，旧 token 写回被拒绝）。`TestVaultPostgresIntegration` 增加有效租约成功、错误资源拒绝、过期租约拒绝。
- 真实运行：真实 oCIS MKCOL 已成功后人为丢弃响应，PostgreSQL 保留 unknown；新 Service 对账采纳同一个真实 native resource；重复对账后删除和恢复继续。不是只清门禁。
- 历史状态：已有 executing/unknown 默认进入 due；首次恢复固定保留目录和 root；不能从元数据安全确认时 last_error 保留并继续安全门禁。详见下方受控恢复步骤。
- 未验证/风险：真实存储测试为已完成 PKCE/token-exchange 的用户原生令牌提供后台凭据；生产 vault refresh/交换/retention worker 全链未在真实 Keycloak 上组合运行（vault SQL 授权用原生 PG＋mock IdP 已通过）。老数据未保存 directory_name 时只能使用当前部署 TrashDir，升级时必须确认未改过保留目录配置。没有执行真实进程 SIGKILL。

### B16：verified（恢复 scope 与目标权限范围）

- 根因：非根 Parent 硬编码 ActionWrite，导致 restore-only 恢复到子目录失败。
- 实现：`internal/application/access/files.go:63`、`:67` 新增 RestoreParent，校验 restore scope、owner、space、file_is_active（含祖先）和 directory/native ID，不改 scopes；mutations 根据 restore 调用该入口，并保留事务内父修订/同名/循环检查。
- 测试：`TestRepairLifecycleRestoreOnlyScope` 覆盖根/子目录仅 restore 成功、无 restore 拒绝、错误父版本拒绝、同名拒绝；恢复后 mkdir 仍 InsufficientScope。`TestRepairLifecycleRestoreParentDenials` 覆盖其他 owner 与已删除祖先拒绝。真实 oCIS 场景 restore-only 在子目录恢复并保持资源 ID。
- 命令：最终正确性、真实存储命令退出码均 0；没有自动增加 drive.write，也不改变普通 Parent 调用者。
- 数据库：本项无需额外迁移。未跑真实 OAuth 下只申请 restore scope 的完整 HTTP/MCP 客户端闭环；业务 Actor scope 与真实 MOVE 已分别覆盖。

## 历史坏状态的受控恢复与诊断

禁止对 unknown 直接改 ready/failed、清 active_operation_id 或删除 holds。000010 可重入测试已通过；升级前应停旧 writer，先备份，确认 TrashDir 没有历史变更，再应用 migration 并启动新 maintenance。

retention 诊断（仅输出非敏感身份/状态）：

```sql
SELECT space_id,operation_id,state,native_resource_id,directory_name,native_parent_id,
       attempt,last_error,retry_at,lease_until
FROM retention_roots WHERE state <> 'ready';
```

当 `LOOKUP_CAPABILITY_REQUIRED`/`DELEGATION_UNAVAILABLE` 时，先修复 adapter 或 owner 的有效离线授权；不能清门禁。`NATIVE_IDENTITY_MISMATCH`/`PARENT_IDENTITY_MISMATCH` 时，操作者须通过原生管理接口确认固定空间、父 root、保留目录为 directory、实际 native ID，并把操作票据和原生查询证据存入受控审计。对错误旧配置仅校正保留路径/父身份，不直接声明 ready，然后重新对账：

```sql
BEGIN;
-- 在工单中保存 SELECT 结果与操作者/原生证据；下列参数由核验结果提供。
SELECT * FROM retention_roots WHERE space_id = :space_id FOR UPDATE;
UPDATE retention_roots SET directory_name=:verified_name,
  native_parent_id=:verified_parent_id,retry_at=now()
WHERE space_id=:space_id AND state<>'ready'
  AND (lease_until IS NULL OR lease_until<=now());
COMMIT;
```

由 worker 或带有效用户令牌的下一次删除重试进入相同恢复流程，最终仍须原生查询证据才能 ready。未确定真实身份时维持 unknown。

file_operations 诊断：查询未决 `state/intent/remote_result/retry_at/lease_until` 与 holds，再用官方 LookupChild/Stat 按稳定 ID 核对。旧 parent 已改名的操作会重算路径；旧 parent 已失效则补偿到旧位置。若旧位置也冲突，暂停该空间写入，人工通过不覆盖的官方 MOVE 将资源恢复到经确认的旧位置并保存证据，再让 maintenance 查证并提交补偿。不要直接篡改不可变 intent 或操作终态；无法安全恢复的记录继续保留门禁并升级人工工单。

## 真实依赖与环境清理

- 原生 PG 的随机 schema、全部迁移、约束、多连接/单连接语义均实际运行。
- 真实 oCIS 使用既有 disposable demo 的合成 owner 账户，经现有 PKCE/token-exchange 流程取得原生凭据；凭据只写仓库外 `/tmp/agent-B-native-private/credential.json`（0600），从未打印/提交。
- 临时创建 loopback `repair-agent-B-ocis-tunnel`，仅测试时接入原 demo 网络与 bridge，使本机 adapter 能访问真实 oCIS；测试结束已 `docker stop`，`--rm` 自动移除。未停止/重启既有服务。
- 真实测试仅创建独立 `agent-B-repair-<UUID>` 目录，保留供复核，未原生 DELETE。最后成功运行的根 resource ID 已写 `agent-B-targeted-native-final.log`。
- 应用数据库测试 schema 随测试清理；真实原生测试目录刻意保留。身份服务的 SubjectStatus/后台 DelegatedCredential 在真实存储测试中为 fixture（令牌来自真实交换），不冒充全套生产授权端到端。

## 共享文件变更与合并注意事项

1. 按顺序 cherry-pick `2493199`、`a6b3adb`。没有未提交工作区修改。
2. `sqlc.yaml` 只加入 000010；与 A/C/D/E 合并后保留全部迁移清单，统一重跑 sqlc generate，生成冲突以生成结果为准。
3. B02 用 file_operations INSERT 触发器实现 holds，不替换 A 的 begin_file_operation；主 Agent 需验证 A 新增上传操作与此触发器的组合，以及所有业务写入继续取得空间锁。
4. `worker.go` 只在 maintenance tasks 加一行 retention 对账，可直接保留 C/A 其他改动。
5. `vault.go` 有最小 retention 授权分支，与 E 显式审核能力修改可能同处 authorizeSource，须手动保留双方条件；retention 例外绝不能扩大到普通文件读取。
6. 扩展 NATS/Meili worker 集成失败尚未定位，由 C/主 Agent 联合验收；本 Agent 未扩大修改范围。
7. 尚无独立交叉复核者签收；B02/B08 因列明的历史异常/生产授权验证缺口仅标 implemented，不以局部通过冒充完全关闭。
