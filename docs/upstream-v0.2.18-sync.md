# 上游 v0.2.18 同步记录

本次从本地 `40929f87e`（`codex/gemini-quota-history-cleanup`）同步 Wei-Shaw/sub2api v0.2.18，固定上游提交 `5c185c5c5d4f5e409e2139806e0964ae944e03e8`。独立工作树与分支为 `sub2api-sync-upstream-20261023` / `codex/sync-upstream-v0.2.18-20261023`；不包含旧 `sub2api` 工作树里的未提交实验。

## 同步内容

按 v0.2.17 之后的提交顺序使用 `cherry-pick -x`，每个提交保留原始上游 SHA。合入流式客户端断开后的用量收尾与计费、异常流终止处理、Codex overload 安全重试与身份规范化、Gemini 负输入 token 修复、工具参数标准化、DeepSeek 图片去重、异步图片下载策略、Vertex 账号配额、Gemini 生图分组门禁、Cloudflare 1010 处理、模型映射签名、调度快照缓存和按配额周期选择累加 SQL。

依赖随上游升级至 Go 1.27.3、DOMPurify 3.4.17、follow-redirects 1.16.1。同步三个 Docker 构建入口和开发指南，保证本 fork 的额外构建入口也符合 go.mod。

不引入赞助商素材/README 变更 `7da09cab4`、`ac1b8f127`，不采用上游 `5c185c5c5` 的 VERSION 写入；保留本 fork 版本与更新仓库身份。

## 定制兼容

- 保留 Gemini Web 接入、额度/健康冷却、有限调度等待、结构化 RPC 错误、历史清理队列和有界操作 CLI。
- 保留 Antigravity 模型冷却隔离、额度重置、组合模型设置和安全批量编辑。
- 保留模型白名单测试、糖果智商测试、完整账号分组、按分组能力过滤的渠道页、门票使用事件、Stripe 内嵌支付和按账号启用的 Codex 协作明文消息。
- 流式收尾保留 fork 的实际响应模型记录、工具映射还原、质量/请求捕获与计费策略。
- 调度缓存保留分组生命周期 fencing、完整账号批量补全及 fork 的模型/分组限制、计费倍率、Excel BPS、Astra 与质量策略字段。模型限流状态使用写时复制，防止请求修改共享快照。

收尾适配修正了调度 pipeline 测试对缓存有效期和 Redis 命令前缀的断言，以及中性 extra 更新测试对 outbox 增量的断言。清理本地 Gemini 历史工具的未检查返回值和错误字符串格式，检查默认 HTTP transport 的类型；无请求上下文的模型检查使用空背景上下文，并移除已无调用方的 Antigravity 重置时间辅助函数。

## 数据与发布

新增 `backend/migrations/246_enable_gemini_image_generation_groups.sql`，将存量 Gemini/Antigravity 分组的生图开关设为开启；不修改 composite 分组。迁移按完整文件名识别，可与现有 `246_group_stream_only.sql` 并存；已有迁移内容保持不变。

上线前需要备份数据库。回退旧程序不会撤销迁移或恢复原生图开关值；需要恢复开关时依据备份逐组处理。本次为源码同步，未部署生产或修改线上账号。

## 验证

后端验证使用 Go 1.27.3、golangci-lint 2.14.0；前端使用 Node 24.11.1、pnpm 9.15.9。详细日志保存在工作区 `output/upstream-sync-v0.2.18-20261023/`。

- 前端冻结锁文件安装、ESLint、TypeScript 检查通过；关键 Vitest 共 87 个文件、1,616 项测试通过。生产构建生成 220 个文件，首页 6 个资源引用均有效。
- 后端 `go test -tags=unit ./...` 与 `go test -tags=integration ./...` 全量通过；新增调度快照与配额累加用例、既有 fork 业务用例共同验证。
- golangci-lint 2.14.0 全量检查通过，0 issues。末轮字符校验表达式和内部错误消息格式调整后，Gemini 相关用例已定向复验，并重新生成最终 embed 构建。
- 调度缓存、快照 pipeline 与模型限流/映射的定向 race 检测通过，覆盖 `internal/repository` 和 `internal/service`，无数据竞争。
- Cloudflare 1010 五平台与 403 处罚边界回归通过，保留 Anthropic safeguard policy 的既有保护。
- 部署脚本语法、Apple Container 模拟生命周期、Compose 安全/网关/运行资源/simple mode 环境及 Caddy 缓存/SSE 检查通过。
- 包含前端静态资源的 macOS arm64 后端 embed 构建及 `--version` 检查通过。验证二进制没有注入发布元数据，使用源码自带版本值。

前端保留一个既有未使用导入警告，以及构建分块、混合导入、Browserslist 数据和 Node 弃用提示；未进行浏览器人工验证。Gemini 历史手工清理 CLI 的可选数据库测试因未设置 `GEMINI_HISTORY_MANUAL_TEST_DSN` 跳过，其他对应局部回归测试使用本地模拟服务，不访问真实账号。

完整验证结果及构建 SHA-256 记录在上述输出目录的 `validation.json`；上游逐提交追溯与完整性核对分别保存在 `source-manifest.json` 和 `source-integrity.json`。
