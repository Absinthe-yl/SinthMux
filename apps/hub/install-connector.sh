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
case "$(uname -s)" in
  Darwin) platform=darwin ;;
  Linux) platform=linux ;;
  MINGW*|MSYS*|CYGWIN*)
    printf 'Windows 请在 SinthMux 网页的“添加设备”中选择 Windows，复制 PowerShell 命令，在 PowerShell 中运行。\n' >&2
    exit 1 ;;
  *) printf '仅支持 macOS、Linux 和 Windows 的 WSL。\n' >&2; exit 1 ;;
esac
case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; x86_64|amd64) arch=amd64 ;; *) printf '不支持此 CPU 架构。\n' >&2; exit 1 ;; esac
if ! command -v curl >/dev/null 2>&1; then printf '请先安装 curl。\n' >&2; exit 1; fi

hub_get() { # hub_get <path> <output>
  if [[ "$hub" == https://* ]]; then
    curl -fsSL --proto '=https' --proto-redir '=https' "${hub%/}$1" -o "$2"
  else
    curl -fsSL "${hub%/}$1" -o "$2"
  fi
}
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

install_dir="$HOME/.local/bin"
mkdir -p "$install_dir"

# tmux comes from, in order: (1) the system, (2) the Hub's bundle installed
# next to the connector, (3) the system package manager.
usable_tmux() { [[ -n "$1" && -x "$1" ]] && "$1" -V >/dev/null 2>&1; }

# The Hub bundle is a self-contained tmux; SHA256SUMS guards against a
# truncated download. Returns non-zero when the Hub has no bundle.
install_bundled_tmux() {
  local name="tmux-linux-$arch" target="$install_dir/tmux" tmp_tmux sums expected
  [[ "$platform" == darwin ]] && name="tmux-darwin-universal"
  tmp_tmux="$(mktemp "$install_dir/.tmux.XXXXXX")"
  sums="$(mktemp)"
  if ! hub_get "/downloads/$name" "$tmp_tmux" 2>/dev/null || ! hub_get "/downloads/SHA256SUMS" "$sums" 2>/dev/null; then
    rm -f "$tmp_tmux" "$sums"; return 1
  fi
  expected="$(awk -v f="$name" '$2 == f || $2 == "*" f { print $1 }' "$sums")"
  rm -f "$sums"
  if [[ -z "$expected" || "$(sha256 "$tmp_tmux")" != "$expected" ]]; then
    printf 'Hub 提供的 tmux 校验失败，改用系统包管理器。\n' >&2
    rm -f "$tmp_tmux"; return 1
  fi
  chmod 755 "$tmp_tmux"
  if [[ "$platform" == darwin ]]; then xattr -d com.apple.quarantine "$tmp_tmux" 2>/dev/null || true; fi
  if ! usable_tmux "$tmp_tmux"; then rm -f "$tmp_tmux"; return 1; fi
  mv -f "$tmp_tmux" "$target"
}

# Package managers must not read stdin: the script is usually piped into bash
# and they would swallow the rest of it. sudo still prompts on the tty.
as_root() {
  if [[ "$(id -u)" == 0 ]]; then "$@" < /dev/null; else sudo "$@" < /dev/null; fi
}

install_packaged_tmux() {
  if [[ "$platform" == darwin ]]; then
    local brew='' candidate
    for candidate in "$(command -v brew 2>/dev/null || true)" /opt/homebrew/bin/brew /usr/local/bin/brew; do
      if [[ -n "$candidate" && -x "$candidate" ]]; then brew="$candidate"; break; fi
    done
    if [[ -n "$brew" ]]; then
      printf '正在用 Homebrew 安装 tmux…\n'
      HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_ENV_HINTS=1 "$brew" install tmux < /dev/null || return 1
      PATH="$(dirname "$brew"):$PATH"; return 0
    fi
    if command -v port >/dev/null 2>&1; then
      printf '正在用 MacPorts 安装 tmux（需要本机管理员密码）…\n'
      as_root port -N install tmux; return
    fi
    printf '本机没有 Homebrew，无法自动安装 tmux。\n' >&2
    return 1
  fi
  if [[ "$(id -u)" != 0 ]] && ! command -v sudo >/dev/null 2>&1; then
    printf '当前用户无法使用 sudo，无法用包管理器安装 tmux。\n' >&2
    return 1
  fi
  printf '正在用系统包管理器安装 tmux（可能需要输入本机管理员密码）…\n'
  if command -v apt-get >/dev/null 2>&1; then
    as_root env DEBIAN_FRONTEND=noninteractive apt-get update -qq && as_root env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq tmux
  elif command -v dnf >/dev/null 2>&1; then as_root dnf install -y -q tmux
  elif command -v yum >/dev/null 2>&1; then as_root yum install -y -q tmux
  elif command -v zypper >/dev/null 2>&1; then as_root zypper --non-interactive install tmux
  elif command -v pacman >/dev/null 2>&1; then as_root pacman -S --noconfirm --needed tmux
  elif command -v apk >/dev/null 2>&1; then as_root apk add tmux
  else printf '未识别本机的包管理器。\n' >&2; return 1
  fi
}

tmux_bin="$(command -v tmux 2>/dev/null || true)"
if usable_tmux "$tmux_bin"; then
  :
elif usable_tmux "$install_dir/tmux"; then
  tmux_bin="$install_dir/tmux"
else
  printf '未检测到 tmux，正在从 Hub 获取…\n'
  if install_bundled_tmux; then
    tmux_bin="$install_dir/tmux"
  elif install_packaged_tmux && tmux_bin="$(command -v tmux 2>/dev/null || true)" && usable_tmux "$tmux_bin"; then
    :
  else
    printf 'tmux 安装未完成。请手动安装（macOS：brew install tmux；Ubuntu/Debian：sudo apt install tmux；Fedora：sudo dnf install tmux），再重新运行这条接入命令。\n' >&2
    exit 1
  fi
  printf '已就绪：%s（%s）\n' "$("$tmux_bin" -V)" "$tmux_bin"
fi
if [[ "$tmux_bin" != /* ]]; then
  tmux_bin="$(cd "$(dirname "$tmux_bin")" && pwd -P)/$(basename "$tmux_bin")"
fi
tmux_dir="$(dirname "$tmux_bin")"

tmp="$(mktemp "$install_dir/.sinthmux-connector.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
hub_get "/downloads/sinthmux-connector-${platform}-${arch}" "$tmp"
chmod 700 "$tmp"
"$tmp" pair --hub "$hub" --code "$code" --reuse-existing
mv -f "$tmp" "$install_dir/sinthmux-connector"
trap - EXIT
if ! "$tmux_bin" list-sessions >/dev/null 2>&1; then
  "$tmux_bin" new-session -d -s sinthmux -c "$HOME"
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
