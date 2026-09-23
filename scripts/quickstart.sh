#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root_dir"

for program in go node npm tmux curl nc; do
  if ! command -v "$program" >/dev/null 2>&1; then
    printf '缺少 %s；请先安装 Go 1.25+、Node.js 20+、tmux 和 curl/nc。\n' "$program" >&2
    exit 1
  fi
done

for port in 8090 5173; do
  if nc -z -w 1 127.0.0.1 "$port" >/dev/null 2>&1; then
    printf '本机端口 %s 已被占用，请先停止现有服务。\n' "$port" >&2
    exit 1
  fi
done

if [[ -f .env ]]; then
  set -a
  # The local .env is user-owned and ignored by Git.
  source .env
  set +a
fi
export SINTHMUX_HUB_ADDR=127.0.0.1:8090
export SINTHMUX_CONNECTOR_HUB_URL=ws://127.0.0.1:8090/ws/v1/connectors/connect

umask 077
mkdir -p .run/bin .run/log

printf '构建 Hub 和设备代理…\n'
go build -o .run/bin/sinthmux-hub ./apps/hub
go build -o .run/bin/sinthmux-connector ./apps/connector
if [[ ! -x apps/web/node_modules/.bin/vite ]]; then
  printf '安装 Web 依赖…\n'
  npm --prefix apps/web ci
fi

hub_pid=''
connector_pid=''
web_pid=''
cleanup() {
  trap - EXIT INT TERM
  for pid in "$web_pid" "$connector_pid" "$hub_pid"; do
    if [[ -n "$pid" ]]; then kill "$pid" 2>/dev/null || true; fi
  done
  for pid in "$web_pid" "$connector_pid" "$hub_pid"; do
    if [[ -n "$pid" ]]; then wait "$pid" 2>/dev/null || true; fi
  done
}
trap cleanup EXIT INT TERM

.run/bin/sinthmux-hub > .run/log/hub.log 2>&1 &
hub_pid=$!
for _ in {1..30}; do
  if curl --silent --fail http://127.0.0.1:8090/health >/dev/null; then break; fi
  if ! kill -0 "$hub_pid" 2>/dev/null; then
    cat .run/log/hub.log >&2
    exit 1
  fi
  sleep 1
done
if ! curl --silent --fail http://127.0.0.1:8090/health >/dev/null; then
  cat .run/log/hub.log >&2
  exit 1
fi

.run/bin/sinthmux-connector > .run/log/connector.log 2>&1 &
connector_pid=$!
(cd apps/web && exec node node_modules/vite/bin/vite.js --host 127.0.0.1) > .run/log/web.log 2>&1 &
web_pid=$!
for _ in {1..30}; do
  if curl --silent --fail http://127.0.0.1:5173/ >/dev/null; then break; fi
  if ! kill -0 "$web_pid" 2>/dev/null; then
    cat .run/log/web.log >&2
    exit 1
  fi
  sleep 1
done
if ! curl --silent --fail http://127.0.0.1:5173/ >/dev/null; then
  cat .run/log/web.log >&2
  exit 1
fi

printf 'SinthMux 已启动：http://127.0.0.1:5173/\n'
printf '日志在 .run/log/；按 Ctrl+C 停止服务，tmux 会话会继续运行。\n'
while true; do
  for pid in "$hub_pid" "$connector_pid" "$web_pid"; do
    if ! kill -0 "$pid" 2>/dev/null; then
      printf '有服务意外退出，请查看 .run/log/ 中的日志。\n' >&2
      exit 1
    fi
  done
  sleep 1
done
