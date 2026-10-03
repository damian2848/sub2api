# 三开发布流程（damian2848/sub2api）

本文约定这个 fork 怎样发版，以及发布说明怎么写。发布由 `.github/workflows/release.yml` 完成，说明文字取自带注释标签的正文。

## 远端

| 远端 | 仓库 | 用途 |
| --- | --- | --- |
| `fork` | damian2848/sub2api | **发布目标**，推送分支和标签只推这里 |
| `origin` | ranxi2001/sub2api | 参考来源，不要推送 |
| `upstream` | Wei-Shaw/sub2api | 官方源，只拉取 |

## 版本号

- 在上游版本基础上递增 `0.2.x`，功能或修复各自合入后发一个新的补丁号，不复用旧号。
- 发布提交里把 `backend/cmd/server/VERSION` 改成目标版本，避免发布后机器人再补一个同步提交。
- 生产部署用的构建带后缀（如 `0.2.15-prism-perf.8`），它只用于部署回执，不是发布版本。

## 发布前检查

本地只跑「和这次改动相关」的检查，完整验证交给推送后的 CI（CI 在同一个提交上跑全部后端、前端、sidecar、Docker 集成和 lint）：

```bash
deploy/releasing/precheck.sh            # 默认对比最近一个 v* 标签；也可传一个 ref
```

它按改动路径选择：动了 `backend/` 只构建并测试改动的包；动了 `frontend/` 跑 `vue-tsc` 和 `vitest --changed`；动了 `tools/prism-browser/` 跑 sidecar 单测；动了 `deploy/releasing/` 跑部署脚本测试。需要真实浏览器验证的改动（比如 sidecar 的页面/轮询逻辑）另外跑：

```bash
cd tools/prism-browser && PRISM_CHROMIUM_EXECUTABLE=<chromium 路径> npm run test:smoke
```

注意：

- 前端不要用默认的 `pnpm`（11.x 会改写 `frontend/pnpm-lock.yaml`），构建用 `npx pnpm@9.15.9 run build`，提交前确认锁文件没有变化。
- 已在生产执行过的迁移文件不能再改，否则迁移校验和对不上，服务无法启动。
- `go test` 会在 `backend/internal/service/data/` 生成临时文件并可能删除已提交的夹具；`precheck.sh` 会还原，手工跑测试后也要 `git checkout -- backend/internal/service/data && git clean -fdq backend/internal/service/data`。
- 本地检查通过不代表 CI 会通过。CI 失败时发布页已经公开（见下），所以动了共享代码、依赖或构建配置时，建议改用「全量本地检查」：`go test -tags unit ./internal/... && cd frontend && vitest run`。

## 发布说明怎么写

标签正文就是发布页正文，面向使用者，不放内部测试流水账。固定结构：

```
Sub2API <版本>

一两句话说明这个版本最重要的变化。

## 亮点
- 每条一句话，写用户能感知到的变化。

## 升级与部署
- 是否有数据库迁移；是否需要重新部署某个服务；新增或变更的环境变量和默认值；回滚方式。

## 已知限制
- 写清楚做不到的事，不要美化。

## 验证
- 用几行说明跑了哪些测试、线上实测了什么，只写已经验证过的事实。

## 变更
- 提交列表（短哈希 + 标题），最后给出对比链接：compare/<上一个标签>...<这个标签>
```

发布页的安装说明、校验命令和文档链接由 `.goreleaser.yaml` 的页脚自动追加，标签正文里不用重复。

## 步骤

```bash
# 1. 发布提交：改 backend/cmd/server/VERSION、必要的文档，提交。
git commit -am "chore(release): prepare v<版本>"

# 2. 一条命令完成：推送 main → 打带注释标签 → 推送标签 → 并行等待 CI 与 Release → 核对发布页（10 个附件）。
deploy/releasing/release.sh <版本> notes.md
```

`release.sh` 的约定：

- 不再单独做「不发布的演练」。标签触发的 Release 构建的就是演练会构建的产物，演练只是把同样的 5–6 分钟多花了一遍。
- CI 和 Release 在推送后**同时**运行，两者都结束才返回。CI 或 Release 失败时命令以非零退出并明确提示。
- 发布前它会拒绝：工作区有未提交改动、`VERSION` 不是目标版本、说明不是以 `Sub2API <版本>` 开头或缺少 `## ` 章节、标签已存在、不在 `main`。
- 推送目标固定为 `fork`，永远不推 `origin`。
- 标签用 `--cleanup=verbatim` 创建，否则 git 会把 `## ` 标题行当注释删掉。
- **CI 失败的后果**：标签已推送、发布页已公开。此时删除发布页和标签（`gh release delete <标签> --cleanup-tag`），修复后用下一个补丁号重发，不要复用旧号。这是并行换来速度的代价；改动涉及共享代码、依赖或构建配置时，先在本地跑全量检查再发布。
- 标签推送会自动触发 Release，不要再手动 `workflow_dispatch`，否则同一个标签会发布两次（并发组会让第二次排队，等第一次完成后再发一遍）。

仓库变量 `PUBLISH_CONTAINER_IMAGES=false`，所以不发布容器镜像，只发布二进制和运行包。

## 部署

```bash
deploy/releasing/deploy.sh <版本>              # 只读预检
deploy/releasing/deploy.sh <版本> --execute    # 执行
```

- 目标版本取参数，目标提交取本地 git 标签（并要求标签已推到 `fork`），回滚基线取**线上正在运行的二进制**（读它自己的 `-version`），不需要手工改任何版本号或提交号。
- 主程序压缩包（约 40 MB）和 Prism 包由**服务器自己从 GitHub 下载**并对照发布的校验和验证，另外校验下载下来的二进制确实是指定版本和提交；不再经过本机再 scp。
- 执行时脚本在服务器上脱离终端运行（断线不会中断切换），本地每 20 秒读回进度并在结束时打印回执。
- 切换前自动等待 Prism sidecar 空闲（默认最多 10 分钟，连续安静 20 秒），等不到就放弃，不会打断进行中的生成。
- 迁移和 sidecar 启动命令不再写死：有新迁移时用 `--new-migration NAME.sql=SHA256`（可重复），本版本需要改 sidecar 启动命令时用 `--sidecar-cmd '["node","src/x.mjs"]'`，其余情况两者都不需要，并且任何其他运行参数变化都会被拒绝。
- 数据库备份、镜像构建、回滚镜像、切换后的校验与自动回滚保持不变，这是回滚能力的来源，不为速度牺牲。

## 发布后核对

`release.sh` 已核对：附件共 10 个、不是草稿、正文章节标题保留。其余在部署时由服务器完成：

- 下载的主程序与发布的 `checksums.txt` 一致，并且 `-version` 返回目标版本和完整提交号；
- `prism-browser_<版本>.tar.gz` 与 `.sha256` 一致，包内包含 `tools/prism-browser/Dockerfile`、`deploy/docker-compose.prism-browser.yml`、`deploy/.env.prism-browser.example` 和 `deploy/PRISM_BROWSER.md`。

需要手工看发布页时：

```bash
gh release view v<版本> --repo damian2848/sub2api --json assets,body
```

已发布的标签不要移动；正文有问题时用 `gh release edit <标签> --notes-file` 修正发布页。
