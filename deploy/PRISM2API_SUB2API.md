# 将 free-astra / Prism2API 接入 Sub2API

Sub2API 已经原生支持 OpenAI 兼容的 API Key 上游，因此不需要新增平台或修改网关代码。本仓库提供一个可选的 Docker sidecar，将 free-astra 的 `prism2api` 与 Sub2API 放进同一个内部网络，再以普通的 `platform=openai`、`type=apikey` 账号接入。

这条链路依赖 Prism 网页会话，不是官方 OpenAI API。会话 Cookie 仍会发送给 `prism.openai.com`；会话通常会过期，Prism sandbox 默认只能串行处理一个请求，usage 是适配器估算值，流式响应是在上游完成后分块输出。只把它用于你有权使用的账号，并把 `session.json`、导入用的 cURL 和 `PRISM_API_KEY` 当作凭据保管。

## 1. 准备 Prism 会话

按照 free-astra 的说明，把浏览器中复制的 Prism 请求导入到数据目录：

```bash
cd /Users/damian/Downloads/free-astra-main
python3 scripts/import_session.py prism-curl.txt -o data/session.json
chmod 600 data/session.json
```

不要把 `prism-curl.txt` 或 `data/session.json` 提交到 Git。会话失效后重新导入并重启 sidecar 即可。

## 2. 启动同网 sidecar

复制并填写两个环境文件。`PRISM_API_KEY` 是 sidecar 与 Sub2API 之间的共享密钥，不是浏览器 Cookie：

```bash
cd /path/to/sub2api/deploy
cp .env.example .env
cat .env.prism2api.example >> .env
# 编辑 .env：至少修改 POSTGRES_PASSWORD、PRISM_API_KEY、PRISM2API_SOURCE 和 PRISM2API_DATA_DIR
chmod 600 .env

# 使用 named-volume 部署
docker compose --env-file .env \
  -f docker-compose.yml -f docker-compose.prism2api.yml up -d --build

# 或使用本地目录部署
docker compose --env-file .env \
  -f docker-compose.local.yml -f docker-compose.prism2api.yml up -d --build
```

sidecar 只暴露给 `sub2api-network`，没有把 8319 发布到宿主机。不要把账号地址写成 `127.0.0.1`：在 Sub2API 容器里那代表 Sub2API 自己，正确地址是 `http://prism2api:8319/v1`。

检查服务状态：

```bash
docker compose --env-file .env \
  -f docker-compose.yml -f docker-compose.prism2api.yml ps
docker compose --env-file .env \
  -f docker-compose.yml -f docker-compose.prism2api.yml logs -f prism2api
```

## 3. 创建 Sub2API 账号

后台创建账号时选择：

- 平台：`OpenAI`
- 类型：`API Key`
- Base URL：`http://prism2api:8319/v1`
- API Key：填写 `.env` 中相同的 `PRISM_API_KEY`
- 并发：`1`
- Endpoint capabilities：只保留文本能力，取消 `embeddings`
- Responses mode：`force_responses`
- 关闭 upstream billing probe

可以直接参考 [prism2api-account.example.json](./prism2api-account.example.json) 的管理员 API payload；创建前需要把 `group_ids` 换成实际分组 ID。模板通过映射只公开 `prism-astra`、`prism-sol`、`prism-terra` 三个别名，并在转发时映射到 Prism 接受的模型 ID。不要打开 `openai_passthrough`，否则任意客户端模型名都会原样发给 Prism，容易把不支持的模型误路由过去。

管理员 API 示例（需要管理员 JWT）：

```bash
curl -X POST "http://127.0.0.1:8080/api/v1/admin/accounts" \
  -H "Authorization: Bearer $SUB2API_ADMIN_JWT" \
  -H 'Content-Type: application/json' \
  --data @prism2api-account.example.json
```

后台测试账号时选 `prism-astra`、`prism-sol` 或 `prism-terra`，不要使用 Sub2API 默认的 `gpt-5.4` 测试模型。创建后把账号绑定到实际分组，再用 Sub2API 的 `/v1/models` 和一次最小文本请求验证。

## 4. 网络与安全配置

如果不使用本仓库的 sidecar overlay，而是单独启动 free-astra 的 compose，两个 compose 默认不在同一个网络，`prism2api` 服务名也无法解析。要么把两个服务加入同一个 external Docker network，要么让 Prism 监听 `0.0.0.0` 并让 Sub2API 使用宿主机可达地址（Linux 容器通常需要 `host.docker.internal:host-gateway`）。生产环境优先使用内部网络或 TLS 反向代理，不要把 8319 直接暴露公网。

启用 Sub2API URL allowlist 时，把 Prism sidecar 的主机名加入允许列表，并允许内部 HTTP；如果策略禁止私网或明文 HTTP，请改用内网 TLS 反代域名。默认模板关闭了 upstream billing probe，因为 Prism 没有 Sub2API billing/usage 端点。

## 5. 已知限制

- Prism 会话和网页内部接口可能随时变化，Cookie 需要定期重新导入。
- 单个 sandbox 默认串行，Sub2API 账号并发应保持为 `1`；多个 sandbox 才能水平扩展。
- usage 是本地估算，不能当作官方账单；如需计费，设置合适的账号倍率并向用户说明这一点。
- `stream=true` 是完成后分块发送的伪流式，不是逐 token 上游流。
- 图片输入目前会被 free-astra 丢弃；不要把它当作图片模型或 embeddings 上游。
- 带 tools 的请求会经过适配器的工具协议转换，system/developer 指令可能被重写；先用最小文本请求验证，再逐步启用工具。
- Prism 可能把模型 fallback 到另一个可用模型，但响应中的模型标识仍可能是请求模型；不要据此做精确模型计费。
