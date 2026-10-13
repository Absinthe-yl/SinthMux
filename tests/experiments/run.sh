#!/usr/bin/env bash
# Experiments for the thesis evaluation. Everything runs isolated, like
# tests/e2e/tier1.sh: a test Hub on 127.0.0.1:18093 against
# SINTHMUX_TEST_DATABASE_URL (its schema is wiped), one real connector with its
# own HOME, config and tmux socket, and an SSH baseline server running tmux on
# the same socket. Your own tmux, Hub and LaunchAgent are untouched.
#
#   Browser ──netproxy :18094──▶ Hub :18093 ◀──netproxy :18094── connector ─▶ tmux (-L smexp)
#   SSH client ──netproxy :18098──▶ sshbase :18096 ─────────────────────────▶ tmux (-L smexp)
#
# netproxy adds delay and simulates outages per traffic class; see netproxy/main.go.
#
#   tests/experiments/run.sh                       # all experiments
#   tests/experiments/run.sh access latency        # a subset
#   QUICK=1 tests/experiments/run.sh               # few trials, to check the setup
#   OUT=/tmp/x ARGS="-scenarios device-switch -n 3" tests/experiments/run.sh recovery
#
# Results: tests/experiments/results/*.csv plus env.txt (machine and versions).
set -euo pipefail
cd "$(dirname "$0")/../.."
set -a; source .env; set +a
: "${SINTHMUX_TEST_DATABASE_URL:?set SINTHMUX_TEST_DATABASE_URL in .env}"
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost
unset SINTHMUX_CONNECTOR_DEVICE_TOKEN SINTHMUX_CONNECTOR_DEVICE_ID SINTHMUX_CONNECTOR_HUB_URL SINTHMUX_CONNECTOR_NAME SINTHMUX_DEV_TOKEN http_proxy https_proxy HTTP_PROXY HTTPS_PROXY all_proxy ALL_PROXY

EXPERIMENTS=("$@")
[[ ${#EXPERIMENTS[@]} == 0 ]] && EXPERIMENTS=(access latency recovery authz revoke load)
OUT=${OUT:-$PWD/tests/experiments/results}
mkdir -p "$OUT"

T=$(mktemp -d /tmp/smexp.XXXXXX)
HUB=http://127.0.0.1:18093
PROXY=http://127.0.0.1:18094
SOCK=smexp
cleanup() {
  [[ "${KEEP:-}" == 1 ]] && { echo "KEEP=1: left running in $T"; return; }
  [[ -f "$T/pids" ]] && while read -r pid; do kill "$pid" 2>/dev/null || true; done < "$T/pids"
  pkill -f "$T/" 2>/dev/null || true
  tmux -L "$SOCK" kill-server 2>/dev/null || true
  rm -rf "$T"
}
trap cleanup EXIT

echo "== build"
go build -o "$T/hub" ./apps/hub
go build -o "$T/conn" ./apps/connector
go build -o "$T/netproxy" ./tests/experiments/netproxy
go build -o "$T/sshbase" ./tests/experiments/sshbase
go build -o "$T/bench" ./tests/experiments/bench

echo "== hub $HUB (public origin $PROXY)"
psql "$SINTHMUX_TEST_DATABASE_URL" -qc 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;' >/dev/null
SINTHMUX_DATABASE_URL="$SINTHMUX_TEST_DATABASE_URL" SINTHMUX_PUBLIC_URL=$PROXY SINTHMUX_HUB_ADDR=127.0.0.1:18093 "$T/hub" > "$T/hub.log" 2>&1 &
HUB_PID=$!
echo $HUB_PID >> "$T/pids"
for _ in $(seq 1 50); do curl -fs $HUB/health >/dev/null && break; sleep 0.2; done
OWNER_TOKEN=$(SINTHMUX_DATABASE_URL="$SINTHMUX_TEST_DATABASE_URL" "$T/hub" bootstrap-token "Exp Owner")
"$T/netproxy" -classify -listen 127.0.0.1:18094 -target 127.0.0.1:18093 -control 127.0.0.1:18095 > "$T/netproxy-hub.log" 2>&1 &
echo $! >> "$T/pids"

echo "== ssh baseline"
ssh-keygen -q -t ed25519 -N '' -f "$T/hostkey"
ssh-keygen -q -t ed25519 -N '' -f "$T/clientkey"
"$T/sshbase" -listen 127.0.0.1:18096 -hostkey "$T/hostkey" -authorized "$T/clientkey.pub" > "$T/sshbase.log" 2>&1 &
echo $! >> "$T/pids"
"$T/netproxy" -listen 127.0.0.1:18098 -target 127.0.0.1:18096 -control 127.0.0.1:18097 > "$T/netproxy-ssh.log" 2>&1 &
echo $! >> "$T/pids"
for _ in $(seq 1 50); do curl -fs http://127.0.0.1:18095/status >/dev/null && curl -fs http://127.0.0.1:18097/status >/dev/null && break; sleep 0.2; done

echo "== device"
J=$T/jar
api() { curl -fsS -b "$J" -c "$J" -H "Origin: $PROXY" -H "X-Sinthmux-CSRF: ${CSRF:-}" -H 'Content-Type: application/json' "$@"; }
api -X POST -d "{\"token\":\"$OWNER_TOKEN\"}" $PROXY/api/v1/auth/token >/dev/null
ME=$(api $PROXY/api/v1/auth/me)
CSRF=$(python3 -c 'import sys,json;print(json.load(sys.stdin)["csrf"])' <<<"$ME")
SPACE=$(python3 -c 'import sys,json;print(json.load(sys.stdin)["spaces"][0]["id"])' <<<"$ME")
CODE=$(api -X POST -d '{"name":"Exp Mac"}' $PROXY/api/v1/spaces/$SPACE/device-pairings | python3 -c 'import sys,json;print(json.load(sys.stdin)["code"])')
SINTHMUX_CONNECTOR_CONFIG="$T/conn.json" "$T/conn" pair --hub $PROXY --code "$CODE" >/dev/null
mkdir -p "$T/home"
# A plain bash prompt makes keystroke echo measurable and identical for both systems.
printf 'PS1="$ "\nunset PROMPT_COMMAND\nbind "set enable-bracketed-paste off" 2>/dev/null\n' > "$T/home/.bashrc"
cat > "$T/home/.tmux.conf" <<EOF
set -g default-command "/bin/bash --rcfile $T/home/.bashrc -i"
set -g status off
set -g history-limit 2000
EOF
# The connector runs under a restart loop (bench restarts it after device outages).
cat > "$T/supervise" <<EOF
#!/bin/bash
while true; do
  HOME="$T/home" SHELL=/bin/bash SINTHMUX_CONNECTOR_CONFIG="$T/conn.json" SINTHMUX_TMUX_SOCKET=$SOCK SINTHMUX_CONNECTOR_LOG="$T/conn.log" "$T/conn"
  sleep 1
done
EOF
chmod +x "$T/supervise"
"$T/supervise" > /dev/null 2>&1 &
echo $! >> "$T/pids"
for _ in $(seq 1 50); do
  online=$(api $PROXY/api/v1/devices | python3 -c 'import sys,json;print(sum(d["status"]=="online" for d in json.load(sys.stdin)["devices"]))')
  [[ $online == 1 ]] && break
  sleep 0.2
done
[[ $online == 1 ]] || { echo "device did not come online"; tail -5 "$T/conn.log"; exit 1; }

{
  echo "date: $(date '+%Y-%m-%d %H:%M:%S %z')"
  echo "commit: $(git rev-parse --short HEAD)$(git diff --quiet || echo ' (dirty)')"
  echo "cpu: $(sysctl -n machdep.cpu.brand_string), $(sysctl -n hw.ncpu) cores, $(( $(sysctl -n hw.memsize) / 1073741824 )) GiB"
  echo "os: $(sw_vers -productName) $(sw_vers -productVersion)"
  echo "go: $(go version | awk '{print $3}')"
  echo "tmux: $(tmux -V)"
  echo "postgres: $(psql "$SINTHMUX_TEST_DATABASE_URL" -Atc 'show server_version')"
} > "$OUT/env.txt"
cat "$OUT/env.txt"

export EXP_HUB=$HUB EXP_PROXY=$PROXY EXP_HUB_CTL=http://127.0.0.1:18095 EXP_SSH=127.0.0.1:18098 EXP_SSH_CTL=http://127.0.0.1:18097
export EXP_SSH_KEY="$T/clientkey" EXP_SOCK=$SOCK EXP_DEVICE="Exp Mac" EXP_OWNER_TOKEN=$OWNER_TOKEN EXP_OUT=$OUT
export EXP_HUB_PID=$HUB_PID EXP_CONN_BIN="$T/conn" EXP_DIR=$T

for experiment in "${EXPERIMENTS[@]}"; do
  echo "== $experiment"
  args=()
  if [[ "${QUICK:-}" == 1 ]]; then
    case $experiment in
      access) args=(-n 10) ;;
      latency) args=(-n 40) ;;
      recovery) args=(-n ${RN:-1} -scenarios ${SCEN:-switch,down-5s,device-switch}) ;;
      authz) args=(-n 2) ;;
      revoke) args=(-n 3) ;;
      load) args=(-levels 10x0,10x2 -idle 5s -active 5s) ;;
    esac
  else
    case $experiment in
      load) args=(-stream 8192) ;;
    esac
  fi
  # ARGS overrides the arguments, e.g. ARGS="-scenarios device-switch -n 3".
  [[ -n "${ARGS:-}" ]] && read -r -a args <<<"$ARGS"
  "$T/bench" "$experiment" ${args[@]+"${args[@]}"}
done
echo "== done; results in $OUT"
