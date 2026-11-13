# 流程文档：构建、发布、部署

## 1. 构建与发布（GitHub Actions）

工作流：`.github/workflows/build-release.yml`

| 触发 | 行为 |
|---|---|
| push 到 `main` | 构建 4 个产物，上传为 artifact，不发布 |
| push tag `v*` | 构建完成后创建 Release 并附上产物 |
| 手动 | Actions 页面 `Run workflow` |

产物：

```
gitea-28.3.0-linux-amd64            # 自包含，前端资源已嵌入
gitea-28.3.0-linux-arm64
gitea-proxy-agent-28.3.0-linux-amd64
gitea-proxy-agent-28.3.0-linux-arm64
SHA256SUMS
```

构建步骤与官方 `tools/build-release.sh` 一致：`make node_modules` → `make frontend` →
`go generate -tags bindata` → `CGO_ENABLED=0 go build -tags bindata`。因此不需要交叉
工具链、QEMU 或 arm 机器，两种架构在同一个 x86 Runner 上产出。

发布新版本：

```bash
git tag v28.3.0-proxy.1
git push origin v28.3.0-proxy.1
```

## 2. 服务器部署

### 2.1 目录布局（自包含）

sing-box 的位置是**写死**的：Gitea 可执行文件所在目录下的 `sing-box/`。不需要配任何路径。

```
/usr/local/bin/gitea                    # 替换官方二进制
/usr/local/bin/gitea-proxy-agent        # 新增，放哪都行
/usr/local/bin/sing-box/                # 新增：数据目录（注意是个目录）
    sing-box                            #   核心二进制
    cache.db                            #   核心缓存，自动生成
    agent-sing-box.json                 #   选优后重写的核心配置
    agent.log                           #   agent 运行日志，4MB 上限自动截断
```

`gitea-proxy-agent` 的存放位置不影响结果：Gitea 启动 agent 时会用
`GITEA_PROXY_AGENT_BASE_DIR` 告诉它「Gitea 自己在哪」，agent 不会用自身位置推断。

例如 unit 里 `ExecStart=/mnt/data/gitea/gitea`，则核心必须放在
`/mnt/data/gitea/sing-box/sing-box`，状态文件也都在这个目录里。

三个可选覆盖（都不设就是上面的默认行为）：

| 变量 | 作用 |
|---|---|
| `GITEA_PROXY_AGENT_BASE_DIR` | 改基准目录；不设则取 Gitea 可执行文件所在目录 |
| `GITEA_PROXY_AGENT_DATA` | 改数据目录；不设则 `<基准目录>/sing-box` |
| `GITEA_PROXY_AGENT_CORE` | 改核心二进制路径；不设则 `<数据目录>/sing-box` |

权限要求只有一条：**数据目录要对 Gitea 运行用户可写**，否则 `cache.db` 和日志写不进去。

```bash
sudo mkdir -p /usr/local/bin/sing-box
sudo install -m 0755 sing-box /usr/local/bin/sing-box/sing-box
sudo chown -R git:git /usr/local/bin/sing-box      # 只有目录需要可写，二进制本身 0755 即可
```

如果目录确实不可写（例如装进 root 拥有的 `/usr/local/bin` 且不想改属主），
agent 会自动把状态挪到临时目录并打一条警告，代理功能不受影响，只是缓存每次重建：

```
proxy-agent: data directory "/usr/local/bin/sing-box" is not writable: ...
proxy-agent: state kept in "/tmp/gitea-proxy-agent-xxxx", it will be discarded when the agent exits
```

### 2.2 systemd

只改 `Environment=`，不新增服务单元：

```ini
[Service]
User=git
Group=git
WorkingDirectory=/var/lib/gitea
ExecStart=/usr/local/bin/gitea web --config /etc/gitea/app.ini
Environment=USER=git HOME=/home/git GITEA_WORK_DIR=/var/lib/gitea

# —— 按需代理 ——
Environment=GITEA_PROXY_AGENT_ENABLED=true
Environment=GITEA_PROXY_AGENT_COMMAND=/usr/local/bin/gitea-proxy-agent
Environment=GITEA_PROXY_AGENT_PORT=17890
Environment=GITEA_PROXY_AGENT_SKIP_HOSTS=gitee.com,gitcode.com,atomgit.com,gitee.io
Environment=GITEA_PROXY_AGENT_SUBSCRIPTION=https://your-subscription-url
Environment=GITEA_PROXY_AGENT_IDLE=90s
```

注意 `GITEA_PROXY_AGENT_CORE` 现在就**不用写了**，核心按 `<Gitea目录>/sing-box/sing-box` 找。

> `sing-box` 是 Gitea 的子进程，**不要**给它单独写 unit。它随 Gitea 启动、随空闲退出，
> 并被放在同一个进程组里，Gitea 重启时会被一起回收。

### 2.3 部署步骤

```bash
# 1. 停服务
sudo systemctl stop gitea

# 2. 备份并替换二进制
sudo cp /usr/local/bin/gitea /usr/local/bin/gitea.bak
sudo install -m 0755 gitea-28.3.0-linux-amd64 /usr/local/bin/gitea
sudo install -m 0755 gitea-proxy-agent-28.3.0-linux-amd64 /usr/local/bin/gitea-proxy-agent

# 3. 核心放到 <gitea目录>/sing-box/ 下，并把目录交给运行用户
sudo mkdir -p /usr/local/bin/sing-box
sudo install -m 0755 sing-box /usr/local/bin/sing-box/sing-box
sudo chown -R git:git /usr/local/bin/sing-box

# 4. 以 gitea 用户验证（此时不需要任何 -core 参数）
sudo -u git /usr/local/bin/gitea-proxy-agent -h
sudo -u git /usr/local/bin/sing-box/sing-box version

# 5. 写入环境变量后启动
sudo systemctl daemon-reload
sudo systemctl start gitea
```

## 3. 验证生效

```bash
# a. 配置被接受，并打印解析出的基准目录
journalctl -u gitea -n 200 | grep -i "proxy agent"
#   期望：Proxy agent enabled: command=... base-dir="/usr/local/bin" ...

# b. 首次镜像同步时，agent 会打印实际用的路径
#   proxy-agent: base dir /usr/local/bin, data dir /usr/local/bin/sing-box, core /usr/local/bin/sing-box/sing-box

# c. 空闲时没有进程
pgrep -a sing-box || echo "空闲，符合预期"

# d. 触发一次 mirror 同步后，数据目录出现四个文件
ls -l /usr/local/bin/sing-box
#   sing-box  cache.db  agent-sing-box.json  agent.log

# e. 同步结束约 90s 后进程消失
sleep 100; pgrep -a sing-box || echo "已自动回收"

# f. 端口只在同步期间存在
ss -lntp | grep 17890
```

手动验证代理本身（不经过 Gitea）：

```bash
sudo -u git /usr/local/bin/gitea-proxy-agent \
  -listen 127.0.0.1:17890 \
  -subscription "https://your-subscription-url" \
  -idle 60s &
sleep 5
curl -x socks5h://127.0.0.1:17890 -o /dev/null -w "%{http_code}\n" https://github.com
git -c http.proxy=socks5h://127.0.0.1:17890 ls-remote --heads https://github.com/go-gitea/gitea.git | head -3
```

## 4. 排查

| 现象 | 原因 | 处理 |
|---|---|---|
| 日志无 `Proxy agent enabled` | 环境变量没进 Gitea 进程 | `systemctl show gitea -p Environment` |
| 日志 `Proxy agent disabled ...` | 配置非法，后面会带具体原因 | 按提示修正；此时行为等同官方 |
| 同步时没有 sing-box 进程 | 目标 host 被跳过 / 冷却期 / 探测失败 | 看 `Proxy agent unavailable (...)` 日志 |
| `no reachable node` | 订阅过期或全部节点不可用 | 手动跑一次验证命令确认 |
| 同步仍失败但代理正常 | 节点质量差 | 调小 `-max-delay`，或换订阅 |
| 冷启动超过 45s 被判定失败 | 节点多、测速慢 | 加大 `GITEA_PROXY_AGENT_START_TIMEOUT=60s`，或降低候选数 |
| gitee 走代理导致变慢 | `SKIP_HOSTS` 被覆盖 | 显式写出完整跳过列表 |

### 4.1 大仓库在固定时长处被切断

如果日志是 `curl 92 HTTP/2 stream ... CANCEL` / `early EOF`，且每次都在差不多的时间
点断开（而不是差不多的大小），那是连接被中间设备按时间掐断，不是选优问题。先试：

```ini
[git.config]
http.version = HTTP/1.1
```

HTTP/2 在长连接上更容易被中间盒重置，退回 HTTP/1.1 通常能解决。这条只影响 git 子进程，
不影响 Gitea 自己的 HTTP 服务。

单次调试点：把 `GITEA_PROXY_AGENT_IDLE` 调大（如 `10m`），方便观察进程状态；问题解决后改回。

## 5. 回滚

```bash
sudo systemctl stop gitea
sudo systemctl edit gitea          # 删掉所有 GITEA_PROXY_AGENT_* 行
sudo cp /usr/local/bin/gitea.bak /usr/local/bin/gitea
sudo systemctl start gitea
```

只删环境变量、不换二进制也可以：未设置变量时行为与官方完全一致，代码路径不会被触发。
