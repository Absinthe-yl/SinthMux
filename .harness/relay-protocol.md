# 连接中继、RPC 与协议

## 两条 WebSocket

| 通道 | 入口 | 认证 | 数据 |
| --- | --- | --- | --- |
| Connector → Hub | `/ws/v1/connectors/connect` | 正式模式：设备证书 + nonce 签名（`X-Sinthmux-Device-*` 请求头），旧版设备令牌在首次证书连接前仍可用；开发模式：开发令牌 | JSON `protocol.Envelope`，包含 hello、heartbeat、RPC 和 stream 消息 |
| Browser → Hub | `/ws/v1/terminal` | 45 秒、一次性票据；正式模式额外绑定 Web session 并复核权限 | 浏览器输入/输出为二进制帧；文本帧为 `{"type":"resize"}` 或 `{"type":"ping","id"}`（Hub 直接回 `pong`，不经过设备）；Hub 每 30 秒 Ping 浏览器，10 秒无回应即断开并释放 tmux attach |

## 主要类型与实现

- `pkg/protocol/messages.go`：现行 `Version = 1`、消息类型、`Envelope`、RPC 和 stream 结构，以及会话名校验。
- `internal/relay/connector_handler.go`：验证 Connector，处理 hello / heartbeat / RPC 回包 / stream 事件，并更新 `devices.Registry`。
- `internal/relay/manager.go`：按 device ID 管理单个活动连接；关联等待中的 RPC 与终端 stream，断线时通知等待者。RPC 默认 10 秒超时。
- `internal/relay/terminal.go`：签发并消费票据，打开 stream，转发输入、输出和 resize，周期性检查权限。
- `apps/connector/main.go`、`tmux.go`、`terminal.go`：协议另一端，执行 RPC 与 PTY 操作。

## 第一档扩展（2026-10）

- 能力位（`protocol.Capability*`）：`file.upload.v1`（全平台）、`terminal.export.v1` 与 `session.notify.v1`（仅 Unix，`platform_*.go` 的 `extendedTmux`）。Hub 用 `devices.Registry.HasCapability` 判断，缺失返回 501。
- RPC：`file.upload.begin/chunk/commit/abort`、`terminal.export.begin/read/close`、`session.notify.clear`，参数在 `RPCRequest.Transfer`，结果在 `RPCResponse.Transfer`；单块 ≤ `TransferChunkSize`（32 KiB）。Hub 端窗口 4 并发（`apps/hub/transfer.go`）。
- 消息 `connector.notifications`：Connector → Hub 的全量提醒快照，连接建立时和变化时发送，Hub 存入 Registry，`/api/v1/devices` 返回。
- Connector 的 RPC 并发执行（信号量 8），不会阻塞同连接上的终端输入。

## 消息流

```text
会话 RPC：HTTP handler → Manager.Call → rpc.request → Connector.handleRPC
        → rpc.response → ConnectorHandler → 等待中的 HTTP handler

终端流：浏览器票据 → TerminalHandler → Manager.OpenStream → stream.open
        → Connector.terminalStreams → tmux PTY
        ↔ stream.data / stream.resize / stream.close ↔ 浏览器 WebSocket
```

同一 device ID 的新连接会替换旧连接；连接丢失时待处理 RPC 和 stream 被结束。`Manager`、Registry 和票据都在 Hub 进程内存中，当前没有跨实例路由。`proto/sinthmux/v1/connector.proto` 可通过 `make proto` 检查定义，但它不是当前在线消息格式；新增字段或类型要先修改运行中的 Go 协议与发送、接收两端，并补相关测试。

## 主要测试

`internal/relay/manager_test.go` 覆盖 RPC 正常、离线、超时、无效回包和重复 ID；`terminal_test.go` 覆盖票据单次使用、浏览器 session 绑定和流断开。协议修改时也检查 `apps/connector/*_test.go`。
