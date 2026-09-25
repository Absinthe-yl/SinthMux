# SinthMux Harness：项目入口与模块索引

SinthMux 是一个多设备持久终端工作台。浏览器连接 Hub；每台目标机器上的设备代理主动连接 Hub，并在本机操作 tmux。关闭浏览器不会结束 tmux 会话。本仓库是毕业设计的主要实现；同级的 `../tmuxhub/` 是较早的本地单机项目，不属于 SinthMux 的运行链路。

## 怎样使用本索引

1. 先读本文件，确定任务涉及的模块和跨模块调用链。
2. 只读下表中与任务相关的 `.harness/` 文件，再进入所列源码与测试。文档用于导航；行为以当前代码、配置和测试为准。
3. 修改跨模块契约时，同时检查调用链两端、协议、权限和部署入口。交付时说明实际运行过的验证命令；若入口或约束改变，同步更新本索引。

## 当前系统形态

```text
Browser (React + xterm.js)
    │ HTTP API / terminal WebSocket
    ▼
Hub (Go + chi) ───── PostgreSQL：用户、空间、设备凭据、登录与审计
    │ connector WebSocket；RPC / terminal stream
    ▼
Connector (Go) ───── tmux / PTY ───── 终端中的用户程序

可选：Hub ↔ Login Broker ↔ GitHub OAuth
```

- **正式模式**：设置 `SINTHMUX_DATABASE_URL` 后启用数据库、用户登录、空间角色和设备凭据；Web 与 Hub 通常由 Docker Compose 启动，设备代理在 tmux 所在机器原生运行。
- **本机开发模式**：不设置数据库地址时，Hub 仅允许绑定 loopback，以开发令牌接受设备代理，Web 不要求登录。
- **当前边界**：一次性设备配对、浏览器终端票据和公网 HTTPS 接入路径已有实现；设备代理 mTLS 尚未实现。`proto/sinthmux/v1/connector.proto` 尚未接入运行时，当前消息使用 `pkg/protocol/messages.go` 中的 JSON envelope。Hub 是可信中继，可以看到终端明文。

## 按任务选择文档

| 要做的事 | 先读 | 随后定位 |
| --- | --- | --- |
| 理解系统和跨模块调用链 | [`.harness/architecture.md`](.harness/architecture.md) | `apps/hub/main.go`、`apps/connector/main.go` |
| 修改 HTTP API、设备列表或会话路由 | [`.harness/hub.md`](.harness/hub.md) | `apps/hub/`、`internal/devices/` |
| 修改目标机器的配对、tmux 或 PTY | [`.harness/connector.md`](.harness/connector.md) | `apps/connector/`、`apps/hub/install-connector.sh` |
| 修改用户、空间、角色、令牌、设备权限或审计 | [`.harness/auth-storage.md`](.harness/auth-storage.md) | `internal/auth/` |
| 修改 WebSocket、RPC、终端票据或消息格式 | [`.harness/relay-protocol.md`](.harness/relay-protocol.md) | `internal/relay/`、`pkg/protocol/` |
| 修改页面、设备/会话操作或浏览器终端 | [`.harness/web.md`](.harness/web.md) | `apps/web/src/` |
| 修改统一 GitHub 登录服务 | [`.harness/login-broker.md`](.harness/login-broker.md) | `apps/login-broker/`、`internal/loginbroker/` |
| 启动、部署、排障或验收 | [`.harness/run-verify.md`](.harness/run-verify.md) | `Makefile`、`scripts/`、`deploy/`、相关 `*_test.go` |

## 关键入口

| 入口 | 用途 |
| --- | --- |
| `apps/hub/main.go` | Hub 启动、模式选择、REST 与 WebSocket 路由装配 |
| `apps/connector/main.go` | 设备代理启动、连接、重试、心跳和消息分发 |
| `apps/web/src/App.tsx` | 登录、空间、设备和会话页面状态 |
| `apps/web/src/TerminalView.tsx` | 浏览器终端、票据获取与断线重连 |
| `apps/login-broker/main.go` | 可选的集中 GitHub 登录服务 |
| `internal/config/config.go` | Hub 与设备代理的环境变量默认值 |
| `Makefile` | 构建、测试和本地启动命令 |

## 修改时优先检查的跨模块关系

- 会话操作：`SessionPanel.tsx` → `apps/hub/sessions.go` → `relay.Manager.Call` → `apps/connector/tmux.go`。
- 终端连接：`TerminalView.tsx` → Hub 票据接口 → `relay.TerminalHandler` → `relay.Manager` → `apps/connector/terminal.go`。
- 设备接入：`App.tsx` → `internal/auth/http.go` 配对接口 → `apps/connector/pair.go` → `relay.ConnectorHandler`。
- 权限：正式模式的 HTTP 请求由 `auth.Server.Require` 与 `auth.Server.Device` 校验；终端 WebSocket 另由票据绑定浏览器会话并复核权限。

`README.md` 负责用户用法，`docs/` 中的方案和 ADR 记录设计背景；本 Harness 负责让编码工具快速找到现有实现及其验证入口。设计文档中的目标能力不自动代表当前已实现。
