# Agent A 报告

分支/HEAD：`repair/A` / `d603f824cedcbe7a1986c3f747653beeb6435d01`

工作区：`/Users/konghayao/code/ai/agent-clouddrive-A`，最终工作区 clean。只修改后端，未检查或修改 `web/`。

提交：
- `4a11935` — `fix(upload): persist PATCH occupancy and fence B01 B04 B05 recovery`
- `d603f82` — `test(upload): cover B01 B04 B05 PostgreSQL recovery and live tus`

## 状态摘要与证据边界

| 项目 | 状态 | 已验证范围 | 尚未关闭的验收 |
|---|---|---|---|
| B01 | implemented（完整关闭 blocked） | 原生 PostgreSQL + 受控原生存储；实际 TCP/Hertz tus；并发准入/abandon、响应丢失、数据库状态恢复、配额/ETag | 真实 oCIS 挂起 PATCH/故障重演 not_run；实际 kill/重启 not_run；无终结证据的取消/失败自动回收尚未实现 |
| B04 | implemented（原生 PG 正确性已通过） | 18 条混合失败任务、完成优先、双 reconciler、过期租约和旧 token fencing | 主 Agent 合并/交叉复核、真实依赖下完整维护运行未做 |
| B05 | implemented（原生 PG + 替身正确性已通过） | 持久 completion/remote_confirmed 绕过临时 HEAD；稳定资源或身份失败不误释放；版本/配额/转换幂等 | 真实 oCIS tus 自然过期 + 实际服务重启 not_run |

没有把 fixture 或 Service 重建称为真实 oCIS 或 OS 进程重启验收。未运行原审计反例测试，原审计材料仅用于理解旧行为；正式新增测试全部断言修复后的正确行为。

## 逐项

### B01：implemented；取消终结自动回收和真实原生验收 blocked

- 根因：旧实现只依据 offset=0 放弃上传；PATCH 在远端返回后才记录进度，零行更新静默成功，提前释放 gate 后可重叠覆盖。
- 实现：
  - `db/migrations/000009_upload_write_occupancy.up.sql:1`：增加 `patch_epoch`、`in_flight_patches`、`abandon_requested_at`、`native_termination_confirmed_at`、`legacy_dispatch_unknown`；`upload_patch_attempts` 保存派发/未知/成功/对账证据；部分唯一索引限制一个未决 attempt。数据库 trigger 拒绝未决写失败释放及未决写提交。
  - `internal/application/upload_patch.go:18`：PATCH 派发前事务锁定 operation/upload，原子检查状态、offset、已有 attempt、finalize 租约并落盘 epoch。旧派发状态不明会话禁止新增 PATCH。
  - `internal/application/upload_patch.go:52`：HTTP 取消后仍用有界独立 context 持久结果；远端错误保持 outcome_unknown。
  - `internal/application/upload_patch.go:69`：进度只更新当前 epoch/未决 state，严格校验 offset 上界及资源身份，检查 RowsAffected；重复、迟到、回退、越界写返回 OPERATION_PENDING 并记录冲突原因，不截断为成功。
  - `internal/application/upload.go:200`：abandon 与准入共用行锁，未知派发保持 gate/配额，记录 abandon_requested_at，返回 OPERATION_PENDING（retryable、retry_after_seconds）。从未准入 PATCH 的会话仍可正常 abandon。
  - `internal/transport/http.go:394`：生产 tus handler 在 StreamUpload 前调用 AdmitUploadPatch，之后 CompleteUploadPatch；`internal/application/upload.go:406` 的 CreateText 同样接入，不留旁路。
  - `internal/application/upload_patch.go:112`：worker 仅用原生 offset 已覆盖 attempt 上界的证据解决未决 attempt；HEAD 404/低 offset 不证明失败，不释放。零长度未决 attempt 不由 HEAD offset=0 猜终结。
  - `internal/application/upload_finalize.go:79`：空间/file/operation 锁顺序下检查 lease token，再原子提交远端结果与版本、配额及转换任务。
- 正确性测试：
  - `TestRepairUploadAbandonInFlight`：覆盖已有版本，挂起 PATCH，abandon 与后续覆盖均被拒绝，保留增量配额 4；放行旧写后新 Service 对账提交，业务 ETag=原生 ETag，无重复版本。
  - `TestRepairUploadAdmissionAbandonAtomic`：8 次真实 PG 并发争用，每次只允许 PATCH 或 abandon 一方成功；第二个 PATCH 与直接错误失败更新被拒绝。
  - `TestRepairUploadCrashBoundaries`：准入前、准入后派发前、响应丢失、远端返回但进度未落盘、进度已落盘但 finalize 未执行；通过重建 Service 验证只依赖 DB 恢复。未知 404 保持占用；有完成证据时恰好一个版本/转换/outbox、一次配额转换。
  - `TestRecordUploadProgressPersistsEachAcceptedChunk`：期望已改为严格 epoch 与冲突断言，而非继续允许静默 clamp。
  - `TestRepairTusLiveHTTPDispatchBeforeAbandon`（`internal/transport/repair_tus_test.go`）：真实本机端口运行生产 Hertz，HTTP PATCH 调入远端替身前检查 DB attempt 已持久，挂起中 abandon 被拒，最终 204/offset=7 且完成证据落盘；已验证优雅关闭、实际 Run 返回。
- 剩余风险：没有可证明原生终结的取消接口，所以不将未知响应/过期/404 猜为失败。未完成 partial/断点未知任务可能长期保留 gate，必须凭原生证据人工核验。`native_termination_confirmed_at` 是保留的人工证据槽，当前没有自动填充/自动取消 worker。不能将 B01 标为完全 verified。

### B04：implemented；原生 PostgreSQL 正确性测试通过

- 根因：旧 LIMIT 10 查询不在领取时更新 retry_at；多个 continue 永久占据队首。
- 实现：`internal/application/worker.go:1046` 的 reconcileUploads 使用 `FOR UPDATE OF o SKIP LOCKED` + `UPDATE ... RETURNING` 原子领取，领取即推进 retry_at（30 秒）和 lease（2 分钟）、递增 fencing token，仍 LIMIT 10。有完成证据或 remote_confirmed 优先。身份、凭据、HEAD、未完成等退出均释放本 token 租约并退避，保留可审计 error_code。
- `internal/application/upload_finalize.go:12`：手动 finalize 也竞争同一租约，避免 PATCH/维护/手动 finalize 重叠；`:24` 为大文件校验每 20 秒续租，失去 token 后取消读取；最终提交再次 fencing。worker 身份/凭据/HEAD probe 使用 30 秒 context。
- 正确性测试：
  - `TestRepairUploadRecoveryFairClaims`：18 个任务，含跨主体身份失败、凭据失败、HEAD 404、未完成传输和队尾完成任务，3 轮内完成队尾，所有失败路径 retry_at 前进且租约清除。
  - `TestRepairUploadCompletionPriorityConcurrentAndLeaseExpiry`：12 个未完成任务 + 一个持久完成任务；活租约不能窃取，过期后两个 reconciler 并发只领取一次（41→42），版本和转换各一个，旧 token=41 提交被拒。
- 边界：真实数据库与 Go race 检测；身份/oCIS 均为受控替身，不等于完整生产依赖验收。

### B05：implemented；原生 PG + 替身正确性测试通过

- 根因：maintenance 未读取 transfer_completed，对所有 executing 强制 HEAD 临时地址，404 阻断已有稳定原件的 finalize。
- 实现：`internal/application/worker.go:1046` 领取 completion 与 remote_confirmed，完成者直接走稳定资源对账；HEAD 仅用于仍在传输者。`internal/application/upload.go:325` 使用持久 resource ID 与 completion；已有 remote_confirmed 时从 remote_result 读取稳定资源/ETag/revision，验证其格式并约束后续对账。沿用 oCIS adapter 原有 TransferCompleted 能力，未修改 ports 或 oCIS 生产接口。
- 正确性测试：
  - `TestRepairUploadCompletionPriorityConcurrentAndLeaseExpiry`：completion 持久化后临时 URL 永久 404，重建 Service 后后台自动提交，临时 HEAD 调用数=0，重复提交无额外版本/转换。
  - `TestRepairUploadRemoteConfirmedWithoutTus`：仅已有 remote_result 也无需临时 HEAD，即可恢复。
  - `TestRepairUploadCompletedMissingResourceOrIdentityPreservesGate`：稳定原件不存在、身份不可用、无完成证据且临时 HEAD 404，均保持 gate/held reservation，不猜成功或失败。
- 边界：没有等待真实 oCIS session 自然过期，也未对运行中的 demo 部署改代码/重启服务。真实 oCIS 故障重演 not_run。`agent-A-native-environment.log` 记录 ocis:8.2.2 容器在运行但无宿主映射，未获取其凭据或擅自变更网络/部署。

## 数据库迁移与历史坏状态恢复

- 新迁移 `000009_upload_write_occupancy.up.sql` 是前向、可重入迁移；测试真实执行了空库全量 migrations，及 000001..000008 建库+旧 executing 数据后升级 000009 并再次执行，断言旧状态未知标记保留，直接 failed_confirmed 被 trigger 拒绝。测试：`TestRepairUploadMigrationUpgradeAndReentry`。
- `sqlc.yaml` 已包含 000009，运行 `GOTOOLCHAIN=go1.25.0 ~/go/bin/sqlc generate` 成功；生成物为 models.go / operations.sql.go。
- 上线前必须停止旧版上传入口并排空/核验在途请求，避免旧进程绕过新准入协议；升级迁移将所有非终态旧会话保守标记 legacy_dispatch_unknown，不能凭 received_bytes=0 清除。
- 已持久完成/已有 remote_result 的历史任务，恢复 maintenance 即会自动对账；若需受控提前安排单条（不改变状态、不释放占用）：

```sql
UPDATE file_operations o
SET retry_at=now(), updated_at=LEAST(o.updated_at,now()-interval '31 seconds')
FROM upload_sessions u
WHERE u.operation_id=o.id AND u.id=:'upload_id'::uuid
  AND o.state IN ('executing','outcome_unknown','remote_confirmed')
  AND (o.lease_until IS NULL OR o.lease_until<=now());
```

- 状态盘点（不输出原生 secret URL）：

```sql
SELECT u.id,o.state,u.patch_epoch,u.in_flight_patches,u.legacy_dispatch_unknown,
       u.received_bytes,u.transfer_completed,u.native_resource_id,
       u.abandon_requested_at,o.retry_at,o.lease_until,o.error_code
FROM upload_sessions u JOIN file_operations o ON o.id=u.operation_id
WHERE o.state IN ('executing','outcome_unknown','remote_confirmed')
ORDER BY o.retry_at;
SELECT upload_id,epoch,state,admitted_at,ended_at,offset_start,offset_end,error_code
FROM upload_patch_attempts WHERE upload_id=:'upload_id'::uuid ORDER BY epoch;
```

- 当前旧版已经错误释放且后续覆盖成功的历史数据无法仅从业务表还原：暂停该文件写入，核验原生稳定资源/历史版本与 ETag，保留所有历史后逐条确认，不能自动回填/删除版本。没有可用终结证据时仍是 blocked，不提供“清 gate”SQL 假装恢复成功。
- 仅在运维已取得“旧实例完全停止、原生已取消/终止且从未提交”的独立可审计证据后，才可由主 Agent 制定人工解除方案；本提交没有执行任何该类生产修复。

## 检查命令、退出码与日志

所有命令工作目录均为 `/Users/konghayao/code/ai/agent-clouddrive-A`。DB 来自 brief 的本机隔离原生 PostgreSQL，每测试随机 schema，非 PGlite。下文 `$DRIVE_TEST_DATABASE_URL` 使用 brief DSN，未在报告复制连接串。

| 命令 | 退出码 | 日志（主工作区 evidence/repair-2026-10-26/logs/） |
|---|---:|---|
| `GOTOOLCHAIN=go1.25.0 ~/go/bin/sqlc generate` | 0 | 已生成并提交，shell 成功输出 |
| `GOTOOLCHAIN=go1.25.0 DRIVE_TEST_DATABASE_URL="$DRIVE_TEST_DATABASE_URL" go test -race -count=1 -run 'RepairUpload\|RepairTus\|RecordUploadProgress' -v ./internal/application ./internal/transport` | 0 | `agent-A-correctness.log`（最终实现复跑，无 skip） |
| `GOTOOLCHAIN=go1.25.0 DRIVE_TEST_DATABASE_URL="$DRIVE_TEST_DATABASE_URL" go test -race -count=1 ./internal/application ./internal/transport ./internal/adapters/ocis` | 0 | `agent-A-packages-race.log` |
| `GOTOOLCHAIN=go1.25.0 DRIVE_TEST_DATABASE_URL="$DRIVE_TEST_DATABASE_URL" go test -race -count=1 -run '^TestRepairTus' -v ./internal/transport` | 0 | `agent-A-live-http.log` |
| `GOTOOLCHAIN=go1.25.0 go vet ./...` | 0 | `agent-A-vet.log`（空文件表示无诊断） |
| `GOTOOLCHAIN=go1.25.0 go build ./cmd/api ./cmd/maintenance ./cmd/worker` | 1 | `agent-A-build.log`：worktree VCS stamping 获取失败，非编译错误 |
| `GOTOOLCHAIN=go1.25.0 go build -buildvcs=false ./cmd/api ./cmd/maintenance ./cmd/worker` | 0 | `agent-A-build-no-vcs.log` |
| `git diff --check` | 0 | 提交前通过 |
| `docker ps --format ...` | 0 | `agent-A-native-environment.log`，只列容器名/镜像/端口 |

实施过程还遇到测试 fixture 试图修改不可变 actor、SQL 参数 UUID/text 类型冲突、测试复制 Service 内 mutex 触发 vet，以及 HTTP 测试 keep-alive 阻碍关闭；均已修复测试 setup 并复跑，没有跳过断言或放宽实现校验。两次 go doc 查询因符号/执行问题失败，不作功能证据。

## 共享文件变更与合并注意事项

- `worker.go` 仅修改 `reconcileUploads`，不修改转换/其它维护函数；与 C 的 worker 变更按函数合并。
- `http.go` 仅修改 tus 的 PATCH 派发/结果记录，不修改 cookie、其它路由或前端。
- `upload.go` 另有 CreateText 的 StreamUpload 路径，必须一起保留，避免旁路。
- 未修改 `internal/ports`、未修改旧迁移；新增 000009 并修改 sqlc schema 列表。与各 Agent 合并后重新 `sqlc generate`，生成物冲突以再生成为准。
- `RecordUploadProgress` 签名增加 epoch；所有本分支调用已迁移，原审计测试不能原样作为正式测试合入，主 Agent 若还有分支外调用须传递准入 epoch。
- B02 的目标祖先 hold 图不在本 Agent 实现；主 Agent 合并 B 的生命周期占用后补上传与目录交叉并发验收。
- 交叉复核建议：主 Agent/Agent B 复核锁顺序、历史 legacy 阻塞策略、worker lease 与生命周期 hold 组合；真实 oCIS B01/B05、实际进程 kill、取消终结回收仍须补齐，不能仅据本提交全量关闭审计项。
