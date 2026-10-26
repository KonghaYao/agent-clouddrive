# Agent F 报告

分支/HEAD：`repair/F` / `d38677eb322d5022d77c782b606e935938191962`（父提交 `2d3b4fff`；三个提交 `c380797`、`f54f062`、`d38677e`）

工作目录：`/Users/konghayao/code/ai/agent-clouddrive-F`。只修改了 `converters/adapter.py`、`converters/tests/test_adapter.py`（1 处契约断言）和新增 `converters/tests/test_repair_pages.py`；未改 Go、前端、迁移、文档、requirements/model-lock。

环境：`/tmp/repair-venv`（Python 3.12.8）+ `requirements-test`，已核对 pypdf 6.10.0 / pypdfium2 5.14.0 / Pillow 12.3.0 / CairoSVG 2.8.2 / firecrawl-anydoc 0.2.4 / reportlab 4.4.9（见 `evidence/.../logs/agent-F-env.log`）。父代理通知该 venv 曾因卡住的 pip 处于半安装状态；本 Agent 重新按 pin 安装了缺失/不一致依赖。

## 逐项

### B11：implemented（真实 TIFF 帧/预览已验证；真实 PaddleOCR、真实 kkFileView 未跑）

- 根因/触发（当前代码）：`png_image()` 固定 `image.seek(0)`，图片分支把任意格式当 1 页，只做一次 OCR/描述；预览也只返回首帧。多帧 TIFF 的第 2…N 帧既不进入文本/描述/资产，也不进入 `completeness.total_pages`，却返回 `ready`。
- 实现：
  - `converters/adapter.py:383 convert_raster()`：用 Pillow `n_frames` 计页，逐帧 `seek`、EXIF 转置、像素上限检查、PNG 重编码、资产按 `page` 归属；每帧独立执行 OCR、描述、markdown 页标记。
  - 页数上限 `max_pages` 超限直接 `limit_exceeded/not_converted`，不截断（`convert_raster:390`）。
  - 单帧失败只影响该页：`add_error(..., page)` 精确进入 `missing_text_pages`/`missing_description_pages`，结果 `partial`，不会 `ready`。
  - 预览模式（`execute` 的 preview 分支）对 TIFF/GIF 全部帧各产出一张 PNG；`recipe.page`/`resume_pages` 支持页选择，`not_processed_pages` 精确列出未处理页（`selected_pages:179`）。
- 正确性测试（`converters/tests/test_repair_pages.py:252`）：
  - `test_every_tiff_frame_is_converted`：真实 3 帧红/绿/蓝 TIFF；断言 `total_pages=3`、每页 OCR/描述各 3 次、3 个资产页号/像素颜色正确、markdown 三页标记与文本顺序正确。
  - `test_failed_second_frame_is_partial_and_not_ready`：第 2 帧 OCR 失败；断言 `partial`、`total_pages=3`、`missing_text_pages=[2]`、`pages[1].text_status=failed`、第 1/3 页内容仍在。
  - `test_preview_contains_every_frame`：预览返回 3 张 PNG、`markdown=""`。
  - `test_page_limit_rejects_instead_of_truncating`：`max_pages=2` → `not_converted/limit_exceeded`、无资产。
  - `test_page_selection_keeps_global_number`：`page=2` 只跑第 2 帧、全局页号保持 2、`not_processed_pages=[1,3]`。
- 数据库迁移/历史状态修复：无迁移。旧产物中多帧 TIFF 的 `total_pages=1` 不可靠推断，建议按源扩展名 `tif/tiff` + 产物 `total_pages=1` 的候选做受控重建代次（见“历史坏状态”）。
- 真实依赖 vs 替身边界：TIFF 生成/解码、页计数、PNG 重编码、预览资产均为真实 Pillow；OCR/描述为显式本地替身。真实 PaddleOCR 模型与真实 kkFileView 闭环未运行（见末尾“未跑项”）。
- 剩余风险/阻塞：多帧 WebP/APNG 依赖 Pillow 的 `n_frames` 支持；超大单帧按页报 `resource_limit`；没有真实 OCR 质量结论。

### B12：F 侧协议 implemented（C 的后端消费不在本 worktree；旧反例在未开启 `page_report` 时仍成立）

- 根因/触发（当前代码）：adapter 只在内存中顺序处理整篇，stdout 只在最后输出一个 JSON；整篇超时/被杀后没有已确认页，重试从头开始。
- 实现：
  - 请求新增可选 `page_report: true`、`resume_pages: [页号]`（`validate_request`）；`resume_pages` 只处理指定页，其余页不重跑（`selected_pages:179`）。
  - `PageReporter:289` 在每个阶段完成时写一行并立即 `flush()`；`page_ref:308` 定义可持久恢复的页切片：`{"markdown": <本阶段新增页 markdown>, "assets": [<本阶段新增资产>], "pages": [{page, block, text_status, description_status}]}`。按事件顺序累积可精确重建该页（测试用最终结果反证）。
  - 最终一行 `{"type":"result", ...完整 ConversionResult...}`，事件与结果均为紧凑单行 JSON（`separators=(",",":")`）。`engine` 增加 `model`/`provider`；`completeness.total_pages` 始终为整篇真实页数，`not_processed_pages` 列出本次未处理页（C 合并时需要）。
  - HTTP 链路同样支持：`page_report=true` 时 `/v1/execute` 返回 `application/x-ndjson` 流式响应（`Handler.execute_child:1365`），页行随子进程输出即时转发；超时/子进程异常时追加一条 `type=result` 的终态行。**C 已实现 CLI 子进程 JSONL 消费，因此 HTTP 流式不是合并阻塞；但现有 Go `HTTP.Execute` 不能解码 NDJSON，C 不要在未改造前对 HTTP 设置 `page_report`。**
  - 页号/总页数超限：`limit_exceeded`（`selected_pages`），不截断、不静默跳过。
- 正确性测试（`converters/tests/test_repair_pages.py:693`）：
  - `test_page_report_jsonl_order_and_incremental_flush`：真实 `--execute` 子进程 + 2 页扫描 PDF + 慢速第 2 页 OCR（2.5 s）。用 `select` 在 1.5 s 内读到首行，证明逐行 flush；断言原始 stdout 每一行都是独立可 `json.loads` 的完整 JSON、行数 = page 事件数 + 1、最后一个 `type=result`、每页阶段顺序 `render→ocr→text→description`；把每个事件的 `markdown/assets/pages` 切片回放，严格等于最终结果的 `markdown/assets/pages`；`engine.model/provider` 正确。
  - `test_page_report_disabled_keeps_single_json`：未开启时仍是单 JSON。
  - `test_resume_pages_only_runs_the_failed_page`：3 页扫描件先让第 2 页 OCR 失败，再 `resume_pages=[2]`；断言 OCR 只调用 1 次、`pages=[{page:2}]`、`total_pages=3`、`not_processed_pages=[1,3]`、markdown 不含第 1/3 页内容；重复运行结果一致；JSONL 只含第 2 页事件。
  - `test_resume_page_out_of_range_is_rejected`：`resume_pages=[3]`（2 页文档）→ `not_converted/limit_exceeded`。
  - `test_http_page_report_timeout_keeps_confirmed_pages`：真实 HTTP + 第 2 页 OCR 阻塞，2 s 超时；断言第 1 页 render/ocr/description 与第 2 页 render 已到达，第 2 页 ocr/description 未到达，最后一行是 `timeout` 结果。
  - `test_http_page_report_streams_ndjson`：HTTP Content-Type/逐行 JSON/终态行。
- 输出样例：`evidence/repair-2026-10-26/logs/agent-F-page-protocol-raw.jsonl`（**原始 stdout 9 行，每行一个独立可 `json.loads` 的完整 JSON；8 条 page 事件 + 单行 `type=result`，无 pretty-print、无嵌入换行**）与 `agent-F-page-protocol-sample.log`（同内容 + 说明性注释行，base64 已截断；注释行不属于 adapter 输出）。节选：
  ```json
  {"type":"page","page":1,"stage":"render","state":"ready","result_ref":{"markdown":"\n<!-- page:1 block:p1-b01 -->\n\n\n![Page 1](asset:page-1-1.png)\n","assets":[{"id":"page-1-1.png","media_type":"image/png","content_base64":"...","sha256":"9945...","page":1}],"pages":[{"page":1,"block":"p1-b01","text_status":"not_required","description_status":"not_required"}]}}
  {"type":"page","page":1,"stage":"ocr","state":"ready","result_ref":{"markdown":"OCR text page 1\n","assets":[],"pages":[{"page":1,"block":"p1-b01","text_status":"ready","description_status":"not_required"}]}}
  {"type":"page","page":1,"stage":"description","state":"ready","result_ref":{"markdown":"\nImage description (page 1): Chart description\n","assets":[],"pages":[{"page":1,"block":"p1-b01","text_status":"ready","description_status":"ready"}],"model":"vision-test","provider":"openai-compatible"}}
  {"schema_version":1,"status":"ready","markdown":"...","assets":[...],"pages":[...],"completeness":{"status":"ready","total_pages":2,"missing_text_pages":[],"missing_description_pages":[],"not_processed_pages":[],"warnings":[]},"errors":[],"engine":{"name":"pdf-page-pipeline","version":"pypdf-6.10.0/pdfium-5.14.0","model":"vision-test","provider":"openai-compatible"},"model":{"provider":"openai-compatible","name":"vision-test","source":"deployment"},"type":"result"}
  ```
- 数据库迁移/历史状态修复：F 无迁移。C 需要持久化页结果；旧任务无法恢复“内存中已丢失的页”，只能重跑，但新协议下已落盘页可跳过。
- 真实依赖 vs 替身边界：真实子进程、真实 stdout/pipe、真实超时/SIGKILL、真实 JSONL 解析；OCR/vision 为本地替身。未验证 C 的数据库页表消费。
- 剩余风险/阻塞：`not_processed_pages` 是新增字段，现有 Go `ports.Completeness` 会丢弃；C 若不新增字段并保证 ready 时为空，可能把 resume 结果误发布。见“Go 侧最小 patch 建议”。

### B18：implemented（真实 PDF 检测 + 本地 vision HTTP 替身；外部视觉模型未配置）

- 根因/触发（当前代码）：`page_has_visual` 只检查 XObject 或用极小正则 `(?:re|c|v|y)` 扫内容流；`m/l/S` 直线图、填充多边形、贝塞尔曲线被当成纯文本。另有一个隐藏根因：pypdf 的 `ContentStream` 为 falsy（`len()` 为 0），旧写法 `content.get_data() if content else b""` 实际总是得到空字节，任何内容流都不会被扫描。
- 实现：
  - `adapter.py:827 content_has_visual`：按 PDF 词法跳过注释、字面量/十六进制字符串、名字、数字和字典分隔符，识别绘制/路径操作符集合 `m l c v y re h S s f F f* B B* b b* sh Do BI`（`VISUAL_OPERATORS:817`）；超长内容流（>16 MiB）或 token 超限时保守判为“有视觉”。
  - `page_has_visual:899`：修正 falsy ContentStream 判断；XObject/内容流不可读时保守判为需要视觉。
  - 需要视觉但模型未配置/失败时，走 `description` 阶段失败：`partial` + `missing_description_pages` + 明确 error code，绝不 `ready`。
- 正确性测试（`test_repair_pages.py:323`）：
  - `test_content_scanner_is_text_safe_and_conservative`：字符串/注释/十六进制字符串里的操作符不误报；`m/l/S`、`re f`、`Do`、`BI` 命中。
  - `test_straight_line_chart_is_sent_to_vision`：真实 reportlab 折线图 PDF（足够文本 + `m/l/S`）；本地 vision HTTP 替身捕获出站请求，断言确实请求了视觉、payload 含 `data:image/png;base64`、结果 `ready` 且描述入 markdown。
  - `test_straight_line_chart_without_vision_is_partial`：无模型时 `partial`、`missing_description_pages=[1]`、`description_status=failed`、有视觉资产。
  - `test_curve_and_filled_polygon_are_detected`、`test_text_only_pdf_skips_vision`、`test_vision_failure_is_partial_not_ready`。
- 数据库迁移/历史状态修复：旧产物中“图表页 `description_status=not_required`”无法自动识别，建议对含矢量绘图且 `missing_description_pages=[]` 的 PDF 产物做受控重建（见“历史坏状态”）。
- 真实依赖 vs 替身边界：PDF 生成/解析/栅格化真实；vision 为本地 HTTP 替身，未连接外部视觉服务，不评价描述质量。
- 剩余风险：保守策略会把 `re W n` 裁剪、Form XObject 等误判为需要视觉（宁可 partial 不漏图）；未解析 annotation appearance stream / AcroForm；未把页面内容流之外的 shading dict 单独建模。

### B19：implemented（真实本地 provider HTTP 替身捕获 payload；未连接外部供应商）

- 根因/触发（当前代码）：`describe()` 只读实时 `DRIVE_VISION_MODEL`，完全忽略 `request.recipe.model`；入队模型 A、部署切换 B 时 payload 发 B，产物仍记 A。
- 实现：
  - `vision_target:771`：`recipe.model` 优先且逐字使用；仅当 recipe 未带 model 时才回退部署默认值，并标注 `source=recipe|deployment`。
  - `describe:786`：按目标 model 构造 payload；provider 以 400/404/422 明确拒绝该 model 时抛 `model_unavailable`（不重试、不替换）。
  - `complete:267`：出现 `model_unavailable` 时整体 `status=blocked`、`completeness.status=blocked`，避免把配方不一致的 partial 产物发布；`error_result` 也把该码映射为 blocked。
  - 结果 provenance：`result.model={provider,name,source}` + `result.engine.model/provider`；仅在实际执行成功时记录，失败时保持空值，绝不虚报。
  - `enforce_output_budget:330` 同时遵守 `recipe.max_output_bytes`（与 Go `ValidateResult` 的 content 预算语义一致）。
- 正确性测试（`test_repair_pages.py:394`）：
  - `test_recipe_model_is_sent_verbatim`：环境 model=B、recipe model=A，捕获 payload 为 A；结果 `model.name=A`、`source=recipe`。
  - `test_provider_rejecting_recipe_model_blocks_without_substitution`：provider 对 A 返回 404；结果 `blocked`、错误码仅 `model_unavailable`、只发出一次请求且 model=A、`model.name=""`（没有虚报执行）。
  - `test_deployment_model_used_only_without_recipe_model`、`test_describe_maps_provider_404_to_model_unavailable`、`test_recipe_model_without_provider_is_not_substituted`。
- 数据库迁移/历史状态修复：旧 manifest 中 “recipe.model=A、实际用 B” 无法从产物反推真实模型；建议按部署变更时间窗筛选可疑代次并重建，且不得原地改旧 snapshot。排队中 model 已不可用的任务重试会明确 `blocked/model_unavailable`，需要显式新代次/新配方。
- 真实依赖 vs 替身边界：真实 HTTP 请求/响应状态码/JSON payload 捕获；provider 是本地替身，不验证外部供应商的模型列表行为。
- 剩余风险：`recipe.engine`/`engine_version` 未被强制校验（Go 对所有格式都写 `engine="anydoc"`，而 adapter 实际引擎是 `pdf-page-pipeline`/`utf8-passthrough`/`firecrawl-anydoc`；强硬校验会直接阻塞全部 PDF/文本任务）。建议 C/主 Agent 先决定 recipe.engine 的语义（按格式填充或从执行选择器降级为记录），再由 adapter 增加一致性校验。OCR 模型 manifest 的一致性也不在本次范围。

### B20：verified（真实子进程超时/SIGKILL + 真实文件系统；kkFileView 时延由本地 HTTP 替身制造）

- 根因/触发（当前代码）：Office 预览把原件写入共享 `PREVIEW_SOURCE_DIR`，只有子进程 `finally` 删除；父进程超时 `SIGKILL` 子进程后 `finally` 不执行，遗留完整原件；`/internal/source` 只做过期 404，不回收空间。
- 实现：
  - `AttemptDirectory:527`：每次请求一个 `attempt-<32hex>` 目录（0700），内含 ownership marker 与 `.lock`；owner 持共享 `flock`，子进程通过 `ADAPTER_ATTEMPT_DIR` 加入同一锁。目录名/marker 双重校验，回收器只处理本服务目录。
  - `stage_source:718`：原件写入 attempt 目录，0600，分块计数、`fsync`，任何 `AdapterError`/`OSError` 都 unlink 残片。
  - `Handler.do_POST:1441`：父进程创建目录、写请求、spawn、`wait/kill` 后清理；**清理在发送响应之前完成**，调用方不会在收到 timeout 后仍看到残留；清理失败写 `cleanup-diagnostics.log` 并在非流式结果加 `temp_cleanup_failed` warning。
  - `reap_orphans:633` + `start_reaper:687`：启动及周期性回收；只回收“marker 属于本服务且（超过 TTL 或 owner pid 已死）且能拿到排他 flock”的目录；活跃请求（共享 flock 持有）与外来目录绝不删除。
  - 子进程在流式客户端断开/转发异常时由父进程 `SIGKILL`，避免孤儿转换。
- 正确性测试（`test_repair_pages.py:458`）：
  - `test_two_real_office_timeouts_leave_no_source_files`：真实 HTTP + 真实子进程 + 慢 kkFileView 替身，连续 2 次 1 s 超时；每次响应后 `sources` 目录为空；替身确实读取到了完整原件（证明超时前已完成暂存）。
  - `test_sigkill_orphan_is_reaped_on_next_start`：请求进行中 `SIGKILL` 整个 adapter server（子进程在独立 session），断言 attempt 目录仍在；新 server 启动时按 owner-pid 已死回收该目录（默认 TTL 未到期也能回收）。
  - `test_reaper_keeps_active_and_foreign_directories`：活跃（共享锁）、外来服务 marker、非 attempt 目录均保留；TTL 过期与 owner 死亡目录被删。
  - `test_cleanup_failure_is_diagnosed`：`rmtree` 注入失败 → 诊断码、日志、结果 warning 均存在。
- 数据库迁移/历史状态修复：无迁移。**旧版本的平铺文件 `<64hex>.<ext>` 不在新回收器的命名空间内**，部署后需一次性人工清理（见“历史坏状态”）。
- 真实依赖 vs 替身边界：真实 OS 进程、真实 SIGKILL、真实超时、真实文件权限/flock/rmtree；kkFileView 是本地慢 HTTP 替身（只制造“卡住”这一触发条件）。
- 剩余风险：共享卷上 `flock` 若不被支持，回收器会把它当作活跃而跳过（安全但可能留下垃圾）；单容器 tmpfs 部署无此问题。

### B21：implemented（预算/暂存机制 verified；真实 kkFileView/LibreOffice 与大文件全尺寸未跑）

- 根因/触发（当前代码）：子进程 `RLIMIT_FSIZE=MAX_OUTPUT(64 MiB)` 同时限制所有常规文件写入，包括输入原件暂存；70 MiB 合法 Office 在写源文件时被截断，留下 1 KiB 残片；输入/输出共用一个预算。
- 实现：
  - 三个独立预算：`MAX_INPUT`（100 MiB，读入+解码）、`MAX_OUTPUT`（64 MiB，结果 JSON）、`MAX_TEMP`（默认 320 MiB，`ADAPTER_MAX_TEMP_BYTES`，单 attempt 的请求信封+原件+结果总量）。
  - `apply_resource_limits:1509`：子进程 `RLIMIT_FSIZE=MAX_TEMP`（覆盖最大输入），输出仍由 `enforce_output_budget`（recipe.max_output_bytes + MAX_OUTPUT）限制。
  - 父进程写 `input.json` 前先检查 `body + MAX_OUTPUT (+ page 事件预算) <= MAX_TEMP`；子进程 `stage_source` 用剩余额度 `MAX_TEMP - 目录已用` 限制原件，超限 `temp_budget_exceeded`。
  - `write_bounded_file:705`/`stage_source` 的所有失败路径都删除残片；父进程无论成功/失败都清理整个 attempt 目录。
  - macOS 不支持 `RLIMIT_AS` 的既有崩溃：只对 `RLIMIT_AS` 做平台容忍（其余限制照常 `raise`），不降低其他限制。这同时修复了基线中 `test_authenticated_http_subprocess_execution` 在 macOS 必然失败的问题。
- 正确性测试（`test_repair_pages.py:575`）：
  - `test_legal_large_input_with_tiny_output_cap_is_previewed`：真实 ~270 KiB DOCX（随机噪声图片保证不可压缩），`ADAPTER_MAX_INPUT_BYTES=2 MiB`、`ADAPTER_MAX_OUTPUT_BYTES=4096`、`MAX_TEMP=8 MiB`；本地 kkFileView 替身读取源 URL，断言取到的字节与完整 DOCX 完全相等（未被截断），结果 `ready`、1 张 PNG；旧代码在同一不等式下会截断/失败。
  - `test_output_budget_exceeded_fails_and_cleans`：`recipe.max_output_bytes=64` → `failed/resource_limit(stage=output)`，无残留。
  - `test_input_over_limit_is_rejected_without_staging`：70 KiB 输入 + 64 KiB 输入上限 → `not_converted/limit_exceeded`，无残留、未访问 provider。
  - `test_total_temp_budget_stops_staging_and_cleans`：`MAX_TEMP=512 KiB` + ~363 KiB 请求信封 → 子进程 `temp_budget_exceeded`，无残留。
  - `test_staging_budget_and_write_failure_leave_no_fragments`：币额不足与 `fsync` 注入 ENOSPC 都无残片；`test_staged_source_is_private` 断言目录 0700、文件 0600。
- 数据库迁移/历史状态修复：无迁移。旧的截断残片由 B20 的一次性清理命令覆盖。
- 真实依赖 vs 替身边界：真实 DOCX、真实请求体/子进程/RLIMIT/文件系统；kkFileView 与 LibreOffice 是本地替身，未跑真实 Office→PDF；未构造 70 MiB/100 MiB 真实大小（用等比例缩小预算覆盖同一确定性边界）。
- 剩余风险：`MAX_TEMP` 默认值按“100 MiB 输入（base64 约 133 MiB）+ 64 MiB 结果 + 100 MiB 原件”设计；若运维把 `MAX_TEMP` 配得小于合法输入+信封，请求会明确 `temp_budget_exceeded`（这是有意失败，不是静默截断），需在部署文档中写清。

## 测试与证据汇总（命令 / 退出码 / 日志）

| 命令 | 结果 | 日志 |
| --- | --- | --- |
| `/tmp/repair-venv/bin/python -m unittest discover -s converters/tests -p 'test_*.py' -v`（修复前，装好 pin 依赖后） | 35 tests：32 pass / 2 skip / **1 fail**（`test_authenticated_http_subprocess_execution` 因 macOS 拒绝 `RLIMIT_AS=1.5 GiB` 返回 `resource_limit`；属环境/既有平台缺陷） | 见本报告说明 |
| 同上（修复后，最终 HEAD） | **67 tests：65 pass / 2 skip，exit 0** | `evidence/repair-2026-10-26/logs/agent-F-converters-full.log` |
| `/tmp/repair-venv/bin/python -m unittest converters.tests.test_repair_pages -v` | **32 tests，exit 0** | `.../logs/agent-F-converters-new.log` |
| `CLOUDDRIVE_REPO=/Users/konghayao/code/ai/agent-clouddrive-F /tmp/repair-venv/bin/python audit-evidence/tests/audit_converters_repro.py -v` | **exit 1（预期）**：5 条原始缺陷断言因修复而失败（TIFF 页数 2≠1、Office 截断残片 0≠1、超时残留 0≠1、图表 partial≠ready、payload model=recipe≠deployment）；1 条 `test_slow_second_pdf_page_discards_successful_first_page` 仍通过，因为该反例未开启 `page_report`，属 C 的持久页调度范围 | `.../logs/agent-F-audit-counterexamples.log` |
| 原始审计 pinned 日志（只读引用，未修改） | 6 项旧行为原始记录 | `/tmp/repair-kit/agent-clouddrive-repair-kit/audit-evidence/logs/converters-repro-pinned.log` |

补充说明：本地首次运行反例脚本时 `/tmp/repair-venv` 处于半安装状态（缺 Pillow），随后发现 macOS 上 `RLIMIT_AS` 设置必然失败，导致所有真实子进程路径返回 `resource_limit`，因而无法在本机原样重现 4 个依赖子进程的旧缺陷；旧行为的权威记录使用仓库自带 pinned 日志，修复后的行为由新增真实子进程测试覆盖。已在 `apply_resource_limits:1509` 修复该平台问题（只容忍 `RLIMIT_AS`，其他限制保持 fail-closed）。

## 共享文件变更建议（未直接修改）

### Go 侧最小 patch 建议（供 C/主 Agent 集成）

1. `internal/ports/ports.go`（`Recipe`）新增：
   ```go
   PageReport  bool  `json:"page_report,omitempty"`
   ResumePages []int `json:"resume_pages,omitempty"`
   ```
   worker 调度页单元时设置；重试只传失败/缺失页。
2. `internal/ports/ports.go`（`Completeness`）新增 `NotProcessedPages []int `json:"not_processed_pages,omitempty"``；`internal/domain/conversion.go` 的 `Completeness.Validate()` 增加：`Status==ready` 时 `NotProcessedPages` 必须为空。否则 resume 结果（只含部分页、status=ready）可能被误发布为完整产物。
3. `internal/ports/ports.go`（`EngineInfo`）可选新增 `Model`/`Provider` 字段；adapter 已在 `engine` 中输出，当前 Go 结构会忽略。
4. `internal/adapters/converters/http.go`：现有 `HTTP.Execute` 用 `io.ReadAll` + `json.Unmarshal`，无法消费 NDJSON。C 走 CLI 子进程则无需改动；若要走 HTTP，需新增流式 `ExecutePages`（逐行解码）并在未改造前不要发送 `page_report=true`。
5. `internal/application/worker.go`（不建议 F 直接改）：转换后比较 `result.Model.Name` 与 `recipe.Model`（当 `result.Model.Source=="recipe"` 时必须相等）；`status=="blocked"` 或错误码 `model_unavailable` 时不得发布，按 blocked 处理。
6. `recipe.engine` 语义：当前 Go 对所有格式写 `engine="anydoc"`，与 adapter 实际引擎（PDF/文本/Office）不一致。请先决定按格式填充还是仅作记录，再在 adapter 增加强校验；否则会阻塞 PDF/文本任务。

### 历史坏状态（无自动 backfill，需受控操作）

- **旧平铺预览源文件**：新回收器只识别 `attempt-*` 目录。部署并 drain 掉旧进程后，在 preview 容器执行一次性清理（先 `ls` 确认）：
  `find /tmp/preview-sources -maxdepth 1 -type f -name '*.doc' -o -name '*.docx' -o ... -mmin +10 -delete`；更简单：`find /tmp/preview-sources -maxdepth 1 -type f -mmin +10 -delete`（不递归、不删目录、不删 attempt dir）。
- **旧产物重建候选**（只做受控重排，不猜状态）：
  - `tif/tiff` 源且产物 `total_pages=1`（可能多帧）；
  - PDF 含矢量绘图但 `missing_description_pages=[]`（可能漏图）；
  - manifest 的 `recipe.model` 与当时实际部署模型不一致的代次（B19 漂移）。
  这些旧代次不可原地修改；应创建新 generation 重跑，旧代次保留可回退。
- **排队中的旧 recipe**：若其 model 已不存在，重试会 `blocked/model_unavailable`；需运维显式创建新配方/新代次，不能静默改用当前部署模型。

## 合并注意事项

1. 变更仅在 `converters/adapter.py`、`converters/tests/*`；与 C 的 `worker.go`/迁移无文件冲突。若 C 也改 `adapter.py`，以本分支的页协议实现为基线合并。
2. 兼容性：非 `page_report` 请求仍返回单个 JSON，字段只增不减（`model`、`engine.model/provider`、`completeness.not_processed_pages`、`warnings` 新值）；现有 Go `ValidateResult` 会忽略未知字段，不会因新增字段拒绝。
3. `recipe.max_output_bytes` 现在被真正执行（content 字节 + JSON 上限），与 Go `ValidateResult(..., recipe.MaxOutputBytes)` 一致；此前 adapter 忽略它。若某些历史任务把该值配得偏小，会在 adapter 明确失败而不是发布不可验证结果。
4. `recipe.engine` 故意未强制校验（见 B19 剩余风险），避免阻断全部 PDF/文本任务；这是合并后的已知跨模块缺口。
5. HTTP `page_report=true` 返回 `application/x-ndjson`（流式）；非流式客户端（现有 Go `HTTP.Execute`）不要开启该标志。CLI 子进程 JSONL 是 C 当前已实现的路径，无合并阻塞。
6. 建议交叉复核：C 复核页事件切片/`resume_pages`/`not_processed_pages` 合并语义；主 Agent 在真实 Compose + 真实 kkFileView 上复跑 B20/B21 验收；安全复核新增 `/internal/source/<attempt>/<file>` 的 marker 校验与目录清理权限。

## 未跑项（真实依赖）

- 真实 PaddleOCR 模型推理（`RUN_REAL_OCR=1` + `PADDLE_MODELS_DIR` 未配置；测试按设计 skip）。
- 真实 kkFileView 5.0.3 + LibreOffice（`KKFILEVIEW_JAR`/`KKFILEVIEW_APP_CONFIG` 未配置；skip）。Office→PDF 闭环与真实临时目录生命周期未在真实后端上跑。
- 外部/私有真实视觉模型服务（未配置 `DRIVE_VISION_BASE_URL`，全部使用本地 HTTP 替身）。
- 70 MiB / 100 MiB 真实大小的端到端运行（用等比例缩小预算覆盖同一确定性边界；未声明吞吐/容量验收）。
- C 的 `conversion_page_results` 持久化与 worker 重启恢复（跨模块，不在本 worktree）。
