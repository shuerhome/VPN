# 落地链路台

把**静态住宅IP**绑定到每一台 iPhone，生成二维码，扫码即用，用于社媒账号日常运营。平台看到的永远是这台手机绑定的那个住宅IP。

每条链路同时给出两种导入方式：

| 方式 | 路径 | 适合 |
|---|---|---|
| **小火箭扫码（推荐）** | iPhone → 你的 VPS（VLESS Reality）→ 住宅IP → 平台 | 小火箭、Loon、Quantumult X、Stash，扫一个码就能用 |
| **Stash 订阅** | iPhone → 机场节点（自动故障切换）→ 住宅IP → 平台 | 想用已购机场节点出境时 |

两条路都是**断线不回落**：住宅IP连不上就断网，绝不会从机场IP或 VPS 的 IP 漏出去。

## 功能

- **机场订阅**：支持 Clash YAML 和 Base64 节点列表（ss / vmess / vless / trojan / hysteria2）；自动读取已用流量、总流量、到期时间；节点测速；每 30 分钟自动同步。
- **住宅IP**：整段粘贴自动识别 IP、端口、账户、密码（十几种常见格式，见下文）；自动检测出口IP、地区、运营商、是否机房IP；到期提醒。
- **链路**：设备 + 账号 + 住宅IP，一个 IP 只能绑一台设备；每 15 分钟自动检测两条路径的出口IP，出口变了会标红。
- **二维码**：中转节点二维码、Stash 订阅二维码、住宅IP节点二维码；泄露了可一键重置，旧码立即失效。
- **使用情况**：每台手机是否在线、从哪个IP连进来、今天和近 30 天用了多少流量（中转模式，按字节精确计量）。
- **一码一机**：同一个二维码被两个以上的IP同时使用时告警，可一键重置。
- **一个订阅链接通吃**：小火箭 / Loon / Quantumult X 拉到中转节点，Stash / Clash 拉到机场链路配置，浏览器打开则是导入页。
- **设备导入页**：给操作手机的同事发一个链接，打开就是二维码和手机时区、语言设置说明，不需要面板密码。
- **Telegram 机器人**：出口IP变化、住宅IP连不上、一码多机、住宅IP / 机场快到期、机场流量用到 90%、订阅同步失败时推送；`/status` 看概况，`/backup` 立即备份；每天凌晨 4 点自动把数据库备份发给你。
- **安全**：面板密码登录（带失败次数限制），住宅IP密码、订阅链接、节点参数 AES-256-GCM 加密存储；面板只通过 Cloudflare Tunnel 对外，不开放端口。

## 部署（Debian 13 VPS + Cloudflare 域名）

### 1. 在 Cloudflare 创建隧道

1. 登录 Cloudflare → **Zero Trust** → **Networks** → **Tunnels** → **Create a tunnel**，类型选 **Cloudflared**，起个名字。
2. 安装方式页面里有一条安装命令，里面有一串 `eyJ...` 开头的 **Token**。复制这串 Token（整条命令也行，安装脚本会自动取出 Token）；不要在服务器上执行那条命令。
3. 下一步 **已发布的应用程序 / Public Hostname**：
   - 子域：例如 `panel`；域：选你的域名
   - **服务 URL：`http://panel:8080`**（旧版页面分两栏时：Type 选 `HTTP`，URL 填 `panel:8080`）
   - 不要填 `localhost` 或你的域名，也不要用 `https`：隧道和面板都在 Docker 里，`panel` 是面板容器的名字。
4. 保存。

### 2. 在 VPS 上安装

```bash
ssh root@你的VPS公网IP
apt-get update && apt-get install -y git
git clone -b claude/optimistic-volta-qx6t7n https://github.com/shuerhome/VPN.git /opt/luodi
cd /opt/luodi
bash deploy/install.sh
```

> 仓库是私有的话，`git clone` 时用 GitHub 用户名 + [Personal Access Token](https://github.com/settings/tokens) 作为密码。

脚本会问五个问题：面板域名、Tunnel Token、VPS 公网 IP（默认自动检测）、中转端口（默认 443，被占用时会建议别的端口）、Telegram 机器人 Token（可跳过）。然后自动安装 Docker、生成密码和加密密钥、编译并启动，最后从外网打开一次面板确认能访问。结束时会打印**登录密码**（之后可以用 `grep PANEL_PASSWORD /opt/luodi/.env` 查看）。

网页终端容易断线，建议先 `apt-get install -y tmux && tmux new -s install` 再运行；断了用 `tmux attach -t install` 回来。

如果 Hostinger 控制台里开了防火墙，放行中转端口（默认 `443/tcp`）。

### 3. 开始使用

1. 打开 `https://你的面板域名`，用打印出来的密码登录。
2. **机场订阅** → 粘贴订阅链接 → 添加（纯 IP 的订阅地址会自动勾选「忽略证书错误」）。
3. **住宅IP** → 把供应商发来的内容整段粘贴 → 加入IP库，几秒后能看到出口IP和地区。
4. **链路** → 新建链路 → 填设备名、账号，选住宅IP → 创建。
5. 用 iPhone 小火箭扫右侧二维码 → 选中节点 → 打开开关。
6. 按「环境检查」提示，把 iPhone 的时区、语言、地区改成和住宅IP一致。

### Telegram 通知（可选）

1. Telegram 里找 **@BotFather** → 发 `/newbot` → 按提示起名，拿到 Token。
2. 安装时填入；或之后在 `/opt/luodi/.env` 里加 `TG_BOT_TOKEN=你的Token`，再执行 `bash deploy/install.sh`。
3. 面板「设置」页会显示一个绑定码，在 Telegram 里给机器人发 `/bind 绑定码`。只有绑定的这个聊天能收到通知、下命令。

### 日常维护

```bash
cd /opt/luodi
docker compose ps -a                  # 看两个容器是否都是 Up
docker compose logs -f panel          # 看日志
git pull && bash deploy/install.sh    # 更新（新镜像编译成功后才替换正在运行的容器）
```

**备份** `/opt/luodi/.env` 和 `/opt/luodi/data/`。`.env` 里的 `SECRET_KEY` 丢了，数据库里加密的密码就解不开了。

### 常见问题

| 现象 | 原因和处理 |
|---|---|
| 打开面板显示 Cloudflare **502 Bad gateway** | 隧道连上了但找不到面板：Cloudflare 里「服务 URL」没填成 `http://panel:8080`。 |
| 显示 **1033** | 隧道没连上：`.env` 里的 `TUNNEL_TOKEN` 不完整，改好后运行 `bash deploy/install.sh`。 |
| `docker compose ps` 什么都没有 | 用 `docker compose ps -a` 看。状态是 Created 一般是端口被占用，重新运行 `bash deploy/install.sh` 会自动换端口。 |
| 面板一直 Restarting，日志说「数据目录不可写」 | `cd /opt/luodi && chown -R 10001:10001 data && docker compose restart panel` |
| 手机扫码后连不上 | Hostinger 控制台防火墙没放行中转端口（`.env` 里的 `RELAY_PORT`，默认 443/tcp）。 |

## 配置项（`.env`）

| 变量 | 说明 | 默认 |
|---|---|---|
| `PANEL_PASSWORD` | 面板登录密码，至少 8 位 | 必填 |
| `SECRET_KEY` | 加密密钥，`openssl rand -base64 32` | 不填则自动生成到 `data/secret.key` |
| `PUBLIC_URL` | 面板网址，用来生成订阅链接 | 按请求推断 |
| `TUNNEL_TOKEN` | Cloudflare Tunnel Token | 必填 |
| `RELAY_HOST` | 手机连接中转用的地址（VPS 公网 IP，不能是走 Cloudflare 代理的域名） | 空则不开启中转 |
| `RELAY_PORT` | 中转对外端口 | `443` |
| `REALITY_SNI` | Reality 伪装的网站 | `www.microsoft.com` |
| `TG_BOT_TOKEN` | Telegram 机器人 Token | 空则不开启通知 |
| `PANEL_LOCAL_PORT` | 面板在本机的调试端口（隧道不经过它），安装脚本自动挑选 | `18080` |
| `SYNC_EVERY` / `CHECK_EVERY` | 订阅同步 / 链路检测间隔 | `30m` / `15m` |
| `CHECK_URLS` | 返回出口IP的检测接口，逗号分隔 | ipify、ifconfig.me、icanhazip |

## 住宅IP智能识别

实现在 [`web/parse-proxy.js`](web/parse-proxy.js)。支持的格式：

| 格式 | 例子 |
|---|---|
| IP:端口:账号:密码 | `203.0.113.24:1080:user:pass` |
| 账号:密码:IP:端口 | `user:pass:203.0.113.24:1080` |
| 账号:密码@IP:端口 | `user:pass@203.0.113.24:1080` |
| IP:端口@账号:密码 | `203.0.113.24:1080@user:pass` |
| 协议链接 | `socks5://user:pass@host:1080#备注`、`http://…` |
| 小火箭 Base64 分享链接 | `socks://dXNlcjpwYXNz…#备注` |
| 空格 / Tab / 竖线分隔 | `203.0.113.24 1080 user pass` |
| 供应商的带标签文本 | `IP地址：… 端口：… 账号：… 密码：… 到期时间：…` |

密码里带 `:`、`@`、`#` 也能正确拆分；批量粘贴自动去重；在 IP 输入框里直接粘贴整串也会自动拆开。

## 工作原理

- **检测**：服务器临时启动一个 mihomo，按手机上一模一样的路径（机场节点 → 住宅IP，或直连住宅IP）访问出口IP接口。第一次检测到的出口IP记为基准，之后出口变化会报警；确认供应商换线后可以点「以当前出口为准」。
- **中转**：VPS 上常驻一个 mihomo，对外开一个 VLESS Reality 入站。每条链路是其中一个用户，按用户把流量送到它绑定的住宅IP；认不出的连接一律拒绝。增删链路会热加载，不影响其他手机。
- **流量计量**：中转发往住宅IP的连接先经过面板进程里的一个本机转发端口（每台手机一个），逐字节计数，所以短连接也不会漏。在线来源IP从中转的连接列表每 2 秒采样一次。Stash 路径不经过 VPS，统计不到。
- **Stash 订阅**：住宅IP节点写 `dialer-proxy: 前置组`，前置组是与住宅IP同国家的机场节点（fallback 自动切换），「社媒出口」组里只有住宅IP一个节点。

## 开发

```bash
go test ./...                                   # 单元测试
MIHOMO_BIN=/path/to/mihomo go test ./...        # 连同 mihomo 集成测试（链路、中转、配置校验）
node --test prototype/parse-proxy.test.mjs      # 住宅IP识别器测试
PANEL_PASSWORD=devpass123 DATA_DIR=./data MIHOMO_BIN=/path/to/mihomo go run ./cmd/panel
```

目录结构：

```
cmd/panel         程序入口
internal/sub      机场订阅拉取与解析
internal/gen      Stash / Clash Meta 配置、分享链接生成
internal/check    用 mihomo 检测链路和节点
internal/relay    中转（VLESS Reality）与流量计量转发
internal/stats    在线状态、来源IP、流量统计
internal/notify   Telegram 机器人
internal/app      检测调度、告警规则、备份
internal/store    SQLite 存储与加密字段
internal/server   网页与接口
web               前端（原生 JS，编译进二进制）
prototype         最早的交互原型
deploy            安装脚本
```
