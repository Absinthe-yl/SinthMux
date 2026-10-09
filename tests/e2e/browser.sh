#!/usr/bin/env bash
# Browser end-to-end checks (B1–B14 in docs/TIER1_DESIGN.md), driven with
# agent-browser against the stack started by tests/e2e/tier1.sh. WEB is the
# stall proxy in front of Vite, so B10 can freeze the live terminal connection.
#
# Required env (set by tier1.sh): OWNER_TOKEN VIEWER_TOKEN WEB CONTROL HUB SOCK E2E_DIR
set -uo pipefail
cd "$(dirname "$0")/../.."
: "${OWNER_TOKEN:?}" "${VIEWER_TOKEN:?}" "${WEB:?}" "${CONTROL:?}" "${SOCK:?}" "${E2E_DIR:?}"
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost

PAGE=$WEB
AB=(agent-browser --session sme2e)
failures=0
pass() { printf '[PASS] %-4s %s%s\n' "$1" "$2" "${3:+  ($3)}"; }
fail() { printf '[FAIL] %-4s %s%s\n' "$1" "$2" "${3:+  ($3)}"; failures=$((failures + 1)); }
result() { if [[ $1 == ok ]]; then pass "$2" "$3" "${4:-}"; else fail "$2" "$3" "${4:-}"; fi; }

trap '"${AB[@]}" close >/dev/null 2>&1' EXIT

ab() { "${AB[@]}" "$@" 2>&1; }
js() { ab eval "$1" | tail -1 | sed -e 's/^"//' -e 's/"$//'; }
# wait_js EXPR SECONDS: poll until the expression is truthy.
wait_js() {
  local deadline=$((SECONDS + $2)) value
  while ((SECONDS < deadline)); do
    value=$(js "Boolean($1)")
    [[ $value == true ]] && return 0
    sleep 0.3
  done
  return 1
}
term_text='(document.querySelector(".xterm-rows")?.innerText ?? "")'
state='(document.querySelector("[data-testid=terminal-state]")?.dataset.state ?? "")'
state_text='(document.querySelector("[data-testid=terminal-state]")?.textContent ?? "")'
# run_in_term CMD: type a command into the focused xterm.
run_in_term() { ab focus '.xterm-helper-textarea' >/dev/null; ab keyboard type "$1" >/dev/null; ab press Enter >/dev/null; }

login() {
  ab open "$PAGE/" >/dev/null
  wait_js "document.querySelector('.login-token-toggle, #login-token')" 15 || return 1
  js "document.querySelector('.login-token-toggle')?.click(), true" >/dev/null
  wait_js "document.querySelector('#login-token')" 5 || return 1
  ab fill '#login-token' "$1" >/dev/null
  js "document.querySelector('#login-token').form.requestSubmit(), true" >/dev/null
  wait_js 'document.querySelector("[data-testid^=device-]")' 15
}

api() { curl -fsS -b "$E2E_DIR/jar" -H "Origin: $WEB" -H "X-Sinthmux-CSRF: $CSRF" -H 'Content-Type: application/json' "$@"; }
CSRF=$(curl -fsS -b "$E2E_DIR/jar" $WEB/api/v1/auth/me | python3 -c 'import sys,json;print(json.load(sys.stdin)["csrf"])')
DEV=$(api $WEB/api/v1/devices | python3 -c 'import sys,json;print([d["id"] for d in json.load(sys.stdin)["devices"] if d["name"]=="E2E Mac"][0])')
for s in b-main b-pin e2e-b1 e2e-b2 e2e-b3; do
  api -X DELETE "$WEB/api/v1/devices/$DEV/sessions/$s" >/dev/null 2>&1
  api -X POST -d "{\"name\":\"$s\"}" "$WEB/api/v1/devices/$DEV/sessions" >/dev/null
done
tmux_cmd() { HOME="$E2E_DIR/home" tmux -L "$SOCK" "$@"; }

ab set viewport 1280 860 >/dev/null
login "$OWNER_TOKEN" && pass "--" "浏览器登录" || { fail "--" "浏览器登录" "$(ab get url)"; exit 1; }

open_session() { # device-name session
  js "localStorage.length >= 0" >/dev/null
  if [[ $(js "Boolean(document.querySelector('[data-testid=session-$2]'))") != true ]]; then
    ab click "[data-testid='toggle-$1']" >/dev/null
    wait_js "document.querySelector('[data-testid=session-$2]')" 10
  fi
  ab click "[data-testid=session-$2] .session-open" >/dev/null
  wait_js "$state === 'live'" 15
}

# ---- B8 expanded state survives reload (device opened by open_session below) ----
open_session "E2E Mac" b-main && pass "--" "打开终端" || fail "--" "打开终端" "$(js "$state_text")"

# ---- B1 paste an image file ----
js "(() => { const bytes = new Uint8Array(4096); for (let i = 0; i < bytes.length; i++) bytes[i] = i % 256; window.__png = bytes; const dt = new DataTransfer(); dt.items.add(new File([bytes], 'shot.png', { type: 'image/png' })); document.querySelector('.xterm-helper-textarea').dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true })); return true; })()" >/dev/null
if wait_js "$term_text.includes('/.sinthmux/uploads/')" 15; then
  path=$(js "($term_text.match(/\\/\\S*\\.sinthmux\\/uploads\\/[0-9]{8}-[0-9]{6}-[0-9a-f]{6}-shot\\.png/) || [''])[0]")
  ab press Control+u >/dev/null
  size=$(stat -f %z "$path" 2>/dev/null || echo missing)
  [[ $size == 4096 ]] && pass B1 "粘贴图片：上传后路径插入终端，文件大小一致" "$path" || fail B1 "粘贴图片" "path=$path size=$size"
else
  fail B1 "粘贴图片" "$(js "$state_text")"
fi

# ---- B2 plain text paste is not uploaded ----
before=$(ls "$E2E_DIR/home/.sinthmux/uploads" | wc -l)
js "(() => { const dt = new DataTransfer(); dt.setData('text/plain', 'echo plain-paste-ok'); document.querySelector('.xterm-helper-textarea').dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true })); return true; })()" >/dev/null
sleep 1.5
after=$(ls "$E2E_DIR/home/.sinthmux/uploads" | wc -l)
if wait_js "$term_text.includes('echo plain-paste-ok')" 5 && [[ $before == "$after" ]]; then pass B2 "纯文本粘贴按文本输入，不触发上传"; else fail B2 "纯文本粘贴" "uploads $before->$after"; fi
ab press Control+u >/dev/null

# ---- B3 mixed clipboard (picture + text) pastes the text ----
before=$(ls "$E2E_DIR/home/.sinthmux/uploads" | wc -l)
js "(() => { const dt = new DataTransfer(); dt.items.add(new File([new Uint8Array(10)], 'image.png', { type: 'image/png' })); dt.setData('text/plain', 'echo mixed-text-wins'); document.querySelector('.xterm-helper-textarea').dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true })); return true; })()" >/dev/null
sleep 1.5
after=$(ls "$E2E_DIR/home/.sinthmux/uploads" | wc -l)
if wait_js "$term_text.includes('echo mixed-text-wins')" 5 && [[ $before == "$after" ]]; then pass B3 "图文混合剪贴板按文本粘贴"; else fail B3 "图文混合剪贴板" "uploads $before->$after"; fi
ab press Control+u >/dev/null

# ---- B4 upload button ----
head -c 300000 /dev/urandom > "$E2E_DIR/b4.bin"
want=$(shasum -a 256 "$E2E_DIR/b4.bin" | cut -c1-64)
ab upload '[data-testid=terminal-upload-input]' "$E2E_DIR/b4.bin" >/dev/null
if wait_js "/uploads\\/[0-9-]+[0-9a-f]{6}-b4\\.bin/.test($term_text)" 15; then
  path=$(js "($term_text.match(/\\/\\S*\\.sinthmux\\/uploads\\/[0-9]{8}-[0-9]{6}-[0-9a-f]{6}-b4\\.bin/) || [''])[0]")
  got=$(shasum -a 256 "$path" 2>/dev/null | cut -c1-64)
  [[ $got == "$want" ]] && pass B4 "上传按钮：文件哈希一致" || fail B4 "上传按钮" "path=$path"
else
  fail B4 "上传按钮" "$(js "$state_text")"
fi
ab press Control+u >/dev/null

# ---- B5 export ----
run_in_term 'for i in $(seq 1 300); do echo "B5-$i 导出"; done'
wait_js "$term_text.includes('B5-300 导出')" 10
# Capture the Blob the page downloads.
js "(() => { window.__exported = null; const orig = URL.createObjectURL; URL.createObjectURL = (blob) => { blob.text().then((t) => { window.__exported = t; }); return orig.call(URL, blob); }; return true; })()" >/dev/null
ab click '[data-testid=terminal-export]' >/dev/null
ab click '[data-testid=export-all]' >/dev/null
if wait_js "window.__exported && window.__exported.includes('B5-1 导出') && window.__exported.includes('B5-300 导出')" 15; then
  pass B5 "导出全部历史并下载 .txt" "$(js "document.querySelector('[data-testid=history-download]')?.download ?? ''")"
else
  fail B5 "导出" "$(js "$state_text")"
fi

# ---- B6 notification badge, tab title, cleared on open ----
ab click '.terminal-back' >/dev/null
wait_js "document.querySelector('[data-testid^=device-]')" 5
tmux_cmd send-keys -t '=b-pin:' "$E2E_DIR/conn notify --color red 'B6 构建完成'" Enter
if wait_js "document.querySelector('[data-testid=notify-badge]')?.textContent.includes('1') && document.title.startsWith('(1)')" 12; then
  open_session "E2E Mac" b-pin
  ab click '.terminal-back' >/dev/null
  wait_js "!document.querySelector('[data-testid=notify-badge]') && !document.title.startsWith('(')" 10 && pass B6 "提醒角标与标题计数出现，打开会话后清除" || fail B6 "打开后未清除" "$(js "document.title")"
else
  fail B6 "提醒角标未出现" "$(js "document.title")"
fi

# ---- B7 pin survives reload and opens the session ----
ab click '[data-testid=pin-b-pin]' >/dev/null
ab reload >/dev/null
if wait_js "document.querySelector('[data-testid=pinned-section] [data-testid=pinned-b-pin]')" 10; then
  # B8: the device panel stays expanded after reload.
  wait_js "document.querySelector('[data-testid=session-b-main]')" 10 && pass B8 "设备展开状态刷新后保留" || fail B8 "展开状态未保留"
  wait_js "!document.querySelector('[data-testid=pinned-open-b-pin]').disabled" 10
  ab click '[data-testid=pinned-open-b-pin]' >/dev/null
  wait_js "$state === 'live'" 15 && pass B7 "置顶刷新后保留，置顶区可直接打开" || fail B7 "置顶区打开失败"
  ab click '.terminal-back' >/dev/null
else
  fail B7 "置顶未保留"
  fail B8 "未验证（置顶失败）"
fi

# ---- B9 batch close ----
ab click '[data-testid=pin-e2e-b1]' >/dev/null
ab click '[data-testid=batch-toggle]' >/dev/null
for s in e2e-b1 e2e-b2 e2e-b3; do ab click "[data-testid=batch-$s]" >/dev/null; done
ab click '[data-testid=batch-close]' >/dev/null
ab click 'dialog button[type=submit]' >/dev/null
sleep 2
left=$(api "$WEB/api/v1/devices/$DEV/sessions" | python3 -c 'import sys,json;print(",".join(s["name"] for s in json.load(sys.stdin)["sessions"] if s["name"].startswith("e2e-b")))')
pinned=$(js "localStorage.length && Object.keys(localStorage).filter((k) => k.endsWith(':pinned')).map((k) => localStorage.getItem(k)).join('')")
[[ -z $left && $pinned != *e2e-b1* ]] && pass B9 "批量关闭 3 个会话并同步取消置顶" || fail B9 "批量关闭" "left=$left pinned=$pinned"

# Record every terminal state change in the page: a quick reconnect can go
# live -> retrying -> connecting -> live between two polls.
watch_states() {
  js "(() => { const el = document.querySelector('[data-testid=terminal-state]'); window.__states = [{ s: el.dataset.state, t: Date.now() }]; window.__stateObserver?.disconnect(); window.__stateObserver = new MutationObserver(() => { const s = el.dataset.state; if (window.__states.at(-1).s !== s) window.__states.push({ s, t: Date.now() }); }); window.__stateObserver.observe(el, { attributes: true, attributeFilter: ['data-state'] }); return true; })()" >/dev/null
}
# recovered_within SECONDS: the page left 'live' and came back to it; prints the time taken.
recovered() { js "(() => { const a = window.__states, lost = a.findIndex((x, i) => i > 0 && x.s !== 'live'); if (lost < 0) return ''; const back = a.findIndex((x, i) => i > lost && x.s === 'live'); return back < 0 ? '' : ((a[back].t - a[0].t) / 1000).toFixed(1) + 's ' + a.map((x) => x.s).join('>'); })()"; }

# ---- B10 silent connection is detected and replaced ----
open_session "E2E Mac" b-main
sleep 1
watch_states
froze=$(curl -fs $CONTROL/freeze)
if wait_js "(() => { const a = window.__states, i = a.findIndex((x, j) => j > 0 && x.s !== 'live'); return i >= 0 && a.slice(i).some((x) => x.s === 'live'); })()" 30; then
  took=$(recovered)
  run_in_term 'echo after-stall-$((6*7))'
  secs=${took%%s*}
  if wait_js "$term_text.includes('after-stall-42')" 10 && (( ${secs%.*} <= 22 )); then pass B10 "静默断线被发现并自动重连，输入恢复" "$took"; else fail B10 "重连后输入或耗时" "$took"; fi
else
  fail B10 "静默断线未恢复" "$(js "JSON.stringify(window.__states)") | $froze"
fi

# ---- B11 coming back online probes at once ----
sleep 1
watch_states
froze=$(curl -fs $CONTROL/freeze)
js "window.dispatchEvent(new Event('online'))" >/dev/null
if wait_js "(() => { const a = window.__states, i = a.findIndex((x, j) => j > 0 && x.s !== 'live'); return i >= 0 && a.slice(i).some((x) => x.s === 'live'); })()" 15; then
  took=$(recovered); secs=${took%%s*}
  (( ${secs%.*} <= 7 )) && pass B11 "网络恢复事件触发立即探测并在 5 s 内重连" "$took" || fail B11 "恢复过慢" "$took"
else
  fail B11 "online 事件后未恢复" "$(js "JSON.stringify(window.__states)") | $froze"
fi

# ---- B12 a stuck ticket request times out and a late answer is ignored ----
sleep 1
js "(() => { window.__sockets = 0; const Original = window.__OrigWS ?? WebSocket; window.__OrigWS = Original; window.WebSocket = class extends Original { constructor(...a) { super(...a); window.__sockets++; } }; const realFetch = window.__origFetch ?? window.fetch; window.__origFetch = realFetch; let hung = false; window.fetch = (url, init) => { if (!hung && String(url).endsWith('/ticket')) { hung = true; return new Promise((_, reject) => init?.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')))); } return realFetch(url, init); }; return true; })()" >/dev/null
watch_states
froze=$(curl -fs $CONTROL/freeze)
js "window.dispatchEvent(new Event('online'))" >/dev/null
if wait_js "(() => { const a = window.__states, i = a.findIndex((x, j) => j > 0 && x.s !== 'live'); return i >= 0 && a.slice(i).some((x) => x.s === 'live'); })()" 30; then
  took=$(recovered); secs=${took%%s*}; sockets=$(js "window.__sockets")
  # One socket for the attempt after the timed-out ticket; none for the hung one.
  [[ $sockets == 1 ]] && (( ${secs%.*} >= 8 && ${secs%.*} <= 16 )) && pass B12 "票据请求卡住 8 s 后超时重试，只建立一个新连接" "$took sockets=$sockets" || fail B12 "票据超时" "$took sockets=$sockets"
else
  fail B12 "票据卡住后未恢复" "$(js "JSON.stringify(window.__states)") | $froze"
fi
js "(() => { window.fetch = window.__origFetch; window.WebSocket = window.__OrigWS; return true; })()" >/dev/null

# ---- B13 real end stops retrying ----
tmux_cmd kill-session -t '=b-main'
if wait_js "$state === 'stopped'" 10 && wait_js "document.querySelector('[data-testid=terminal-reconnect]')" 3; then
  sleep 3
  [[ $(js "$state") == stopped ]] && pass B13 "会话被关闭后停止重连并显示立即重连按钮" "$(js "$state_text")" || fail B13 "仍在重连"
else
  fail B13 "未进入结束状态" "$(js "$state_text")"
fi
ab click '.terminal-back' >/dev/null

ab screenshot "${SHOTS:-$E2E_DIR}/desktop-home.png" >/dev/null

# ---- B14 mobile viewport: upload and badge still usable ----
api -X POST -d '{"name":"b-mobile"}' "$WEB/api/v1/devices/$DEV/sessions" >/dev/null
ab set viewport 390 844 >/dev/null
ab reload >/dev/null
tmux_cmd send-keys -t '=b-mobile:' "$E2E_DIR/conn notify --color green 'B14'" Enter
badge=no; wait_js "document.querySelector('[data-testid=notify-badge]')?.getBoundingClientRect().width > 0" 12 && badge=yes
open_session "E2E Mac" b-mobile
visible=$(js "(() => { const r = document.querySelector('[data-testid=terminal-upload]')?.getBoundingClientRect(); return Boolean(r && r.width > 0 && r.right <= innerWidth); })()")
[[ $badge == yes && $visible == true ]] && pass B14 "手机视口下角标与上传按钮可见" || fail B14 "手机视口" "badge=$badge upload=$visible"
ab screenshot "${SHOTS:-$E2E_DIR}/mobile-terminal.png" >/dev/null
ab click '.terminal-back' >/dev/null
ab set viewport 1280 860 >/dev/null

# ---- viewer cannot batch-close ----
ab cookies clear >/dev/null
login "$VIEWER_TOKEN"
ab click "[data-testid='toggle-E2E Mac']" >/dev/null 2>&1
sleep 1.5
[[ $(js "Boolean(document.querySelector('[data-testid=batch-toggle]'))") == false ]] && pass B9v "viewer 看不到批量管理" || fail B9v "viewer 能看到批量管理"

echo
if ((failures > 0)); then echo "$failures browser check(s) failed"; exit 1; fi
echo "all browser checks passed"
