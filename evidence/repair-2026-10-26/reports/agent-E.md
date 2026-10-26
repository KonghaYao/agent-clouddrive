# Agent E 报告

分支/HEAD：`repair/E` / `9634a8f64130331d4568336c70bf6f47c17a93db`

工作目录：`/Users/konghayao/code/ai/agent-clouddrive-E`。未检查或修改前端，未部署生产，未切换/合并分支。最终 worktree 干净。

提交：
- `4c881bbe6a1f80ceadd50c5ce4e31aaee9f5b613` — `fix(B09,B10): 校验显式审核能力并重放身份停用事件`
- `9634a8f64130331d4568336c70bf6f47c17a93db` — `fix(B22): 按可信代理与真实协议保护身份 Cookie`

## 逐项

### B09：implemented（真实 vault / PostgreSQL 授权链 verified；真实 oCIS 整链 not_run）

- 根因/触发：旧 AuditContent 未传审核上下文，vault 普通源读取拒绝文件或祖先处于 trash 的源版本。
- 实现：
  - `internal/ports/ports.go` 新增 AuditCapability，绑定 auditor principal、不可复活的 grant UUID、file、version、purpose 和授权有效期。
  - `internal/application/access/content.go:15` 新增 AuditOriginalVersion；`internal/application/audit.go:46` 由明确的人类审核入口构造能力，仍写 audit_reads。
  - `internal/adapters/delegation/vault.go:163` 在获取原生凭据的事务中重查 grant、版本 native_revision_ref 与 file/owner 归属、有效期、owner/auditor epoch、owner/auditor/grantor 业务状态及权威实时状态。
  - 仅审核能力允许 active/trashed 目标及祖先；deleting/restoring 仍拒绝。普通 owner/分享/Agent 源读取原有 active 门禁不变。
  - 审核与 grant 撤销用数据库锁串行；refresh 后再次验证有效期。不用 JobID、空 SubjectToken 或普通 scope 充当审核能力。带 token 的伪造审核请求直接拒绝。
  - owner 停用后，独立保留产物审核沿用既有契约；owner epoch 校验仅限制原件委托，不把保留 Markdown/assets 一并禁用。
- 正确性测试：
  - `TestRepairAuthRetainedOriginalRealVault`：active、文件 trashed、父目录 trashed 的固定字节与 ETag；普通读取、Agent、其他 file/version/pin、无 grant、到期/删除/regrant 后旧 capability、owner/auditor 实时停用、owner 离线授权撤销；三次成功原件读取均有 audit_reads。
  - `TestRepairAuthVaultFixedVersionAndConcurrentRevoke`：原生 PG 下跨文件版本拒绝、有效期绑定、deleting 拒绝、grantor 停用、并发 revoke 与凭据校验串行、撤销提交后立即失败。
  - 既有 `TestVaultPostgresIntegration` 与 `TestReviewAuditRequiresSpecificActiveHumanGrant` 通过。
  - 下面“关键命令”中的聚焦测试退出码 0，日志 `agent-E-focused-release.log`。
- 数据库迁移/历史状态修复：000013 增加 audit_grants.id、owner identity_epoch、auditor_epoch；既有 grant 不自动扩大权限。已发生 owner/auditor epoch 变化的原件审核授权需要授权操作员重新授予，不改历史源版本，不删 audit_reads。
- 真实依赖 vs 替身边界：PostgreSQL 17.11、真实 application/access/vault/SQL/触发器已执行；IdP refresh/token exchange、原生字节服务使用明确 fixture。没有声称真实 oCIS 与真实离线授权整链通过。
- 剩余风险/阻塞：真实 oCIS 回收站原件 + 真实 Keycloak offline grant 整链 `not_run`，因此按共享设计保守保留 implemented；需要主 Agent 在合并后用真实 consent/offline grant 再验。当前真实 owner 无效仍 fail closed，不提供管理员 impersonation。

### B10：verified（本文定义的单机 Keycloak Admin Events + 原生 PG 边界）

- 根因/触发：每分钟只采样 enabled，会遗漏零请求期间 disabled→enabled。
- 实现：
  - `db/migrations/000013_identity_epoch_events.up.sql:1`：principals/share_grants epoch；identity_event_cursors；source+event_id 唯一的 identity_events。所有历史均保留。
  - `internal/adapters/identity/events.go:15`：用真实服务账号查询 Keycloak events/config 和分页 admin-events；验证配置、事件 ID 与 representation；停用、删除和无法证明未停用的 USER UPDATE 保守处理。realm 配置更新保守撤销该 issuer 的旧分享。
  - 使用保留事件 ID 作为连续性锚点；覆盖同毫秒乱序、重投去重、停机补拉。锚点丢失/初始接入/超过 10000 条有界补拉窗口，则提升 issuer 全体 epoch、撤销旧分享并从当前头建立新边界。空日志没有可信锚点，拒绝新确认直至管理员产生保留事件。
  - `internal/application/identity_events.go:12`：每 issuer 用事务 advisory lock；事件去重、epoch 提升、撤销以及游标推进原子提交。重复与乱序永不恢复旧 grant。
  - 停用事件使该主体创建/拥有的活动分享 state='revoked'，有 disabled_reason；pending approval 拒绝。事件处理不把当前 enabled 用户错误永久标为 disabled。
  - `internal/application/worker.go:1103` 身份维护前先消费事件；`internal/application/shares.go` 分享申请、确认、内部解析以及 `internal/application/preview.go:30` 公开解析先追平事件；SQL 用 share_identity_valid 校验 owner/creator epoch 和业务状态，原有权威 enabled 查询仍保留。
  - realm Python 与静态模板同时开启管理员事件详情、赋予只读 view-events；无 LOGIN 活动采集，不打印/持久化管理员事件原始 representation。
- 正确性测试：
  - `TestRepairAuthIdentityEpochShortDisableReplay`：零访问短停用、旧公开/用户/组分享持久 revoked、旧公开/内部解析失败、新确认可用、6 个并发消费者、乱序重投、全新 Service 重启、缺失游标、接口失败与恢复。原生 PostgreSQL；事件源 fixture。
  - `TestRepairIdentityReplayPaginationAndGaps`：102 条分页、同毫秒晚到停用、配置关闭、缺 representation 配置、无 ID、外 issuer、空日志拒绝。HTTP fixture。
  - `TestRepairIdentityMigrationReentrantAndEpochMonotonic`：空库全迁移、基线升级、000013 空库/有数据重复执行、停用后重新启用 epoch 不回退。
  - `TestRepairIdentityLiveKeycloak`：隔离真实 Keycloak 26.8.1 realm + 原生 PG + 真实 adapter/application；先建立三类分享，再 PUT disabled→enabled，中间无云盘请求/轮询，随后旧三类 grant 全部 revoked、旧公开链接拒绝、新确认链接可用；清空真实 admin-events 后拒绝旧分享，建立新事件后旧分享仍拒绝、只能新确认。
  - 真实 runner `deploy/compose/tests/repair_keycloak_live.py` 退出码 0；日志 `agent-E-keycloak-gap-verified.log`。只读取现有容器的管理员 secret 创建随机隔离 realm，最终删除隔离 realm；未修改 demo realm。
- 数据库迁移/历史状态修复：无法证明迁移前停用历史，第一次成功同步缺失游标时保守提升 epoch 并关闭旧分享/pending approval；需要人工重新确认。服务不可用/事件配置不可信时持久关闭已有授权并拒绝分享。恢复 enabled 或事件服务不会自动复活旧分享。不删除分享/审核历史。
- 真实依赖 vs 替身边界：真实 Keycloak 管理事件、服务账号授权、当前状态查询和原生 PostgreSQL 已运行；分享内容的 oCIS/预览字节不是该事件测试的真实依赖。没有部署完整 API/worker/maintenance Compose，也没有真实浏览器分享点击。
- 剩余风险/阻塞：
  - 既有部署必须显式开启 adminEventsEnabled/adminEventsDetailsEnabled，并给身份服务账号 view-events；重新生成 realm 文件不等于已经迁移线上 realm。未执行生产配置变更。
  - 需要可信管理员事件历史：不支持绕开 IdP 的直接用户数据库写入。恶意管理员同时篡改/恢复日志及配置不在本补丁可证明范围。
  - 分享路径会同步访问事件 API；不可用时失败关闭，会牺牲可用性。每次扫描最新页并持久去重，尚未做大 realm/高并发性能验收。
  - 无事件的 realm 需要先产生一个可信保留管理员事件。管理员事件详情可能包含敏感操作信息，部署需保护 Keycloak 数据库与管理员查询权限；业务侧仅保存最小事件字段，不保存详情。

### B22：verified（真实 nginx + TLS + 明确代理 CIDR）

- 根因/触发：旧 map 直接使用客户端 X-Forwarded-Proto，默认 nosecure，可能剥除 TLS Cookie Secure。
- 实现：
  - `deploy/compose/gateway/nginx.conf:17` 默认不信任任何转发头；geo 受信地址列表与 scheme 合成 identity_scheme，直连 TLS 不可能被伪造 http 头降级。
  - Cookie 默认 secure；非开发路径不修改上游 SameSite=None 或其他既有 SameSite；HttpOnly 不受影响。
  - `deploy/compose/setup.py:30` 增加显式 --trusted-proxy-cidr；校验规范 CIDR，拒绝全 Internet 信任。仅 --development-http 生成且匹配明确本机 hostname 的 HTTP 路径允许 nosecure/samesite=lax。HTTPS 生成不含 nosecure。
  - 向 Keycloak 转发经过验证的 identity_scheme，而不是忽略可信 TLS 代理信息。
- 正确性测试：`deploy/compose/tests/test_repair_gateway_cookie.py` 4 项全部通过。
  - 实际 Docker nginx 运行修改后的配置；TLS 客户端信任本次随机自签证书并验证 hostname，没有关闭 TLS 验证。
  - 使用 Cookie fixture 上游，分别验证非受信来源与精确配置的实际 Docker peer CIDR；每种都执行 HTTPS/HTTP × 无头/http 头/https 头，共 12 个请求；TLS 保留 Secure/HttpOnly/SameSite=None、可信代理 HTTPS 保留安全属性、伪造头不降级、明确开发 HTTP 可用。
  - HTTPS setup 生成配置另有断言：TLS listener 存在、不含 nosecure 或 samesite=lax。
  - 真实测试容器最终全部删除，未改 demo gateway。
- 数据库迁移/历史状态修复：无数据库变更；重新生成 nginx 配置并受控重载才能生效，已被客户端存储的旧 Cookie 建议登录后更新。未部署生产。
- 真实依赖 vs 替身边界：真实 nginx 容器、OpenSSL 证书、真实 TLS 请求与 proxy_cookie_flags；Cookie 上游是 HTTP fixture，不是实际 Keycloak 登录响应，也未跑浏览器。
- 剩余风险/阻塞：部署必须仅信任真正 TLS 终止代理的地址；不要把公网客户端所在网段加入列表。真实浏览器/完整 Keycloak 登录 Cookie 验收 `not_run`，但真实 nginx/TLS 的本项属性处理已验证。

## 关键命令、退出码与证据

以下命令均在 Agent E worktree 执行。原生 PG 连接配置通过既定 DRIVE_TEST_DATABASE_URL 环境提供；本报告不记录其值。

| 命令 | 退出码 | 结果/日志 |
| --- | --- | --- |
| `GOTOOLCHAIN=go1.25.0 go test -race -count=1 -run 'RepairAuth\|RepairIdentityReplay\|RepairIdentityMigration\|ReviewAudit\|VaultPostgres' -v ./internal/application ./internal/adapters/delegation ./internal/adapters/identity` | 0 | 7 个聚焦测试通过，无 skip；`agent-E-focused-release.log` |
| `GOTOOLCHAIN=go1.25.0 go test -race -count=1 ./...` | 0 | 提交后全量包通过；`agent-E-committed-go.log`。未配置的可选真实依赖仍会 skip，不以 aggregate PASS 代表它们实跑 |
| `GOTOOLCHAIN=go1.25.0 go vet ./...` | 0 | 提交后通过；`agent-E-committed-vet.log` |
| `~/go/bin/sqlc generate`、`git diff --exit-code`、`git diff --check` | 0 | 提交后重生成零漂移，worktree 干净 |
| `REPAIR_NGINX_LIVE=1 /tmp/repair-venv/bin/python -m unittest discover -s deploy/compose/tests -p 'test_*.py' -v` | 0 | 共 9 项：8 pass，1 RustFS 原生 binary 未配置 skip；`agent-E-deploy-verified.log` |
| `REPAIR_NGINX_LIVE=1 /tmp/repair-venv/bin/python -m unittest discover -s deploy/compose/tests -p 'test_repair_gateway_cookie.py' -v` | 0 | 本项 4/4 通过、无 skip；`agent-E-gateway-cidr.log` |
| `/tmp/repair-venv/bin/python deploy/compose/tests/repair_keycloak_live.py` | 0 | 真实 Keycloak + PG，短停用与真实日志清空/恢复通过；`agent-E-keycloak-gap-verified.log` |

日志目录：`evidence/repair-2026-10-26/logs/`（主工作区），均以 agent-E- 命名。未写凭据、token、原始管理员事件详情或响应内容。

中间失败如实记录：首次 application 全包测试因过宽 owner epoch 检查破坏独立保留产物审核而退出 1，已限定原件分支；首次 vet 因测试复制含 mutex 的 Service 退出 1，已改为构造新 Service。最终命令均已重新执行通过。第一轮日志保留，不作为最终通过依据。

## 共享文件变更建议（若未直接修改）

- 本分支直接改了 worker.go 的身份同步入口（仅 3 行）；不涉及上传、生命周期和转换段。
- preview.go 只改 linkFile 分享授权入口的事件追平与 epoch 条件，未改预览选择/读取算法。此处是 B10 必要联动，与 Agent D 的预览改动可能冲突，合并时需保留双方逻辑。
- ports 采用附加 Audit 字段和独立可选 IdentityEventSource 接口，未修改 IdentityAdapter 方法签名，A/C 的普通 DelegationRequest 无需改写。
- mutationIdentity 测试 fixture 增加显式可信空事件源，不是生产绕过；缺少事件能力的生产实现仍 fail closed。

## 合并注意事项

1. 按以上两个提交顺序合并；不要覆盖其他 Agent worktree。迁移编号固定 000013，合并后按共享设计统一 sqlc generate。
2. 000013 是前向、可重入迁移，已在原生 PG 随机 schema 完整执行；未对 demo/生产业务库执行。历史分享首次同步会被保守撤销，需在上线说明中明确人工重新确认。
3. 必须同时迁移数据库、启用真实 realm 事件详情/服务账号权限、部署新后端与生成的网关配置；单独导入模板或只部署后端会使分享失败关闭。
4. 建议主 Agent 交叉复核：审核 grant 锁顺序及 expiry；事件缺口与空日志策略；identity_epoch 触发器与其他迁移触发器兼容性；Agent D 的 linkFile 合并；实际生产代理地址范围。
5. 未完成真实依赖：B09 真实 oCIS + 离线授权整链、完整浏览器/Compose 业务流程、生产代理拓扑、多节点/大 realm 性能。RustFS 实跑不是 E 的验证范围。报告不把这些 not_run 项计为通过。
