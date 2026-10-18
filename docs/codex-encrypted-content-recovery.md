# Codex 加密内容的账号连续性与恢复

`invalid_encrypted_content` / `thinking_signature_invalid` 可能出现在 HTTP 400，也可能出现在 HTTP 200 的 SSE `response.failed` 中。`amsg_…` 对应的加密代理消息承载任务内容，不能套用删除 reasoning 的恢复方式。

## 行为

- 网关在完整上游工具调用写给客户端之前，记录加密参数摘要与生成账号的关系。Codex 将 `collaboration.spawn_agent`、`send_message` 或 `followup_task` 的 `arguments.message` 放入后续 `agent_message` 时，子会话即使使用不同 session ID，也会回到生成账号。
- 按认证 API Key 与分组隔离，兼容 HTTP JSON、SSE、WebSocket 及 WS/HTTP 桥接。只从实际上游输出建立归属，不从客户端提交的历史记录认领归属。
- 已知归属具有严格约束：仍执行账号资格、模型、分组、隐私、额度和并发检查，但不会因生成账号不可用而换成其他账号。已有 WS 连接收到另一账号的已知密文时返回可重连错误，不将该帧转给上游。
- 只含可恢复 reasoning 密文的请求，在尚未出现文字或工具输出时，可以清理对应项目并在同一账号重试一次。HTTP 错误和 SSE 错误共用重试预算；任务密文、compaction/compaction_summary 压缩历史、混合加密输入和 `amsg_` 诊断不走清理。明确的密文拒绝不会触发普通跨账号故障转移。
- 上游先发裸 `error`、再发 `response.failed` 时，是否重试由后者决定：失败帧带有输出项、输出/推理 token 或 `amsg_` 诊断时不重放。只有裸 `error` 且流正常结束时才会重试；读取出错或超时不重试，避免丢掉报告已执行工作的终帧。

## 缓存与错误

Redis 保存 SHA-256 摘要的绑定键及账号 ID，不额外保存任务文本或密文正文。绑定通过 Lua 原子认领，其他账号不能覆盖；命名空间与普通 response ID 绑定分离。无需数据库迁移或新增开关。

绑定期限沿用 `gateway.openai_ws.sticky_response_id_ttl_seconds`，未设置时为一小时；活跃的已知历史会续期。本地缓存与单次请求的摘要数均有上限。Redis 故障时，相关请求停止并返回可重试错误，避免猜测归属。

| 错误码 | 含义与处理 |
| --- | --- |
| `encrypted_content_account_unavailable` | 生成账号当前不能接单；稍后重试，或新建会话重新发送原任务。 |
| `encrypted_content_account_mismatch` | 当前 WS 连接使用其他账号；重连并携带完整输入历史。 |
| `encrypted_content_account_conflict` | 输入包含不同账号的已知密文；新建会话重新发送原任务。 |
| `encrypted_content_affinity_unavailable` | 归属缓存暂不可用；保留请求内容并稍后重试。 |
| `encrypted_content_limit_exceeded` | 单次请求超过 512 个不同密文摘要；新建会话并重新发送任务。 |

## 范围与验证

该改动防止新观测到的密文被路由至其他账号，不执行解密。升级之前生成、缓存已过期或来自其他入口的密文可能没有归属记录，会原样交由上游验证；已损坏的旧会话可能仍需新建会话。code mode 将协作调用藏在 JavaScript 字符串中时，网关不能可靠提取未作为结构化工具输出出现的密文归属。同一账号下的上游密钥或会话变化也无法由本地账号绑定修复。

本地回归使用合成密文验证传输顺序、父子会话路由、错误终止与不重放工具调用；它不能代替真实 Codex 多代理会话的发布后验证。验收应覆盖父会话生成任务、子会话接收、结果回传，以及生成账号繁忙或不可用时的行为。

上游背景：[agent_message 问题 #7506](https://github.com/Wei-Shaw/sub2api/issues/7506)、[reasoning HTTP 400 修复 #7703](https://github.com/Wei-Shaw/sub2api/pull/7703)。后者不覆盖全部 SSE 失败和代理任务密文场景。
