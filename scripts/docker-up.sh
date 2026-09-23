#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root_dir"

for program in docker curl openssl; do
  if ! command -v "$program" >/dev/null 2>&1; then
    printf '缺少 %s。此安装方式需要 Docker Compose、curl 和 openssl。\n' "$program" >&2
    exit 1
  fi
done
if ! docker compose version >/dev/null 2>&1; then
  printf 'Docker Compose 不可用，请先安装并启动 Docker。\n' >&2
  exit 1
fi

umask 077
compose_env=deploy/.env.local
if [[ ! -f "$compose_env" ]]; then
  printf 'SINTHMUX_COMPOSE_DB_PASSWORD=%s\n' "$(openssl rand -hex 24)" > "$compose_env"
fi
compose=(docker compose --env-file "$compose_env" -f deploy/docker-compose.yml)

"${compose[@]}" up -d --build
for _ in {1..40}; do
  if curl --silent --fail http://127.0.0.1:5173/health >/dev/null; then break; fi
  sleep 1
done
if ! curl --silent --fail http://127.0.0.1:5173/health >/dev/null; then
  "${compose[@]}" logs --tail=40 >&2
  printf 'SinthMux 启动失败，请查看上面的日志。\n' >&2
  exit 1
fi

bootstrap=deploy/.bootstrap-token
if [[ ! -f "$bootstrap" ]]; then
  if token="$("${compose[@]}" run --rm -T hub bootstrap-token Owner)" && [[ "$token" =~ ^smt_[0-9a-f]{32}_[0-9a-f]{64}$ ]]; then
    printf '%s\n' "$token" > "$bootstrap"
    printf '首次登录令牌已保存到 %s（24 小时内有效）。\n' "$bootstrap"
  else
    printf '未生成首次令牌。若数据库已有用户，请使用此前创建的登录令牌；否则检查 Hub 日志。\n'
  fi
fi
printf 'SinthMux 已启动：http://127.0.0.1:5173/\n'
printf '创建浏览器登录后，在页面添加设备，再为本机设备代理配置设备令牌。\n'
