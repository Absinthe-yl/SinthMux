# SinthMux

**从一个浏览器查看多台机器上的 tmux 会话。**

SinthMux 是一个面向个人和小团队的多主机终端工作台。每台机器上的 Agent 主动连接 Hub，因此设备可以位于 NAT 或防火墙后面，无需为 Agent 开放入站端口。项目的目标是在浏览器中管理、连接和恢复各机器上的持久 tmux 会话。

> **项目状态：早期开发。** 当前已打通设备注册、心跳和远程 tmux 会话列表查询。浏览器终端、会话管理和正式认证仍在开发中；请勿将当前版本直接部署到公网。

## 当前可以做什么

| 功能 | 状态 |
| --- | --- |
| Agent 主动连接 Hub，设备上线、心跳与离线状态 | 已实现 |
| Web 查看设备、平台、Agent 版本和在线状态 | 已实现 |
| Web 查看指定设备的 tmux 会话列表 | 已实现 |
| 创建、重命名、关闭 tmux 会话 | 计划中 |
| 在浏览器中连接和恢复终端 | 计划中 |
| 设备配对、mTLS 与浏览器认证 | 计划中 |

## 快速开始

开发环境需要 **Go 1.25+、Node.js 20+、npm 和 tmux**。Agent 所在机器需安装 tmux；当前示例在同一台机器上运行所有组件。

```bash
git clone https://github.com/Absinthe-yl/SinthMux.git
cd SinthMux
go mod download
npm --prefix apps/web install
```

在三个终端分别运行：

```bash
make dev-hub
```

```bash
make dev-agent
```

```bash
make dev-web
```

打开 [http://127.0.0.1:5173](http://127.0.0.1:5173)，在设备列表中点击 **查看会话**。如果还没有 tmux 会话，可以在运行 Agent 的机器上执行：

```bash
tmux new-session -d -s demo
```

然后在 Web 页面重新打开该设备的会话列表。也可以直接调用 API：

```bash
curl http://127.0.0.1:8090/api/v1/devices/local-dev/sessions
```

Hub 默认监听 `127.0.0.1:8090`，Web 开发服务器默认使用 `5173` 端口。Agent 的设备 ID、名称及连接地址可通过 `SINTHMUX_AGENT_DEVICE_ID`、`SINTHMUX_AGENT_NAME` 和 `SINTHMUX_AGENT_HUB_URL` 配置，示例见 [`.env.example`](.env.example)。

> 当前 Agent 连接使用共享开发 Token，浏览器 API 尚无正式认证。该 Token 仅用于本地联调；不要把开发服务或 Hub 暴露到公网。

## 工作方式

```text
Browser ── HTTP ──▶ Hub ── WebSocket RPC ──▶ Agent ──▶ tmux
                    ▲                       │
                    └────── 主动出站连接 ────┘
```

1. Agent 主动连接 Hub，报告设备信息并定期发送心跳。
2. 浏览器请求 `GET /api/v1/devices/{deviceId}/sessions`。
3. Hub 向对应 Agent 发送带 `requestId` 的 `tmux.sessions.list` RPC，并等待响应或超时。
4. Agent 使用参数数组执行 `tmux list-sessions`，返回会话名称、窗口数、连接状态和创建时间。

当前控制消息采用 JSON envelope；[Protobuf 协议草案](proto/sinthmux/v1/agent.proto)已定义但尚未接入运行链路。正式公网方案中的 TLS、设备配对和 mTLS 见[架构决策](docs/adr/0001-connection-architecture.md)。

## 开发与验证

```bash
make test   # Go 测试与 Web 类型检查
make build  # Go 构建与 Web 生产构建
make proto  # 验证 Protobuf 定义
```

Hub 与 Agent 的 RPC 集成测试覆盖正常返回、Agent 离线、超时、非法响应和重复 requestId。项目目录：

```text
apps/hub/          Hub 入口与 HTTP API
apps/agent/        出站 Agent 与 tmux 查询
apps/web/          React 控制台
internal/devices/  设备状态注册表
internal/relay/    Agent 连接与 RPC 管理
pkg/protocol/      当前 JSON 消息结构
proto/             Protobuf 协议草案
docs/              实现方案与架构决策
```

## 路线图与文档

下一步是会话创建、重命名和关闭，再实现浏览器 PTY 终端流。随后补齐设备配对、短期证书、浏览器认证和持久化。完整任务顺序见[实现方案](docs/IMPLEMENTATION_PLAN.md)。

项目在设计阶段参考了 [ShellHub](https://github.com/shellhub-io/shellhub) 的 Agent 与网关拓扑、[MeshCentral](https://github.com/Ylianst/MeshCentral) 的设备生命周期，以及 [ttyd](https://github.com/tsl0922/ttyd) 的浏览器终端经验。SinthMux 为独立实现。
