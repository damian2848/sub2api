# Release matrix

## 日常发版：一次 CI，一次构建

1. 本地只运行改动涉及的回归测试；完整单测、集成测试、lint 和前端检查交给分支 CI，不在本地再跑一整轮发布检查。
2. 将待发布提交推送到目标仓库分支，再创建并推送版本标签。Release 自动等待该提交的最新分支 CI 成功；已有成功结果会立即复用。标签不再触发第二轮完整 CI。
3. 等待一次 Release 结果，核验安装包校验和和版本。无需重复下载所有平台、重复本地编译或对同一提交再次跑完整测试；只有失败、追加修改或具体风险才补充对应检查。

例如，确认 `fork` 指向发布仓库、当前提交和发布说明无误后：

```bash
git push fork HEAD
git tag -a vX.Y.Z -F /path/to/release-notes.md
git push fork refs/tags/vX.Y.Z
```

CI 必须匹配完整提交 SHA；旧提交的通过结果、PR 合并提交、失败/取消的运行都不能替代。CI 失败时先修复或重跑该 CI，再重跑 Release；没有 CI 记录时先推送分支，或手动运行 `backend-ci.yml`，选择指向该提交的分支。正式发布默认最多等待 CI 45 分钟；dry run 只验证构建，不要求 CI 已通过。

此调整保留完整平台安装包和两种架构的重登运行包，不改变 `SIMPLE_RELEASE` / `PUBLISH_CONTAINER_IMAGES` 配置。重登运行包通过 BuildKit 缓存复用依赖和已验证构建层；默认分支的运行包 CI 会在相关源码变更时预热缓存，标签发布可读取该缓存。缓存未命中仍完整构建和验证。不发镜像时跳过 QEMU / Buildx 初始化。发布不等于生产部署。

## Build and publication

The release workflow builds the frontend once, then runs each configured Go target on its own Linux runner. `CGO_ENABLED=0` permits cross-compilation. The target matrix is read from `.goreleaser.yaml`, including its exclusions; simple releases select Linux amd64 only.

Each build uses GoReleaser OSS in snapshot mode with the selected release version and one target. Archive naming, bundled files, Go flags and release templates remain in the existing GoReleaser configurations. Every archive is accompanied by its source commit, target, version and SHA256. The publishing job verifies the complete matrix before building images or publishing. It uses GoReleaser's `extra_files` support to publish existing archives and checksums, with builds disabled. No Pro license is needed.

Go caches are isolated by target and refreshed on each source commit, with fallback to accessible preceding target caches (GitHub cache branch/tag visibility still applies). Release builds use committed dependency files without running `go mod tidy` per platform. Matrix jobs upload uniquely named artifacts. The publishing job extracts only the regular Linux binary from each verified archive and restores its executable permission before constructing Docker contexts. QEMU is only initialized for multi-architecture container publication. DockerHub images are omitted when its credentials are absent; GHCR is always retained. Simple mode still publishes only the amd64 GHCR image and the simple release description.

All build jobs use the commit resolved by `prepare`, including a manual release's selected tag. Helper scripts come from the workflow revision and are passed as a run-local artifact, so older application tags do not need to contain the new scripts. The workflow serializes release runs to prevent simultaneous updates to moving image tags.

## Validate without publication

From a branch containing this workflow:

```bash
gh workflow run release.yml --ref <branch> \
  -f tag=<branch> -f dry_run=true -f simple_release=false
```

A dry run builds all selected archives and both runtime images, verifies artifact provenance and produces the final checksum file. It exports images locally as OCI archives instead of pushing them. It skips registry logins, GitHub Release publication, DockerHub description updates, Telegram notifications and VERSION synchronization. Test the simple path separately with `simple_release=true`.

Dry-run artifacts are available in the Actions run, including `release-dry-run-report`. Compare job start/end times, GoReleaser's build duration and cache restore results. Do not present an initial cold-cache run as a warmed-cache benchmark; publishing network time is not measured by dry runs.

Helper checks:

```bash
python -m pip install -r .github/release-tools/requirements-release.txt
python -m unittest discover -s .github/release-tools -p 'test_*.py'
bash -n .github/release-tools/release-images.sh
bash -n tools/reauth-runtime/build.sh
```
