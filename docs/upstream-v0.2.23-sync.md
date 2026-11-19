# 上游 v0.2.22 / v0.2.23 同步记录

本次从 fork/main `5e36f1dd1`（v0.2.83）同步 Wei-Shaw/sub2api v0.2.22（2026-11-17）和 v0.2.23（2026-11-18，tag 指向 `5af9afb58`）。上次同步固定在上游 v0.2.21（`10ba8f9b4`），本次范围为 `10ba8f9b4..5af9afb58`：128 个非 merge 提交，91 个 merge。工作树与分支为 `sub2api-upstream-v0.2.23` / `codex/sync-upstream-v0.2.23-20261119`。

上游 tag 与 fork 自己的 `v0.2.2x` 同名，取到 `refs/upstream-tags/*`，不覆盖本地 tag。

## 一、应用

按拓扑顺序 `cherry-pick -x` 117 个提交（其中 2 个部分采纳），另有 1 个解决冲突后为空、10 个跳过。主要内容：

- 模型与计费：GPT-6.2 Sol / Astra（272K 以上长上下文、Fast 2 倍、Ultra Fast 2027-03-01 前 3 倍后 6 倍，Pro 200 / Pro 500 档位放行）；GPT-6.1 Sol Ultra Fast 倍率改为 6 倍；渠道定价新增 Ultra Fast 倍率；Gemini 4 Carbon 支持、Gemini 4 Hydrogen 计费；Vertex us/eu 多区域端点（默认定价下加价 10%）；DeepSeek 显式 token 价格覆盖；硅基流动 Qwen3-Embedding / Reranker 兜底价。
- 缓存写入计费：Anthropic ↔ OpenAI Responses / Chat Completions 互转保留 5m/1h 明细；明细之和小于总量时差额按 5m 计；OpenAI 网关走 Anthropic 原生路径时 1h 按 1h 价；流中只更新缓存字段时不再覆盖 prompt 总量。
- 分组：强制 OpenAI Ultra Fast（按模型门控，可与强制 Fast 并存）。
- 扣费性能：同一用户的扣费按用户通道合并为批量事务（`UsageBillingLaneCombiner`），批量失败逐条回退到单条 Apply。
- 网关：Anthropic 请求与流事件转发少做 JSON 解码；请求体零拷贝读取；上游死连接检测约 30 秒；无法设置 TCP_USER_TIMEOUT 时不再拨号失败；启动时远程价格哈希检查改为后台；concurrency.ping_interval=0 可关闭等待 ping。
- OpenAI：首个输出前空闲超时换号重试（最多 1 次）；流空闲计时随数据重置；keepalive 转发与 keepalive 后 SSE 错误；兼容路径输出后流错误的账号副作用；Messages 桥接首包前错误 failover；工具 Schema 的 patternProperties / Python re 兼容；Codex additional_tools ID、分叉会话 prompt cache key；透传账号可选按模型白名单过滤；额度刷新失败时保留旧观测并提示。
- Gemini / Antigravity：兼容入口转发 response_format；同名并行工具调用、分片工具参数快照；原生请求按稳定前缀做粘性哈希；兼容流思考档位读取 output_config.effort。
- Claude：并发 token 轮换后恢复请求；账号测试默认模型 claude-sonnet-4-6；Fable 周额度与 7d_oi 恢复；5h 窗口与重置时间对齐。
- 调度：快照保留冷却中账号，读时按 IsSchedulable 过滤；openai_capabilities、透传模型过滤开关进入调度投影；客户端断开后仍写粘性绑定。
- 其它：RPM 覆盖限定所属分组、分组倍率/RPM 同步事务化；支付宝退款重试幂等；按账户筛选时分布图实际扣费不再替换为账号成本；Kimi 新月度套餐；Bedrock 错误语义区分；Grok 媒体错误信息与搜索调用；设置注入的 HTML 不缓存；登录回填 email 失败不 panic；follow-redirects 安全补丁（fork 已有，为空提交）。

## 二、跳过

| 上游提交 | 原因 |
| --- | --- |
| `6dd5a045b`、`15871f904` | VERSION 同步 |
| `530c5e3a4`、`9cd1a1eb2` | 只改 README 赞助商；fork 的 README 已精简 |
| `a29321497`、`ddb7eac63`、`e257b7811`、`202e23b2b` | Go 1.27.4 升级，fork 在 v0.2.83（`f38833256`）已完成，九处版本钉一致 |
| `5cbcb1b59` + 测试 `d3716a173` | 只处理「首包前 keepalive 已提交 200 后的空流」。fork 在 v0.2.19 同步时跳过了首包前 keepalive（`bd48b2b2d`），兼容流首包前不提交 200，空流照常 failover |
| `7b879c56a` | 解决冲突后为空：fork 已有同样的 overrides 与锁文件 |

## 三、部分采纳

- `e055d7baf`（Antigravity 兼容流思考与首包超时锚点）：采纳思考档位读取 `output_config.effort` 和兼容层 thinking 配置；不采纳首包前 keepalive 与超时锚点（同上，fork 无该机制）。只补了上游的 `TestAntigravityCompatGeminiThinkingConfig`。
- `9c3280098`（调度快照按 bucket 合并回源）：服务层的 `readSnapshotAccounts` / `compactSchedulableAccounts` 并入 fork 已有的 singleflight 回源与空桶负缓存；仓储层 `scheduler_cache.go` 保留 fork 版本（fork 的 pipeline 读取和母本 memo 已和上游分叉），因此仓储没有实现 `GetSnapshotValues`，命中路径仍走 `GetSnapshot` + 解引用。`scheduler_snapshot_fallback_coalesce_test.go` 取上游 v0.2.23 最终版。

## 四、冲突与兼容处理

- **调度快照成员**（`39bac35a3`）：命中、本地快照、DB 回源三条读路径都按 IsSchedulable 过滤；空桶负缓存仍按成员数判断（全部冷却的桶不算空）。区域导出仍在导出时过滤。fork 测试桩改为实现 `ListModelAvailabilityCandidates`，测试账号补 `Status/Schedulable`。
- **批量扣费**（`2101f4f94`）：wire 重新生成；fork 单条 Apply 的重试与 `ReplayedCommittedAttempt` 保留，批量失败回退到它。fork 的扣费效果函数与上游 v0.2.21 相同，批量路径覆盖同样的扣费项。重放分支用到的 `billingCtx` 移到分支之前。
- **Codex 会话 ID**（`74ba91aa6`）：fork 的 harvest-pin 包装函数增加 `clientSessionID` 参数并透传。
- **Gemini 并行工具**（`b0d60a8c8`）：fork 自己的前缀判断（`499d8ba57`）换成上游 `geminiToolCallStartsNewBlock` 与后续快照修复，Gemini Web 测试通过。
- **透传模型过滤**（`9d5fb150b`）：并入 fork 的模型列表、分组白名单、别名与 Copilot SDK 判断；前端 `isOpenAIModelRestrictionDisabled` = 透传且未开过滤，或 Copilot SDK。
- **额度刷新失败**（`e67746254`）：`UtilizationUnknown`、`quota_refresh_failed` 并入 fork 用量服务；告警加在 fork 的 Codex 用量单元格里，`loadActiveUsage` 对 openai 也写 error。
- **Ultra Fast 分组**（`330198321`）：ent 重新生成；认证快照版本 30 → 31。
- **GPT-6.2 长上下文**（`6107cf174`）：并入 fork 的 `openAIGPT61SolLongContextExempt`，GPT-6.2 只在订阅凭据（OAuth / Setup Token）上豁免，与上游一致。
- **缓存写入**（`e6bde6b3c` 起）：`billingUsageTokens` 与上游最终形态一致（5m/1h 与 30m/12h 并存）；fork 的 Excel BPS「缓存写入计入输入」在其结果上把写入并回输入并清空全部 TTL 明细。
- **merge 独有内容**：上游 merge `661770bf3` 带来的 `TestCacheWriteTTLProjectionAndPricingCoexist` 没有独立提交，已补（`544ad1a00`）。其余带冲突解决内容的 merge（`2354c7828`、`03c17491b`、`470c02b39`、`7f3fe5bfd`）内容已在。

## 五、同步后的 fork 修正

- `ab1d81fbc`：Antigravity 调度与冷却用的思考档位改为和转发一样读转换后的 `output_config.effort`。否则 `reasoning_effort=low` 实际发往 `-low`，429 却冷却 `-high`。fork 两条记录旧行为（low → high）的用例改为期望 low。
- 同一提交：取消测试的 writer 补 `WriteString`（上游改用 `io.WriteString` 转发）；删除不再使用的 `applyObservedGatewayResponseModel`。
- `1e74d95d6`：上游改了 Ultra Fast 提示文案但没改测试（上游 v0.2.23 本身也失败），按现文案断言。
- 上游新测试按 fork 适配：`NewOpenAIGatewayService` 多一个 `ProxyRepository` 参数；测试上游桩补 `DoWithTLS`；仓储桩实现 fork 发送前准入要求的 `OpenAITurnAdmissionReader`。

## 六、行为变化（需要知道）

- OpenAI 兼容入口（Chat Completions / Responses）请求裸 Gemini 模型并带 `reasoning_effort=low` 时，Antigravity 现在用 `-low` 变体（以前是默认 `-high`）。调度、冷却与转发一致。
- 调度快照里包含冷却中的账号，冷却到期立即可选，不再等 outbox 事件。
- 同一用户的扣费合并为批量事务。

## 七、迁移

- `251_group_force_openai_ultrafast.sql`：groups 加列，默认 false。
- `251_group_usage_rollup_nonblocking_build.sql`：单行状态表加三列（`lock_timeout 5s`），替换失效函数；触发器沿用 222 已建的。
- `252_channel_pricing_ultrafast_multiplier.sql`：channel_model_pricing 加列与 CHECK 约束。

三个都是小表加列和函数替换，不扫 usage_logs。文件名保留上游编号，runner 按文件名补跑；旧二进制容忍多出的迁移记录，回滚安全。

## 八、验证

| 检查 | 结果 |
| --- | --- |
| `go build ./...`、`go vet -tags=unit ./...` | 通过 |
| `golangci-lint run ./...`（GOOS=linux，清缓存） | 0 issues |
| 后端 `go test -tags=unit ./...` | 除 `internal/mihomo`（macOS 无 mihomo 内核，按惯例失败，CI 覆盖）外全部通过 |
| 前端 `vue-tsc --noEmit`、eslint | 通过（1 个既有 warning） |
| 前端 `vitest run`（全部） | 501 个文件、4547 个用例通过，9 个原有 skip |

集成测试（Docker）未在本地运行，由 CI 覆盖。
