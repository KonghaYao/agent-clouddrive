# R4 example 独立打包安全审查

本记录审查源码交付边界，不代表 Docker Compose、真实浏览器或全规格验收通过。机器可读记录见 [package-review.json](../evidence/example-r4/package-review.json)。最终生成的不可变 ZIP 另行核对，不能用本记录替代最终 ZIP 校验。

## 已完成

- `scripts/package.py` 与 `scripts/release-readiness.py` 的目录及运行时文件排除集合一致，包含新补充的 `preflight-result.json`。
- 在仓库外创建隔离的合成策略 fixture。87 个凭据、运行时 journal/result、缓存、构建目录、私有诊断目录、敏感后缀及外部 symlink 路径均未进入 ZIP。留下的 5 个源文件逐项 manifest SHA256 与 ZIP CRC 正确。
- 加入这些排除对象没有改变 fixture 的 source digest；打包脚本拒绝覆盖现有 ZIP，拒绝嵌入的私钥头标记。
- release-readiness 的 21 项单元测试通过；这是门禁逻辑测试，不是发布门禁通过。
- 候选交付文件中的私钥头、GitHub/AWS/OpenAI/Slack token 形状及 JWT 形状扫描无命中。模式扫描只是辅助检查，不能证明任意文本都不可能是秘密。
- 另对本轮明确指定的 7 个私有合成凭据文件做仅内存逐值比较：14 个独立秘密值均未命中候选交付文件。没有将凭据值或原始配置写入仓库或报告。额外敏感字面值筛查仅命中两个显式测试 fixture，占位测试字符串不算真实泄漏。
- 根 README 与中文 example 手册的 `start`、`check`、`status`、`stop` 命令与 argparse 接口相符。启动器拒绝源码目录内部或父目录作为 runtime，并拒绝 runtime symlink；运行数据与账号不会由正常启动流程写入源码树。
- Docker 构建上下文排除整个 `examples` 与 `evidence`，以及 node_modules、缓存、运行时秘密及结果文件。真实 example 工具从只读源码挂载执行，私有 runtime 使用独立挂载。

## 最终交付应维持的边界

最终 ZIP 应逐项核对 `SHA256SUMS.json`，确认无多余或遗漏条目、无 symlink、无 runtime 或构建垃圾；解压后的 source digest 必须与 `docs/acceptance.json` 一致。release-readiness 应保持非零退出、错误列表为空、11 pass / 22 partial / 4 blocked，不得把剩余必需验收改为通过。

R3 输入 ZIP 的 SHA256 为 `ad57cf17ae4a31b348d83ee86b3a357447feaed4f84644564011e2069300e492`；最终交付前再次确认它未被修改。

当前环境未运行完整 Docker Compose，也未完成真实浏览器验收。本审查不改变这些限制。
