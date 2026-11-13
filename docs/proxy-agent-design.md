# 设计文档：Gitea 按需代理（28.3.0）

## 1. 目标与约束

| 目标 | 约束 |
|---|---|
| 拉取/镜像国外仓库时自动走代理 | 必须与官方版行为完全兼容 |
| 没有需求时进程不存在 | 不引入常驻 systemd 服务 |
| 每次启动重新拉订阅并选优 | 不新增 Go 依赖（只用 vendor 里已有的） |
| 国内站点（gitee 等）直接跳过 | 跳过规则可配置 |
| 配置只在环境变量 | 不新增 app.ini 配置项 |

## 2. 为什么改动只有四处

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
| `services/gitproxy/server.go` | +18 行，拨号/CONNECT 超时改为可配置（默认仍是 30s） |
| `routers/init.go` | +4 行，初始化与超时联调 |

`git` 的任何调用点都没有改动。

第四处（`services/gitproxy/server.go`）是为了配合**按需启动**：官方把「拨号到上游」限制
在 30 秒（`proxyDialer.Timeout` 与 `handleConnect` 的 30s 上下文），而冷启动要拉订阅、
排序、测速，实测 14~40 秒。不放开这两处，冷启动一旦超过 30 秒，第一个 git 请求就收到
`CONNECT tunnel failed, response 504`，而代理几秒后就好了。现在 launcher 启动时把它调到
`START_TIMEOUT + 15s`（所以还是只在配置了才生效），默认值不变。

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
        ┌──────────────────────── Idle（无进程） ────────────────────────┐
        │                                                               │
   第一次有非跳过 host 的 git 请求                                 空闲 90s
        │                                                               │
        ▼                                                               │
   Starting ──agent 报 ready──▶ Probing（SOCKS5 CONNECT 到目标）──▶ Ready ┘
        │                             │
   超时/失败                     探测失败
        │                             │
        ▼                             ▼
   Cooldown 30s（期间直连）◀────────────┘
```

- **启动条件**：`ENABLED` && host 未被跳过 && 无存活进程 && 不在冷却期。
- **停止条件**：agent 侧由 relay 统计连接数，空闲 `IDLE` 后自行退出；或 Gitea 退出（
  `context.AfterFunc`）；或启动/探测失败立即回收。
- **单例**：`startMu` 串行化冷启动，一批并发 mirror 只会拉起一个进程。
- **就绪判定**：以 agent 自己打印的 `ready listen=...` 行为准，不看端口是否可连。
  端口可连是进程无关的信号——旧实例还在退出时端口仍可连，新启动会误判「已就绪」，
  探针随即打到垂死的旧实例上。
- **切换目标**：`stopLocked` 会等旧进程真正被 reap 之后才启动新实例，避免两个实例
  抢同一个监听端口和同一份核心状态。

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

但「能连」不等于「能跑」：2026-11-13 的一次实测里，节点能通过 `-check-url`，却一个字节
也搬不动，agent 于是兜底选了它，随后三个仓库同步全部 `CONNECT tunnel failed 502/504`。
所以现在加了一条硬规则（`-require-speed`，默认开）：

- 只要有候选**真的读到了字节**，就只在有测量值的候选里选最快的；
- 所有候选都测不到吞吐时**拒绝服务**，让 launcher 回退直连并进入冷却，而不是挂一条死隧道。

兜底不是二选一：`-require-speed=false` 可恢复旧行为（仍然只在没有任何测量值时使用
「能连但测不到速」的节点），用于少数目标确实无法被探针读取的场景。

### 7.3 测速针对的是「这次要访问的目标」

launcher 把本次要访问的目标通过 `GITEA_PROXY_AGENT_TARGET` 传给 agent，agent 就测这个目标，
而不是某个固定的公共地址。但**能拿到多精确的目标，取决于请求形态**：

| 远端方式 | Gitea 侧看到的请求 | 传给 agent 的 target |
|---|---|---|
| `https://`（绝大多数，走 CONNECT） | 只有 host，URL 在隧道里 | `github.com:443` → 探 `https://github.com/` |
| `http://`（绝对 URI 形式） | 完整 URL，含路径 | 还原出的仓库地址 → 探 `<repo>/info/refs` |

HTTPS 远端拿不到路径是协议决定的：git 先发 `CONNECT github.com:443`，真正的
`GET /owner/repo.git/info/refs` 在加密隧道里，中间的代理看不到。所以对 HTTPS 远端，
实际探测的就是 host 根，失败再退 `http://host/`。

这个精度对绝大多数站点够用，因为**节点对目标的选择性极强**：

| 目标 | 美国节点 | 日本节点 |
|---|---|---|
| `https://github.com/` 根 | 143–310 KB/s | 162–271 KB/s |
| `https://git.ffmpeg.org/ffmpeg.git`（http 形式测得） | 105–115 KB/s | 0（读不到数据） |

已知短板：`git.ffmpeg.org` 这类站点的根路径只回 301/302。2026-11-13 实测（v28.3.0-proxy.6，
114 上真实订阅）：301 的响应**带一个很小的实体**，探针能读到字节，速度记为 **1 KB/s**。
所以 `-require-speed` 不会拒它，选点照常完成（`git ls-remote https://git.ffmpeg.org/ffmpeg.git`
经代理 3.7s 通过）。只是这个数值是噪音，等于「哪个节点先回了实体就选谁」，没有带宽排序意义。

只有**真的一字节不回**的目标（纯 204/HEAD，或所有节点对该目标都 5xx）才会因为
`-require-speed` 整体回退直连。确实需要给这类 host 走代理时，见下面的开关说明。
想让这类目标也有带宽排序，可以在 `app.ini` 里改用 `http://` 形式的 clone 地址
（那样 launcher 能还原出完整的 `<repo>/info/refs`），或把 `-check-url` 指到一个该 host 上
确实返回大实体的地址。

注意 `-require-speed` 默认开启后，这类「探针读不到实体」的目标会**整体回退直连**
（因为没有任何候选拿到测量值）。确实需要给这类 host 走代理时，用环境变量关掉这条规则：

```ini
GITEA_PROXY_AGENT_ARGS=-listen {addr} -idle {idle} -require-speed=false
```

关掉后恢复 proxy.4 的行为：仍然优先用有测量值的节点，只有在完全没有测量值时才使用
「能连但测不到速」的节点。

### 7.4 实例隔离

核心状态按**实例**隔离，不共用文件：

| 文件 | 作用域 | 生命周期 |
|---|---|---|
| `cache-<pid>-<n>.db` | 每个核心实例 | 核心退出即删 |
| `agent-sing-box-<pid>-<n>.json` | 每个核心实例 | 核心退出即删 |
| `agent.log` | 整个数据目录 | 长期保留，4MB 截断 |

原因有两个，都被线上事故验证过：

1. sing-box 对 `cache_file` 加独占锁（bbolt，5s 超时）。共用一个 `cache.db` 时，
   只要两个核心有一瞬间重叠，后启动的那个就会
   `FATAL start service: initialize cache-file: timeout`；而它正是要参与测速的核心，
   于是整轮候选全部「测不到速」。
2. 配置文件同理：第二个 agent 覆写 `agent-sing-box.json` 时，正在运行的核心配置
   已经被换掉了。

配合 `removeStale`（启动时清理 2 小时前的实例文件）和「候选不共享核心」——
同一时刻只允许一个核心占用核心端口，所以选点分成两步：**扫描阶段每个候选都是
「启动 → 测速 → 立刻停掉」，扫描结束再为胜出的节点启动一次**。早先的做法是把测得最快的
核心留在端口上继续试下一个，结果后续候选全部 `bind: address already in use`
（或被 `waitPortFree` 直接拒绝），实际只有第一个候选被测到。

### 7.5 预算控制

选优会启动多个 core，必须防止冷启动无限拖长：

- `-start-budget`（40s）覆盖「拉订阅 + 排序 + 选优」全过程，超时就用当前最优；
  单个候选的 core 启动、请求校验、带宽采样都受它约束
- `START_TIMEOUT`（60s）是 launcher 的等待上限，必须大于前者；agent 若中途判定
  无可用节点会立即退出，launcher 看到进程退出就马上回退，不会傻等到 60s
- 实测：默认阈值下 8~14s 就绪（试 1~2 个候选）；强制测满 6~8 个候选约 40s。
  后者超过了官方 gitproxy 的 30s 拨号上限，所以 launcher 会把该上限抬到
  `START_TIMEOUT + 15s`（见 §2 第四处改动），否则第一个请求会先收到 504

带宽采样在**首字节到达之后**才开始计时，否则一次 4s 的 TLS 握手会吃掉整个采样窗口，
把「握手慢但传输快」的节点误判成 0。

### 7.6 并发采样

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
