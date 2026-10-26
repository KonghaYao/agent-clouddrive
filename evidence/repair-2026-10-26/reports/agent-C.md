# Agent C 报告（实施中，协议对齐草稿）
分支：repair/C；最终 HEAD 与实测结果将在交付时补齐。

## 与 Agent F 的 v2 协议对齐点
### 当前实际契约差异（需 F/主 Agent 同步）
直接读取 F 工作树后确认：F 的 page_report/resume_pages 位于 recipe（SHARED_DESIGN 示例为请求顶层），C 将同时发送两处以兼容；F result_ref 是阶段增量片段，C 将按 stage 顺序累计，不当作整页快照覆盖。
**阻塞精确续跑的缺失字段**：F 的 page_ref 当前不含 total_pages，也不含 engine。请 F 在每个 page 事件 result_ref 附加 `total_pages`（真实文档页数、非已选页数）及 `engine:{name,version,model?,provider?}`，最好在 PageReporter/convert_pdf/raster 设置上下文并统一注入，首个事件就必须有这些。否则子进程被杀时 C 无法知道缺了几页，也无法创建可校验 manifest，不会猜测最大已收页号。
多行 result：C 使用 json.Decoder 连续读 JSON 对象，已天然兼容 pretty-print，正补回归/真实 F 验证。

- 请求 `page_report:true`、`resume_pages:[全局 1-based 页号]`，缺省首次全量；重试只传失败/缺失页。
- 消费每行 `type:page/page/stage/state/result_ref/error` 与最终 `type:result`；旧单 JSON 继续支持。
- **result_ref 的续跑最低需求**：必须持久可恢复该页 bytes 与文档总页数。C 当前实现接受 ConversionResult 页切片：`{markdown, assets, pages:[{page,block,text_status,description_status}], completeness:{total_pages}, engine:{name,version,model,provider}}`，并兼容顶层 `total_pages`。最终阶段事件应带完整该页切片，之前阶段可以带同页部分切片。不能仅引用子进程临时文件。
- `text/description` 的 ready 或 not_required 才视为该页成功；failed 必须 error.code。成功页不再进入 resume_pages。页号/总页数超限为 limit_exceeded，不发布截断结果。
- `engine.model` 记录实际模型；配置 recipe.model 时，执行结果必须相同，否则 blocked/model_unavailable。不落库密钥或依赖原始异常信息。
- C 实现 HTTP JSONL 增量消费及本地 Process（`--execute file`）消费。部署仍保留 HTTP，需要 F 的 HTTP 父进程把子进程 page 事件及时流式透传，而不是等进程退出后整体返回；若 F 仅实现 CLI，HTTP 链路应明确列为合并阻塞。

## 逐项
B06/B12/B17：implementing。数据库迁移和初版实现已通过现有 converters/application 测试（原生 PG）；专项正确性、真实子进程和 PDF 验收进行中，不作为最终 verified。
