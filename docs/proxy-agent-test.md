# 测试文档

## 1. 测试环境

| 项 | 值 |
|---|---|
| 编译 | Go 1.27.3（`GOOS=linux GOARCH=amd64/arm64`，`CGO_ENABLED=0`） |
| 单测运行 | Windows amd64 与 linux amd64 均通过 |
| 真机 | 192.168.15.114，x86_64，4C/7G，无外网直连 |
| 代理 | sing-box 1.14.4（linux-amd64） |
| 订阅 | 真实商业订阅，49 个节点 |

## 2. 单元测试

```bash
go test ./modules/proxy/launcher/ -v
go test ./cmd/proxyagent/ -v
```

结果：`launcher` 14 个用例、`proxyagent` 21 个用例，全部通过。

### 2.1 launcher（Gitea 侧）

| 用例 | 覆盖点 |
|---|---|
| `TestLoadDisabledWhenUnset` | 不设环境变量 → 未启用且不报错（**兼容性基线**） |
| `TestLoadRejectsIncompleteConfiguration` | 缺 COMMAND / 相对路径 / 文件不存在 / 端口越界 → 报错且不启用 |
| `TestLoadAcceptsValidConfiguration` | 合法配置的默认值：地址、socks URL、默认参数、超时 |
| `TestLoadUsesDefaultSkipList` | 默认跳过表包含 `gitee.com` |
| `TestProxyForWithoutSupervisor` | 全局未设置 → 返回 nil（直连） |
| `TestSupervisorStartsAndCaches` | **真实拉起子进程** → 探测通过 → 返回 socks URL → 第二次命中缓存不再拉起 |
| `TestSupervisorFallsBackWhenProcessDies` | 子进程秒退 → 返回 nil 且进入冷却 |
| `TestSupervisorFallsBackWhenBinaryMissing` | 命令不存在 → 返回 nil，不 panic |
| `TestMatch` | 跳过关键字、`localhost`、私有/回环/链路本地 IP、公网 IP |
| `TestMatchAllowList` | 白名单精确匹配与 `*.` 通配，不误放行 `api.github.com` |
| `TestTargetHostPort` | 443/80/自定义端口的推导 |
| `TestProbeSOCKS5` | 对可用端点成功、对关闭端口失败 |

其中 `TestSupervisorStartsAndCaches` 用「测试二进制自身」作为被拉起的进程（
`-test.run=^TestHelperSocks$` 启动一个最小 SOCKS5 服务），因此是**真实的进程启停 + 真实探测**，
不是 mock。

### 2.2 proxyagent

| 分组 | 用例 |
|---|---|
| URI 解析 | vless+reality、vless+ws、vmess(base64 json)、trojan、ss(SIP002)、ss(legacy)、7 种非法输入 |
| 订阅 | 混合内容跳过计数、整体 base64、UTF-8 BOM |
| 选优 | 可达/不可达混合只选活的、全不可达报错、空列表报错 |
| 配置生成 | reality（utls+pbk+sid）、ws（Host 头）、shadowsocks、trojan+grpc、非法监听地址 |
| relay | 双向转发正确、连接计数增减、空闲计时递增 |
| 端到端 | 见下 |

## 3. 端到端测试

`TestAgentEndToEnd`：起一个 HTTP 服务当订阅源、一个 TCP 服务当节点、编译
`testdata/fakecore` 当 sing-box，然后跑真实 agent 二进制，断言：

1. 拉订阅 → 排名 → 拉起 core → 通过 core 发出真实 HTTP 请求（返回 204）
2. 通过 agent 的 relay 端口再发一次请求同样成功
3. 空闲 2s 后进程**自行退出**，退出码 0

`TestAgentExitsWhenNoNodeWorks`：订阅里全是死节点时，agent 在超时内以非 0 退出，
供 Gitea 侧回退到直连。

## 4. 真机验证

### 4.1 兼容性矩阵

同一二进制、同一 sqlite 实例，只改环境变量：

| 配置 | 监听成功 | 代理日志 | fatal |
|---|---|---|---|
| 有效 | ✅ 3999 | 2 行（enabled / skips） | 0 |
| `COMMAND=/nope/agent` | ✅ 3999 | 1 行（disabled 警告） | 0 |
| 未设置 | ✅ 3999 | 0 行 | 0 |

结论：三种情况下 Gitea 都正常启动；只有有效配置才进入新代码路径。

### 4.2 真实链路

```
subscription returned 49 nodes
48 nodes reachable
selected node 🇺🇸美国圣何塞02 (rank 1/48)
ready: listen=127.0.0.1:17890 core=127.0.0.1:17891

github.com                      200   1.64s
codeload.github.com (tar.gz)    200  14.42s
git ls-remote --heads           24 refs  1.94s
退出后 ss -lntp | grep 17890    0（端口已释放）
```

`git ls-remote` 通过代理成功，是「Gitea 能镜像仓库」最直接的证据。

### 4.3 为什么必须有「启动后校验」

第一版只按握手延迟选优，同一订阅下：

| 指标 | 仅握手选优 | 加真实请求校验 |
|---|---|---|
| `https://github.com` | 25s 超时（code 000） | 200，1.64s |
| `git ls-remote` | 成功 | 成功 |

握手 40ms 的节点可能在首个 HTTPS 请求上超时。校验步骤会对前 4 名候选逐个试，
失败自动换下一个，这个改动直接来自真机观测。

### 4.4 按目标 host 测速（proxy.3）

同一订阅、同一台机器，4 连接 × 3s 采样：

| 目标 | 节点 | 吞吐 |
|---|---|---|
| `https://github.com/` | 圣何塞 04 | 310 KB/s |
| `https://github.com/` | 洛杉矶 08 | 143 KB/s |
| `https://git.ffmpeg.org/ffmpeg.git` | 圣何塞 02/03/04/07 | 105–115 KB/s |
| `https://git.ffmpeg.org/ffmpeg.git` | 东京 01/02/03/05/09 | 读不到数据（记 0） |

结论：**同地区内单节点差异大于地区差异**，且不同目标的最优节点不同，所以必须按目标实测。

冷启动耗时（`https://github.com/bkaradzic/bgfx.git` 为目标）：

| 场景 | 到 ready | 候选数 |
|---|---|---|
| 默认阈值 80 KB/s | 8.5s | 2 |
| `-min-speed` 拉满（强制测满） | 27.7s | 6 |

`-start-budget` 默认 30s，`START_TIMEOUT` 默认 45s，实测最坏情况仍在预算内。

一例真实故障（proxy.2 时期，用于说明问题存在）：同一仓库连续三次迁移都在约 14MB 处
报 `curl 92 HTTP/2 stream ... CANCEL`，选中的都是握手最快、实测仅 25 KB/s 的节点；
换成按目标测速后，同一订阅能选到 240–310 KB/s 的节点。

## 5. 复跑

```bash
# 单元 + 端到端（会编译两个临时二进制，约 10s）
go test ./modules/proxy/launcher/ ./cmd/proxyagent/ -count=1

# 跳过端到端，只跑纯单测
go test ./modules/proxy/launcher/ ./cmd/proxyagent/ -short

# 交叉编译双架构
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w" -o /tmp/gitea-$arch .
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w" -o /tmp/agent-$arch ./cmd/proxyagent
done

# 架构校验（3e00=amd64, b700=arm64）
od -An -tx1 -j18 -N2 /tmp/gitea-arm64
```

## 6. 发布产物验证

Release `v28.3.0-proxy.1`，CI 产出 5 个文件：

```
gitea-28.3.0-linux-amd64                 119812256
gitea-28.3.0-linux-arm64                 111214752
gitea-proxy-agent-28.3.0-linux-amd64       6967456
gitea-proxy-agent-28.3.0-linux-arm64       6422688
SHA256SUMS
```

| 检查 | 结果 |
|---|---|
| `sha256sum -c SHA256SUMS` | 4 个全部 OK |
| ELF e_machine | amd64 `3e00`、arm64 `b700` |
| `gitea --version` | `gitea version 28.3.0 built with go1.27.4`（与官方同版本号） |
| 首页 | HTTP 200，15738 字节，标题正确 |
| 静态资源 | `/assets/css/index.CqGlyyh2.css` → 200（`bindata` 已嵌入，单文件自包含） |
| 启动 fatal | 0 |
| 代理初始化 | `Proxy agent enabled: command=... listen=127.0.0.1:17890` |
| 三配置启动矩阵 | 有效 / `COMMAND` 不存在 / 未设置，三者均 `Listen: http` 成功、`[F]` 计数 0 |

> 二进制里内嵌了构建时的 VCS revision，同一个 tag 在不同提交上重建会得到不同的 sha256。
> 校验时请以同一次 Release 里的 `SHA256SUMS` 为准，不要跨次构建比对。

> 下载 arm64 时若出现 `sha256sum: FAILED`，先看文件大小是否与 Release 页面一致——
> 大文件经代理传输可能被截断，断点续传重下即可，不是产物问题（已验证）。

## 6.1 自包含目录与回退验证

目标：不配任何路径，把 `gitea` 与 `sing-box/` 放同一目录即可工作。

测试布局（故意让 agent 二进制与 gitea 分居两处，验证 `BASE_DIR` 确实由 Gitea 提供）：

```
/tmp/sc/gitea                        Gitea 二进制
/tmp/sc/sing-box/sing-box            核心（唯一必需的手工放置）
/tmp/agentbin/gitea-proxy-agent      agent 放在别处
```

| 检查 | 结果 |
|---|---|
| 启动日志 | `base-dir="/tmp/sc"`（取自 Gitea 自身，而非 agent 位置） |
| agent 自述 | `base dir /tmp/sc, data dir /tmp/sc/sing-box, core /tmp/sc/sing-box/sing-box` |
| 真实镜像（`octocat/Hello-World`） | `201`，6.5s，仓库 21MB、3 个分支，直连该 host 时常失败 |
| data 目录产物 | `sing-box`、`cache.db`、`agent-sing-box.json`、`agent.log` 四个文件 |
| `sing-box check -c agent-sing-box.json` | `config OK`（`experimental.cache_file` 写法有效） |
| agent 自身目录 | 未被创建 `sing-box/` 子目录，说明未以自身位置推断 |
| 空闲退出（`IDLE=60s`） | 61s 后 agent 退出，`pgrep sing-box` 为空，无孤儿进程 |
| 移除核心后触发同步 | agent 打印 `fatal: no sing-box binary found: expected /tmp/sc/sing-box/sing-box`，退出码 1；launcher 记 `falling back to a direct connection`；Gitea 首页仍 200 |
| 恢复核心后再同步 | `201`，1.5s，自动重新拉起 |

## 7. 未覆盖 / 已知限制

| 项 | 说明 |
|---|---|
| Gitea 完整镜像流程 | 未在测试实例上跑端到端 mirror（需要外网仓库 + 较长时间），以 `git ls-remote` 等价验证 |
| arm64 真机运行 | 只验证了 ELF 架构与交叉编译产物，未在 arm 机器上跑 |
| 订阅协议覆盖 | vless/vmess/trojan/ss；hysteria2、tuic 等未实现（会跳过，不影响其他节点） |
| 空闲判定 | 依据 relay 的连接计数；若上层用 UDP 或长连接池，判定会偏保守（不会误杀） |
