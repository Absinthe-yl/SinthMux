# Connector：设备配对、tmux 与 PTY

## 生命周期

`apps/connector/main.go` 有 `pair` 子命令和常驻连接模式。常驻模式先读环境变量；未设置设备令牌时尝试从用户配置目录的 `sinthmux/connector.json` 加载配对结果（设备私钥、证书，或旧版设备令牌），也可通过 `SINTHMUX_CONNECTOR_CONFIG` 指定路径。连接失败会退避重试；连接后发送 hello，每 15 秒发送一次心跳。设备代理只主动连 Hub，不监听入站端口。

## 设备接入

1. Web 的“添加设备”请求 `POST /api/v1/spaces/{spaceId}/device-pairings`，获得 5 分钟、单次使用的配对码。
2. 页面按系统生成命令：macOS/Linux 下载 `install-connector.sh`，Windows 在 PowerShell 中运行 `install-connector.ps1`。脚本按 CPU 架构下载设备代理，并按“系统 tmux → Hub 随附 tmux（`/downloads/tmux-*`，按 `SHA256SUMS` 校验）→ 包管理器（brew/apt/dnf/yum/zypper/pacman/apk/winget）”准备 tmux。随附版本放在设备代理旁边（`~/.local/bin/tmux`、`%LOCALAPPDATA%\SinthMux\tmux.exe`）。若默认 socket 上已有不同版本的 tmux 服务（客户端会报 “server exited unexpectedly”），脚本改用独立服务 `tmux -L sinthmux`，并通过 `SINTHMUX_TMUX_SOCKET` 传给设备代理。
3. `apps/connector/pair.go` 在本机生成 P-256 私钥和 CSR，调用 `POST /api/v1/connectors/pair` 兑换设备 ID 与 24 小时证书，写入权限为 `0600` 的本机配置文件；私钥不离开设备。安装脚本重跑或使用 `--repair` 时，对同一 Hub 复用仍有效的凭据。已配对其他 Hub（或旧配置无法读取）时，只有带新配对码才会改接：配对成功后旧配置改名为 `connector.json.bak-<时间>` 保留，失败则原配置不动；不带配对码的 `--repair` 不会换 Hub。
4. 安装脚本在无 tmux 会话时创建 `sinthmux` 会话，并尝试用 systemd user service 或 LaunchAgent 托管；其他情况下以后台进程启动。后台服务保存 tmux 的绝对路径和 PATH，更新时重启服务，macOS 日志写入 `~/.config/sinthmux/connector.log`。macOS 上 `launchctl bootstrap` 失败（SSH 或受限终端无法进入 GUI 会话）时回退为后台进程，并提示在本机“终端”中重跑以启用自启。Windows 写入 `HKCU\…\Run` 登录自启，用户环境变量 `SINTHMUX_TMUX_BIN` 指向 psmux；设备代理以 GUI 子系统编译（无控制台窗口），日志在 `%APPDATA%\sinthmux\connector.log`。

入口文件是 `apps/hub/install-connector.sh`、`apps/connector/pair.go` 和 `internal/auth/http.go` / `store.go` 的配对方法。正式模式连接前先取 `POST /api/v1/connectors/nonce`，再在握手头中带上证书和对 nonce 的签名（`apps/connector/credential.go`、`internal/devicecert`）。证书剩余不足 1/3 时用同一私钥续期（`POST /api/v1/connectors/certificate`），连接期间每小时检查一次；过期 30 天内仍可续期。旧版只有设备令牌的配置会在启动或 `--repair` 时自动换成证书。本机开发模式使用 `SINTHMUX_DEV_TOKEN`。

## tmux 与终端

| 文件 | 处理 |
| --- | --- |
| `apps/connector/tmux.go` | `tmux.sessions.list/create/rename/close` RPC；校验会话名，按 `SINTHMUX_TMUX_BIN` → 设备代理同目录的随附 tmux → PATH 定位 tmux，并把常见错误转成稳定错误码 |
| `apps/connector/terminal.go` | 对每个 stream 启动 `tmux attach-session`；处理输入、输出、窗口尺寸和关闭 |
| `apps/connector/terminal_unix.go` / `terminal_windows.go` | 伪终端：macOS/Linux 用 `creack/pty`，Windows 用 ConPTY（`UserExistsError/conpty`） |
| `apps/connector/platform_unix.go` / `platform_windows.go` | 平台差异：会话目标（psmux 不加 `=` 前缀）、隐藏子进程窗口、Windows 日志文件、随附 tmux 文件名 |
| `apps/connector/main.go` | 分发 RPC 与 stream 消息；连接退出时关闭现有 PTY 流 |

tmux 会话与浏览器连接生命周期分开。关闭浏览器只结束那次 `attach-session`，显式执行“关闭会话”才调用 `tmux kill-session`。修改 stream 时还需同步核对 `internal/relay/terminal.go` 和 `pkg/protocol/messages.go`。

## 定位与验证

- 配对：`apps/connector/pair_test.go`。
- tmux 会话：`apps/connector/tmux_test.go`，需本机 tmux 才会运行生命周期测试。
- 终端流：`apps/connector/terminal.go` 与 `internal/relay/terminal_test.go`。
- 端到端：`go run ./tests/e2e/termcheck -hub <URL> -device <设备名>`（令牌放 `SINTHMUX_E2E_TOKEN`），以浏览器方式检查登录、会话、票据、终端输入输出、尺寸、UTF-8、断线重连与票据防重放。

## 文件上传、历史导出与会话提醒

- `transfer.go`：上传写到 `~/.sinthmux/uploads`（目录 0700、文件 0600，拒绝符号链接；`SINTHMUX_UPLOAD_DIR` 可覆盖），文件名 = 时间戳-随机数-清洗后的原名（只留 `[A-Za-z0-9._-]`），`WriteAt` 分块、commit 时重算 SHA-256；启动及每 6 小时删除 1 小时前的 `.part` 和 7 天前的文件（只删符合上传命名规则的文件，自定义目录里的其他文件不动）。导出用 `capture-pane -p -J -t =name: -S -N|-`，上限 32 MiB；未完成的上传和导出由 `cleanUploads` 每 2 分钟检查，闲置 2 分钟释放。
- `notify.go`：`sinthmux-connector notify [--color] [--clear] [--session] 消息` 在会话上写用户选项 `@sinthmux_notify`（`v1:<ms>:<color>:<base64url>`）并 `wait-for -S sinthmux-notify`；Connector 阻塞在 `wait-for` 并每 15 秒兜底重读，变化时推送快照。浏览器打开会话（stream.open）时自动清除。notify 从 `$TMUX` 取 socket 路径，即使安装器用了 `-L sinthmux` 也能找到正确的 tmux 服务。
- tmux 目标：会话类命令用 `sessionTarget`（`=name`），面板/选项类命令必须用 `paneTarget`（`=name:`），否则报 `can't find pane`。
- systemd 单元带 `KillMode=process`：tmux 服务由 Connector 拉起、落在同一 cgroup，默认 control-group 会在重启 Connector 时杀掉所有会话。macOS plist 带 `AbandonProcessGroup`。
