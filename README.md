# Agent CloudDrive v1

A single-node, agent-native cloud drive implementation: **Hertz + official MCP Go SDK**, **PostgreSQL / sqlc / pgx**, **NATS JetStream**, official **oCIS**, **RustFS**, **Keycloak**, **Meilisearch**, and **React / Ant Design / Uppy tus**. Native storage is private; REST and MCP share the same business authorization and durable operations.

## Delivery status

The source suite and several **real native integration loops passed**. This is **not a full-spec acceptance pass**: Docker Compose runtime, the actual Peri client, a real-browser 10 GiB interrupted/resumed Uppy transfer, and configured external vision-model acceptance have not passed in this environment. See the machine-readable [acceptance matrix](docs/acceptance/acceptance.json), [human matrix](docs/acceptance/acceptance.md), and [known limitations](docs/operations/known-limitations.md). `python3 scripts/release-readiness.py` deliberately exits nonzero while any mandatory gate is incomplete.

The second source delivery adds executable real-browser, joint-API, 10 GiB and response-loss acceptance drivers with explicit `not_run` boundaries. See [the change report](docs/acceptance/acceptance-changes-20261023.md) and [driver runbook](docs/acceptance/acceptance-drivers.md); prepared scripts are not evidence that their live scenarios passed.

The third source delivery adds bounded native pressure/fault testing and fixes reproduced index-ordering, batching, fencing and allocation defects. See [the R3 report](docs/acceptance/stress-and-fault-r3.md), [independent review](docs/security/stress-security-review-r3.md), and [source test evidence](evidence/source-r3/README.md). Safety budgets are not measured production capacity, and unexecuted gates remain unaccepted.

Implemented and tested paths include:

- Real Keycloak PKCE login, live account checks, supported oCIS token exchange, explicit offline consent, and encrypted refresh rotation.
- Stable file/source/artifact identities, streamed tus transfer, durable per-file and target-path admission, latest-original quota reservation, idempotency and unknown-outcome reconciliation.
- Business MOVE-to-trash and restore without native purge or loss of old version references.
- Immutable Markdown/manifests/image assets, recipe fingerprints, lease fencing, JetStream/outbox recovery, async search publication and DB reauthorization.
- Real AnyDoc Office extraction, page-aware PDF handling, real CPU PaddleOCR, explicit partial results for missing visual descriptions, controlled raster previews.
- Interactive share approval, latest/fixed-version enforcement, per-resource revocation checks, no Agent access to received shares or approval.
- Dedicated anonymous feedback persistence without identity/source timestamps or ordinary audit events; independently granted audit-history routes.

No cross-user deduplication, content reuse, embedding/vector search, native purge API, silent overwrite, administrator impersonation, or public native object URL is provided.

## 一条命令启动可操作示例

准备好本机 Docker Engine / Docker Desktop、Compose v2 和 Python 3.10+ 后，在项目根目录运行：

```sh
python3 examples/local-demo/demo.py start
```

这个隔离演示会生成随机合成账号，启动真实登录、存储、搜索、转换和前端服务，只有真实原生预检及样例上传/搜索/PDF预览验证成功才报告就绪。账号保存在命令打印的私有文件，不写入分发源码。网页地址固定为 `http://drive.localhost:8080`。停止、重启和中文逐步验收见 [example 操作说明](examples/local-demo/README.md)。

该轻量示例使用 local 原件存储，文本/PDF路径无需外部模型；OCR、外部视觉描述、Office专用预览和S3不在默认演示中。当前执行环境没有Docker Engine且真实浏览器启动受限，因此准备和原生协议验证不冒充该Compose/浏览器已运行；实际证据见 [R4验证记录](docs/acceptance/example-r4-verification.md)。

## Start a disposable local deployment

Requires a working Linux Docker Engine and Compose v2.39.4+, not just the CLI.

```sh
cd deploy/compose
python3 setup.py --mode s3 --development-http
./up.sh
```

Open `http://drive.localhost:8080`. Review [deployment instructions](docs/operations/deployment.md) before starting. The setup generates **local** secrets and never contains production credentials. Integration gates are fail-closed until the operator verifies the chosen oCIS/IdP combination. Production requires HTTPS, trusted certificates, exact redirects and private dependency endpoints. Do not use `docker compose down -v` on real data.

## Build and self-test

```sh
go build ./cmd/api ./cmd/worker ./cmd/maintenance
bash scripts/selftest.sh
python3 scripts/release-readiness.py
```

Requirements: Go 1.25+, Node 22+, Python 3.12, and a C toolchain for the Go race detector. The source runner performs Go race/vet, sqlc generation drift, frontend clean-install tests/build, and converter/deployment tests. Native integration variables opt into disposable services; missing native prerequisites are explicitly reported as skipped, never accepted. See [self-test instructions](docs/operations/selftest.md).

The recorded native core loop uses **real** Keycloak, oCIS, PostgreSQL, NATS and Meilisearch with the same application binaries. Its exact newly-uploaded file/version was retrieved and indexed. [Evidence and replay contract](docs/evidence/native-application-loop.md) are separate from unit tests using explicitly labeled identity/storage fixtures.

## Project map

| Directory | Purpose |
|---|---|
| `cmd/api`, `cmd/worker`, `cmd/maintenance` | Runtime entry points |
| `internal/domain`, `internal/application` | Invariants and shared REST/MCP use cases |
| `internal/ports`, `internal/adapters` | Official protocol integrations and contract boundaries |
| `db/migrations`, `db/queries`, `internal/dbgen` | PostgreSQL truth and generated sqlc access |
| `web` | React/AntD file manager, PKCE, real Uppy tus client |
| `converters` | Bounded conversion/OCR/preview adapter processes |
| `deploy/compose` | Pinned reference stack, secret/config generation |
| `tests`, `evidence`, `docs/evidence` | Reproducible tests and scoped actual results |

REST is the stable integration contract: [OpenAPI](docs/reference/openapi.json). It is the only public interface for external integrations such as Fenix. MCP is beta/internal: [21 tool schemas](docs/reference/mcp-tools.json), official SDK Streamable HTTP on the internal `api:8081/mcp` listener; the reference gateway returns 404 for `/mcp` and does not proxy it. All mutations use explicit idempotency/revision rules; errors are structured. Received sharing and audit remain web/REST-only, not Agent tools.

## Important operational boundaries

Original history and published artifacts are retained. A timeout is **unknown**, not permission to retry a destructive write or release quota. Native oCIS does not supply the final tus compare-and-swap semantics this application requires; the database admission gate must cover the whole transfer and native bypass routes must remain private. Fixed reads verify response ETags and resolve matching immutable history, because the tested oCIS GET does not enforce stale `If-Match` itself.

Before real use, close every blocked acceptance gate, supply a real vision endpoint if descriptions are required, validate client-side feedback logging, and verify the deployment's identity lifecycle and retention settings. This repository contains no external credentials or deployed production service.
