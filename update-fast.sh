#!/usr/bin/env bash
# 快速更新：不做源码编译，用预编译好的静态二进制重建镜像（约 5~10 秒）。
#
#   ./update-fast.sh                          # 用同目录的 codex2api-workbuddy-linux-amd64
#   ./update-fast.sh /path/to/new-binary      # 或指定新二进制（会自动拷进构建上下文）
#
# 与 update.sh 的区别：update.sh 走多阶段构建（npm build + go build，3~8 分钟）；
# 这个脚本只 COPY 二进制，前提是你已经从开发机拿到 linux/amd64 的二进制。
set -euo pipefail
cd "$(dirname "$0")"

BIN="${1:-./codex2api-workbuddy-linux-amd64}"
COMPOSE="-f docker-compose.sqlite.yml -f docker-compose.sqlite.build.yml"

[ -f "$BIN" ] || { echo "找不到二进制：$BIN"; exit 1; }
if [ "$(cd "$(dirname "$BIN")" && pwd)/$(basename "$BIN")" != "$PWD/codex2api-workbuddy-linux-amd64" ]; then
  cp -f "$BIN" ./codex2api-workbuddy-linux-amd64
fi
chmod +x ./codex2api-workbuddy-linux-amd64

echo "==> 1/4 备份 SQLite（命名卷）"
TS=$(date +%Y%m%d_%H%M%S)
docker run --rm -v codex2api-sqlite_sqlite-data:/data -v "$PWD":/backup alpine \
  sh -c "cp /data/codex2api.db /backup/codex2api.db.bak_$TS" 2>/dev/null \
  && echo "   已备份 codex2api.db.bak_$TS" || echo "   跳过备份（卷不存在）"

echo "==> 2/4 构建镜像（只 COPY 二进制）"
APP_DOCKERFILE=Dockerfile.bin docker compose $COMPOSE build codex2api

echo "==> 3/4 重启容器"
docker compose $COMPOSE up -d --no-build codex2api

echo "==> 4/4 健康检查"
for i in $(seq 1 15); do
  if out=$(docker compose $COMPOSE exec -T codex2api wget -qO- "http://127.0.0.1:${CODEX_PORT:-8080}/health" 2>/dev/null); then
    echo "   $out"
    break
  fi
  sleep 2
done
docker image prune -f >/dev/null
echo "==> 完成"
