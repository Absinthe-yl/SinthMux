#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ $# -gt 1 ]]; then
  printf '用法：%s [Compose 环境文件]\n' "$0" >&2
  exit 1
fi
compose_env="${1:-$root_dir/deploy/.env.local}"
if [[ ! -f "$compose_env" ]]; then
  printf '环境文件不存在：%s。首次部署请运行 scripts/docker-up.sh。\n' "$compose_env" >&2
  exit 1
fi

# Only parse our key; never source the env file or print its credentials.
proxy_secret_found=false
while IFS= read -r line || [[ -n "$line" ]]; do
  line="${line%$'\r'}"
  if [[ "$line" == SINTHMUX_PROXY_SECRET=* ]]; then
    if [[ "$proxy_secret_found" == true || ! "$line" =~ ^SINTHMUX_PROXY_SECRET=[0-9a-fA-F]{64}$ ]]; then
      printf '请在 %s 中保留一个有效的 SINTHMUX_PROXY_SECRET（openssl rand -hex 32）。\n' "$compose_env" >&2
      exit 1
    fi
    proxy_secret_found=true
  fi
done < "$compose_env"
if [[ "$proxy_secret_found" == false ]]; then
  umask 077
  proxy_secret="$(openssl rand -hex 32)"
  printf '\nSINTHMUX_PROXY_SECRET=%s\n' "$proxy_secret" >> "$compose_env"
fi
