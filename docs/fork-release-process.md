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

在仓库根目录运行，全部通过再继续：

```bash
cd backend && go build ./... && go vet -tags unit ./internal/service/ ./internal/handler/... ./internal/repository/ ./internal/server/...
cd backend && go test -tags unit ./internal/service/ ./internal/handler/... ./internal/repository/ ./internal/server/... ./internal/config/... -count=1
cd frontend && node_modules/.bin/vitest run && node_modules/.bin/vue-tsc --noEmit
cd tools/prism-browser && node --test test/*.test.mjs
```

注意：

- 前端不要用默认的 `pnpm`（11.x 会改写 `frontend/pnpm-lock.yaml`），构建用 `npx pnpm@9.15.9 run build`，提交前确认锁文件没有变化。
- 已在生产执行过的迁移文件不能再改，否则迁移校验和对不上，服务无法启动。
- `go test` 会在 `backend/internal/service/data/` 生成临时文件，不要提交。

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
# 1. 发布提交：改版本号、必要的文档
git commit -am "chore(release): prepare v<版本>"

# 2. 推送到 fork，并在 main 上做一次不发布的演练（构建全部产物、校验、打包，但不发布）
git push fork main
gh workflow run release.yml --repo damian2848/sub2api --ref main \
  -f tag=main -f dry_run=true -f simple_release=false -f publish_images=false

# 3. 演练通过后，在同一个提交上打带注释标签，正文使用上面的结构。
#    必须加 --cleanup=verbatim：默认会把以 # 开头的行当作注释删掉，章节标题（## 亮点 等）会全部丢失。
git tag -a v<版本> -F notes.md --cleanup=verbatim
git push fork v<版本>

# 4. 推送标签会自动触发 Release（事件为 push，不发布镜像、不发通知）。先确认：
gh run list --repo damian2848/sub2api --workflow release.yml --limit 3
#    只有在列表里没有这个标签的 push 运行时才手动触发；两次都跑会对同一个标签重复发布
#    （并发组会让第二次排队，等第一次完成后再发一遍）。需要手动触发时：
gh workflow run release.yml --repo damian2848/sub2api --ref main \
  -f tag=v<版本> -f dry_run=false -f simple_release=false -f publish_images=false -f notify_release=false
```

仓库变量 `PUBLISH_CONTAINER_IMAGES=false`，所以不发布容器镜像，只发布二进制和运行包。

## 发布后核对

```bash
gh release view v<版本> --repo damian2848/sub2api --json assets,body
```

- 附件共 10 个：五个平台的主程序、`checksums.txt`、两个架构的重新登录运行时、`prism-browser_<版本>.tar.gz` 及其 `.sha256`。
- 下载一个主程序，`sha256sum -c checksums.txt --ignore-missing` 通过，运行 `--version` 返回目标版本和完整提交号。
- 解开 `prism-browser_<版本>.tar.gz`，确认包含 `tools/prism-browser/Dockerfile`、`deploy/docker-compose.prism-browser.yml`、`deploy/.env.prism-browser.example` 和 `deploy/PRISM_BROWSER.md`。
- 发布页正文和标签正文一致（包括 `## ` 章节标题），页脚的安装命令指向这个标签。已发布的标签不要移动；正文有问题时用 `gh release edit <标签> --notes-file` 修正发布页。
