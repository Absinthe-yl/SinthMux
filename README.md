# SinthMux

SinthMux 是一个面向个人与小团队的公网多主机持久终端工作台。用户在自己的电脑、Linux 主机或云服务器上安装 Agent，Agent 主动连接公网 Hub；随后可从电脑、手机或平板浏览器集中管理各主机上的 tmux 会话。

当前阶段：Hub 可通过 Agent 出站连接查询远端 tmux 会话，Web 控制台可查看各设备的会话列表。

## 核心目标

- 一个 Hub 管理多台位于 NAT、防火墙或不同网络后的主机。
- 远程主机只建立出站连接，不开放 SSH 或 Agent 公网端口。
- 浏览器可以创建、恢复、切换和操作持久 tmux 会话。
- 支持桌面、手机和平板，并在网络切换后自动恢复终端。
- 默认采用短期凭据、设备身份、细粒度授权和安全审计。
- 形成适合软件工程本科毕设的完整系统、实验和论文材料。

## 已确定方案

```text
Browser / PWA
      | HTTPS + WSS
      v
SinthMux Hub
      | mTLS WebSocket (Agent outbound connection)
      v
SinthMux Agent
      | structured argv / PTY
      v
tmux sessions
```

技术栈：

- Hub：Go、Chi、Coder WebSocket、PostgreSQL、Redis（按规模启用）。
- Agent：Go 单文件程序，systemd/launchd 托管。
- Web：React、TypeScript、Vite、TanStack Query、xterm.js、PWA。
- 协议：Protobuf 定义，控制消息走 JSON/Protobuf envelope，终端数据走二进制帧。
- 部署：Docker Compose + Caddy，公网统一使用 TLS 1.3 和 TCP 443。

详细方案见 [docs/IMPLEMENTATION_PLAN.md](docs/IMPLEMENTATION_PLAN.md) 和 [docs/adr/0001-connection-architecture.md](docs/adr/0001-connection-architecture.md)。

## 本地启动

要求 Go 1.25+、Node.js 20+ 和 npm。

```bash
npm --prefix apps/web install
go mod download
```

分别启动三个进程：

```bash
make dev-hub
make dev-agent
make dev-web
```

打开 `http://127.0.0.1:5173`。开发 Agent 会通过出站 WebSocket 注册到 Hub，并出现在设备列表中。
点击设备的“查看会话”会调用 `GET /api/v1/devices/{deviceId}/sessions`。Hub 发送带 requestId 的 `tmux.sessions.list` RPC，Agent 使用本机运行用户的 tmux 查询会话。没有会话时返回空列表；Agent 离线或 RPC 超时时返回相应错误。创建会话和终端流尚未实现。

> 当前 `SINTHMUX_DEV_TOKEN` 只用于本地框架联调。正式开发将按 ADR 替换为一次性配对码和 mTLS 设备证书。

## 调研来源

- [ShellHub](https://github.com/shellhub-io/shellhub)
- [MeshCentral](https://github.com/Ylianst/MeshCentral)
- [Nexterm](https://github.com/gnmyt/Nexterm)
- [sshx](https://github.com/ekzhang/sshx)
- [TermPair](https://github.com/cs01/termpair)
- [ttyd](https://github.com/tsl0922/ttyd)
- [Teleport](https://github.com/gravitational/teleport)
- [Apache Guacamole](https://github.com/apache/guacamole-client)
- [OWASP WebSocket Security Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/WebSocket_Security_Cheat_Sheet.html)
