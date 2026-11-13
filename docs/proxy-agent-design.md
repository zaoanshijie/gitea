# 设计文档：Gitea 按需代理（28.3.0）

## 1. 目标与约束

| 目标 | 约束 |
|---|---|
| 拉取/镜像国外仓库时自动走代理 | 必须与官方版行为完全兼容 |
| 没有需求时进程不存在 | 不引入常驻 systemd 服务 |
| 每次启动重新拉订阅并选优 | 不新增 Go 依赖（只用 vendor 里已有的） |
| 国内站点（gitee 等）直接跳过 | 跳过规则可配置 |
| 配置只在环境变量 | 不新增 app.ini 配置项 |

## 2. 为什么改动只有三处

Gitea 28.x 的 git 出站流量本来就经过一个**内置的本地正向代理**：

```
git 子进程  ──http.proxy──▶  127.0.0.1:<随机端口>（services/gitproxy，随 Gitea 启动）
                                      │
                                      └──▶ policy.Proxy(req) 决定再往哪走
```

- `services/gitproxy/server.go` 启动时监听 `127.0.0.1:0`，通过 `gitcmd.SetExtraEnvs()` 把
  `http.proxy=http://gitea:<随机密码>@127.0.0.1:<port>` 注入**所有** git 子进程。
- `modules/egress/policies.go` 的 `gitProxySelector()` 决定这个本地代理的上游：
  `[git.config] http.proxy` → `[proxy] PROXY_URL` → 环境变量 → **直连**。

因此「动态」体现在**上游存不存在**，而不是「git 走不走本地代理」。注入的地址恒定，
不需要在每次 fetch 前后修改环境变量或 git config，也就不会破坏任何并发场景。

改动清单：

| 文件 | 改动 |
|---|---|
| `modules/proxy/launcher/config.go` | 新增，环境变量解析与校验 |
| `modules/proxy/launcher/launcher.go` | 新增，进程托管与探测 |
| `modules/proxy/launcher/proc_{unix,windows}.go` | 新增，进程组与权限差异 |
| `modules/egress/policies.go` | +10 行，插入一个分支 |
| `routers/init.go` | +2 行，初始化 |

`git` 的任何调用点都没有改动。

## 3. 兼容性保证

三条规则，任何一条不满足都退回官方行为：

1. **未设置** `GITEA_PROXY_AGENT_ENABLED`（或为假）→ `Load()` 返回 `(nil,false,nil)`，
   `launcher.Enabled()` 为 false，选择器保持原样。
2. **设置但非法**（命令不存在/不是绝对路径/不可执行、端口越界、超时非正数）→
   `Load()` 返回错误，`Init` 只打一条 `[W]` 日志，全局 supervisor 保持为空。
3. **运行时失败**（进程起不来、探测失败、连接断开）→ `ProxyFor` 返回 `nil`，
   即「这次直连」，**永不返回 error**。

实测（同一台机器、同一个二进制、sqlite 实例）：

| 环境变量 | 启动结果 | 代理日志行数 | fatal |
|---|---|---|---|
| 有效配置 | 正常监听 3999 | 2 | 0 |
| `COMMAND=/nope/agent` | 正常监听 3999 | 1（警告） | 0 |
| 完全未设置 | 正常监听 3999 | 0 | 0 |

## 4. 环境变量

前缀统一为 `GITEA_PROXY_AGENT_`。

| 变量 | 默认 | 说明 |
|---|---|---|
| `ENABLED` | `false` | 总开关，`1/true/yes/on` 为真 |
| `COMMAND` | 空（必填） | 代理程序**绝对路径** |
| `ARGS` | `-listen {addr} -idle {idle}` | 参数模板，支持 `{addr}` `{idle}` |
| `PORT` | `17890` | 本地监听端口，仅绑定 127.0.0.1 |
| `SKIP_HOSTS` | `gitee.com,gitcode.com,atomgit.com,gitee.io` | 跳过关键字（子串匹配） |
| `ALLOW_HOSTS` | 空 | 非空时**只**代理这些 host（精确或 `*.x`） |
| `START_TIMEOUT` | `60s` | 冷启动上限（含拉订阅+测速+最多 4 个候选）；agent 自身预算 40s |
| `PROBE_TIMEOUT` | `5s` | 单次连通性探测上限 |
| `PROBE_TTL` | `60s` | 同一 host 探测结果缓存时长 |
| `IDLE` | `90s` | 传给 agent，空闲自退出时长 |
| `SUBSCRIPTION` | 空 | 由 agent 读取，每次启动重新拉取 |
| `BASE_DIR` | Gitea 可执行文件所在目录 | 由 launcher 推导后传给 agent，只在需要把状态放到别处时才显式设置 |
| `CORE` | `<DATA>/sing-box` | 由 agent 读取；默认就是「Gitea 同级 `sing-box/` 目录里的那个」 |
| `DATA` | `<BASE_DIR>/sing-box` | 由 agent 读取，核心配置、缓存、日志都落在这里 |

### 4.1 路径推导

目标：解压一个发布包就能跑，路径不配置也对。

```
Gitea 进程
  └─ os.Executable() → /mnt/data/gitea/gitea
       └─ BASE_DIR  = /mnt/data/gitea          （通过环境变量传给 agent）
            ├─ DATA    = /mnt/data/gitea/sing-box
            └─ CORE    = /mnt/data/gitea/sing-box/sing-box
```

agent 侧解析顺序：显式 `-core`/`CORE` → `<BASE_DIR>/sing-box/sing-box` → `PATH`。
显式指定但文件不存在时**直接报错**，不会静默改用别的（否则打错字很难发现）。

数据目录是唯一可以降级的东西：不可创建或不可写时换用临时目录并打警告，代理照常工作，
只是缓存每次重建。核心找不到则 agent 退出，launcher 记录警告并回退直连——Gitea 本身
不受影响，这也符合「代理出问题不能影响官方行为」这条底线。

## 5. 生命周期

```
        ┌─────────────────────── Idle（无进程） ───────────────────────┐
        │                                                             │
   第一次有非跳过 host 的 git 请求                               空闲 90s
        │                                                             │
        ▼                                                             │
   Starting ──端口可连──▶ Probing（SOCKS5 CONNECT 到目标）──▶ Ready ───┘
        │                        │
   超时/失败                 探测失败
        │                        │
        ▼                        ▼
   Cooldown 30s（期间直连）◀──────┘
```

- **启动条件**：`ENABLED` && host 未被跳过 && 无存活进程 && 不在冷却期。
- **停止条件**：agent 侧由 relay 统计连接数，空闲 `IDLE` 后自行退出；或 Gitea 退出（
  `context.AfterFunc`）；或启动/探测失败立即回收。
- **单例**：`startMu` 串行化冷启动，一批并发 mirror 只会拉起一个进程。

探测使用 SOCKS5 `CONNECT` 到**真实目标**（不是写死的健康检查地址）：SOCKS5 的成功应答
发生在上游连接建立之后，所以成功即代表「这个代理确实能到那个站点」，单纯探端口做不到。

## 6. host 匹配

按顺序判断，任一条命中即直连：

1. `localhost` / `*.localhost`
2. IP 字面量属于 loopback / private / link-local / unspecified（内网 mirror 走代理必然失败）
3. `SKIP_HOSTS` 任一为**子串**（`gitee.com` 也会匹配 `mirror.gitee.com.cn`）
4. `ALLOW_HOSTS` 非空且无一匹配（精确或 `*.example.com`）

白名单用精确匹配、跳过用子串匹配：前者防误放行，后者符合「跳过关键字」的语义。

## 7. agent 设计

```
拉订阅 ──▶ 解析节点 ──▶ 并发 TCP+TLS 握手测速 ──▶ 按延迟排序
                                                      │
                    ┌─────────────────────────────────┘
                    ▼
   取前 N 名（默认 4），逐个：生成配置 → 启动 core → 真实请求校验
                              → 对目标地址做 4 连接并发带宽采样
                    │                                         │
                    └──────── 带宽更优则替换，达到阈值即停 ◀───┘
                    ▼
   在 127.0.0.1:PORT 起 relay（转发到 core 的 :PORT+1），打印 ready
                    ▼
   每 2s 检查：连接数为 0 且空闲超过 IDLE → 退出
```

### 7.1 为什么不能只看握手延迟

握手延迟和吞吐几乎不相关。实测（同一订阅、同一台机器）：

| 节点 | 到 `github.com` 的吞吐 |
|---|---|
| 圣何塞 04 | 310 KB/s |
| 圣何塞 01 | 191 KB/s |
| 洛杉矶 08 | 143 KB/s |

同地区能差一倍以上，按地区或按延迟筛都会踩到慢节点。所以排序只用来决定**先试谁**，
真正的取舍靠实测带宽。

### 7.2 测速即校验

早先的实现用固定的 `https://github.com/robots.txt` 做「启动后校验」。这在
`git.ffmpeg.org` 这类目标上会误判：节点能到 github 不代表能到目标，反之亦然。
现在**向目标发的那次请求本身就是校验**——能拿到数据就算通过，同时得到带宽；
拿不到才退回 `-check-url` 兜底，两者都失败才判定节点不可用。

### 7.3 测速针对的是「这次要访问的目标」

`ProxyFor(host)` 触发启动时，launcher 会把本次请求的仓库地址通过
`GITEA_PROXY_AGENT_TARGET` 传给 agent：

```
https://github.com/bkaradzic/bgfx.git/info/refs?service=git-upload-pack
        │                                     │
        └── agent 从中还原出 ──────────────────┘
            https://github.com/bkaradzic/bgfx.git/info/refs?service=git-upload-pack
```

测的就正好是 git 真正发的那个请求。这一点很关键，因为**节点对目标的选择性极强**：

| 目标 | 美国节点 | 日本节点 |
|---|---|---|
| `https://github.com/` | 105–115 KB/s | 0（读不到数据） |
| 目标整体（`github.com` 根） | 143–310 KB/s | 162–271 KB/s |

`git.ffmpeg.org` 的根路径只回 301/302、没有实体，所以 agent 会依次尝试
`info/refs` → 仓库根 → host 根 → `http://`，命中哪个就用哪个继续测其余候选；
全都拿不到数据时不算节点失败（有些 git 服务会拒绝普通 HTTP 请求），只是速度记为 0。

### 7.4 预算控制

选优会启动多个 core，必须防止冷启动无限拖长：

- `-start-budget`（40s）覆盖「拉订阅 + 排序 + 选优」全过程，超时就用当前最优；
  单个候选的 core 启动、请求校验、带宽采样都受它约束
- `START_TIMEOUT`（60s）是 launcher 的等待上限，必须大于前者；agent 若中途判定
  无可用节点会立即退出，launcher 看到进程退出就马上回退，不会傻等到 60s
- 实测：默认阈值下 8~12s 就绪（试 1~2 个候选）；强制测满时约 28s

带宽采样在**首字节到达之后**才开始计时，否则一次 4s 的 TLS 握手会吃掉整个采样窗口，
把「握手慢但传输快」的节点误判成 0。

### 7.5 并发采样

单条 TCP 连接经常被窗口限制卡住，测不出节点容量，所以默认开 4 条并发连接
（`-speed-conns`）同时拉取，用 Range 头取不同区段，累加后再除以耗时。

几个刻意的选择：

- **relay 而不是直接用 core 的端口**：SOCKS 服务无法告诉你「现在有没有人在用」，
  一个 TCP 中转可以精确计数，这是「用完就关」能成立的前提。
- **握手测速后再加一层真实校验**：握手快不代表能跑流量。实测中一个 40ms 握手的节点
  对 `github.com` 25s 超时，加校验后自动换到下一个。校验失败即换候选，最多试 4 个。
- **不做定时轮换**：每次启动都会重新拉订阅、重新选优，已满足「每次都要拉取订阅」；
  中途换节点会打断正在传输的 clone，得不偿失。
- **纯标准库**：只用了 vendor 里已有的 `golang.org/x/net/proxy`，不新增依赖。

## 8. 安全

- 只监听 `127.0.0.1`，不对外暴露。
- 生成的 sing-box 配置写入 `0700` 临时目录、文件 `0600`，退出即删。
- 节点凭证不会进日志：`Node.String()` 只有名字、协议、地址。
- `ALLOW_HOSTS` 之外的目标不会被代理，避免成为通用出口。
- 仍然经过 Gitea 原有的 `policy` 黑白名单（`Migrations.ALLOWED_HOST_LIST` / `BLOCKED_HOST_LIST`）。

## 9. 与上游同步的成本

新增代码全部在 `modules/proxy/launcher/` 与 `cmd/proxyagent/`，对上游文件的修改只有
`modules/egress/policies.go`（10 行）与 `routers/init.go`（2 行）。

升级 Gitea 时：把 `modules/proxy/launcher/` 原样带入，重新把两个分支点接上即可。
若上游 `gitProxySelector()` 签名变化，只需保证 `return launcher.ProxyFor, nil` 这一句仍在
`[proxy] PROXY_URL` 分支之后。
