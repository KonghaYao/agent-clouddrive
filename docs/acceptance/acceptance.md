# 验收矩阵与发布判定

记录时间：2026-10-24T00:16:00Z。机器可读结果是 [acceptance.json](acceptance.json)。

## 当前结论

**NOT RELEASE READY：尚未完成全部真实验收，不能声明整栈或全部功能已验收通过。**

共 37 项：规格 G01–G10、A01–A24 共 34 项，另列 3 项不可省略的交付/外部依赖要求。当前 **11 项通过、22 项部分通过、4 项受阻**。

真实 Keycloak → 应用 → oCIS 上传/下载 → PostgreSQL/outbox → NATS/Worker 离线委托 → Meilisearch 的 local 小文件闭环已经通过。这个证据不等于 Docker Compose 实际启动、真实 Peri 联调、外部视觉模型或 10 GiB 浏览器专项通过。

原始交付 ZIP 的源码自测状态：**通过**。该已冻结源码自测通过：75 个 Go 顶层测试及 34 个子测试、go vet、sqlc 零漂移、PostgreSQL 不变量、50 个前端测试/构建、35 个转换测试（真实 OCR/Office）、5 个部署测试（真实 RustFS 建桶）及 20 个验收检查器测试。Go S3/oCIS 适配器在该聚合运行中跳过，另有独立真实证据；浏览器/Compose/Peri/10 GiB 未通过，源码成功不代表发布就绪。

第二版新增真实浏览器/10 GiB/联合 API/故障验收驱动。浏览器与10 GiB仍为 `not_run`；新增原生 API 最终五场景通过186个断言，经独立审查提升A14/A15/A16/A17。A15增强实跑发现并修复维护outbox饥饿，失败报告保留，修复后真实Meili路径收敛通过。第二版冻结源码完整聚合复验也已通过（真实PG/NATS/Meili、OCR/Office/RustFS、前端构建，以及新增驱动137项离线检查），并绑定当时第二版源码摘要；它仍不代表发布就绪。详见 [验收驱动与缺口](acceptance-drivers.md)。

第三版 R3 修复了真实回归复现的索引倒序、删除/恢复竞态、1001分块上限、撤权提交、租约续约和文本结构/分配放大问题。生产源码聚合复验通过，最后驱动改动另有221项Python及37项Node离线检查绑定；本轮转换复验34通过、1项kkFileView跳过，不能重用旧Office证据冒充新通过。实际本地原生联合186断言、1/4/8/16/32/64并发压力和四项故障77断言已通过，完整故障范围仍partial。A23仅新增真实finalize响应丢失与重试证据，不提升为完整通过。详见 [R3范围/数据/失败记录](stress-and-fault-r3.md) 与 [源码绑定](../../evidence/source-r3/README.md)。全部37项规格状态保持11通过/22部分/4受阻，未为局部压力成功放宽门槛。

第四版 R4 新增[可直接操作的本地 Example](../../examples/local-demo/README.md)。冻结版真实 native 预检、样例 seed/check、当前版本搜索、PDF 光栅预览、改名后 app 重启保留均通过；整包当前源码聚合自测通过，缺失 OCR/Office/RustFS/browser 测试明确跳过，不复用旧结果冒充本轮实跑。官方 Compose 配置解析通过，但当前环境没有 Docker Engine，真实浏览器正常 sandbox 被环境拒绝，故一键容器启动与浏览器操作仍未执行。详见 [R4 验证范围](example-r4-verification.md)，不提升任何全规格门禁。

### 如何解释结果

- `pass`：对应条目的明确范围已有可复核证据，不代表其他条目通过。
- `partial`：存在真实组件或合同/数据库测试，但缺少指定联合场景或规模；剩余工作不能省略。
- `blocked`：所需执行环境、实际客户端或外部服务不可用，未执行要求的真实验证。
- `native_end_to_end`：指定范围内使用实际依赖的联合流程，不代表所有格式、部署方式和故障场景。
- `native_component`：实际依赖的组件验证；`native_component_report`：实际组件结果的说明记录；二者不能替代整栈。
- `real_database*`：实际 PostgreSQL 的事务/锁/约束验证；`contract` 后缀说明原件或身份接口仍是明确的合同夹具。
- `protocol_contract`：官方 SDK/协议或域规则测试，不等同于真实 Peri 客户端。
- `source_static`、`source_test`、`blocker_record` 与失败回归记录均不构成未执行集成的通过证据。

## 发布检查命令

```sh
python3 scripts/release-readiness.py
python3 scripts/release-readiness.py --json
```

退出码：`0` 仅用于所有必需门槛通过且证据有效；`1` 表示记录有效但有未完成门槛；`2` 表示记录或证据无效。该命令只验证验收清单，不执行或代替验收测试。

检查器固定要求全部 37 个 ID，拒绝缺项、重复项、未知状态、空证据、越界路径、证据 SHA256 变化、尚有剩余工作的通过项，以及原生门槛仅引用合同测试的情况。标记源码自测通过还必须绑定当前交付源码树摘要；后续源码/配置/测试/合同变更会使记录失效。没有“忽略受阻门槛”参数。

独立源码自测命令及范围见 [selftest.md](../operations/selftest.md)。源码自测通过不能覆盖任何未完成发布门槛。检查器本身已通过 20 个独立的有效/无效清单测试，包括证据篡改、缺文件、缺门槛、合同冒充原生、旧源码摘要与错误 READY 声明；这些只是检查器测试，不是产品验收。

## M0 集成闸门 G01–G10

| ID / 状态 | 规格条目及要求 | 已验证范围 | 尚缺验证 | 证据 |
|---|---|---|---|---|
| G01 / **部分通过** | 官方 oCIS 用户、空间、后端凭据与 Keycloak<br>业务主体映射稳定；后台可合法读取授权源版本；停用可被执行 | 真实主体/个人空间、PKCE、离线委托读取与核心闭环通过；两名真实 IdP 测试用户的原件/搜索隔离也已验证。<br>级别：`native_end_to_end` | 仍需真实账号停用/重新启用的完整访问与分享传播证据。 | [native_core](../evidence/native-application-loop.md)、[native_identity](../evidence/keycloak-native-m0.md)、[native_application_expanded](../../evidence/native-full-application-final.log)、[native_portable_final](../../evidence/native-portable-acceptance-final.log) |
| G02 / **受阻** | Peri + MCP OAuth<br>授权发现、PKCE、audience、scope、刷新/重授权、Streamable HTTP 真实可用 | 实际 HTTP MCP 2026-07-28 discovery/list/call 与独立 2025-11-25 初始化通过；实际 Keycloak PKCE 通过。它们不是 Peri 客户端联调。<br>级别：`native_component` | 缺少真实 Peri 客户端；未执行 Peri 授权发现、刷新/重授权与 Streamable HTTP 联调。 | [mcp_contract](../../evidence/mcp-shared-version-postgres.txt)、[native_identity](../evidence/keycloak-native-m0.md)、[native_application_expanded](../../evidence/native-full-application-final.log)、[native_portable_final](../../evidence/native-portable-acceptance-final.log) |
| G03 / **通过** | tus 覆盖与最终条件提交<br>冲突写入无法覆盖先提交版本；重试不重复生成版本 | 真实应用/oCIS 在上传期间排他拒绝竞争者；首个提交后旧期望版本再次请求返回 VERSION_CONFLICT，当前字节和历史不变；重复同键返回同一上传 ID，重复 finalize 返回同一源版本。目标路径竞争修复有 DB race 回归。依赖业务门禁，不宣称原生 tus 自带最终 CAS。<br>级别：`native_end_to_end` | 无（限本条已述范围） | [postgres_app](../../evidence/domain-mutations-upload-tests.log)、[native_ocis](../evidence/ocis-native-m0.md)、[target_race_initial](../../evidence/security-review-target-race-initial.log)、[security_review_fixed](../../evidence/security-review-final.log)、[native_application_expanded](../../evidence/native-full-application-final.log)、[native_portable_final](../../evidence/native-portable-acceptance-final.log) |
| G04 / **通过** | 原生版本固定读取<br>旧任务在新版本上传后仍读取原来的字节；重命名不改变引用 | 原生覆盖后旧 ETag/版本映射仍读取原字节；重命名/MOVE 后固定引用可读。<br>级别：`native_component` | 无（限本条已述范围） | [native_ocis](../evidence/ocis-native-m0.md)、[ocis_adapter](../../evidence/ocis-adapter-real-integration.txt)、[native_application_expanded](../../evidence/native-full-application-final.log)、[native_portable_final](../../evidence/native-portable-acceptance-final.log) |
| G05 / **通过** | MOVE 业务回收站<br>移入/移出保留目录后资源身份、所有原生版本与审核读取保留 | 真实单文件原生保留目录 MOVE/移回和应用删除/恢复均通过，资源身份、当前内容及生成的历史修订保持。<br>级别：`native_component` | 无（限本条已述范围） | [native_ocis](../evidence/ocis-native-m0.md)、[ocis_adapter](../../evidence/ocis-adapter-real-integration.txt)、[native_application_expanded](../../evidence/native-full-application-final.log)、[native_portable_final](../../evidence/native-portable-acceptance-final.log) |
| G06 / **部分通过** | RustFS 与 oCIS S3<br>基本 CRUD、Range、multipart、原生修订读写兼容 | RustFS/S3 产物 CRUD、Range、不可变发布，以及 oCIS s3ng 小文件修订/MOVE 通过。<br>级别：`native_component` | 小样本没有执行 multipart；10 GiB 默认 S3 与完整应用 S3 闭环未完成。 | [native_ocis](../evidence/ocis-native-m0.md)、[native_artifact_search](../../evidence/adapters-real-integration.txt)、[native_deployment_setup](../../evidence/deployment-native-5-tests.log) |
| G07 / **部分通过** | AnyDoc/PDF/OCR/图片资产<br>样本格式矩阵与页码映射清晰；部分失败可被真实表示 | 真实 AnyDoc 样本、PDF 提取/渲染、CPU OCR、扫描 PDF 恢复、持久图片与缺页报告通过；私有 kkFileView 三种 Office 样本输出 PNG 通过。<br>级别：`native_component` | 真实外部视觉模型未配置；200 页真实 OCR 五页失败的完整读取/检索场景未执行；未测格式不扩大宣称。 | [conversion_matrix](../architecture/conversion.md)、[source_selftest](../../evidence/selftest/source-checks.log)、[native_conversion](../../evidence/converters-native-35-tests.log)、[preview_build_provenance](../../evidence/converter-deployment-native.md) |
| G08 / **部分通过** | 原生配额与清理<br>业务配额允许的写入可完成；无后台任务清除审核历史 | 业务配额和保留约束有数据库测试；原生自动清理禁用按配置落实。<br>级别：`real_database` | 仍需在真实部署证明原生配额不先行拒绝业务允许的覆盖/历史保留，及运行清理配置核验。 | [sql_invariants](../../evidence/postgresql-invariants-test.log)、[deployment_config](../operations/deployment.md)、[native_ocis](../evidence/ocis-native-m0.md) |
| G09 / **部分通过** | 受控预览<br>匿名链接不能获得原件下载接口；撤销、换版后资源请求失效 | 分享实时门禁、固定/当前版本访问已有回归；实际 kkFileView DOCX/XLSX/PPTX 经明确 PDF 补丁转换为 PNG，PDF/SVG 安全栅格化通过。<br>级别：`native_component` | 实际匿名完整资源请求链、撤销/换版后所有预览资源即时失效仍需端到端证据。 | [share_races](../../evidence/share-privacy-status-regression.log)、[mcp_contract](../../evidence/mcp-shared-version-postgres.txt)、[conversion_matrix](../architecture/conversion.md)、[native_conversion](../../evidence/converters-native-35-tests.log)、[preview_build_provenance](../../evidence/converter-deployment-native.md) |
| G10 / **部分通过** | 匿名链路<br>DB、网关、应用、MCP、APM 与消息中无法关联提交身份 | 匿名反馈不写主体活动/关联 outbox，MCP 专用无来源路径已有回归。<br>级别：`real_database_contract` | 尚未运行完整网关/应用/MCP/APM/消息部署并全面检查持久日志与来源关联。 | [feedback](../../evidence/feedback-privacy-test.log)、[mcp_contract](../../evidence/mcp-shared-version-postgres.txt)、[deployment_config](../operations/deployment.md) |

## 功能验收 A01–A24

| ID / 状态 | 规格条目及要求 | 已验证范围 | 尚缺验证 | 证据 |
|---|---|---|---|---|
| A01 / **部分通过** | 用户 A 上传私有文件，用户 B 搜索/猜测 ID<br>B 无正文、摘要、路径、图片、下载或计数泄露 | 实际两个 Keycloak 用户中，B stat/下载 A 私有文件均 404，搜索不返回该文件；域/SDK 权限过滤与无未授权计数有回归。<br>级别：`native_end_to_end` | 仍需真实双用户覆盖正文片段、路径、图片、预览、下载和计数所有入口的完整隔离场景。 | [source_selftest](../../evidence/selftest/source-checks.log)、[mcp_contract](../../evidence/mcp-shared-version-postgres.txt)、[native_application_expanded](../../evidence/native-full-application-final.log)、[native_portable_final](../../evidence/native-portable-acceptance-final.log) |
| A02 / **通过** | Agent 获得部分 scopes<br>越权操作被拒绝；文件范围仍为整个自有个人空间 | 部分 scopes 的服务端拒绝和 Agent 自有空间限制通过域/官方 SDK 回归；实际 read-only token 创建目录返回 403。<br>级别：`native_end_to_end` | 无（限本条已述范围） | [postgres_app](../../evidence/domain-mutations-upload-tests.log)、[mcp_contract](../../evidence/mcp-shared-version-postgres.txt)、[native_application_expanded](../../evidence/native-full-application-final.log)、[native_portable_final](../../evidence/native-portable-acceptance-final.log) |
| A03 / **部分通过** | 用户收到他人只读分享<br>网页可按授权访问；Agent 搜索、读取与下载均拒绝 | 实际确认后接收者 REST 下载成功，同接收者 MCP 查询该文件被拒绝；固定历史分享与 Agent 自有空间限制有真实 DB/协议回归。<br>级别：`native_end_to_end` | 实际网页接收者操作及 Agent 搜索/正文/下载全部入口的联合场景仍需记录；HTTP 协议探测不能称为 Peri 联调。 | [mcp_contract](../../evidence/mcp-shared-version-postgres.txt)、[postgres_app](../../evidence/domain-mutations-upload-tests.log)、[native_application_expanded](../../evidence/native-full-application-final.log)、[native_portable_final](../../evidence/native-portable-acceptance-final.log) |
| A04 / **通过** | 两 Agent 读同版后覆盖<br>A 成功，B 冲突；A 内容和历史保留 | 使用共同原期望版本的两方请求，第二方在首个上传期间被拒；首个完成后第二方用旧期望版本再次写入仍 409。随后精确读回首个新正文和原历史正文。真实 HTTP/应用/oCIS 流程，不冒充 Peri 客户端。<br>级别：`native_end_to_end` | 无（限本条已述范围） | [postgres_app](../../evidence/domain-mutations-upload-tests.log)、[native_ocis](../evidence/ocis-native-m0.md)、[target_race_initial](../../evidence/security-review-target-race-initial.log)、[security_review_fixed](../../evidence/security-review-final.log)、[native_application_expanded](../../evidence/native-full-application-final.log)、[native_portable_final](../../evidence/native-portable-acceptance-final.log) |
| A05 / **通过** | 相同幂等请求重复发送<br>返回同一操作/版本，不重复扣配额或创建版本 | 真实应用/oCIS 的相同幂等请求返回同一上传 ID，重复 finalize 返回同一源版本；实际数据库并发与配额测试证明不重复预留/计费，异体同键冲突。<br>级别：`native_end_to_end` | 无（限本条已述范围） | [postgres_app](../../evidence/domain-mutations-upload-tests.log)、[native_portable_final](../../evidence/native-portable-acceptance-final.log) |
| A06 / **部分通过** | 新版转换未完成<br>所有者检索旧内容，明确旧版本和新版处理状态 | 当前源版本与已发布搜索代次分离、版本优先级和真实索引发布分别通过。<br>级别：`native_component` | 需记录真实应用新版处理中检索旧内容的响应，含返回旧版本和新版状态。 | [worker_native](../../evidence/application-postgres-nats-meili-test.log)、[sql_invariants](../../evidence/postgresql-invariants-test.log) |
| A07 / **部分通过** | 最新版分享遇到新版处理中<br>可预览当前原件；无权读取旧版转换或旧原件 | 最新版分享拒绝旧版原件/旧产物，固定历史授权不会被替换成当前版本。<br>级别：`real_database_contract` | 需要实际匿名/内部预览在新版处理中只访问当前原件的端到端记录。 | [mcp_contract](../../evidence/mcp-shared-version-postgres.txt)、[postgres_app](../../evidence/domain-mutations-upload-tests.log) |
| A08 / **部分通过** | 200 页样本有 5 页 OCR 失败<br>成功页可读可搜，精确列出 5 页及不完整状态 | 200 页真实 PDF 的五页缺失映射测试通过；小型扫描件真实 CPU OCR 通过。<br>级别：`native_component_report` | 五页故障注入不能替代 200 页实际 OCR 后成功页读取、搜索和精确五页失败的完整验收。 | [conversion_matrix](../architecture/conversion.md)、[source_selftest](../../evidence/selftest/source-checks.log) |
| A09 / **部分通过** | 仅视觉描述失败<br>文本成功可用；描述缺失明确，不能标完整 | 文本提取成功、视觉服务缺失时返回 partial，并明确缺失描述，不伪报完整。<br>级别：`native_component_report` | 真实配置的视觉服务失败/恢复场景及描述语义质量尚未验收。 | [conversion_matrix](../architecture/conversion.md)、[source_selftest](../../evidence/selftest/source-checks.log)、[native_conversion](../../evidence/converters-native-35-tests.log) |
| A10 / **部分通过** | 超过自动转换上限<br>原件成功保存，显示未转换；调参后可重试 | 超过限制仍保留原件且 worker 标记未转换的真实 DB 测试通过。<br>级别：`native_component` | 需记录真实应用修改配置后手动重试成功的完整场景。 | [worker_native](../../evidence/application-postgres-nats-meili-test.log)、[conversion_matrix](../architecture/conversion.md) |
| A11 / **部分通过** | 转换器升级、手动重转旧文件<br>新代次发布；旧引用仍返回旧源版本/旧代次 | 不可变源版本/产物、多代次优先级与旧原件固定读通过。<br>级别：`native_component` | 需实际升级配方、手动重转并同时读取旧引用和新代次的联合证据。 | [sql_invariants](../../evidence/postgresql-invariants-test.log)、[native_ocis](../evidence/ocis-native-m0.md)、[worker_native](../../evidence/application-postgres-nats-meili-test.log) |
| A12 / **通过** | 旧 Worker 或索引任务晚到<br>不能覆盖较新代次；消息重投不重复发布 | 过期 lease/fencing token、旧版本/旧代次发布被数据库拒绝；真实 JetStream/Meilisearch 异步完成与持久消费回归通过。<br>级别：`native_component` | 无（限本条已述范围） | [sql_invariants](../../evidence/postgresql-invariants-test.log)、[worker_native](../../evidence/application-postgres-nats-meili-test.log) |
| A13 / **部分通过** | 删除已分享文件<br>立即隐藏并停用分享；原生 MOVE 后历史可审核 | 数据库删除立即失效分享与祖先门禁通过，真实原生 MOVE 保留历史独立通过。<br>级别：`native_component` | 真实已分享文件从应用删除到受控历史审核读取的联合场景待记录。 | [share_races](../../evidence/share-privacy-status-regression.log)、[ocis_adapter](../../evidence/ocis-adapter-real-integration.txt)、[postgres_app](../../evidence/domain-mutations-upload-tests.log) |
| A14 / **通过** | 恢复目标同名<br>返回路径冲突；指定新目标后恢复；分享不自动恢复 | 真实 Keycloak 双用户与应用/oCIS 联合通过：同名占位恢复409且占位原件不变；改名恢复保留ID/历史/配额；旧分享仍拒绝当前/历史/Range读取，新CSRF确认后才可访问。<br>级别：`native_end_to_end` | 无（限本条已述范围） | [postgres_app](../../evidence/domain-mutations-upload-tests.log)、[share_races](../../evidence/share-privacy-status-regression.log)、[native_application_expanded](../../evidence/native-full-application-final.log)、[native_portable_final](../../evidence/native-portable-acceptance-final.log)、[native_joint_candidate](../../evidence/deployed-api-candidate-run1.json)、[native_joint_provenance](../../evidence/deployed-api-candidate-provenance.json) |
| A15 / **通过** | 删除/移动整个目录<br>子项门禁正确，ID 与历史不变，路径标签更新 | 真实子树55项断言：MOVE/删除/恢复、固定历史、当前入口门禁，以及真实Meili原始source_path收敛；version/artifact不变。维护积压修复有PG/race回归。<br>级别：`native_end_to_end` | 无（限本条已述范围） | [最终原生结果](../../evidence/deployed-api-candidate-run2.json)、[预先哈希](../../evidence/deployed-api-candidate-run2-provenance.json)、[维护回归](../../evidence/maintenance-scheduling-regression.log)、[保留的初次失败](../../evidence/deployed-api-subtree-search-run1.json) |
| A16 / **通过** | 十个版本后删除<br>配额仅计最新大小；删除后额度不变 | 真实应用/oCIS依次上传十个不同源版本，每次配额仅计最新大小；十个版本在删除前、回收期间及恢复后逐字节读取一致；删除额度不变、恢复不重复收费。<br>级别：`native_end_to_end` | 无（限本条已述范围） | [postgres_app](../../evidence/domain-mutations-upload-tests.log)、[sql_invariants](../../evidence/postgresql-invariants-test.log)、[native_joint_candidate](../../evidence/deployed-api-candidate-run1.json)、[native_joint_provenance](../../evidence/deployed-api-candidate-provenance.json) |
| A17 / **通过** | 100 项批处理有一项冲突<br>独立项继续，逐项结果清晰，重试不重复成功项 | 真实应用/oCIS执行100个独立目录创建项，99成功1个PATH_CONFLICT；同批次和新批次相同逐项键重试复用99个操作/文件ID，最终恰好100个目录含原占位项。<br>级别：`native_end_to_end` | 无（限本条已述范围） | [postgres_app](../../evidence/domain-mutations-upload-tests.log)、[native_joint_candidate](../../evidence/deployed-api-candidate-run1.json)、[native_joint_provenance](../../evidence/deployed-api-candidate-provenance.json) |
| A18 / **部分通过** | Agent 请求分享<br>用户确认前不可访问；Agent 不能调用批准接口 | Agent 无批准工具/能力，确认哈希不可变；实际 REST 批准缺少交互 CSRF 返回 403，正确会话+CSRF 批准后接收者可下载。<br>级别：`native_end_to_end` | 实际 Agent 发起请求到网页人工确认前后访问状态的完整链路仍需记录。 | [postgres_app](../../evidence/domain-mutations-upload-tests.log)、[mcp_contract](../../evidence/mcp-shared-version-postgres.txt)、[share_races](../../evidence/share-privacy-status-regression.log)、[native_application_expanded](../../evidence/native-full-application-final.log)、[native_portable_final](../../evidence/native-portable-acceptance-final.log) |
| A19 / **部分通过** | 账号停用再启用<br>所有访问与分享停用；启用后分享仍需确认 | 真实 DB 停用/启用不恢复分享、维护同步、公开请求观察停用的回归通过。<br>级别：`real_database_contract` | 真实 IdP 停用并重新启用用户后所有访问与分享的联合场景待验证。 | [share_races](../../evidence/share-privacy-status-regression.log)、[native_identity](../evidence/keycloak-native-m0.md)、[mcp_contract](../../evidence/mcp-shared-version-postgres.txt) |
| A20 / **部分通过** | 撤销组成员或公开分享<br>后续预览、图片、Range/下载请求无法绕过 | 实时组成员/分享状态/源版本检查的代码与组件拒绝测试存在。<br>级别：`real_database_contract` | 真实组撤销及公开撤销后图片、预览、Range 和下载各入口联合验证未完成。 | [postgres_app](../../evidence/domain-mutations-upload-tests.log)、[share_races](../../evidence/share-privacy-status-regression.log)、[mcp_contract](../../evidence/mcp-shared-version-postgres.txt) |
| A21 / **部分通过** | 匿名链接预览 PDF/Office/SVG<br>不下发原件下载地址；SVG 无主动脚本执行 | PDF/SVG 安全栅格化通过；实际私有 kkFileView DOCX/XLSX/PPTX → PDF → PNG 输出、临时源清理通过。原生 Office 使用明确的 PDF-only 源码补丁。<br>级别：`native_component` | 真实匿名浏览器完整请求链、没有原件或内部 URL 泄露、撤销后资源拒绝仍需验证。 | [conversion_matrix](../architecture/conversion.md)、[source_selftest](../../evidence/selftest/source-checks.log)、[native_conversion](../../evidence/converters-native-35-tests.log)、[preview_build_provenance](../../evidence/converter-deployment-native.md) |
| A22 / **部分通过** | 提交反馈后检查持久数据与日志<br>无来源关联；反馈不出现在 Agent 读取与搜索 | 反馈独立表、无主体活动/outbox 来源记录、MCP 反馈回归通过。<br>级别：`real_database_contract` | 完整部署的网关/APM/消息日志扫描与 Agent 正文/搜索排除联合场景仍待验收。 | [feedback](../../evidence/feedback-privacy-test.log)、[mcp_contract](../../evidence/mcp-shared-version-postgres.txt)、[deployment_config](../operations/deployment.md) |
| A23 / **部分通过** | 远端操作成功但业务响应丢失<br>重试/对账得到原结果，不产生错误覆盖或重复版本 | 丢失 MOVE 响应由目标身份和保留版本读取对账，原生 MOVE 不重放；重复上传提交不增版本。<br>级别：`real_database_contract` | 真实 native MOVE 响应丢失仍未实跑；finalize 响应丢失/重试已通过，未知 MKCOL 仍保持门禁。 | [postgres_app](../../evidence/domain-mutations-upload-tests.log)、[sql_invariants](../../evidence/postgresql-invariants-test.log) |
| A24 / **部分通过** | local 与 S3 模式最小文件闭环<br>两种配置均可保存、读取、转换；专项大文件只测默认 S3 | local 模式真实应用小文件保存、下载、后台转换/索引闭环通过；S3/oCIS 原生组件独立通过。<br>级别：`native_end_to_end` | 完整应用 S3 模式闭环与默认 S3 10 GiB 专项仍未完成。 | [native_core](../evidence/native-application-loop.md)、[native_ocis](../evidence/ocis-native-m0.md)、[native_artifact_search](../../evidence/adapters-real-integration.txt)、[native_portable_final](../../evidence/native-portable-acceptance-final.log) |

## 额外交付要求

| ID / 状态 | 规格条目及要求 | 已验证范围 | 尚缺验证 | 证据 |
|---|---|---|---|---|
| D01_COMPOSE_RUNTIME / **受阻** | 单机 Docker Compose 实际启动与整栈联通<br>通过交付 Compose 启动完整依赖及应用，并记录健康与端到端功能，而不仅是 compose config。 | local/S3 配置生成、独立 Compose 语法检查、真实 RustFS 签名建桶及 native 服务启动分别通过，但没有 Docker Engine 运行。<br>级别：`source_static` | 执行环境无可用 Docker/Podman/daemon；未完成交付 Compose 的实际整栈运行。 | [runtime_blocker](../evidence/native-dependency-setup.md)、[deployment_config](../operations/deployment.md)、[native_deployment_setup](../../evidence/deployment-native-5-tests.log) |
| D02_UPPY_10_GIB / **受阻** | 一次真实 Uppy+tus 10 GiB 断网续传<br>默认 S3 Compose 环境，真实浏览器 Uppy，10,737,418,240 字节，断网后同会话 offset 续传，单次源版本/正确配额，并从正式下载入口验证完整 SHA256。 | 小文件原生 tus 分块续传存在真实证据；不替代此专项。<br>级别：`blocker_record` | Chromium 在页面创建前被宿主限制阻止。；没有可用 Docker 整栈。；未实际传输/下载 10 GiB、记录断网 offset 和 SHA256 对照。 | [browser_blocker](../../web/README.md)、[runtime_blocker](../evidence/native-dependency-setup.md)、[native_ocis](../evidence/ocis-native-m0.md) |
| D03_EXTERNAL_VISION / **受阻** | 真实配置的图片/图表视觉描述服务<br>实际视觉模型为图片/图表生成描述并进入保留产物与检索；网络是否外发服从后端配置。 | 适配器合约、可控外网配置、缺失服务的 partial/blocked 行为已实现并测试。<br>级别：`source_static` | 没有实际视觉模型 endpoint/模型与授权凭据；没有语义描述与搜索接受记录。 | [conversion_matrix](../architecture/conversion.md) |

## 证据目录与边界

完整 SHA256 记录在 JSON。下列文件可能包含不同场景的结果，必须按各条目的“已验证范围”引用，不能把单项成功扩写成整栈成功。

| 证据 | 级别 | 精确范围 |
|---|---|---|
| [native_core](../evidence/native-application-loop.md) | `native_end_to_end` | Actual Keycloak/oCIS/PostgreSQL/NATS/Meilisearch application loop, local artifact store and direct UTF-8 text; no identity/original/message/search mocks. |
| [native_identity](../evidence/keycloak-native-m0.md) | `native_component` | Actual PKCE, consent, token exchange and offline refresh observations; documented limitations remain. |
| [native_ocis](../evidence/ocis-native-m0.md) | `native_component` | Actual local and s3ng small-file protocol probes, historical reads and MOVE; proves native precondition counterexamples, not final tus CAS or multipart. |
| [ocis_adapter](../../evidence/ocis-adapter-real-integration.txt) | `native_component` | Actual oCIS Go adapter create/resume, fixed ETag-gated reads, Range, MOVE/restore with history. |
| [native_artifact_search](../../evidence/adapters-real-integration.txt) | `native_component` | Actual S3 and Meilisearch component operations; not the complete S3 application loop. |
| [postgres_app](../../evidence/domain-mutations-upload-tests.log) | `real_database_contract` | Real PostgreSQL and race detector; identity/original-store contract fixtures are not native identity/oCIS acceptance. |
| [worker_native](../../evidence/application-postgres-nats-meili-test.log) | `native_component` | Real PostgreSQL/NATS/Meilisearch/local artifacts; identity and original-store fixtures remain explicit contracts. |
| [sql_invariants](../../evidence/postgresql-invariants-test.log) | `real_database` | Actual PostgreSQL invariants, not service/browser behavior. |
| [share_races](../../evidence/share-privacy-status-regression.log) | `real_database_contract` | Actual DB approval/delete/account-disable serialization; identity/native content fixtures. |
| [feedback](../../evidence/feedback-privacy-test.log) | `real_database_contract` | Actual DB no principal activity write/outbox correlation; does not inspect deployed gateway/APM logs. |
| [mcp_contract](../../evidence/mcp-shared-version-postgres.txt) | `protocol_contract` | Official MCP SDK/HTTP protocol and real PostgreSQL; not the Peri application or real browser identity flow. |
| [source_selftest](../../evidence/selftest/source-checks.log) | `source_test` | Executed Go race/vet, sqlc drift, real DB invariants, frontend build/tests, converters and deployment generation; explicit native/browser skips. |
| [conversion_matrix](../architecture/conversion.md) | `native_component_report` | Actual sample-format and local CPU OCR report; 200-page injected failure accounting is not full real OCR acceptance; external vision unavailable. |
| [browser_blocker](../../web/README.md) | `blocker_record` | Chromium launch failed before page creation; no browser smoke screenshot or 10 GiB upload accepted. |
| [runtime_blocker](../evidence/native-dependency-setup.md) | `blocker_record` | Native dependencies started without Docker/Podman; no complete Compose runtime acceptance. |
| [deployment_config](../operations/deployment.md) | `source_static` | Configuration and operator instructions only; not evidence that containers ran. |
| [target_race_initial](../../evidence/security-review-target-race-initial.log) | `regression_failure` | Initial failing target-path race reproduction, retained as historical evidence and superseded by security_review_fixed. |
| [security_review_fixed](../../evidence/security-review-final.log) | `real_database_contract` | Ten focused security regressions repeated three times with real PostgreSQL and race detector; native identity/original calls remain labeled contracts. Includes target-path race, stale parent reload, cancellation cleanup, retained audit and share isolation. |
| [native_application_expanded](../../evidence/native-full-application-final.log) | `native_end_to_end` | Actual two-user HTTP application, Keycloak, oCIS, PostgreSQL, NATS and Meilisearch: exclusive overwrite admission, fixed history, trash/restore, private isolation, scoped denial, confirmed REST sharing and MCP received-share exclusion. Does not prove every entry point or Peri/browser use. |
| [native_conversion](../../evidence/converters-native-35-tests.log) | `native_component` | 35 actual converter tests, including native AnyDoc, PaddleOCR CPU HTTP/scanned-PDF recovery, and patched kkFileView DOCX/XLSX/PPTX to PDF to PNG. No real external vision or 200-page OCR throughput acceptance. |
| [native_deployment_setup](../../evidence/deployment-native-5-tests.log) | `native_component` | Five setup/configuration tests, including actual signed RustFS bucket creation twice. Standalone Compose configuration validation is not Docker Engine or Compose startup. |
| [preview_build_provenance](../../evidence/converter-deployment-native.md) | `native_component_report` | Pinned official-source kkFileView project build, explicit spreadsheet PDF patch, native test scope, upstream Maven tests skipped, no Docker runtime or vision provider. |
| [native_portable_final](../../evidence/native-portable-acceptance-final.log) | `native_end_to_end` | Frozen-source shipped HTTP probe against actual Keycloak/oCIS/PostgreSQL/NATS/Meilisearch: exact-file core loop, concurrent admission denial, stale second reader after first commit rejected, same idempotency key returns same upload ID, duplicate finalization returns same source version, retained originals/trash/restore, two-user isolation and actual MCP 2026-07-28. Not Peri, browser, full S3 or 10 GiB. |

| [native_joint_candidate](../../evidence/deployed-api-candidate-run1.json) | `native_end_to_end` | 实际新增5组原生API联合场景178断言；仅A14/A16/A17达到完整条目，目录检索路径、浏览器、Compose、10GiB不在范围。 |
| [native_joint_provenance](../../evidence/deployed-api-candidate-provenance.json) | `native_component_report` | 绑定该次API驱动、辅助代码、结果、原生应用二进制的SHA256与既有真实依赖版本；local后端。 |

## 更新与复验规则

1. 先运行要求的真实场景，记录依赖版本、后端模式、输入规模、成功断言与缺项。不要把未执行、跳过、合同夹具或失败回归写成通过。
2. 仅在对应条目要求完全满足时，才更新状态为 `pass`、清空剩余工作并引用实际证据。修复一个已知问题不自动完成整个闸门。
3. 任何源码变更后重新运行源码检查；最终冻结后记录 `source_verification.tree_sha256`。摘要输入由检查器的 `SOURCE_PATHS` 明确限定，包括代码、配置、测试、脚本和机器可读 API/MCP 合同；排除依赖目录、构建输出、缓存、日志及证据/说明文档。
4. 阅读变化后的证据再更新该文件 SHA256；不能仅为消除错误自动刷新摘要。证据不存在或内容不符时保持非通过状态。
5. 同步 JSON 与本表，然后运行发布检查。只有所有必需条目实际通过时，才能将 `release_decision` 改为 `ready`。打包源码或源码自测通过本身不改变该判定。

## 当前不可替代的缺项

- Docker Compose 需要在具备可用容器运行时的环境实际启动，native 进程不能代替这一交付要求。
- Uppy+tus 需要真实 Chromium 会话完成精确 10,737,418,240 字节、断网/恢复、相同会话与 offset、单次业务提交、单次配额结算及下载 SHA256；小文件或 HTTP 脚本不能代替。
- Peri 必须用实际客户端验证发现、PKCE、audience/scopes、刷新/重授权及 Streamable HTTP。
- 图片/图表视觉描述需要实际配置的模型服务与授权；缺失服务正确报告 partial 不能等于描述能力验收。
