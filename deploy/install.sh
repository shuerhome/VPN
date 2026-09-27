#!/usr/bin/env bash
# 落地链路台 一键安装（Debian / Ubuntu，root 运行）
# 用法：在仓库目录里执行  bash deploy/install.sh
# 重复运行是安全的：已有的 .env 和 data/ 会保留，只重新构建和启动。
set -euo pipefail

cd "$(dirname "$0")/.."
[ "$(id -u)" = 0 ] || { echo "请用 root 运行：sudo bash deploy/install.sh"; exit 1; }

say() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
warn() { printf '\033[1;31m%s\033[0m\n' "$*"; }

# 端口是否已被占用（包括其他 Docker 容器发布的端口）
port_busy() {
  if command -v ss >/dev/null 2>&1 && ss -Htln "sport = :$1" 2>/dev/null | grep -q .; then return 0; fi
  # 兜底：直接连一下本机端口，连得上就说明有程序在监听
  if (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; then return 0; fi
  docker ps --format '{{.Ports}}' 2>/dev/null | grep -Eq "(^|[ ,])[^ ,]*:$1->" && return 0
  return 1
}
# 占用端口的程序名（查不到时为空）
port_owner() {
  local who
  who=$(ss -Htlnp "sport = :$1" 2>/dev/null | sed -n 's/.*users:(("\([^"]*\)".*/\1/p' | head -1)
  if [ -z "$who" ]; then
    who=$(docker ps --format '{{.Names}} {{.Ports}}' 2>/dev/null | grep -E "[^ ,]*:$1->" | awk '{print "容器 "$1}' | head -1)
  fi
  echo "${who:-其他程序}"
}
first_free() {
  local p
  for p in "$@"; do
    if ! port_busy "$p"; then echo "$p"; return 0; fi
  done
  return 1
}
# 修改或追加 .env 里的一项
set_env() {
  if grep -q "^$1=" .env; then sed -i "s|^$1=.*|$1=$2|" .env; else echo "$1=$2" >> .env; fi
}

# ---------- Docker ----------
if ! command -v docker >/dev/null 2>&1; then
  say "安装 Docker"
  if ! curl -fsSL https://get.docker.com | sh; then
    echo "官方脚本失败，改用系统软件源安装"
    apt-get update
    apt-get install -y docker.io docker-compose
  fi
  systemctl enable --now docker
fi
if docker compose version >/dev/null 2>&1; then
  COMPOSE="docker compose"
elif command -v docker-compose >/dev/null 2>&1; then
  COMPOSE="docker-compose"
else
  apt-get update && apt-get install -y docker-compose-plugin || apt-get install -y docker-compose
  COMPOSE="docker compose"
  docker compose version >/dev/null 2>&1 || COMPOSE="docker-compose"
fi
command -v openssl >/dev/null 2>&1 || apt-get install -y openssl
command -v ss >/dev/null 2>&1 || apt-get install -y iproute2 || true

# 重新运行安装时先停掉面板自己的旧容器，端口检查才准确（数据在 data/ 里，不受影响）
if [ -f .env ]; then
  $COMPOSE down --remove-orphans >/dev/null 2>&1 || true
fi

# ---------- 配置 ----------
if [ ! -f .env ]; then
  say "生成配置 .env"
  read -rp "面板域名（例如 panel.example.com）: " DOMAIN
  DOMAIN=${DOMAIN#https://}; DOMAIN=${DOMAIN#http://}; DOMAIN=${DOMAIN%/}
  read -rp "Cloudflare Tunnel Token: " TUNNEL_TOKEN
  PUBIP=$(curl -fsS4 --max-time 8 https://api.ipify.org || true)
  read -rp "VPS 公网 IP（手机扫码直连用）[${PUBIP}]: " RELAY_HOST
  RELAY_HOST=${RELAY_HOST:-$PUBIP}
  DEFAULT_RELAY=443
  if port_busy 443; then
    DEFAULT_RELAY=$(first_free 8443 2083 2087 2096 9443 10443 || echo 8443)
    echo "443 端口已被 $(port_owner 443) 占用，中转默认改用 ${DEFAULT_RELAY}"
  fi
  read -rp "中转端口 [${DEFAULT_RELAY}]: " RELAY_PORT
  RELAY_PORT=${RELAY_PORT:-$DEFAULT_RELAY}
  read -rp "Telegram 机器人 Token（可选，直接回车跳过，以后可在 .env 里补）: " TG_BOT_TOKEN
  PANEL_PASSWORD=$(openssl rand -base64 24 | tr -d '/+=' | cut -c1-16)
  SECRET_KEY=$(openssl rand -base64 32)
  cat > .env <<ENVFILE
PANEL_PASSWORD=${PANEL_PASSWORD}
SECRET_KEY=${SECRET_KEY}
PUBLIC_URL=https://${DOMAIN}
TUNNEL_TOKEN=${TUNNEL_TOKEN}
RELAY_HOST=${RELAY_HOST}
RELAY_PORT=${RELAY_PORT}
REALITY_SNI=www.microsoft.com
TG_BOT_TOKEN=${TG_BOT_TOKEN}
SYNC_EVERY=30m
CHECK_EVERY=15m
ENVFILE
  chmod 600 .env
  NEW_INSTALL=1
else
  echo ".env 已存在，沿用现有配置"
fi
set -a; . ./.env; set +a
RELAY_PORT=${RELAY_PORT:-443}

# ---------- 端口 ----------
# 面板在本机的调试端口。Cloudflare 隧道走 Docker 内部网络，不经过它，挑一个空闲的就行。
if [ -z "${PANEL_LOCAL_PORT:-}" ] || port_busy "$PANEL_LOCAL_PORT"; then
  PANEL_LOCAL_PORT=$(first_free $(seq 18080 18180)) || { warn "18080-18180 端口都被占用了"; exit 1; }
  set_env PANEL_LOCAL_PORT "$PANEL_LOCAL_PORT"
fi
# 中转端口：手机直接连，必须空闲
while port_busy "$RELAY_PORT"; do
  SUGGEST=$(first_free 8443 2083 2087 2096 9443 10443 || echo "")
  warn "中转端口 ${RELAY_PORT} 已被 $(port_owner "$RELAY_PORT") 占用。"
  if [ -z "${NEW_INSTALL:-}" ]; then echo "（改端口后，已经导入的手机需要重新扫码）"; fi
  read -rp "改用哪个端口 [${SUGGEST}]: " NEWPORT
  RELAY_PORT=${NEWPORT:-$SUGGEST}
  [ -n "$RELAY_PORT" ] || { warn "没有可用端口"; exit 1; }
  set_env RELAY_PORT "$RELAY_PORT"
done

# ---------- 数据目录和防火墙 ----------
mkdir -p data
chown -R 10001:10001 data
chmod 700 data
if command -v ufw >/dev/null 2>&1 && ufw status | grep -q "Status: active"; then
  say "放行中转端口 ${RELAY_PORT}/tcp"
  ufw allow "${RELAY_PORT}/tcp"
fi

# ---------- 启动 ----------
say "构建并启动（第一次要编译几分钟）"
if ! $COMPOSE up -d --build; then
  warn "启动失败，上面是 Docker 的报错。请把最后 30 行截图发给开发者。"
  exit 1
fi

say "等待面板和隧道启动"
PANEL_OK=0
for _ in $(seq 1 60); do
  if curl -fsS -o /dev/null "http://127.0.0.1:${PANEL_LOCAL_PORT}/"; then PANEL_OK=1; break; fi
  sleep 2
done
TUNNEL_OK=0
for _ in $(seq 1 30); do
  if $COMPOSE logs cloudflared 2>&1 | grep -q "Registered tunnel connection"; then TUNNEL_OK=1; break; fi
  sleep 2
done
$COMPOSE ps

FAILED=0
if [ "$PANEL_OK" != 1 ]; then
  warn "面板没有正常启动，最近日志："
  $COMPOSE logs --tail 30 panel || true
  FAILED=1
fi
if [ "$TUNNEL_OK" != 1 ]; then
  warn "Cloudflare 隧道没有连上，最近日志："
  $COMPOSE logs --tail 20 cloudflared || true
  warn "常见原因：Tunnel Token 粘贴不完整。编辑 .env 里的 TUNNEL_TOKEN 后执行：${COMPOSE} up -d"
  FAILED=1
fi

echo
echo "────────────────────────────────────────────"
echo " 面板地址： ${PUBLIC_URL:-}"
echo " 本机访问： http://127.0.0.1:${PANEL_LOCAL_PORT} （SSH 隧道调试用）"
if [ "${NEW_INSTALL:-}" = 1 ]; then
  echo " 登录密码： ${PANEL_PASSWORD}"
  echo " 加密密钥已写入 .env，请把 .env 和 data/ 一起备份。"
else
  echo " 登录密码： 见 .env 里的 PANEL_PASSWORD（cat .env 查看）"
fi
echo " 中转地址： ${RELAY_HOST:-}:${RELAY_PORT}"
echo "   如果 Hostinger 控制台开了防火墙，要放行 ${RELAY_PORT}/tcp。"
echo " Telegram： 在面板「设置」页按提示绑定机器人"
echo " 查看日志： ${COMPOSE} logs -f panel"
echo " 更新版本： git pull && bash deploy/install.sh"
echo "────────────────────────────────────────────"
if [ "$FAILED" = 1 ]; then
  warn "安装没有完全成功，请把上面红色的提示和日志截图发给开发者。"
  exit 1
fi
echo "安装完成。"
