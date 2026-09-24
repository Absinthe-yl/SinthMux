#!/usr/bin/env bash
set -euo pipefail

hub=''
code=''
while (($#)); do
  case "$1" in
    --hub) hub="${2:-}"; shift 2 ;;
    --code) code="${2:-}"; shift 2 ;;
    *) printf '未知参数：%s\n' "$1" >&2; exit 2 ;;
  esac
done
if [[ -z "$hub" || -z "$code" ]]; then
  printf '用法：install-connector.sh --hub <HTTPS 地址> --code <配对码>\n' >&2
  exit 2
fi
if [[ "$hub" != https://* && "$hub" != http://127.0.0.1:* && "$hub" != http://localhost:* ]]; then
  printf 'Hub 必须使用 HTTPS；HTTP 仅支持本机。\n' >&2
  exit 2
fi
if ! command -v curl >/dev/null 2>&1; then printf '请先安装 curl。\n' >&2; exit 1; fi
if ! command -v tmux >/dev/null 2>&1; then printf '请先安装 tmux 后重试。\n' >&2; exit 1; fi

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
"$tmp" pair --hub "$hub" --code "$code"
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
Restart=always
RestartSec=5
[Install]
WantedBy=default.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable --now sinthmux-connector.service
elif [[ "$platform" == darwin ]]; then
  plist="$HOME/Library/LaunchAgents/com.sinthmux.connector.plist"
  mkdir -p "$(dirname "$plist")"
  escaped_path="${install_dir//&/&amp;}"
  escaped_path="${escaped_path//</&lt;}"
  cat > "$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>com.sinthmux.connector</string>
<key>ProgramArguments</key><array><string>$escaped_path/sinthmux-connector</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
</dict></plist>
EOF
  launchctl bootout "gui/$(id -u)/com.sinthmux.connector" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$plist"
else
  mkdir -p "$HOME/.config/sinthmux"
  nohup "$install_dir/sinthmux-connector" > "$HOME/.config/sinthmux/connector.log" 2>&1 < /dev/null &
  printf '已在后台启动；此系统未检测到用户服务管理器，重启后需再次启动 ~/.local/bin/sinthmux-connector。\n'
fi
printf '设备代理已启动。返回 SinthMux 网页，设备上线后即可创建并打开 tmux 会话。\n'
