# 本地 Example 独立审查（R4）

## 结论与证据分层

本次审查对象是 `examples/local-demo/` 入口、真实服务预检、样例写入与中文操作说明，不是重做或替代全规格验收。当前代码没有假登录、预制搜索结果、模拟原件服务或自动分享批准。缺失外部 OCR/视觉模型、Office 预览、S3 和 Peri 客户端的能力明确不在本例范围。

**可交付结论：** 在审查覆盖的安全边界内，可交付本地示例源码与操作指南；当前未发现尚未修复的明确代码阻塞。真实原生后端工作流及重启保留数据已通过，但完整 Docker Engine 冷启动和真实浏览器操作仍未通过，不能称为全环境“开箱即用已实测”。以下结果已经独立核对：

- 23 项原生预检离线负向/协议测试通过。
- 16 项 launcher 与 27 项 seed 离线测试通过，共 66 项。另有 21 项发布/源摘要边界测试通过。它们验证控制流和安全边界，不构成真实 Compose 或浏览器通过证据。
- 当前环境没有 Docker executable 或 Docker socket。实际运行 `demo.py start` 退出码为 1，未创建指定 runtime，未创建容器，未打印成功。此项证明失败关闭，不证明成功启动。
- `evidence/example-r4/compose-config-final.json` 记录冻结配置经真实官方 standalone Compose 解析通过：只有 gateway 发布 `127.0.0.1:8080`，内部服务不发布宿主端口。该报告明确 `engine_started=false`、`browser_started=false`，不是 Docker Engine 验收。初次解析记录另行保留。
- 首次真实 native 预检失败保留在 `evidence/example-r4/native-run1.log`，错误为 `native_revision_href_invalid`，没有被覆盖或描述成通过。
- 修复后 run2 原生预检通过，但外置实跑 shell 在启动应用前出现未定义变量而终止，保留于 `native-run2.log`。不是 seed 或 Docker 完整通过。
- `evidence/example-r4/native-run3.log` 及 `native-run3/{preflight,page,seed,check}.json` 记录真实原生服务成功：9 项预检、实际构建 HTML/JS/runtime config、真实登录/session、3 文件/4 源版本、精确当前 artifact 搜索、真实 PDF PNG、reader 隔离与第二次严格 check。已检查实跑 shell、调用模块与 proxy，没有模拟业务后端。
- 上述 run3 使用原生进程、真实 Keycloak/oCIS TLS 和测试专用 loopback Python proxy；调用交付的同一 preflight/seed 代码，但不是交付 Nginx、固定 `drive.localhost:8080` 或 Docker Engine 容器路径的运行证明。`page` 是 HTTP 资产/配置检查，不执行浏览器。
- 冻结后 run4 的同一完整预检/seed/check 通过；新增改名专项的外置 harness 错读了 `status`，真实 API 字段是 `state`，该失败记录保留。修复只涉及测试 harness，按返回 operation ID 只读轮询，没有盲目重放旧 MOVE。
- **最终 `evidence/example-r4/native-run5.log` 与 `native-run5/` 全流程通过**：9 项原生 proof、构建页面与配置、完整 seed、严格 check、实际改名、API/worker/maintenance 三进程重启、轻量 ready、改名后当前版本不变、全部原件/历史字节仍相同、seed journal 未改写，以及重复 native 只读预检。独立校验六份阶段报告均 passed，四份交付 Python 运行文件的运行前后 SHA256 与审查时源码完全一致。
- 浏览器启动实际失败日志见 `evidence/example-r4/browser-startup-blocked.log`，核心错误为创建 socket 时 `Operation not permitted`。没有通过禁用 sandbox 或绕过权限把它改记成成功。

## 已发现并修复的局部问题

1. **宿主环境可污染本地网络配置。** Compose 的环境插值可能让宿主 `DRIVE_BIND_ADDRESS=0.0.0.0` 覆盖示例 `.env`。命令入口现剥离 `DRIVE_*`、`STORAGE_USERS_*`、`COMPOSE_*`，并再次校验实际解析后的唯一 loopback 端口、内部网络、OIDC origin、存储驱动及项目资源命名。
2. **Docker context 优先级可绕过本地 daemon 检查。** `DOCKER_CONTEXT` 优先于 `DOCKER_HOST`，不能只看后者。入口现按真实优先级读取 endpoint，拒绝远程连接，随后所有 Docker 命令固定到已核对的本地 `--host` 并删除冲突环境。依据：[Docker CLI 环境变量](https://docs.docker.com/reference/cli/docker/)。
3. **工具容器缺少真实共享 harness 的路径。** 现以只读 `/source` 挂载源码，执行 `/source/examples/local-demo/seed.py`；与 `demo_support.py` 定位共享真实 HTTP/OAuth harness 的方式一致。
4. **初始配置与状态检查不足以证明成功。** launcher 先等待真实依赖、再迁移；实际检查发布的网页、非空构建 JS、运行时配置和 OIDC discovery。原生预检必须产生同项目的完整 proof 才开集成 gate；未初始化的 `check` 返回非零。
5. **搜索结果只验证 UUID 形状不足。** seed 现把每条命中的 artifact ID 与真实文件的已发布 `search_artifact_set_id`、当前版本和路径一起核对，并读取该精确 artifact 的 Markdown。错 artifact 的回归测试通过。
6. **真实 oCIS 历史目录形状差异。** 首跑证实 oCIS 8.2.2 用同一资源 `/dav/meta/{resource}/` 表示无 ETag 的 self collection。修复仅额外允许这一精确同资源目录；外国 origin、其他资源目录、任意父路径仍拒绝。对应正向及负向协议回归、真实 run2/run3 预检都通过。
7. **误拷本地状态进发布目录的防护。** 源包与源摘要过滤显式排除凭据、demo state、seed/native journal、readiness 结果等 runtime 文件名；Docker context 也排除 example/tests/docs 与相关私有文件。源摘要负例证明这些文件不会改变已交付源码范围，example 源码本身则纳入摘要。

## 安全与操作边界

- 固定使用 `http://drive.localhost:8080`。HTTP 只用于 loopback；没有关闭 TLS 验证、绕过浏览器警告或禁用浏览器 sandbox。内部 DNS alias 与外部 OIDC issuer、回调 URL、cookie 模式保持同一 origin。
- 账号和密码首次运行随机生成，只有合成本地 owner/reader；没有生产固定口令。私有 runtime 在源码树之外，目录 `0700`，凭据和 journal 为 `0600`；运行状态不回显密码、token、授权 URL 或有签名的上传 URL。
- 只管理独立随机 Docker project，不接管已有外部目录或共享卷。拒绝符号链接、硬链接、非本人所有或外部可写 runtime。`stop` 只停止容器，不删除卷或历史。
- 原生预检先持久化操作意图；响应丢失或结果不明停止，不盲重放。已通过 probe 的再次检查只读；旧 success marker 在实时检查前失效，失败不能沿用旧成功。
- 原生 stale tus admission 拒绝**不等于**最终提交 CAS。当前 GET 的 `If-Match` 可能不执行，固定读取必须检查响应 ETag 和字节。应用仍依赖自己的持久操作互斥与版本门禁，没有借 demo 放宽它们。
- 首次 seed 通过真实 OAuth、业务 REST/tus、转换和搜索写入唯一合成目录，核验四份源写入形成的三文件及两个 Markdown 历史版本。未知上传先 finalize/HEAD，只在已确认 offset 下补传；历史和当前字节均真实读取。
- `start` 重复执行仅做轻量实时身份/API 检查，不覆盖用户已修改的样例。`check` 是严格初始样例复验，用户改动或主动分享会使它失败，而不是自动修复用户数据。
- 不自动创建公开链接或批准分享。中文指南中的“固定历史版本分享”必须先选择历史版本，符合实际 UI，而非假设不存在的策略下拉框。

## 尚不能宣称的通过项

最终原生后端预检、seed 和重启专项成功，仍不能据此宣称完整 Docker Engine 冷启动、网页真实浏览器操作、所有 OS/Docker Desktop 兼容、扫描件 OCR、外部视觉推理、Office 预览、10 GiB 续传或全规格验收通过。当前 Docker Engine 缺失与浏览器 socket 拒绝是明确运行环境限制，不是脚本已运行成功。任何最终交付说明必须保留这些边界。

## 复现独立离线检查

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests -p test_local_demo_preflight.py -v
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s examples/local-demo -p 'test_*.py' -v
```

实际启动与严格样例检查仍应在本地 Docker Engine 可用的机器执行，使用 README 中同一个私有 runtime，不能靠修改 gate、编辑 proof 或关闭证书校验来跳过失败。
