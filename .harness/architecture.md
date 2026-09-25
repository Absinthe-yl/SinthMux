# 整体架构与调用链

## 部署单元与状态归属

| 单元 | 运行位置 | 主要职责 | 状态 |
| --- | --- | --- | --- |
| Web | 浏览器；Vite 开发服务或 Caddy 静态站点 | 设备、空间、会话管理和终端交互 | 页面状态、React Query 缓存、主题偏好 |
| Hub | Go HTTP 服务 | 鉴权、设备注册、RPC 转发、终端中继 | 在线设备注册表、连接、RPC、票据在单进程内存中 |
| PostgreSQL | 正式模式数据库 | 用户、空间、成员、登录令牌、Web 会话、设备、配对码和审计 | 持久化；`auth.Store.Open` 启动时执行建表迁移 |
| Connector | 每台目标机器的普通用户进程 | 主动连 Hub、执行 tmux 命令、启动 PTY 附着会话 | 本机配对凭据文件；实际终端会话保存在 tmux 中 |
| Login Broker | 可选独立 Go 服务 | 集中持有 GitHub OAuth 凭据，并签发 Hub 可验证的身份票据 | 待处理 OAuth state 和一次性兑换码在内存中 |

## 主链路

1. **上线**：`apps/connector/main.go` 建立到 `/ws/v1/connectors/connect` 的出站 WebSocket，发送 `connector.hello`；`internal/relay/connector_handler.go` 验证凭据并在 `Manager` 和 `devices.Registry` 登记，随后接收心跳。断线后设备代理退避重连。
2. **列会话或操作会话**：Web 访问 `/api/v1/devices/{deviceId}/sessions`；`apps/hub/sessions.go` 把 HTTP 请求转为 RPC，`Manager` 按设备 ID 发给在线连接；设备代理在 `tmux.go` 执行 tmux，再按 request ID 回传结果。
3. **打开终端**：浏览器先 POST 申请 45 秒、单次使用的票据，再以 `sinthmux.v1` 和 `sinthmux.ticket.<ticket>` 子协议连接 `/ws/v1/terminal`。Hub 创建 stream，设备代理通过 PTY 运行 `tmux attach-session`，双方转发输入、输出和尺寸。浏览器断线只关闭这次附着流，不会结束 tmux session。
4. **正式模式的身份数据**：`internal/auth/http.go` 提供登录、空间、成员、设备和配对 API；`store.go` 持久化并检查角色。Hub 的设备在线信息仍来自内存注册表，再与数据库中用户可见的设备合并。

## 关键边界

- `internal/config` 只读取环境变量；`apps/hub` 负责组装 `auth`、`devices` 与 `relay`。
- `pkg/protocol/messages.go` 是现行 Connector ↔ Hub JSON 消息契约，版本号为 1。`proto/.../connector.proto` 是未接入的定义，修改运行协议应先核对现行 Go 类型与双方处理器。
- Web ↔ Hub 的终端通道与 Connector ↔ Hub 的长连接是两条不同的 WebSocket；前者用短期票据，后者用设备凭据或本机开发令牌。
- Hub 当前是单实例设计：在线路由与终端票据在内存中。数据库持久化不等于跨 Hub 实例共享长连接状态。
- 现行设备认证使用 Bearer 设备令牌；ADR 所写的 mTLS 是目标方案，当前还未实现。

## 首选阅读顺序

`apps/hub/main.go` → `internal/relay/connector_handler.go` / `terminal.go` → `apps/connector/main.go` → `apps/web/src/App.tsx` / `TerminalView.tsx`。针对具体任务，再沿 `harness.md` 的模块索引深入。
