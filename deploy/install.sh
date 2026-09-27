#!/usr/bin/env bash
# 落地链路台 一键安装（Debian / Ubuntu，root 运行）
# 用法：在仓库目录里执行  bash deploy/install.sh
set -euo pipefail

cd "$(dirname "$0")/.."
[ "$(id -u)" = 0 ] || { echo "请用 root 运行：sudo bash deploy/install.sh"; exit 1; }

say() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }

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

# ---------- 配置 ----------
if [ ! -f .env ]; then
  say "生成配置 .env"
  read -rp "面板域名（例如 panel.example.com）: " DOMAIN
  DOMAIN=${DOMAIN#https://}; DOMAIN=${DOMAIN#http://}; DOMAIN=${DOMAIN%/}
  read -rp "Cloudflare Tunnel Token: " TUNNEL_TOKEN
  PUBIP=$(curl -fsS4 --max-time 8 https://api.ipify.org || true)
  read -rp "VPS 公网 IP（手机扫码直连用）[${PUBIP}]: " RELAY_HOST
  RELAY_HOST=${RELAY_HOST:-$PUBIP}
  read -rp "中转端口 [443]: " RELAY_PORT
  RELAY_PORT=${RELAY_PORT:-443}
  PANEL_PASSWORD=$(openssl rand -base64 24 | tr -d '/+=' | cut -c1-16)
  SECRET_KEY=$(openssl rand -base64 32)
  cat > .env <<EOF
PANEL_PASSWORD=${PANEL_PASSWORD}
SECRET_KEY=${SECRET_KEY}
PUBLIC_URL=https://${DOMAIN}
TUNNEL_TOKEN=${TUNNEL_TOKEN}
RELAY_HOST=${RELAY_HOST}
RELAY_PORT=${RELAY_PORT}
REALITY_SNI=www.microsoft.com
SYNC_EVERY=30m
CHECK_EVERY=15m
EOF
  chmod 600 .env
  NEW_INSTALL=1
else
  echo ".env 已存在，沿用现有配置"
fi
set -a; . ./.env; set +a

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
$COMPOSE up -d --build

say "等待面板启动"
for _ in $(seq 1 60); do
  if curl -fsS -o /dev/null http://127.0.0.1:8080/; then break; fi
  sleep 2
done
$COMPOSE ps

cat <<EOF

────────────────────────────────────────────
 面板地址： ${PUBLIC_URL}
 本机访问： http://127.0.0.1:8080 （SSH 隧道调试用）
EOF
if [ "${NEW_INSTALL:-}" = 1 ]; then
  cat <<EOF
 登录密码： ${PANEL_PASSWORD}
 加密密钥已写入 .env，请把 .env 和 data/ 一起备份。
EOF
fi
cat <<EOF
 中转地址： ${RELAY_HOST}:${RELAY_PORT}
   如果 Hostinger 面板里开了防火墙，要放行 ${RELAY_PORT}/tcp。
 查看日志： ${COMPOSE} logs -f panel
 更新版本： git pull && ${COMPOSE} up -d --build
────────────────────────────────────────────
EOF
