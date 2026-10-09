#!/usr/bin/env bash
# Local end-to-end run for the tier-1 features (upload, export, notifications,
# heartbeat, old-connector compatibility). Everything is isolated: a test Hub on
# 127.0.0.1:18092 against SINTHMUX_TEST_DATABASE_URL, a connector with its own
# HOME, config and tmux socket, and a Vite dev server on 127.0.0.1:15173 behind a
# freezable proxy on :15174 for the browser checks (tests/e2e/browser.sh). Your own tmux and LaunchAgent are untouched.
#
#   tests/e2e/tier1.sh                 # protocol checks + browser checks
#   BROWSER=0 tests/e2e/tier1.sh       # protocol checks only
#   PROTOCOL=0 tests/e2e/tier1.sh      # browser checks only
set -euo pipefail
cd "$(dirname "$0")/../.."
set -a; source .env; set +a
: "${SINTHMUX_TEST_DATABASE_URL:?set SINTHMUX_TEST_DATABASE_URL in .env}"
# .env may carry development connector settings; the test connectors must use
# their paired config files instead.
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost
unset SINTHMUX_CONNECTOR_DEVICE_TOKEN SINTHMUX_CONNECTOR_DEVICE_ID SINTHMUX_CONNECTOR_HUB_URL SINTHMUX_CONNECTOR_NAME SINTHMUX_DEV_TOKEN

T=$(mktemp -d /tmp/sme2e.XXXXXX)
HUB=http://127.0.0.1:18092
# Clients use the stall proxy's origin (it forwards to Vite on :15173, which
# proxies to the Hub). The Hub checks Origin against SINTHMUX_PUBLIC_URL, so
# every client must share this one origin.
VITE=127.0.0.1:15173
WEB=http://127.0.0.1:15174
CONTROL=http://127.0.0.1:15175
SOCK=sme2e
J=$T/jar
export E2E_DIR=$T
cleanup() {
  [[ "${KEEP:-}" == 1 ]] && { echo "KEEP=1: left running in $T"; return; }
  [[ -f "$T/pids" ]] && while read -r pid; do kill "$pid" 2>/dev/null || true; done < "$T/pids"
  pkill -f "$T/supervise" 2>/dev/null || true
  pkill -f "$T/conn" 2>/dev/null || true
  pkill -f "$T/old-conn" 2>/dev/null || true
  pkill -f "$T/stallproxy" 2>/dev/null || true
  tmux -L "$SOCK" kill-server 2>/dev/null || true
  git worktree remove --force "$T/old-src" 2>/dev/null || true
  rm -rf "$T"
}
trap cleanup EXIT

echo "== build"
go build -o "$T/hub" ./apps/hub
go build -o "$T/conn" ./apps/connector
git worktree add --detach "$T/old-src" df0394f >/dev/null 2>&1
(cd "$T/old-src" && go build -o "$T/old-conn" ./apps/connector)

echo "== hub $HUB"
psql "$SINTHMUX_TEST_DATABASE_URL" -qc 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;' >/dev/null
SINTHMUX_DATABASE_URL="$SINTHMUX_TEST_DATABASE_URL" SINTHMUX_PUBLIC_URL=$WEB SINTHMUX_HUB_ADDR=127.0.0.1:18092 "$T/hub" > "$T/hub.log" 2>&1 &
echo $! >> "$T/pids"
for _ in $(seq 1 50); do curl -fs $HUB/health >/dev/null && break; sleep 0.2; done
OWNER_TOKEN=$(SINTHMUX_DATABASE_URL="$SINTHMUX_TEST_DATABASE_URL" "$T/hub" bootstrap-token "E2E Owner")

echo "== web $WEB"
(cd apps/web && SINTHMUX_HUB_TARGET=$HUB npx vite --host 127.0.0.1 --port 15173 --strictPort > "$T/web.log" 2>&1) &
echo $! >> "$T/pids"
go build -o "$T/stallproxy" ./tests/e2e/stallproxy
"$T/stallproxy" -listen 127.0.0.1:15174 -target $VITE -control 127.0.0.1:15175 > "$T/stallproxy.log" 2>&1 &
echo $! >> "$T/pids"
for _ in $(seq 1 100); do curl -fs $WEB/health >/dev/null && break; sleep 0.2; done

# Browsers reach the Hub through the web origin; the test clients do the same
# so Origin checks and the dev proxy are exercised like in real use.
api() { curl -fsS -b "$J" -c "$J" -H "Origin: $WEB" -H "X-Sinthmux-CSRF: ${CSRF:-}" -H 'Content-Type: application/json' "$@"; }
api -X POST -d "{\"token\":\"$OWNER_TOKEN\"}" $HUB/api/v1/auth/token >/dev/null
ME=$(api $HUB/api/v1/auth/me)
CSRF=$(python3 -c 'import sys,json;print(json.load(sys.stdin)["csrf"])' <<<"$ME")
SPACE=$(python3 -c 'import sys,json;print(json.load(sys.stdin)["spaces"][0]["id"])' <<<"$ME")
VIEWER_TOKEN=$(api -X POST -d '{"name":"E2E Viewer","role":"viewer"}' $HUB/api/v1/spaces/$SPACE/members | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')

pair() { # name config binary
  local code
  code=$(api -X POST -d "{\"name\":\"$1\"}" $HUB/api/v1/spaces/$SPACE/device-pairings | python3 -c 'import sys,json;print(json.load(sys.stdin)["code"])')
  SINTHMUX_CONNECTOR_CONFIG=$2 "$3" pair --hub $HUB --code "$code" >/dev/null
}

echo "== devices"
mkdir -p "$T/home"
pair "E2E Mac" "$T/conn.json" "$T/conn"
pair "E2E Old" "$T/old.json" "$T/old-conn"
# The connector runs under a restart loop so N6 can restart it from inside tmux.
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
HOME="$T/home" SHELL=/bin/bash SINTHMUX_CONNECTOR_CONFIG="$T/old.json" SINTHMUX_TMUX_SOCKET=${SOCK}old "$T/old-conn" > "$T/old.log" 2>&1 &
echo $! >> "$T/pids"
online=0
for _ in $(seq 1 50); do
  online=$(api $HUB/api/v1/devices | python3 -c 'import sys,json;print(sum(d["status"]=="online" for d in json.load(sys.stdin)["devices"]))')
  [[ $online == 2 ]] && break
  sleep 0.2
done
[[ $online == 2 ]] || { echo "devices did not come online"; tail -3 "$T/conn.log" "$T/old.log"; exit 1; }

echo "== C1 old connector"
OLD=$(api $HUB/api/v1/devices | python3 -c 'import sys,json;print([d["id"] for d in json.load(sys.stdin)["devices"] if d["name"]=="E2E Old"][0])')
OLD_CAPS=$(api $HUB/api/v1/devices | python3 -c 'import sys,json;print(",".join([d for d in json.load(sys.stdin)["devices"] if d["name"]=="E2E Old"][0]["capabilities"]))')
c1_upload=$(curl -s -o /dev/null -w '%{http_code}' -b "$J" -H "Origin: $WEB" -H "X-Sinthmux-CSRF: $CSRF" -H 'Content-Type: application/octet-stream' --data-binary x "$HUB/api/v1/devices/$OLD/uploads?name=x")
api -X POST -d '{"name":"c1"}' $HUB/api/v1/devices/$OLD/sessions >/dev/null
c1_export=$(curl -s -o /dev/null -w '%{http_code}' -b "$J" "$HUB/api/v1/devices/$OLD/sessions/c1/scrollback")
c1_ticket=$(curl -s -o /dev/null -w '%{http_code}' -b "$J" -H "Origin: $WEB" -H "X-Sinthmux-CSRF: $CSRF" -X POST "$HUB/api/v1/devices/$OLD/sessions/c1/ticket")
if [[ $c1_upload == 501 && $c1_export == 501 && $c1_ticket == 200 && $OLD_CAPS != *file.upload* ]]; then
  echo "[PASS] C1  老设备代理：无新能力位，上传/导出 501，终端票据仍 200  (caps=$OLD_CAPS)"
else
  echo "[FAIL] C1  老设备代理 upload=$c1_upload export=$c1_export ticket=$c1_ticket caps=$OLD_CAPS"; C1_FAILED=1
fi
tmux -L ${SOCK}old kill-server 2>/dev/null || true

status=0
if [[ "${PROTOCOL:-1}" == 1 ]]; then
echo "== featurecheck"
SINTHMUX_E2E_TOKEN=$OWNER_TOKEN SINTHMUX_E2E_VIEWER_TOKEN=$VIEWER_TOKEN SINTHMUX_E2E_NOTIFY_BIN="$T/conn" \
  go run ./tests/e2e/featurecheck -hub $WEB -device "E2E Mac" -restart "pkill -f '^$T/conn\$'" || status=1

echo "== P2 silent browser"
go run ./tests/e2e/stallcheck -hub $WEB -token "$OWNER_TOKEN" -device "E2E Mac" -socket $SOCK || status=1

echo "== termcheck regression"
SINTHMUX_E2E_TOKEN=$OWNER_TOKEN go run ./tests/e2e/termcheck -hub $WEB -device "E2E Mac" -session e2e-term > "$T/termcheck.log" 2>&1 && echo "[PASS] termcheck 14 项回归" || { status=1; cat "$T/termcheck.log"; }
fi

if [[ "${BROWSER:-1}" == 1 ]]; then
  echo "== browser"
  OWNER_TOKEN=$OWNER_TOKEN VIEWER_TOKEN=$VIEWER_TOKEN WEB=$WEB CONTROL=$CONTROL HUB=$HUB SOCK=$SOCK E2E_DIR=$T tests/e2e/browser.sh || status=1
fi
[[ -z "${C1_FAILED:-}" ]] || status=1
exit $status
