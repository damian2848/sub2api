# 复用密码/TOTP Worker 登录 Excel/BPS

现有 toSub2 worker 的密码和 TOTP 步骤可以用于官方 Excel OAuth，但需要从 Excel 授权 URL 开始，使用同一条 PKCE/state 会话完成登录。先取得 ChatGPT Web 会话再切 Excel 授权，在本次真实验证中被要求重新登录。

本改动新增显式的隔离 CLI，复用 `process_password_claim` 和现有 toSub2 运行时。普通队列任务默认仍为 Codex；本改动没有增加管理 UI、修改数据库、让现有队列自动选择 Excel，或部署生产 worker。

## 使用

先准备 root 或操作者所有、0600 的 JSON 输入文件，父目录 0700。字段示例中的值均为假值：

```json
{
  "account_id": 123,
  "login_email": "fixture@example.invalid",
  "password": "synthetic-password",
  "totp_secret": "SYNTHETIC_BASE32_SECRET",
  "expected_workspace": "expected-workspace-id",
  "proxy_url": "socks5://127.0.0.1:1080"
}
```

密码、TOTP 密钥、实际代理密码、工作区标识不得提交到 Git。`expected_workspace` 应取原账号，避免选到另一个组织；如没有该字段，只验证所得 Excel 会话的邮箱、客户端和内部工作区一致性。代理由输入显式指定，不能从本例推断业务出口。

```bash
TOSUB2_PYTHON=/opt/sub2api-reauth/venv/bin/python3 \
  /opt/sub2api-reauth/venv/bin/python3 tools/openai_excel_2fa_login.py \
  --input-file /root/private/excel-login.json \
  --tosub2-root /opt/sub2api-reauth/current/tosub2 \
  --output-dir /root/private/new-excel-session
```

必须使用新输出目录。成功产物：`credentials.json`、`account.json` 和 `result.json`。其中凭据文件敏感；`private-protocol-output.json` 即使登录失败也可能包含会话数据，只留在受限目录，禁止普通日志或整目录公开。此 CLI 不调用生产账号写回接口；返回 `production_account_updated=false`。若后续手动刷新了 RT，应使用刷新后的完整会话，不能混合新旧 Token。

toSub2 支持约定会在临时副本中校验；依赖布局变化时拒绝运行，原共享 runtime 不修改。适配使用官方 Excel client ID、固定 callback、`organization.read` scope、`/api/accounts/authorize` 和 `/oauth/token?unified=true`。不会通过修改原 Codex Token 或 client ID 伪造 Excel 会话。

登录复用 toSub2 密码/TOTP 方法，保留证书验证。Excel 分支禁用代理轮换和额外安全挑战的自动处理；遇到邮箱/电话验证码等交互要求退出，不将密码或验证码转交给第三方服务。该分支登录超时为 10 分钟；原 Codex worker 默认行为不改。

## 已验证范围（2026-10-08）

在一个用户明确授权的既有账号上进行了隔离验证，未更换生产队列 worker：

| 检查 | 结果 |
| --- | --- |
| 先 ChatGPT Web 登录再 Excel 授权 | 密码和 TOTP 通过，随后 Excel 授权返回登录页；未产生 Excel Token |
| 直接 Excel PKCE 内完成密码/TOTP | 成功取得完整 access/refresh/id Token，客户端、邮箱、工作区、有效期一致 |
| Excel 凭据调用 BPS 短请求 | HTTP 200、完整 `response.completed`、OK，约 21 秒 |
| Excel refresh token 刷新 | HTTP 200，RT 轮转，身份一致性校验通过 |
| BPS 回复是否带原生票据 | 仅 `__cf_bm` / `__cflb`，无 `x-codex-turn-state` / `__oailb`；响应模型声明为 Luna |

因此已确认 2FA → Excel OAuth → BPS 的可用性，但没有证明 BPS 返回原生 Codex 票据、不降智或半小时票据有效期。未测试其它账号/套餐、邮箱 OTP 分支、所有工作区选择或刷新后的新生成请求。用户另报的一组 7 次请求和 22,329 输入 token 不属于这里自行执行的请求统计，不能合并为已复核证据。

## 离线检查与打包

```bash
EXCEL_TEST_TOSUB2_ROOT=/path/to/pinned/tosub2 \
  python3 -B -m unittest tools.test_openai_excel_oauth_adapter \
  tools.test_openai_oauth_reauth_worker tools.test_openai_oauth_reauth_engines \
  tools.test_openai_oauth_reauth_runtime_controls -q
node --test tools/test_openai_excel_password_flow.mjs
```

Python 校验覆盖共享 runtime 不被修改、依赖布局失败、客户端/邮箱/工作区冲突、凭据保密以及原 worker 行为。Node mock 验证同 PKCE 流程、错误 state 拒绝和邮箱验证要求不被忽略。

Docker 与可迁移 runtime 包含新增适配文件；CI 对固定 toSub2 提交执行语法与 mock 检查。生产机只使用已有 Python/Node 运行时测试，未编译应用或在生产环境构建 runtime 包。只有在对应构建/CI 结果出现后才能宣称包验证通过。
