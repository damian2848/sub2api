# 上游 v0.2.21 同步与调度优化记录

本次从 fork/main `9b669d584`（v0.2.80）同步 Wei-Shaw/sub2api v0.2.21（tag 指向 `10ba8f9b4`，2026-11-11 发布），并在同一分支上落地一批调度、并发、上游 IO 和计费方面的修复。上次同步固定在上游 v0.2.20（`dab829a4e`），本次范围为 `dab829a4e..10ba8f9b4`：44 个非 merge 提交，加 15 个带冲突解决内容的 merge。独立工作树与分支为 `sub2api-upstream-v0.2.21` / `codex/sync-upstream-v0.2.21-20261111`。

上游 tag 与 fork 自己的 `v0.2.21` 同名，取到 `refs/upstream-tags/v0.2.21`，不覆盖本地 tag。

分支分两段，可以分开发布：

- 第一段（到 `5d896dd3c`）：上游同步与补漏。
- 第二段（`5d896dd3c..HEAD`）：fork 自己的修复与优化，45 个提交（含独立审查后的修正）。

## 一、上游同步

### 应用

按拓扑顺序 `cherry-pick -x` 40 个提交。主要内容：

- 模型：Claude Fable 5.2 与 Mythos 5.2（目录、Anthropic 预设、Bedrock 映射、OpenCode 配置、兜底定价）；Sonnet 5.5 兜底缓存读取价改为 $0.10；定价目录哈希不一致时重试同步；内置 Claude Code 版本基线 2.1.321。
- OpenAI 计费：缓存写入按 30m / 12h TTL 拆分（迁移 `250_openai_cache_write_ttl.sql`）；普通输入免费时保留 Fast 倍率。
- OpenAI 网关：合成的 reasoning effort 应用映射与上限；显式模型级冷却规则对过载生效，关键词只匹配实际错误字段；Codex 元数据事件不再提前提交响应流；原生 WS 后续轮次限流不再重放首帧；OAuth 池化 WS 在启用搜索前重连；WS 首 token 时间跟随 TTFT 口径；Codex Desktop 页面上下文规范化；原生 Codex 生图保留错误状态码；非法 UTF-8 的 Responses 请求体直接 400；Responses 会话种子在工具回合中保持稳定。
- 兼容层：保留 sealed function call 元数据；跳过外来的 CC Switch reasoning 封包；Anthropic 兼容路径遵从账号错误策略；Gemini 兼容入口非流式响应不再混入思考摘要。
- Antigravity：并行工具结果里的图片不再打断 functionResponse 序列；Pro 账号 Argon 配额冷却隔离。
- 调度与鉴权：账号 hydrate 失败时释放已占用的槽位；refresh token 轮转时原子消费。
- 资源：请求体内存预算未配置时取进程内存上限的 1/4（最低 1 GiB）；cgroup 上限读取限定在挂载点内。
- 管理端：API 密钥列表不再等 30 天用量统计（迁移 `249_add_usage_logs_api_key_cost_index_notx.sql`）；账号列表刷新时保留现有行；系统设置保存按钮吸附；余额充值倍率支持 6 位小数；EasyPay 退款成功码按实例配置；安全审计任务入队后立即唤醒 worker。

### 跳过

| 上游提交 | 原因 |
| --- | --- |
| `5184ff55f`、`993526857` | 只改 README 生图章节；fork 的 README 已精简，没有落点 |
| `4ac1675b5` Antigravity 用量「查询」按钮绕过缓存 | fork 已有同等功能且更完整：`getAntigravityUsage(ctx, account, force)` 强制刷新会绕过成功与错误缓存，前端已有 `antigravity-query` 按钮。上游测试依赖 fork 没有的缓存结构，不适用 |
| `6b4b37db4` | VERSION 同步 |

上一次遗留的 `0b535b5c0`（Antigravity `responseJsonSchema` 改写为 `responseSchema`）仍未采纳，等真实账号实测。

### 冲突与兼容处理

- **OpenAI 缓存写入 TTL**（`openai_gateway_usage.go`）：保留 fork 的 Excel BPS「cache creation 计入 input」分支，30m / 12h 拆分随聚合值一起置 0。下游 usage 归一化和受控实验的成本估算同步带上这两个字段。gpt-6-astra 的 12h 写入按上游规则计 2.5 倍输入价，相关测试的期望成本已更新。
- **Antigravity Pro Argon 冷却**（`model_rate_limit.go`）：fork 写入时只冷却最终模型，不按前缀推断家族 scope。保持这一写入语义，读路径新增 `antigravityModelRateLimitReadKeys`，Pro 的 Argon 变体读独立 scope，不受共享 Gemini scope 影响。上游写入用例按 fork 语义改写。
- **调度 hydrate 失败释放槽位**（`gateway_scheduling.go`）：fork 已有同等修复，并多一个 `acquired` 守卫，保留 fork 条件。
- **合成 effort 校验**：保留 fork 的 `validateGPT61CompatRequest` 命名和 Sol + Terra 覆盖范围，按上游拆成原始显式选择校验与完整校验两步；Messages 两条桥在策略映射之后再做完整校验。
- **WS 首 token**（`openai_ws_v2/passthrough_relay.go`）：fork 的 `turnWroteDownstream` 与上游的 `isFirstTokenEvent` 并存。
- **OpenAI 流终止副作用**：fork 的 Grok 专属分支保持在前，上游「非 OpenAI 平台的 503 直接返回」紧随其后。
- **前端账号页**：`AccountUsageCell.vue` 保留 fork 的 `ExcelBPSCredentialStatus`，加上游的 `aria-busy`；fork 的 Codex ticket 与升级进度用例全部保留。
- 上游测试适配 fork 签名：`NewOpenAIGatewayService` 多一个 ProxyRepository。

### 补漏

逐个 cherry-pick 拿不到 merge 提交里的冲突解决内容。用两个脚本核对：按「上游新增行在本树的覆盖情况」逐文件比对，以及反查「上游某个提交加了、上游最终又删了、fork 却保留」的行。

查出一处：上游 merge `f6ea845c5`（保留已配置的 GLM 降级语义）删掉了 `aa245176a` 新增的 `applyOpenAIReasoningEffortPolicyForGLMFromContext`，fork 单独 cherry-pick 后还留着它，导致 GLM 在 Messages 转 Chat 的回退路径上被二次规整：low 映射被改成 high、xhigh 上限被压到 high、low / medium 上限直接 403。补漏提交 `5d896dd3c` 把回退路径改回上游写法并删除该函数，三个测试文件取上游版本，保留 fork 的 Terra 断言。

其余差异逐个核对，均为 fork 有意不同或脚本按行比对的误报。

### 数据库迁移

- `249_add_usage_logs_api_key_cost_index_notx.sql`：在 `usage_logs` 上用 `CONCURRENTLY` 建 `(api_key_id, created_at) INCLUDE (actual_cost)` 索引。不锁表，表大时首次启动耗时较长。
- `250_openai_cache_write_ttl.sql`：`usage_logs` 加两列 `INTEGER NOT NULL DEFAULT 0`；渠道与账号统计的 4 张价卡表各加一列可空的 `cache_write_12h_price`。均为 `ADD COLUMN IF NOT EXISTS`。

保留上游文件名。fork 已有同号文件，执行器按完整文件名记录，不冲突。两个迁移都只新增，回滚到旧二进制不受影响。

## 二、fork 自己的修复与优化

来源是对 v0.2.80 的 12 个方向的只读审计，每条结论由另一名审查者逐条反证，只落地了被确认且评为 P0 / P1 的条目。实现后每组再做独立代码审查，审查提出的 major 全部处理，minor 处理了一部分，其余列在「审查遗留」。

### 新增配置项

| 配置键 | 默认 | 作用 |
| --- | --- | --- |
| `gateway.scheduling.fallback_probe_hard_slots` | `true` | 所有候选都满载时，排队前先用还有真实空闲槽位的账号。`false` 恢复旧行为 |
| `gateway.scheduling.queue_full_reselect` | `true` | 首选账号等待队列已满时重选一次，只接受当场抢到槽位的账号 |
| `gateway.scheduling.gemini_long_cooldown_skip_same_account_retry` | `true` | Gemini 429 冷却超过 16 秒时不再同号重试，直接换号 |
| `gateway.model_availability_diagnosis_cache_ttl_ms` | `500` | 选不到账号时的模型可用性诊断短缓存，0 关闭，上限 5000 |

### 对外可见的行为变化

- 全部候选的负载率都达到 100% 时，有空闲硬槽位的账号会按优先级直接承接请求。`load_factor` 只决定 Layer 2 的满载判断。全部占满时仍只在首位账号排队，队列满照旧 429，不会把排队溢出到下一个账号。排队请求之间不保证先到先得：新到的请求可能先于已排队的请求拿到刚空出的槽位，这一点与改动前相同。
- 非 OpenAI 分组的 `/v1/responses`、`/v1/chat/completions` 和 SystemOne 排队时计入账号等待数，队列满立即返回 429 `gateway_queue_full`。以前是无上限等待到超时。
- 批量清空负载因子现在真的写成 NULL。以前请求报成功但库里的值不变。
- Info 级日志不再输出 `sticky.*` 事件，也不再记录原始客户端标识。排障时需要临时调到 debug。
- Gemini（非 Antigravity）长冷却的 429 立即换号，不再同号重试约 15 秒。countTokens 仍返回本地估算。单账号分组会更快拿到 429。
- 直连且启用 TLS 指纹的账号，新建连接 10 秒内建连或握手不成功即报错并走 failover。以前会挂约 130 秒。
- 单账号到同一上游 host 的连接上限为并发数加 2。上线后各账号的 HTTP client 会重建一次。
- 停机时如果有请求没有随排空结束，最多多等 3 秒让它们释放槽位，另有最长 10 秒的 ops 错误日志排空。compose 的 `stop_grace_period` 需要不少于排空延迟、优雅关停超时（5 秒）、这 3 秒与清理余量之和，Docker 默认的 10 秒偏紧。
- 账号状态变化（配置、限流、模型冷却）最多晚 0.5 秒反映到「选不到账号」时 404 与 429 / 503 的判定上。空的调度桶里靠时间流逝恢复的账号最多晚 250 毫秒可见。
- API Key 限速缓存不再自行开启窗口。窗口未开启或到期后的第一笔扣费会丢弃缓存条目，下一次检查从数据库回源，每个 Key 每次窗口滚动多一次回源。

### 提交清单

通用网关调度：

- `136f5edde` Layer 3 排队前使用真实空闲槽位。`1f83066e6`（探测时扣掉欠给排队请求的槽位）已由 `7890a439b` 回退，原因见下
- `3c9d69227` 批量清空 load_factor 写库
- `f8fd5331f` 队列满与等待失败时释放 max_sessions 注册；`3d2f18731` 重选后保留已绑定会话并复查 warmup 拦截；`56606d46d` Gemini 排队换号后同样复查 warmup 拦截
- `b90fa6bfa` Responses、Chat Completions、SystemOne 计入账号等待数
- `759263ee1` 队列满时重选一次
- `4ffb8d131` sticky 调试日志降为 debug

并发槽位：

- `b226ee263`、`76a768f79` 抢槽结果不明确时按精确成员补偿，补偿在后台执行
- `bd2b46076`、`976ca3cd0` 停机时释放本进程持有的槽位与等待计数
- `c49ab5098` 活跃索引维护并入抢槽与等待脚本；`7023817aa` 离开等待队列的减一不再占用首字节路径。不排队请求的并发相关 Redis 往返约从 12 次降到 4 次

上游 IO：

- `2ae359b32` TLS 指纹直连加建连与握手超时
- `236d4a4d7` Antigravity 流式 chunk 先做字节预筛，不再逐个解 JSON
- `6e8bb1b1e` 连接池留出并发之上的余量
- `ec4c0e629` 首输出超时清理时归还 SSE 队列预算

Antigravity 与 Gemini：

- `122698dc6` 每个上游错误只处理一次
- `f62e118ea`、`8431fdc1d` 长冷却 429 不再同号重试，退避响应取消；countTokens 保持本地估算
- `76cde7681`、`60a241507` 客户端断开后冷却仍落库，同一账号与模型的并发写入合并

OpenAI 调度：

- `c70ad1065` Grok 免费额度门控缓存在两个调度器间共享
- `a4e7702c3` 原生 OpenAI 粘性会话只让给合格的 BPS 账号
- `3e85813d3` `selectBestAccount` 按排序依次复核，第一个通过者即返回
- `ffd1a230e` 旧路径先抢槽再做 DB 复核，复核有预算
- `f47820669` 审查修正：`selectBestAccount` 不设复核上限；count_tokens 走的旧粘性路径同样只让位给合格的 BPS 账号；旧路径隐私判定前移、预算耗尽后跳过第二轮

调度快照：

- `9d01671e0`、`34bd7beed` 同桶的 DB 回退合并为一次，空桶做 250 毫秒负缓存；共享加载里的 panic 转成错误，脱离请求后的加载上限 5 秒
- `135952b72` outbox 事件失败时水位推进到最后一个已处理事件，lag 检查照常运行

用量与计费：

- `82f619f67`、`4cd2a9c8f` 从未开启的限速窗口不再触发重置，同一 key 的重置去重；缓存不再自行开启窗口
- `6e7b7e190` 限速用量增量在队列满时不再丢弃
- `1bb882234` 停机时排空 ops 错误日志
- `e95617a2a` 写入 `usage_logs` 前清洗有列宽上限的文本列
- `05cf8af4f`、`2115992a6` 扣费事务遇到瞬时数据库错误（含连接重置）时有界重试。去重认领与扣减在同一事务内，重放不会重复扣费；重放命中去重时丢弃限速、余额、订阅和认证缓存，让它们回源

诊断与其他：

- `bfd212c91`、`aa62f1403` 模型可用性诊断候选账号短缓存
- `fa21b9ad8` 重新生成 `wire_gen.go`；`90cc965c9`、`7df5fab4f` lint 与测试竞态修正

### 回退的一项

`1f83066e6` 让 Layer 3 探测把「欠给排队请求的槽位」排除在外，本意是不让新请求插队。独立审查指出两点，核实后回退：

- 没有达到目的。被探测跳过的请求仍会拿到首位账号的等待计划，而 handler 的等待一开始就会立即抢一次槽，结果只是晚一次 Redis 往返拿到同一个空槽。
- 引入了新的失效方式。`wait:account:{id}` 是裸计数器，只有优雅停机会清掉。进程被 SIGKILL 或 OOM 杀掉时如果有请求在排队，计数会残留 30 分钟以上，探测会认为该账号的空槽都被欠着，请求改去首位账号排队或 429，即 2026-11-02 的症状。

### 审查遗留

以下是独立审查提出、核实属实但这次没有处理的 minor：

- 粘性账号的会话注册在抢到槽位之后的三条路径（转发失败换号、客户端断开）上仍可能被误删，彻底的做法是给会话注册加引用计数。
- BPS 账号并发全满或 RPM 打满时，原生粘性会话仍会让位再被重新绑定到另一个原生账号。应改成抢到 BPS 槽位才迁移。
- 多实例共用 Redis 时，outbox 水位写入不是单调的，失败路径可能把水位回拨，后果是重复处理（幂等）。单实例不受影响。
- 旧路径和 `selectBestAccount` 新增分支的测试覆盖不全：抢到槽后复核失败的释放、并发上限变化后的重抢、预算耗尽落入等待计划。
- 调度快照投影不含 `privacy_mode`，而高级调度器在列表副本上判定隐私设置。如果线上有开启 `require_privacy_set` 的 OpenAI 分组且用高级调度器，候选可能被全部滤掉。这是既有问题，不在本次改动范围，需要单独核实。

### 有意没做

- 选等待目标时跳过队列已满的账号。这就是线上回放否定过的「排队溢出到下一个账号」。
- 崩溃或 `kill -9` 之后残留的槽位成员与等待计数的回收（需要心跳或把等待计数改成带截止时间的集合），以及 API Key 槽位成员的停机释放。
- 另有 27 条被确认但评为 P2 / P3 的条目没有落地，包括：槽位释放后唤醒等待者、普通 Gemini 账号排队时重选、调度快照读路径的往返与拷贝、OpenAI 与通用网关重复的 7 个调度辅助函数、约 1600 行死代码与不再读取的配置项、`usage_logs` 上的冗余单列索引。

上线前建议只读检查生产 Redis 的 `wait:account:*` 是否已有残留虚高。

## 三、验证

均在本地运行，没有生产操作，没有 push。

| 检查 | 结果 |
| --- | --- |
| `go build ./...` | 通过 |
| `go vet -tags unit ./...`、`go vet -tags integration ./...` | 通过 |
| `golangci-lint cache clean && GOOS=linux golangci-lint run ./...` | 0 issues |
| `go test -tags=unit ./...`（全量） | 76 个包全部通过 |
| wire（v0.7.0 重新生成后无差异） | 通过 |
| 前端 `vue-tsc --noEmit` | 通过 |
| 前端 `eslint` | 0 错误；1 个 warning 为 fork 原有 SettingsView 测试的未使用变量 |
| 前端 `vitest run`（全部） | 498 个文件、4503 个用例通过，9 个原有 skip |

没有覆盖的部分：

- `go test -tags=integration` 没有运行，本机 Docker 未启动。集成测试只做了 vet，其中包括本次改过的 3 个活跃索引用例和上游新增的缓存写入 TTL 用例。
- 没有做线上回放或压测。Redis 往返次数、DB 查询次数的下降来自单测里的计数，不是线上测量。
- 前端检查在审查修正之前运行；之后的修正只改了后端。
