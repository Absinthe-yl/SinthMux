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
  printf 'SINTHMUX_COMPOSE_DB_PASSWORD=%s\nSINTHMUX_GITHUB_CLIENT_ID=\nSINTHMUX_GITHUB_CLIENT_SECRET=\n' "$(openssl rand -hex 24)" > "$compose_env"
fi
if { [[ -z "${SINTHMUX_GITHUB_CLIENT_ID:-}" ]] && ! grep -q '^SINTHMUX_GITHUB_CLIENT_ID=.' "$compose_env"; } ||
   { [[ -z "${SINTHMUX_GITHUB_CLIENT_SECRET:-}" ]] && ! grep -q '^SINTHMUX_GITHUB_CLIENT_SECRET=.' "$compose_env"; }; then
  printf '请先在 %s 配置 GitHub OAuth Client ID 和 Client Secret。回调地址：http://127.0.0.1:5173/api/v1/auth/github/callback\n' "$compose_env" >&2
  exit 1
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

printf 'SinthMux 已启动：http://127.0.0.1:5173/\n'
printf '使用 GitHub 登录后，在页面添加设备，再为本机 Agent 配置设备令牌。\n'
