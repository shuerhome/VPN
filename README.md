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
- **安全**：面板密码登录（带失败次数限制），住宅IP密码、订阅链接、节点参数 AES-256-GCM 加密存储；面板只通过 Cloudflare Tunnel 对外，不开放端口。

## 部署（Debian 13 VPS + Cloudflare 域名）

### 1. 在 Cloudflare 创建隧道

1. 登录 Cloudflare → **Zero Trust** → **Networks** → **Tunnels** → **Create a tunnel**，类型选 **Cloudflared**，起个名字。
2. 安装方式页面里会出现一串 `eyJ...` 开头的 **Token**，复制下来（不用在服务器上按它的命令安装，我们的脚本会用 Docker 跑）。
3. 下一步 **Public Hostname**：
   - Subdomain：例如 `panel`，Domain：选你的域名
   - Service：Type 选 `HTTP`，URL 填 `panel:8080`
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

脚本会问四个问题：面板域名、Tunnel Token、VPS 公网 IP（默认自动检测）、中转端口（默认 443）。然后自动安装 Docker、生成密码和加密密钥、编译并启动。结束时会打印**登录密码**。

如果 Hostinger 控制台里开了防火墙，放行中转端口（默认 `443/tcp`）。

### 3. 开始使用

1. 打开 `https://你的面板域名`，用打印出来的密码登录。
2. **机场订阅** → 粘贴订阅链接 → 添加（纯 IP 的订阅地址会自动勾选「忽略证书错误」）。
3. **住宅IP** → 把供应商发来的内容整段粘贴 → 加入IP库，几秒后能看到出口IP和地区。
4. **链路** → 新建链路 → 填设备名、账号，选住宅IP → 创建。
5. 用 iPhone 小火箭扫右侧二维码 → 选中节点 → 打开开关。
6. 按「环境检查」提示，把 iPhone 的时区、语言、地区改成和住宅IP一致。

### 日常维护

```bash
cd /opt/luodi
docker compose logs -f panel          # 看日志
git pull && docker compose up -d --build   # 更新
```

**备份** `/opt/luodi/.env` 和 `/opt/luodi/data/`。`.env` 里的 `SECRET_KEY` 丢了，数据库里加密的密码就解不开了。

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
internal/relay    中转（VLESS Reality）
internal/store    SQLite 存储与加密字段
internal/server   网页与接口
web               前端（原生 JS，编译进二进制）
prototype         最早的交互原型
deploy            安装脚本
```
