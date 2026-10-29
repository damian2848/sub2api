# 上游 v0.2.19 同步记录

本次从 fork/main `4235cec56`（v0.2.78）同步 Wei-Shaw/sub2api v0.2.19 及其后 main 上的提交，固定上游提交 `a3298b46e`（2026-10-28）。独立工作树与分支为 `sub2api-upstream-v0.2.19` / `codex/sync-upstream-v0.2.19-20261029`。

同时补齐此前从未同步的上游 v0.2.9～v0.2.10 Claude 额度重置功能，并逐个核对早于 v0.2.18、既没有 `cherry-pick -x` 标记也无法用 patch-id 匹配的上游提交。

## 判定方法

fork 与上游自 `efe9aab1e`（2026-09-17）分叉，此后上游有 505 个非 merge 提交。一个上游提交按以下顺序判定为已覆盖：

1. fork 提交信息中的 `cherry picked from commit <sha>` 标记；
2. `git cherry` 的 patch-id 匹配；
3. `docs/upstream-sync/v0.2.15.md`、`docs/upstream-v0.2.16-sync.md`、`docs/upstream-v0.2.18-sync.md` 中记录的跳过项。

三者都不满足的，计算该提交新增代码行在 fork 当前树中的留存比例，低于约 80% 的逐个对照代码判断。

## 生产补丁

先以 merge 方式并入 `e13d687dc`（线上 `0.2.77+pool-recovery.1`，2026-10-29 20:33 起运行）：Antigravity 通用 429 跨账号扇出上限与近期恢复短等，由 `ANTIGRAVITY_POOL_RECOVERY_GROUP_IDS` 按组开启。

## Claude 额度重置（上游 v0.2.9～v0.2.10）

此前各次同步只处理当次 tag 的增量，最早从 v0.2.14 开始，这一段从未评估。按原顺序 `cherry-pick -x` 以下 10 个提交：`6a8f92165`、`f8984b8a9`、`48970f61b`、`421da3f0d`、`35232c394`、`860782e6f`、`196b47757`、`1872d44b5`、`dc2efc6b9`、`514ad0aa8`。内容为管理端按需查询 Claude 原生重置额度、在重置次数旁增加确认后执行的重置按钮、由服务端选择额度兑换重置并以幂等记录和 Redis 租约防重复。

冲突仅在依赖注入与路由接线：`handler/wire.go`、`service/wire.go`、`routes/admin.go` 两边保留，`cmd/server/wire_gen.go` 用 wire v0.7.0 重新生成。同时带入同日的 `475c0839c`（gofmt 一个测试文件）。

## 上游 v0.2.19 与 main 增量

按 `upstream-v0.2.18..upstream/main` 的拓扑顺序 `cherry-pick -x` 51 个提交。主要内容：

- 按操作与资源范围授权的管理员 API 密钥（`e445cc3e2`，迁移 247）。
- 多图请求按实际出图张数计费，分组级开关与端点、计价模式门控（`fd998129e`、`aa1a1b11c` 等，迁移 248）。
- Grok 模型目录观测、同步、差异诊断与编辑界面，约 25 个提交。
- GPT-6.1 Terra；Gemini 同账号重试次数可配置（默认行为不变）；Gemini 默认模型列表刷新；国产 Coding Plan 频率型 429 秒级冷却；分页溢出、管理员保护规则、OAuth 透传非流式、渠道 web_search 维度、图片上传超限等修复。
- `golang.org/x/net` 升至 v0.62.0，修复 GO-2026-6860。

跳过：`3f1c8f36c`（赞助商素材）、`5c185c5c5` 与 `a9240d501`（上游 VERSION）、`c01b2cb81`（空提交重触发 CI）。

### 冲突与兼容处理

- 管理员 API 密钥与 fork 的观察者限定权限共存。账号仓库的 `Create` / `GetByID` / `BindGroups` 先执行上游的事务内转发，再保留 fork 的初始质量计划事务、`observerAccountQuery` 与 `ValidateObserverGroupBindings`，最后进入上游的范围密钥分支。代理列表先检查范围密钥，再检查观察者。账号创建后的隐私设置改用上游的提交后执行，fork 的 `recordAutoConfigInitial` 仍在事务内；清除账号错误时，在提交后同时清除调度阻塞（上游）并通知 Codex harvest（fork）。设置路由采用上游的 `stepUpAuth` 签名并保留 fork 的请求抓取路由。
- `ent/group.go` 等生成代码在合并 schema 后用 ent 重新生成，保留 fork 的分组字段（如 `stream_only`）。
- API key 鉴权快照版本由 fork 的 27 升到 28（上游为 25），使包含 `image_count_billing` 的新快照生效。
- 账号更新的 `lockAndMergeAccountProbeExtra` 同时读取 fork 的 `current_extra` 和上游的 `grok_observed_models`、`upstream_model_metadata`；相关 sqlmock 测试同步补列。上游 `fetchUpstreamModelListRequest` 返回值增加 `*http.Request` 后，fork 的 Excel BPS 与 Gemini Web 分支返回 `nil` 请求。
- DeepSeek V4 Pro 计价：采用上游的不再按日期切换价格，保留 fork 的显式缓存存储单价。
- Codex 模型列表 singleflight 复查缓存：保留 fork 已抽出的 `fetchAndCacheOpenAIModels`（含 ctx 取消和 nil 检查）。
- 依赖：在 fork 的 go.mod 基础上 `go get` 上游目标版本，取两边较高者（`x/image` 保持 fork 的 v0.45.0）。
- 前端：`ModelWhitelistSelector` 保留 fork 的 `SyncUpstreamPreviewParams` 类型并加入上游 `showSyncButton`；账号编辑、类型定义、模型下拉候选总表和测试文件两边合并。

## 早于 v0.2.18 的未覆盖提交

| 上游提交 | 处理 | 说明 |
| --- | --- | --- |
| `27a9421e6` fix(anthropic): rewrite tool names in one body pass | 应用 | 此前遗漏，干净应用 |
| `2840cdece` feat(frontend): hide unsupported clients for Claude Code-only groups | 应用 | fork 有 `claude_code_only` 分组 |
| `2e3c2cf59` fix(i18n): correct the custom error codes warning text | 应用 | 原文案「其他错误将返回 500」与实际行为不符 |
| `5b5f315b8` chore: remove model selector review screenshot | 应用 | 仓库清理 |
| `bd48b2b2d` fix(antigravity): keep compat streams alive before first content | 跳过 | fork 的兼容流只在出现实际内容后发 keepalive。首包前发送会提前提交 HTTP 200，之后无法换号，与 fork 的故障切换和 pool-recovery 冲突 |
| `c2cca510f` revert(accounts): restore inline account filters | 跳过 | fork 的 `f86ba0316` 已撤回该改版，并在同一组件加入家庭组筛选 |
| `6073704fd` fix: omit Codex model catalog for OpenAI groups | 跳过 | fork `2abce6503` 有意为 OpenAI 分组显示 Codex 目录（GPT 模型名会路由到其他上游） |
| `c4c2e6607` feat(models): GPT-6 Sol, Luna and Claude Opus 5.5 | 已有 | 早已移植，fork 将 Opus 5.5 处理扩展到 Sonnet/Haiku 5.5 |
| `bcc73f8d4` fix(openai): DeepSeek reasoning_content on chat fallback | 已有 | fork 的 `isDeepSeekSemanticsChatUpstream` 覆盖范围更广 |
| `17ce21c03` test(scheduler): pass explicit multiplier pointer | 已有 | fork 测试已按指针传参 |
| `d9d433379`、`99efaa477` 安全策略文档 | 跳过 | 内容为上游项目的漏洞报告渠道 |
| VERSION 同步、赞助商 | 跳过 | 保留 fork 版本与仓库身份 |

## 数据库迁移

新增 `247_scoped_admin_api_keys.sql`（只依赖 `users`）与 `248_group_image_count_billing.sql`（只修改 `groups`），保留上游文件名。迁移执行器按完整文件名记录并按文件名排序执行，未执行过的迁移会补跑；已部署库会在启动时执行这两个文件。全新数据库中它们排在 fork 的 248～287 之前，两者依赖的表在更早的迁移中已创建。保留上游文件名也避免以后再同步时出现重复迁移。

## 收尾适配

- 上游 `31564b2e8` 从 Gemini 兜底模型列表移除已停用的 2.x 模型后，fork 的 Gemini Web 作用域回归测试改用 `gemini-3.1-pro-preview`：它仍在兜底列表内，但不在 Gemini Web 默认范围，测试前提保持成立，网关行为不变。
- 格式化 fork 原有的 `api_key_admission_integration_test.go`。

## 验证

均在本地运行，没有生产操作。

| 检查 | 结果 |
| --- | --- |
| `golangci-lint run ./...`（v2.14.0，与 CI 相同） | 0 issues |
| `govulncheck ./...` | No vulnerabilities found（GO-2026-6860 已随 `x/net` v0.62.0 修复） |
| `go test -tags=unit ./...` | 通过（含 `internal/mihomo`） |
| `go test -tags=integration ./...` | 通过，本地 Docker PostgreSQL / Redis |
| 前端 `lint:check`、`typecheck` | 通过；1 个 warning 为 fork 原有 SettingsView 测试的未使用变量 |
| 前端 `vitest run`（全部） | 489 个文件、4406 个用例通过，9 个原有 skip |
| 前端 `build` | 通过 |
| `pnpm audit` 例外检查 | 剩余 `vue-i18n` / `@intlify/*` GHSA-83rm-6c8r-g728：修复版本为 11.4.14，需从 9.x 跨两个大版本升级，上游同样未处理，本次不纳入 |
