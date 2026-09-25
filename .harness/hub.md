# Hub：路由、设备与会话 API

## 模块职责

`apps/hub/main.go` 读取 `internal/config/config.go` 的环境变量，创建设备在线注册表、连接管理器，以及可选的认证服务。设置 `SINTHMUX_DATABASE_URL` 时连接 PostgreSQL 并启用正式模式；否则只允许绑定本机 loopback。`/health`、`/api/v1/system/status` 是状态入口。

## 路由索引

| 路由族 | 实现 | 说明 |
| --- | --- | --- |
| `/api/v1/devices` | `apps/hub/main.go` | 开发模式返回注册表；正式模式按用户可访问设备过滤，并补上离线设备 |
| `/api/v1/devices/{deviceId}/sessions` | `apps/hub/sessions.go` | GET 列表、POST 创建；子路径 PATCH 重命名、DELETE 关闭 |
| `/api/v1/devices/{deviceId}/sessions/{sessionName}/ticket` | `apps/hub/main.go`、`internal/relay/terminal.go` | 颁发终端 WebSocket 一次性票据 |
| `/ws/v1/terminal` | `internal/relay/terminal.go` | 浏览器终端中继 |
| `/ws/v1/connectors/connect` | `internal/relay/connector_handler.go` | 设备代理长连接 |
| `/api/v1/auth/*`、`/api/v1/spaces/*`、`/api/v1/connectors/pair` | `internal/auth/http.go` | 正式模式认证、成员、设备与配对 |
| `/install/connector.sh`、`/downloads/{file}` | `apps/hub/main.go` | 安装脚本和受限文件名的代理二进制下载 |

## 会话请求如何流动

`sessionHandler` 将设备 ID 和 `protocol.RPCRequest` 交给 `relay.Manager.Call`；Manager 以 request ID 关联回包。连接离线、超时和设备端错误会映射为 HTTP 状态码。会话名称的共同约束是 `pkg/protocol.ValidSessionName`：字母或数字开头，后续可含字母、数字、下划线或连字符，最多 64 字符。修改 API 时同时检查 Web 的请求路径、Connector RPC 方法和错误映射。

## 设备在线状态

`internal/devices/registry.go` 记录 hello 元数据、连接时间、最近心跳与在线状态；`relay.Manager` 负责真实 WebSocket 连接、等待中的 RPC 和终端流。正式模式中的设备记录在 `auth.Store`，在线状态仍靠 Registry。吊销设备时 Hub 通过 `OnDeviceRevoked` 断开当前连接。

## 定位与验证

- 路由/会话：`apps/hub/main.go`、`apps/hub/sessions.go`、`apps/hub/sessions_test.go`。
- 设备生命周期：`internal/devices/registry.go`、`registry_test.go`。
- 鉴权和流转：接着看 `auth-storage.md` 与 `relay-protocol.md`。
