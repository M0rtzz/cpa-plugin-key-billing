# 服务器提交与部署流程

本文用于将本仓库的计费插件和 `cpa-plugin-privacyfilter` 部署到 `ssh 100` 上的现有 CPA 实例。服务器以 `collab` 用户运行，CPA 工作目录为 `/data/collab/Programs/cliproxyapi`，进程位于 `screen` 会话 `3483341.cpa`，启动命令为 `./cli-proxy-api --config ./config.yaml`。

以下命令按阶段执行，不要把客户端 Key、管理密钥或数据库加入 Git。当前 CPA 源码没有随这两个插件修改；保留已安装、支持动态库插件的 CPA 二进制，在最后重启它即可。如果另行升级 CPA，按文末的 CPA 二进制更新步骤操作。

## 1. 本地审查、测试、提交

在本地计费插件仓库确认目标分支和工作区。先审查改动，再按逻辑变更分别提交；已有的暂存文件也要审查，不要直接对全部文件执行 `git add .`。

```bash
cd ~/Workspaces/Misc/cpa-plugin-key-billing
git status --short --branch
git diff --check
git diff --cached --check
git diff --stat
git diff --cached --stat
```

计费行为变更至少运行下列检查。`gofmt -l .` 必须没有输出；如果涉及前端，还需启动 `python3 scripts/frontend_dummy_backend.py --port 18765`，用 Playwright 检查桌面与窄屏布局。`scripts/e2e_cpa_billing.sh v7.2.143` 是仓库要求的计费端到端检查。

```bash
gofmt -l .
go mod tidy -diff
go test -race ./...
go vet ./...
npm ci --prefix scripts
npm test --prefix scripts
node scripts/format_ui.mjs --check
node scripts/format_ui.mjs --check internal/plugin/usage.html
node scripts/format_ui.mjs --check internal/plugin/quota.html
node scripts/check_user_pages.mjs
scripts/e2e_cpa_billing.sh v7.2.143
```

按仓库的 Conventional Commits 规则提交，然后推送实际部署的分支。以下分支名是当前工作分支；如果将来改用其他分支，后续服务器拉取命令也要同步修改。

```bash
git add -p
git diff --cached --check
git commit -m 'feat(billing): describe the completed change'
git push origin feat/user-self-service-pages
git rev-parse HEAD
```

提交信息应描述各次提交的实际内容，示例中的 `describe the completed change` 需要替换。等待该 SHA 对应的 GitHub Actions `Check` 工作流通过，再把同一 SHA 部署到服务器。若 `cpa-plugin-privacyfilter` 没有跟踪文件改动，不需要为它创建提交；其 `scripts/__pycache__/` 不应进入 Git。

## 2. 登录服务器并拉取相同提交

```bash
ssh 100
su - collab
```

输入 `collab` 用户密码后，先确认两个仓库的服务器工作区没有未提交改动。若有改动，先查清来源并保留，不能用强制重置清除。然后快进拉取指定分支，并核对计费插件的 SHA 与本地刚推送的 SHA 一致。

```bash
export BILLING_REPO=/data/collab/Programs/cpa-plugin-key-billing
export GUARD_REPO=/data/collab/Programs/cpa-plugin-privacyfilter
export CPA_DIR=/data/collab/Programs/cliproxyapi

git -C "$BILLING_REPO" status --short --branch
git -C "$GUARD_REPO" status --short --branch

git -C "$BILLING_REPO" switch feat/user-self-service-pages
git -C "$BILLING_REPO" pull --ff-only origin feat/user-self-service-pages
git -C "$BILLING_REPO" rev-parse HEAD

git -C "$GUARD_REPO" switch feat/chinese-input-guard
git -C "$GUARD_REPO" pull --ff-only origin feat/chinese-input-guard
git -C "$GUARD_REPO" rev-parse HEAD
```

## 3. 在服务器编译并暂存产物

服务器需要 Go 1.26、C 编译器、`make` 和 Python 3。必须启用 CGO；使用仓库脚本构建可避免漏掉计费插件的 `cshared` 构建标签。

```bash
go version
cc --version

cd "$BILLING_REPO"
bash scripts/build-self-service.sh
cd dist
sha256sum -c SHA256SUMS
cd "$BILLING_REPO"

cd "$GUARD_REPO"
go test ./...
make build BUILD_DIR=dist

file "$BILLING_REPO/dist/cpa-key-billing.so" "$GUARD_REPO/dist/privacyfilter.so"
```

这次部署的两个用户页面已经嵌入 `cpa-key-billing.so`。`dist/usage.html` 和 `dist/quota.html` 是单独交付的副本，不需要复制到 CPA 的 `plugins` 目录。

先将新动态库放到 CPA 插件目录**之外**的暂存目录；此时不要覆盖正在加载的文件。

```bash
cd "$CPA_DIR"
stage_dir="$CPA_DIR/deploy-staging/$(git -C "$BILLING_REPO" rev-parse --short HEAD)"
install -d -m 700 "$stage_dir"
install -m 644 "$BILLING_REPO/dist/cpa-key-billing.so" "$stage_dir/cpa-key-billing.so"
install -m 644 "$GUARD_REPO/dist/privacyfilter.so" "$stage_dir/privacyfilter.so"
sha256sum "$stage_dir"/*.so
```

## 4. 核对现有配置并停止 CPA

先查看插件文件、`plugins.configs.cpa-key-billing` 配置和实际 `state_file`。下文以 `plugins/cpa-key-billing-state-v1.db` 为例；现场若使用其他路径，备份和回滚必须使用实际路径。不要把含密钥的完整 `config.yaml` 输出到公开日志。

```bash
cd "$CPA_DIR"
find plugins/linux/amd64 plugins -maxdepth 1 -type f \( -name 'cpa-key-billing*.so' -o -name 'privacyfilter*.so' \) -print
rg -n '^[[:space:]]*(plugins:|dir:|cpa-key-billing:|privacyfilter:|state_file:|version:|release-tag:|enabled:)' config.yaml
screen -ls
```

CPA 会用 `plugins.configs.cpa-key-billing.store.version`，若无该字段则用 `store.release-tag`，选择对应版本的动态库。若它们仍固定为旧版本，例如 `1.3.12`，复制新库后仍可能加载旧版。部署从分支编译、尚未发布 GitHub Release 的版本时，删除这两个**旧的版本固定值**，保留其他配置；正式发布后也可把它们同时更新为实际发布版本。核对当前源码 `internal/plugin/types.go` 的版本以及现有 `plugins.configs.privacyfilter` 设置，包括 `skip_models`，不要覆盖其他插件选项。

附加现有会话，用 `Ctrl-C` 停止 CPA，并确认已回到 shell 提示符：

```bash
TERM=xterm screen -r 3483341.cpa
```

以下备份与替换命令在**停止 CPA 后**的同一 `screen` shell 中执行。`screen` 中的旧 shell 不继承刚才 SSH shell 的变量，所以先重新定义路径。若不确定当前目录，先运行 `pwd`。在实际替换前，将 `billing_live` 改成上面 `find` 输出中当前加载的计费插件路径，并确认路径存在。

## 5. 备份、替换与启动

```bash
CPA_DIR=/data/collab/Programs/cliproxyapi
BILLING_REPO=/data/collab/Programs/cpa-plugin-key-billing
stage_dir="$CPA_DIR/deploy-staging/$(git -C "$BILLING_REPO" rev-parse --short HEAD)"
deploy_stamp=$(date +%Y%m%d-%H%M%S)
cd "$CPA_DIR"
billing_live='plugins/linux/amd64/cpa-key-billing-v1.3.12.so'  # 示例：改成现场实际路径
state_file='plugins/cpa-key-billing-state-v1.db'                # 示例：改成 config.yaml 的实际 state_file
test -f "$billing_live"
test -f "$state_file"
test -f "$stage_dir/cpa-key-billing.so"
test -f "$stage_dir/privacyfilter.so"

backup_dir="$CPA_DIR/backups/deploy-$deploy_stamp"
install -d -m 700 "$backup_dir"
cp -p config.yaml cli-proxy-api plugins/privacyfilter.so "$billing_live" "$state_file" "$backup_dir/"
for suffix in -wal -shm; do
  if test -e "$state_file$suffix"; then cp -p "$state_file$suffix" "$backup_dir/"; fi
done
```

在 `config.yaml` 中处理前述旧版本固定值，并保留两插件的 `enabled: true`。确认备份齐全后安装新文件；旧计费插件移出插件扫描目录，确保其中仅有一个要加载的计费插件版本。不要把备份 `.so` 留在 `plugins` 下。

```bash
vi config.yaml
mv "$billing_live" "$backup_dir/old-cpa-key-billing.so"
install -m 644 "$stage_dir/cpa-key-billing.so" "plugins/linux/amd64/cpa-key-billing-v1.3.18.so"
install -m 644 "$stage_dir/privacyfilter.so" plugins/privacyfilter.so
./cli-proxy-api --config ./config.yaml
```

示例目标文件名中的 `1.3.18` 必须与本次编译的 `internal/plugin/types.go` 版本一致。启动日志应报告 `plugin_id=cpa-key-billing version=1.3.18`、`plugin_id=privacyfilter`，并显示 `API server started successfully on: :18316`。如果启动失败，保持服务停止并检查日志；不要反复启动可能触发迁移的旧插件。

启动成功后按 `Ctrl-A d` 脱离 `screen`，再检查进程、端口和两个页面：

```bash
screen -ls
ps -eo pid,args | rg '[c]li-proxy-api --config ./config.yaml'
curl -fsS -o /dev/null -w 'usage HTTP %{http_code}\n' http://127.0.0.1:18316/v0/resource/plugins/cpa-key-billing/usage.html
curl -fsS -o /dev/null -w 'quota HTTP %{http_code}\n' http://127.0.0.1:18316/v0/resource/plugins/cpa-key-billing/quota.html
```

最后用一枚有效的测试 Key 检查本人用量、费用和额度；用 privacyfilter 的中文拦截与英文放行样例检查插件行为。图片模型的 `skip_models` 应继续生效。计费插件的上游额度快照保存在内存中，CPA 重启后需由管理员重新获取。页面返回 HTTP 200 只证明资源可访问，不代表上述功能验收通过。

## 6. 数据库迁移与回滚

当前计费插件 `1.3.18` 使用 SQLite schema 18；启动时会从支持的旧版本迁移。迁移前必须在 CPA 完全停止后备份数据库，包括存在时的 `-wal` 和 `-shm` 文件。旧计费插件不能直接打开新 schema。

如果新服务**尚未产生需要保留的账单**，可停服后把备份的旧计费插件、privacyfilter、`config.yaml`、CPA 二进制和数据库一起恢复，再按原命令启动。若已产生新账单，不能用旧数据库覆盖新增记录；保留当前数据库，修复或升级插件后重新部署。不要通过修改 `PRAGMA user_version` 伪装数据库版本。

## CPA 二进制确实需要更新时

这两个插件的改动不要求修改 CPA 源码。如果另有 CPA 升级需求，先确定要部署的 CPA 提交或官方版本，并确认它支持动态库插件。若服务器有对应源码 checkout，可在停止服务前用 `CGO_ENABLED=1 go build -o ... ./cmd/server` 构建候选二进制；停止后再备份并替换 `cli-proxy-api`。不要把尚未验证的新 CPA 与本次插件部署混成一次无法分辨原因的故障。恢复时也要使用与备份插件、数据库相配的 CPA 二进制。
