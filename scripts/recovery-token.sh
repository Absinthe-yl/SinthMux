#!/usr/bin/env bash
# Issue a 24-hour login token for a user who lost all of theirs.
#   ./scripts/recovery-token.sh            # one user: print a token; several: list them
#   ./scripts/recovery-token.sh <user-id>  # token for that user
# Run on the Hub host from the repository root. Uses the same Docker Compose
# setup as scripts/docker-up.sh (public overlay included when present).
set -euo pipefail
cd "$(dirname "$0")/.."
env_file=deploy/.env.local
if [[ ! -f "$env_file" ]]; then
  printf '找不到 %s；请在运行 Hub 的机器上、仓库根目录执行。\n' "$env_file" >&2
  exit 1
fi
compose=(docker compose --env-file "$env_file" -f deploy/docker-compose.yml)
if grep -q '^SINTHMUX_SITE=' "$env_file" && [[ -f deploy/docker-compose.public.yml ]]; then
  compose+=(-f deploy/docker-compose.public.yml)
fi
if ! docker info >/dev/null 2>&1; then compose=(sudo "${compose[@]}"); fi
token="$("${compose[@]}" exec -T hub /sinthmux-hub recovery-token "$@")"
printf '\n恢复令牌（24 小时内有效，仅显示这一次）：\n\n  %s\n\n在登录页选择“使用令牌登录”粘贴它；登录后到“登录令牌”新建一个长期令牌并妥善保存。\n' "$token"
