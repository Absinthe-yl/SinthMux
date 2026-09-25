#!/usr/bin/env bash
set -euo pipefail

hub=''
code=''
repair=false
while (($#)); do
  case "$1" in
    --hub)
      if (($# < 2)) || [[ -z "$2" ]]; then printf '缺少 --hub 地址。\n' >&2; exit 2; fi
      hub="$2"; shift 2 ;;
    --code)
      if (($# < 2)) || [[ -z "$2" ]]; then printf '缺少 --code 配对码。\n' >&2; exit 2; fi
      code="$2"; shift 2 ;;
    --repair) repair=true; shift ;;
    *) printf '未知参数：%s\n' "$1" >&2; exit 2 ;;
  esac
done
if [[ -z "$hub" || ( -z "$code" && "$repair" != true ) ]]; then
  printf '用法：install-connector.sh --hub <HTTPS 地址> --code <配对码>；已配对设备可用 --hub <地址> --repair 更新\n' >&2
  exit 2
fi
if [[ "$hub" != https://* && "$hub" != http://127.0.0.1:* && "$hub" != http://localhost:* ]]; then
  printf 'Hub 必须使用 HTTPS；HTTP 仅支持本机。\n' >&2
  exit 2
fi
if ! command -v curl >/dev/null 2>&1; then printf '请先安装 curl。\n' >&2; exit 1; fi
if ! command -v tmux >/dev/null 2>&1; then
  printf '请先安装 tmux 后重试：macOS 运行 brew install tmux；Ubuntu/Debian 运行 sudo apt install tmux。\n' >&2
  exit 1
fi
tmux_bin="$(command -v tmux)"
if [[ "$tmux_bin" != /* ]]; then
  tmux_bin="$(cd "$(dirname "$tmux_bin")" && pwd -P)/$(basename "$tmux_bin")"
fi
tmux_dir="$(dirname "$tmux_bin")"

case "$(uname -s)" in Darwin) platform=darwin ;; Linux) platform=linux ;; *) printf '仅支持 macOS 和 Linux。\n' >&2; exit 1 ;; esac
case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; x86_64|amd64) arch=amd64 ;; *) printf '不支持此 CPU 架构。\n' >&2; exit 1 ;; esac

install_dir="$HOME/.local/bin"
mkdir -p "$install_dir"
tmp="$(mktemp "$install_dir/.sinthmux-connector.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
if [[ "$hub" == https://* ]]; then
  curl -fsSL --proto '=https' --proto-redir '=https' "${hub%/}/downloads/sinthmux-connector-${platform}-${arch}" -o "$tmp"
else
  curl -fsSL "${hub%/}/downloads/sinthmux-connector-${platform}-${arch}" -o "$tmp"
fi
chmod 700 "$tmp"
"$tmp" pair --hub "$hub" --code "$code" --reuse-existing
mv -f "$tmp" "$install_dir/sinthmux-connector"
trap - EXIT
if ! tmux list-sessions >/dev/null 2>&1; then
  tmux new-session -d -s sinthmux
fi

if [[ "$platform" == linux ]] && command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1; then
  service_dir="$HOME/.config/systemd/user"
  mkdir -p "$service_dir"
  cat > "$service_dir/sinthmux-connector.service" <<EOF
[Unit]
Description=SinthMux device connector
After=network-online.target
[Service]
ExecStart=%h/.local/bin/sinthmux-connector
Environment="SINTHMUX_TMUX_BIN=$tmux_bin"
Environment="PATH=$tmux_dir:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
Restart=always
RestartSec=5
[Install]
WantedBy=default.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable sinthmux-connector.service
  systemctl --user restart sinthmux-connector.service
elif [[ "$platform" == darwin ]]; then
  plist="$HOME/Library/LaunchAgents/com.sinthmux.connector.plist"
  log_dir="$HOME/.config/sinthmux"
  mkdir -p "$(dirname "$plist")"
  mkdir -p "$log_dir"
  escaped_path="${install_dir//&/&amp;}"
  escaped_path="${escaped_path//</&lt;}"
  escaped_tmux_dir="${tmux_dir//&/&amp;}"
  escaped_tmux_dir="${escaped_tmux_dir//</&lt;}"
  escaped_tmux_bin="${tmux_bin//&/&amp;}"
  escaped_tmux_bin="${escaped_tmux_bin//</&lt;}"
  escaped_log_dir="${log_dir//&/&amp;}"
  escaped_log_dir="${escaped_log_dir//</&lt;}"
  cat > "$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>com.sinthmux.connector</string>
<key>ProgramArguments</key><array><string>$escaped_path/sinthmux-connector</string></array>
<key>EnvironmentVariables</key><dict>
<key>PATH</key><string>$escaped_tmux_dir:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
<key>SINTHMUX_TMUX_BIN</key><string>$escaped_tmux_bin</string>
</dict>
<key>StandardOutPath</key><string>$escaped_log_dir/connector.log</string>
<key>StandardErrorPath</key><string>$escaped_log_dir/connector.log</string>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
</dict></plist>
EOF
  launchctl bootout "gui/$(id -u)/com.sinthmux.connector" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$plist"
else
  mkdir -p "$HOME/.config/sinthmux"
  pid_file="$HOME/.config/sinthmux/connector.pid"
  if [[ -f "$pid_file" ]]; then
    old_pid=''
    read -r old_pid < "$pid_file" || true
    if [[ "$old_pid" =~ ^[0-9]+$ ]] && kill -0 "$old_pid" 2>/dev/null && [[ "$(ps -p "$old_pid" -o args=)" == "$install_dir/sinthmux-connector" ]]; then
      kill "$old_pid"
    fi
  fi
  SINTHMUX_TMUX_BIN="$tmux_bin" PATH="$tmux_dir:$PATH" nohup "$install_dir/sinthmux-connector" > "$HOME/.config/sinthmux/connector.log" 2>&1 < /dev/null &
  printf '%s\n' "$!" > "$pid_file"
  printf '已在后台启动；此系统未检测到用户服务管理器，重启后需再次启动 ~/.local/bin/sinthmux-connector。\n'
fi
printf '设备代理已启动。返回 SinthMux 网页确认设备在线；如果显示离线，请检查 ~/.config/sinthmux/connector.log（macOS/Linux 后台模式）或 journalctl --user -u sinthmux-connector（systemd）。\n'
