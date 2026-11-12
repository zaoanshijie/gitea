# 上游升级同步流程

本仓库是 <https://github.com/go-gitea/gitea> 的 fork，改动只放在 `proxy-agent` 分支上，
且全部基于某个**正式 release tag**，因此升级就是一次 rebase。

## 1. 当前基线

| 项 | 值 |
|---|---|
| 基线版本 | `v28.3.0` |
| 改动分支 | `proxy-agent`（同时也是默认分支） |
| 提交数 | 3 个（launcher / agent / docs+ci） |
| 改动文件 | 新增 24 个，修改 2 个（`policies.go` +10 行、`init.go` +2 行） |
| 上游 tag | 完整 264 个（fork 时由 GitHub 服务端同步） |

查看相对基线的差异：

```bash
git log --oneline v28.3.0..proxy-agent
git diff --stat v28.3.0..proxy-agent
```

预期只有这些：

```
 cmd/proxyagent/**                      新增
 modules/proxy/launcher/**              新增
 modules/egress/policies.go             +10 行
 routers/init.go                        +2 行
 docs/proxy-agent-*.md                  新增
 .github/workflows/build-release.yml    新增
```

## 2. 升级到新版本

以官方发布 `v28.4.0` 为例：

```bash
git clone git@github.com:zaoanshijie/gitea.git
cd gitea
git remote add upstream https://github.com/go-gitea/gitea.git

git fetch upstream --tags
git checkout proxy-agent

# 把改动提交整体挪到新 tag 上
git rebase v28.4.0
```

冲突几乎只会出现在这两处：

| 文件 | 冲突原因 | 处理 |
|---|---|---|
| `modules/egress/policies.go` | `gitProxySelector()` 被上游改动 | 保留上游新逻辑，在 `[proxy] PROXY_URL` 分支**之后**插入 `if launcher.Enabled() { return launcher.ProxyFor, nil }` |
| `routers/init.go` | 初始化顺序调整 | 保证 `launcher.Init(ctx)` 在 `gitproxy_service.Run` 之前执行 |

新增的两个目录（`modules/proxy/launcher/`、`cmd/proxyagent/`）上游没有同名文件，不会冲突。

如果某个新版本改动了 launcher 依赖的接口，按编译错误修即可——它对 Gitea 的依赖只有
`modules/log`、`modules/setting` 的 `GitConfig`，以及 gitproxy 的 `policy.WithProxy` 约定
（`func(*http.Request) (*url.URL, error)`）。

## 3. 升级后必做的验证

```bash
# 1. 单测
go test ./modules/proxy/launcher/ ./cmd/proxyagent/ -count=1

# 2. 编译
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/gitea .

# 3. 兼容性基线：不设环境变量时，行为必须与官方一致
grep -n "launcher.Enabled()" modules/egress/policies.go

# 4. 推送并让 CI 出包
git push --force-with-lease origin proxy-agent
git tag v28.4.0-proxy.1
git push origin v28.4.0-proxy.1
```

tag 名必须是 `v<版本>-proxy.<序号>`，CI 会把它还原成 `<版本>` 写进二进制的 `main.Version`，
保证 `gitea --version` 与官方一致。

## 4. 同步主分支

fork 的 `main` 与上游一致，直接在 GitHub 页面点 **Sync fork → Update branch** 即可，
或者：

```bash
git fetch upstream main
git push origin upstream/main:main
```

`main` 不参与构建，只用于看差异；构建始终基于 `proxy-agent`。

## 5. 为什么不用「在 main 上改」

`main` 每周都有几十个提交，功能分支跟着 main 走会导致：

- 每次 rebase 都要处理大量无关改动带来的冲突
- 打出来的包是开发版（`28.x+dev-xxx`），与官方 release 不可比对
- 出问题时无法判断是上游改动还是我们的改动

所以固定「一个 release tag + 一个功能分支」，升级时整体平移。
