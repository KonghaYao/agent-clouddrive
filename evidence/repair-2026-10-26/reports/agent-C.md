# Agent C 报告（实施中，协议对齐草稿）
分支：repair/C；最终 HEAD 与实测结果将在交付时补齐。

## 与 Agent F 的 v2 协议对齐点
- 请求 `page_report:true`、`resume_pages:[全局 1-based 页号]`，缺省首次全量；重试只传失败/缺失页。
- 消费每行 `type:page/page/stage/state/result_ref/error` 与最终 `type:result`；旧单 JSON 继续支持。
- **result_ref 的续跑最低需求**：必须持久可恢复该页 bytes 与文档总页数。C 当前实现接受 ConversionResult 页切片：`{markdown, assets, pages:[{page,block,text_status,description_status}], completeness:{total_pages}, engine:{name,version,model,provider}}`，并兼容顶层 `total_pages`。最终阶段事件应带完整该页切片，之前阶段可以带同页部分切片。不能仅引用子进程临时文件。
- `text/description` 的 ready 或 not_required 才视为该页成功；failed 必须 error.code。成功页不再进入 resume_pages。页号/总页数超限为 limit_exceeded，不发布截断结果。
- `engine.model` 记录实际模型；配置 recipe.model 时，执行结果必须相同，否则 blocked/model_unavailable。不落库密钥或依赖原始异常信息。
- C 实现 HTTP JSONL 增量消费及本地 Process（`--execute file`）消费。部署仍保留 HTTP，需要 F 的 HTTP 父进程把子进程 page 事件及时流式透传，而不是等进程退出后整体返回；若 F 仅实现 CLI，HTTP 链路应明确列为合并阻塞。

## 逐项
B06/B12/B17：implementing。数据库迁移和初版实现已通过现有 converters/application 测试（原生 PG）；专项正确性、真实子进程和 PDF 验收进行中，不作为最终 verified。
