#!/usr/bin/env bash
# 服务器自助更新：从 GitHub Release 下载最新预编译二进制，校验后重建镜像并重启。
# 相比源码构建（10~15 分钟），这条路约 15 秒，且不占服务器 CPU/内存。
#
#   ./update.sh                # 更新到最新构建
#   ./update.sh --check        # 只看有没有新版本，不动服务
#
# 二进制由 .github/workflows/server-binary.yml 在每次 push 到 main 时构建，
# 发布到滚动 Release `build-latest`。
set -euo pipefail
cd "$(dirname "$0")"

REPO="${REPO:-9160994/codex2api}"
TAG="${TAG:-build-latest}"
ASSET="codex2api-linux-amd64.tar.gz"
PATH_PART="https://github.com/${REPO}/releases/download/${TAG}/${ASSET}"
# 国内直连 GitHub Release 常年 10~20KB/s（24MB 要半小时），按顺序试加速镜像，
# 谁先跑到 150KB/s 以上就用谁；直连放最后兜底。
MIRRORS=("https://ghfast.top/" "https://gh-proxy.com/" "")
BIN="./codex2api-workbuddy-linux-amd64"
COMPOSE="-f docker-compose.sqlite.yml -f docker-compose.sqlite.build.yml"

check_only=0
[ "${1:-}" = "--check" ] && check_only=1

echo "==> 1/5 查询最新构建"
info=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/tags/${TAG}" 2>/dev/null || true)
if [ -z "$info" ]; then
  echo "    无法读取 Release（仓库私有或 Actions 还没跑过）。"
  exit 1
fi
remote_note=$(printf '%s' "$info" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('body','').replace(chr(10),' '))" 2>/dev/null || echo "未知")
echo "    最新构建：${remote_note}"

if [ "$check_only" = "1" ]; then
  echo "    当前运行：$(curl -s "http://127.0.0.1:${CODEX_PORT:-18080}/health" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("build_version","?"))' 2>/dev/null || echo '?')"
  exit 0
fi

echo "==> 2/5 备份 SQLite（命名卷）"
TS=$(date +%Y%m%d_%H%M%S)
docker run --rm -v codex2api-sqlite_sqlite-data:/data -v "$PWD":/backup alpine \
  sh -c "cp /data/codex2api.db /backup/codex2api.db.bak_$TS" 2>/dev/null \
  && echo "    已备份 codex2api.db.bak_$TS" || echo "    跳过备份（卷不存在）"

echo "==> 3/5 下载并校验二进制"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
downloaded=0
for prefix in "${MIRRORS[@]}"; do
  url="${prefix}${PATH_PART}"
  echo "    尝试下载：${prefix:-直连 GitHub}"
  if curl -fsSL -C - --max-time 900 --speed-limit 50000 --speed-time 15        -o "$tmp/$ASSET" "$url"; then
    downloaded=1
    echo "    下载完成：$(du -h "$tmp/$ASSET" | cut -f1)（来源 ${prefix:-直连}）"
    break
  fi
  echo "    该来源过慢或失败，换下一个"
done
if [ "$downloaded" != "1" ]; then
  # 最后兜底：不限速慢慢拉直连，宁可慢也不让更新失败（24MB 最坏几分钟）
  echo "    各镜像都不理想，改用直连不限速续传（可能较慢）"
  if curl -fsSL -C - --max-time 1800 -o "$tmp/$ASSET" "${PATH_PART}"; then
    downloaded=1
    echo "    下载完成：$(du -h "$tmp/$ASSET" | cut -f1)（直连）"
  fi
fi
if [ "$downloaded" != "1" ]; then
  echo "    所有下载源都失败，终止（未改动服务）"
  exit 1
fi
for prefix in "${MIRRORS[@]}"; do
  if curl -fsSL --max-time 60 -o "$tmp/$ASSET.sha256" "${prefix}${PATH_PART}.sha256" 2>/dev/null; then break; fi
done
if [ -s "$tmp/$ASSET.sha256" ]; then
  ( cd "$tmp" && sha256sum -c "$ASSET.sha256" ) || { echo "    校验失败，终止"; exit 1; }
  echo "    sha256 校验通过"
else
  echo "    未取到校验文件，跳过（不影响使用）"
fi
tar xzf "$tmp/$ASSET" -C "$tmp"
mv -f "$tmp/codex2api-linux-amd64" "$BIN"
chmod +x "$BIN"
echo "    已就位：$(grep -ao 'ci-[0-9a-f]\{7\}' "$BIN" | head -1 || echo "$BIN")"

echo "==> 4/5 重建镜像并重启"
APP_DOCKERFILE=Dockerfile.bin docker compose $COMPOSE build codex2api
docker compose $COMPOSE up -d --no-build codex2api

echo "==> 5/5 健康检查"
for i in $(seq 1 20); do
  if out=$(docker compose $COMPOSE exec -T codex2api wget -qO- "http://127.0.0.1:${CODEX_PORT:-8080}/health" 2>/dev/null); then
    echo "    $out"
    break
  fi
  sleep 2
done
docker image prune -f >/dev/null
echo "==> 完成"
