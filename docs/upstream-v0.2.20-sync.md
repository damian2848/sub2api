# 上游 v0.2.20 同步记录

本次从 fork/main `d0fef05bd`（v0.2.79，即线上 `e1ad6125d` 加 VERSION 同步）同步 Wei-Shaw/sub2api v0.2.20（tag 指向 `dab829a4e`，2026-11-07 发布）。上次同步固定在上游 `a3298b46e`，本次范围为 `a3298b46e..v0.2.20`：67 个非 merge 提交，加 3 个带冲突解决内容的 merge。独立工作树与分支为 `sub2api-upstream-v0.2.20` / `codex/sync-upstream-v0.2.20-20261107`。

上游 tag 与 fork 自己的 `v0.2.20` 同名，fetch 时取到 `refs/upstream-tags/v0.2.20`，不覆盖本地 tag。

## 应用

按拓扑顺序 `cherry-pick -x` 58 个提交。主要内容：

- 管理 MCP：OAuth 认证的管理 MCP 接口、授权确认与连接管理页面（迁移 `249_mcp_oauth.sql`）。需配置 `MCP_PUBLIC_URL` / `mcp.public_url` 才启用，默认关闭；MCP go-sdk v1.4.1。
- 管理员密钥：补全权限项与中英文展示，新增需输入确认文字的「全权兼容模式」（`adminaccess.RestrictedFromContext`）。
- 注册邮箱精确黑名单（批量粘贴、Gmail 点号/+后缀变体、保存时批量禁用命中用户）。
- OpenAI 生图账号按 1K/2K/4K 尺寸档位分流；GPT Image 2.6 原生链路；生图测试参数修复。
- 网关资源上限：请求体内存预算（`gateway.request_body_memory_budget_bytes`，默认 1 GiB，超出 503 + Retry-After）、请求体单次读取超时（`gateway.request_body_read_timeout_seconds`，默认 120 秒，408）、下游单次写超时（`gateway.downstream_write_timeout_seconds`，默认 120 秒）、客户端断开后的上游 drain 窗口、有界会话缓存、SSE 队列字节预算；请求体读取失败按 408 / 499 / 413 / 400 分类并记录不含内容的诊断。
- 热路径优化：thinking 过滤、Codex 引导/压缩判断不再解码全文，Gemini 流增量不再复制前缀，RPM 覆盖与分组隐私门控改从鉴权快照读取。
- 其余修复：Kimi 并发 403 短冷却、Antigravity 身份前导、Antigravity 图像模型 INTERNAL 500 只冷却该模型、Antigravity 连接测试发送管理员提示词、OpenAI 透传空 instructions、非流式 `/v1/responses` 的 incomplete/cancelled、WebSocket 桥接 Codex 指纹、首 token 时间、强制路由模式不再探测、API Key 校验时断开/格式错误不再 500、Grok Voice / Grok 委派消息、apicompat reasoning 与消息字段、Gemini 原生 function response 工具媒体、Bedrock EventStream 帧长度、Cloudflare REST beta 参数、Anthropic setup-token 周用量、用户导出 CSV 每页 1000 条、Codex 导出默认 GPT-6.1 Sol 并开启实时联网搜索、GPT-6.1 Terra 官方描述与 effort 校验、可关闭 OpenAI 传输错误冷却。
- 依赖：vue-i18n 升到 v11（同时消除上次记录的 GHSA-83rm-6c8r-g728 例外）、brotli v1.2.7、klauspost/compress v1.20.2、follow-redirects 1.16.2、decode-uri-component 0.5.1。

## 跳过

| 上游提交 | 原因 |
| --- | --- |
| `e353c8322`、`251094436`、`876b96d20`、`25608d216`、`e6c19082f` | 赞助商素材 |
| `7e3b6c72f` cyber transcript 按已屏蔽前缀长度精确匹配（#8519） | fork 已删除 transcript 屏蔽，改为按显式会话身份（thread_id / session_id）屏蔽，长会话误拦的代码路径不存在 |
| `bf4e16b90` Antigravity 配额组真实 5h/周窗口 | fork 自 0.2.70（`948302955`）起已有同类实现：按 proto 解析 `retrieveUserQuotaSummary`，只查询账号实际生成所用的端点，已在线上运行 |
| `455751402` Pro 套餐 Argon 配额单独成行 | 上一项的前端补丁；fork 按上游返回的组名逐组显示，Argon 组本就单独一行 |
| `0b535b5c0` Antigravity 把 `responseJsonSchema` 改写为 `responseSchema` | 与线上 gemini-enums 补丁的结论相反：fork 有测试锁定「`responseJsonSchema` 保留原样并保留类型化 enum」，上游则称 Antigravity 会静默忽略该字段。改写还会经 `CleanJSONSchema` 把整型 enum 改成字符串。需用真实 Antigravity 账号实测哪方正确后再决定 |

上游 main 在 v0.2.20 之后只有 `6b4b37db4`（VERSION 同步），同样不取。

## 冲突与兼容处理

- **管理 MCP**：CORS 同时放行 fork 的 Pelican 展示页预检和上游的 MCP OAuth 公共端点，允许头与暴露头取并集（`Anthropic-Version`、`X-Client-Request-ID`、`MCP-*`、`WWW-Authenticate`）。侧边栏、功能开关、公共设置类型同时保留 fork 的工单入口与上游的 MCP 入口。
- **管理员密钥全权模式**：账号用量探测写快照时先做上游的范围校验，再执行 fork 的 OpenAI 额度恢复（`recoverOpenAIQuotaRateLimit`）；受限密钥不触发自动重置通知。fork 拆出的 `persistOpenAICodexProbeResponse` 增加 ctx 参数。
- **OpenAI 透传 instructions 回显**：fork 的账号准入复读与协作明文还原保留在前，上游的 instructions 回显还原接在后面。
- **生图尺寸分档**：调度入口按尺寸档逐层选号，保留 fork 的具名返回值与代理隔离 fail-open 二次选号；`scheduler_cache` 同时保留 fork 的 Excel BPS 字段和上游的尺寸档字段。
- **请求体内存预算（`httputil/body.go`）**：以上游预算版为基础，加回 fork 的请求抓取钩子（`requestcapture`）与 `ReadRequestBodyWithPreallocLimit`（按调用方解压上限，zstd 内存上限随之变化）。上游 drain 超时经 fork 的 OpenAI 预请求重试封装透传到 `doUpstreamRequest`，fork 的请求计时与抓取 defer 保留。fork 的 API Key 槽位租约上下文（`detachAPIKeyUpstreamContext`）改为在上游 `DetachUpstreamContext` 之上构造，使客户端断开时同样启动有界 drain。平台配额降级写库采用上游的同步写法。会话缓存换成上游的有界 `sessionMemoryCache`，关闭时与 fork 的区域计量、Astra 停止一起执行。OpenAI 透传流式读取保留 fork 的 `openAISSEReadPump`（含首输出超时与心跳），不换回上游的 bounded scanner。
- **请求体读取超时**：上游 `3425ba366` 先加了 `server.request_body_idle_timeout`，随后 merge `fdfadb244` 用 `WithBodyReadTimeout`（`gateway.request_body_read_timeout_seconds`）取代并删除了它，同时把 `handleRequestBodyReadError` / `RejectRequestBodyReadError` 改为带日志参数、读超时不再被当成客户端断开。逐个 cherry-pick 拿不到 merge 的内容，所以这些文件（`request_body_limit.go`、`request_body_read_log.go`、`body_read_error.go`、`request_body_memory.go`、`request_body_read_failure.go` 及其测试）直接对齐上游 v0.2.20 最终版，并删除 idle 超时的代码、配置项与文档。
- **鉴权快照版本**：fork 已是 28，上游这次两处升级（RPM 覆盖分组 ID、分组隐私门控）依次记为 29、30。
- **Gemini 流增量**：fork 的「参数不延续则另起一个工具块」判断改用新 `geminiTextDelta` 的已发送内容。
- **Gemini 工具媒体**：Gemini Web 路径传空模型名，工具图片仍作为独立 part（Gemini Web 自行拼接提示词，不支持 functionResponse 内嵌媒体）；保留 fork 对 `document` 块的支持。
- **Responses 输出序列化**：message 与 tool_search_call 交给上游 `responsesItemWire`，保留 fork 的 reasoning 始终输出 summary 数组。
- **Antigravity 连接测试**：采用上游「发送管理员提示词（默认 hi）+ 256 输出 token」；只有 Pelican 测试（上下文带 Pelican 标记）使用 32000 token。
- **GPT-6.1 长上下文附加费**：保留 fork 规则（Sol 在 OpenAI OAuth / Setup Token / API Key 上都豁免），Terra 按上游只在订阅凭据（OAuth / Setup Token）上豁免。
- **API Key 校验**：非 UTF-8 / 超长 key 直接按不存在处理（上游），区域本地鉴权分支保持在前。
- **前端**：Codex API Key 导出改用 `experimental_bearer_token`，`requires_openai_auth = false`，fork 测试断言随之更新；保留 fork 的 `api_key_model_discovery` 断言。`pnpm-lock.yaml` 用 pnpm 9 重新生成。
- 上游测试适配 fork 签名：`NewOpenAIGatewayService` 多一个 ProxyRepository、`ProvideHTTPServer` 多一个 Lifecycle；MCP 路由测试的设置仓库桩补 `GetMultiple`（fork 的 `UpdateSettings` 会检测 Codex harvest 设置变化）。

## 核对方法

全部提交应用后，对上游在本区间改过的每个文件，统计「上游新增而本树缺失」与「上游删除而本树仍保留（且非 fork 新增）」的行。除跳过的四项外，剩余差异逐个核对，均为 fork 自有逻辑或计数误报，并据此补回了上述 merge 内容。

## 数据库迁移

新增 `249_mcp_oauth.sql`：建 `mcp_oauth_grants`、`mcp_oauth_codes`、`mcp_oauth_credentials` 三张表，只引用 `users`。保留上游文件名；fork 已有 `249_account_ops_alerts.sql` 等同号文件，执行器按完整文件名记录、按文件名排序，已部署库会在启动时补跑。只新增表，回滚到旧二进制不受影响。

## 线上配置影响

- 线上未配置以下新项，将使用默认值：请求体内存预算 1 GiB（主机 60 GB、容器无内存限制）、请求体单次读取超时 120 秒、下游单次写超时 120 秒（按单次写/flush 计，不限制流的总时长）、SSE 队列字节预算 8 MiB、单事件 4096 行。SSE 单行上限默认值原本就是 500 MiB，只是配置示例从旧的 40 MB 更正过来。
- 管理 MCP 默认关闭。
- 客户端断开后的上游 drain 有上限，超时截断会影响该请求计费口径（上游破坏性变更说明）。
- GPT-6.1 Terra：订阅凭据超过 272k 不再收长上下文附加费；兼容路径的 minimal effort 改为返回 400。

## 验证

均在本地运行，没有生产操作。

| 检查 | 结果 |
| --- | --- |
| `golangci-lint run ./...` | 0 issues |
| `govulncheck ./...` | 代码可达漏洞 0 |
| `go vet ./...`、`go vet -tags unit ./...` | 通过 |
| `go test -tags=unit ./...` | 全部通过 |
| 前端 `typecheck`、`lint:check` | 通过；1 个 warning 为 fork 原有 SettingsView 测试的未使用变量 |
| 前端 `vitest run`（全部） | 498 个文件、4483 个用例通过，9 个原有 skip |
| 前端 `build` | 通过 |
