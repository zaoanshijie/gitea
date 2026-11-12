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

### 2.1 目录布局

```
/usr/local/bin/gitea                      # 替换官方二进制，其余不变
/usr/local/bin/gitea-proxy-agent          # 新增
/usr/local/bin/sing-box                   # 新增（第三方）
/etc/gitea/app.ini                        # 不动
```

权限：

```bash
chown root:git /usr/local/bin/gitea /usr/local/bin/gitea-proxy-agent /usr/local/bin/sing-box
chmod 0750 /usr/local/bin/gitea-proxy-agent /usr/local/bin/sing-box
```

Gitea 进程用户只需要「可执行」，不需要写权限。

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
Environment=GITEA_PROXY_AGENT_CORE=/usr/local/bin/sing-box
Environment=GITEA_PROXY_AGENT_IDLE=90s
```

> `sing-box` 是 Gitea 的子进程，**不要**给它单独写 unit。它随 Gitea 启动、随空闲退出，
> 并被放在同一个进程组里，Gitea 重启时会被一起回收。

如果 unit 里开了 `ProtectSystem=strict`，补一行 `RuntimeDirectory=gitea-proxy`，
否则 agent 的临时配置目录可能不可写。

### 2.3 部署步骤

```bash
# 1. 停服务
sudo systemctl stop gitea

# 2. 备份并替换二进制
sudo cp /usr/local/bin/gitea /usr/local/bin/gitea.bak
sudo install -m 0755 gitea-28.3.0-linux-amd64 /usr/local/bin/gitea
sudo install -m 0755 gitea-proxy-agent-28.3.0-linux-amd64 /usr/local/bin/gitea-proxy-agent
sudo install -m 0755 sing-box /usr/local/bin/sing-box

# 3. 以 gitea 用户验证权限
sudo -u git /usr/local/bin/gitea-proxy-agent -h
sudo -u git /usr/local/bin/sing-box version

# 4. 写入环境变量后启动
sudo systemctl daemon-reload
sudo systemctl start gitea
```

## 3. 验证生效

```bash
# a. 配置被接受
journalctl -u gitea -n 200 | grep -i "proxy agent"
#   期望：Proxy agent enabled: command=... listen=127.0.0.1:17890 ...

# b. 空闲时没有进程
pgrep -a sing-box || echo "空闲，符合预期"

# c. 触发一次 mirror 同步后，进程出现
#    Gitea 后台 -> 仓库 -> 设置 -> 镜像 -> 立即同步
pgrep -a sing-box

# d. 同步结束约 90s 后进程消失
sleep 100; pgrep -a sing-box || echo "已自动回收"

# e. 端口只在同步期间存在
ss -lntp | grep 17890
```

手动验证代理本身：

```bash
sudo -u git /usr/local/bin/gitea-proxy-agent \
  -listen 127.0.0.1:17890 \
  -subscription "$GITEA_PROXY_AGENT_SUBSCRIPTION" \
  -core /usr/local/bin/sing-box \
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
| gitee 走代理导致变慢 | `SKIP_HOSTS` 被覆盖 | 显式写出完整跳过列表 |

单次调试点：把 `GITEA_PROXY_AGENT_IDLE` 调大（如 `10m`），方便观察进程状态；问题解决后改回。

## 5. 回滚

```bash
sudo systemctl stop gitea
sudo systemctl edit gitea          # 删掉所有 GITEA_PROXY_AGENT_* 行
sudo cp /usr/local/bin/gitea.bak /usr/local/bin/gitea
sudo systemctl start gitea
```

只删环境变量、不换二进制也可以：未设置变量时行为与官方完全一致，代码路径不会被触发。
