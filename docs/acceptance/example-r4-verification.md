# R4：本地可操作 Example 的验证记录

日期：2026-10-24（UTC）。这次增量是 `examples/local-demo/` 的真实服务演示入口，不是全规格验收或新的容量承诺。原 R3 压测、失败记录及既有验收矩阵均保留。

## 用户启动体验

本机 Docker Engine / Docker Desktop Linux 容器引擎已运行，安装 Python 3.10+、Compose 2.39.4+，8080 端口空闲后，在解压目录执行：

```sh
python3 examples/local-demo/demo.py start
```

启动器输出实际私有 runtime 路径和 `http://drive.localhost:8080`。在自己的终端读取该目录的 `credentials.json`，使用随机 owner 账号登录。不要改为其他域名，不要把 runtime 或口令发给他人。

完整中文逐步操作：[examples/local-demo/README.md](../examples/local-demo/README.md)。包括上传、正文搜索、PDF 预览、历史下载、人工确认分享、reader 隔离、回收站恢复、状态/检查/停止/重启。

## 本轮实际验证

| 范围 | 结果与证据 |
|---|---|
| 离线 example 控制流与负向测试 | 43 项通过：16 launcher、27 seed/support；`evidence/example-r4/offline-demo-final.log` |
| 原生存储预检负向/协议边界 | 23 项通过；`preflight-unit-final.log` |
| 发布门禁/源码范围 | 21 项通过；`readiness-tests.log` |
| 原有验收驱动回归 | 221 Python + 37 Node 通过；`acceptance-driver-regression.log`。这是驱动准备验证，不是浏览器/部署验收 |
| Go 当前源码 | `go test -race -count=1 -v ./...` 与 `go vet ./...` 通过，实际 PG/NATS/Meili；`go-native-regression.log`。此处 S3/oCIS 专用测试明确跳过，oCIS 由下述真实链路单独验证 |
| 前端 | 50 项测试及 TypeScript/Vite 生产构建通过；`frontend-tests.log`、`frontend-build.log` |
| 官方 Compose 配置解析 | 实际 standalone Compose 2.39.4 对生成源配置和最终配置均解析通过；13 服务、只有 gateway 发布 `127.0.0.1:8080`；`compose-config-final.json`。没有 Docker Engine 执行 |
| 冻结版本真实原生演示 | `native-run5.log` 与 `native-run5/*.json`：完整预检、样例 seed/check、改名后 app 重启、实时 ready、原件/历史保持，全部通过 |

完整聚合入口 `bash scripts/selftest.sh` 最终退出 0，见 `evidence/example-r4/aggregate/source-checks.log`：真实 PG/NATS/Meili 的 Go race/vet、sqlc 零漂移、SQL 不变量、50 前端测试与构建、35 转换测试（33 通过，OCR/Office 两项明确跳过）、5 部署测试（4 通过，RustFS 一项明确跳过）、43 example 测试、91 根目录 Python 测试及 221 Python/37 Node 驱动准备检查。其余真实集成跳过项仍逐项列于日志；不把 skipped 改成 pass。177 个 R3 生产应用/DB/前端/转换/部署文件逐字节不变，见 `production-source-comparison.json`。

`native-run5/source-before.json` 与 `source-after.json` 保存实际运行前后四个 example 代码文件 SHA256，完全一致；不会用早期结果绑定后续代码。

真实链路使用 PostgreSQL 17.11、NATS 2.15.1、Meilisearch 1.55.0、Keycloak 26.8.1、oCIS 8.2.2、本项目实际 Go API/worker/maintenance、真实 AnyDoc 与 PDF 光栅转换器。没有假身份、假原件、假搜索或假预览。所有服务和驱动在同一执行命名空间运行，使用隔离的合成账号/目录与本地存储，TLS 由明确 CA 验证。

具体已执行：

1. 真实 PKCE 和标准 confidential token exchange（`openid`），个人空间访问。
2. 真实 tus 创建/覆盖、历史读取、MOVE 保持稳定 ID 与旧历史；过期 admission 拒绝。**此项不是 native 最终提交 CAS 证明**，应用持久互斥仍必需。
3. 通过真实业务接口建立一个样例文件夹、三个文件、四份源写入，其中 Markdown 有两个不可变历史版本。
4. 当前原件和旧版本字节核对；搜索必须命中精确当前版本、已发布 artifact 指针和真实正文；读取 Markdown 与真实 PDF PNG。
5. 第二个真实用户看不到 owner 的私有原件和搜索结果。不创建或批准任何分享。
6. 严格样例 check 第二次通过；之后用真实 API 改名，停止并重新启动 API/worker/maintenance，调用与启动器相同的 `ready` 检查。改名、当前版本、全部历史字节和 seed journal 均保留；不会自动重新写样例。
7. 已通过原生 probe 再次只读验证，没有重新创建 probe 或重放 MOVE。

测试的 HTTP 前端服务是 loopback 测试代理，读取真实 `web/dist` 和实际 runtime config。HTML/JS 可取不等于浏览器已操作。它不是交付的 Nginx 容器、`drive.localhost:8080` Docker 网络路径或全栈 Compose 重启的替代证明。

## 保留的失败与修复

- run1：真实 oCIS 历史列表 self collection 为同一资源 `/dav/meta/{id}/`，预检过严误拒。只额外允许这一精确同资源目录；不同资源、外域与任意父路径仍拒。增加负向回归，之后真实预检通过。
- run2：原生预检通过后，外置测试 shell 的同时 `export` 引用未定义变量，应用尚未启动便停止。修复测试脚本，不计完整通过。
- run3：完整 seed/check 通过；随后 seed journal 校验进一步加严，因此最终重新执行 run5，不拿 run3 替代最终源码。
- run4：完整预检、page、seed/check 已过；额外改名检查误读 JSON `status`，实际 mutation 字段为 `state`。修正外置断言并对 pending operation 只读轮询，不改业务代码、不重放旧未知 mutation；失败日志原样保留。run5 在新的隔离 PG/合成样例上完整通过。
- 独立审查还发现并修复宿主环境插值污染 loopback、Docker context 优先级、工具容器共享 harness 路径，以及 seed 搜索 artifact 绑定不足，见[审查记录](local-demo-security-review-r4.md)。

## 明确未执行或不包含

当前云环境没有 Docker Engine/CLI/socket；真实浏览器正常 sandbox 启动在创建 socket 时被环境拒绝。没有禁用 sandbox、跳过证书检查或绕过访问限制。因此：

- **完整一键 Compose 冷启动、真实浏览器交互、完整 Docker stop/start：not_run。** 已提供入口、配置验证、真实 native 后端与恢复验证，但用户本机仍须按指南执行。
- 本例使用 oCIS local + 私有本地产物卷，不启动 RustFS/S3、PaddleOCR 模型、kkFileView/Office 或外部视觉模型。
- 文本、Markdown、文本型 PDF 与 PDF/图片受控预览可在本例内验收；扫描件 OCR、图片描述、Office 预览等缺依赖时应明确失败/partial，不是完整 ready。
- Peri、10 GiB 断线续传、全身份撤销及其他尚未完成规格门禁不因本例新增而晋级。

全规格 release-readiness 仍应非零：11 pass、22 partial、4 blocked。当前源码自测通过与“全规格可发布”是不同结论。
