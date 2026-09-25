# Connector：设备配对、tmux 与 PTY

## 生命周期

`apps/connector/main.go` 有 `pair` 子命令和常驻连接模式。常驻模式先读环境变量；未设置设备令牌时尝试从用户配置目录的 `sinthmux/connector.json` 加载配对结果，也可通过 `SINTHMUX_CONNECTOR_CONFIG` 指定路径。连接失败会退避重试；连接后发送 hello，每 15 秒发送一次心跳。设备代理只主动连 Hub，不监听入站端口。

## 设备接入

1. Web 的“添加设备”请求 `POST /api/v1/spaces/{spaceId}/device-pairings`，获得 5 分钟、单次使用的配对码。
2. 页面生成的命令下载 Hub 提供的 `install-connector.sh`；脚本按 macOS/Linux 和 CPU 架构下载二进制。
3. `apps/connector/pair.go` 调用 `POST /api/v1/connectors/pair` 兑换设备 ID 与令牌，写入权限为 `0600` 的本机配置文件；若已有配置则拒绝覆盖。
4. 安装脚本在无 tmux 会话时创建 `sinthmux` 会话，并尝试用 systemd user service 或 LaunchAgent 托管；其他情况下以后台进程启动。

入口文件是 `apps/hub/install-connector.sh`、`apps/connector/pair.go` 和 `internal/auth/http.go` / `store.go` 的配对方法。正式模式连接时使用设备 ID 请求头与 Bearer 设备令牌；本机开发模式使用 `SINTHMUX_DEV_TOKEN`。

## tmux 与终端

| 文件 | 处理 |
| --- | --- |
| `apps/connector/tmux.go` | `tmux.sessions.list/create/rename/close` RPC；校验会话名，执行 tmux，并把常见错误转成稳定错误码 |
| `apps/connector/terminal.go` | 对每个 stream 启动 `tmux attach-session` 的 PTY；处理输入、输出、窗口尺寸和关闭 |
| `apps/connector/main.go` | 分发 RPC 与 stream 消息；连接退出时关闭现有 PTY 流 |

tmux 会话与浏览器连接生命周期分开。关闭浏览器只结束那次 `attach-session`，显式执行“关闭会话”才调用 `tmux kill-session`。修改 stream 时还需同步核对 `internal/relay/terminal.go` 和 `pkg/protocol/messages.go`。

## 定位与验证

- 配对：`apps/connector/pair_test.go`。
- tmux 会话：`apps/connector/tmux_test.go`，需本机 tmux 才会运行生命周期测试。
- 终端流：`apps/connector/terminal.go` 与 `internal/relay/terminal_test.go`；跨端行为需结合手工连接验证。
