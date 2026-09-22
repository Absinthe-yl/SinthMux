# 基于 Agent 与 tmux 的多主机远程开发终端管理系统的设计与实现

系统名称：SinthMux

## 实现方案

#### 一、背景与目标
##### 1.1 产品定位
（1）SinthMux 是一个公网多主机持久终端工作台，不依赖 DevCloud，也不要求设备拥有公网 IP。

（2）用户在每台电脑或云服务器上运行 SinthMux Agent，由 Agent 主动连接 Hub；浏览器只连接 Hub。

（3）终端任务运行在 tmux 中。浏览器关闭、设备切换或短时断网不会终止任务。

（4）首要用户场景是个人拥有两台电脑和一台云服务器，通过手机、平板和电脑继续同一组终端工作。

##### 1.2 毕设研究问题
（1）在 NAT 和防火墙环境下，Agent 主动反向连接能否降低多主机接入复杂度。

（2）心跳、短期票据和断线恢复机制能否提升移动网络下终端会话连续性。

（3）细粒度权限和审计能否在可用性与远程终端安全之间取得平衡。

（4）多主机集中管理相较逐台 SSH，能否降低会话恢复时间和操作步骤。

##### 1.3 明确不做
（1）首版不实现 RDP、VNC、Kubernetes、数据库代理等 Teleport/Guacamole 级别的多协议平台。

（2）首版不自行实现 VPN、NAT 打洞或 QUIC 数据面。

（3）首版不支持 Agent 以 root 身份跨 Unix 用户操作。

（4）首版不实现 Hub 不可见终端明文的端到端加密；Hub 是受信任控制面，这一边界必须在论文中明确。

#### 二、开源项目调研结论
##### 2.1 值得借鉴的项目
（1）[ShellHub](https://github.com/shellhub-io/shellhub)：重点借鉴公网 Hub、NAT 后 Agent、设备注册和集中 SSH 管理；整体网络模型与 SinthMux 最接近。

（2）[MeshCentral](https://github.com/Ylianst/MeshCentral)：重点借鉴 Agent 生命周期、心跳、能力协商、自动升级和浏览器中继。

（3）[Nexterm](https://github.com/gnmyt/Nexterm)：重点借鉴现代多服务器管理界面、连接管理、SFTP 和 Engine 分层。

（4）[sshx](https://github.com/ekzhang/sshx) 与 [TermPair](https://github.com/cs01/termpair)：重点借鉴自动重连、协作终端、安全分享和可选端到端加密。

（5）[ttyd](https://github.com/tsl0922/ttyd)：重点借鉴 PTY、xterm.js、尺寸同步、IME、心跳和文件传输细节。

##### 2.2 不直接作为底座
（1）Teleport：功能和工程体量远超毕设范围，且 AGPL-3.0 对衍生产品有额外约束。

（2）Apache Guacamole：面向 RDP/VNC/SSH 协议转换，Java、C 与 guacd 链路较重，不符合 tmux 持久工作区主线。

（3）MeshCentral：虽功能接近，但历史代码和单体结构较重，直接 Fork 不利于展示个人架构设计成果。

（4）WeTTY、ttyd、GoTTY：适合单终端暴露，不具备完整的设备注册、Hub 路由和多主机模型。

##### 2.3 许可证策略
（1）SinthMux 建议使用 `Apache-2.0`。

（2）可借鉴 Apache-2.0 和 MIT 项目的设计；复用代码时保留许可证和版权声明。

（3）不复制公司内部 TmuxHub 源码，也不把 AGPL 项目代码直接并入核心实现。

#### 三、总体架构
##### 3.1 系统拓扑
```text
┌────────────────────────────────────────────┐
│ Browser / Installable PWA                  │
│ Sessions / Workspace / Files / Settings    │
└───────────────────┬────────────────────────┘
                    │ HTTPS + WSS :443
┌───────────────────▼────────────────────────┐
│ SinthMux Hub                               │
│ Auth / RBAC / Device Registry / Router     │
│ Ticket / Terminal Relay / Audit / REST API │
└───────────────────┬────────────────────────┘
                    │ Agent outbound mTLS WSS
           ┌────────┴───────────┐
┌──────────▼─────────┐ ┌────────▼──────────┐
│ Agent: Laptop      │ │ Agent: Cloud VM   │
│ tmux / PTY / Files │ │ tmux / PTY / Files│
└────────────────────┘ └───────────────────┘
```

##### 3.2 Hub 技术方案
（1）语言使用 Go。理由是单进程部署、并发连接成本低、交叉编译方便，并可与 Agent 共享协议和数据类型。

（2）HTTP 路由使用 `chi`，WebSocket 使用 `coder/websocket`，数据库访问使用 `pgx` 与 `sqlc`。

（3）PostgreSQL 保存用户、设备、主机、权限、审计和配置；单机开发允许使用 PostgreSQL Docker 容器，不设计第二套 SQLite 语义。

（4）Redis 只用于多 Hub 实例的在线路由、一次性票据和广播；MVP 单实例时通过内存实现相同接口。

（5）Caddy 负责 TLS 证书、HTTPS 入口和 WebSocket 反向代理。

##### 3.3 Agent 技术方案
（1）Agent 使用 Go，输出 Linux、macOS 单文件二进制；Windows 首版通过 WSL 支持。

（2）Agent 由 `systemd --user` 或 `launchd` 托管，默认普通用户权限运行。

（3）Agent 实现四个适配器：设备信息、tmux、PTY、受限文件系统。

（4）Agent 只主动连接 Hub 的 `443` 端口，不监听公网端口，不要求端口映射。

##### 3.4 Web 技术方案
（1）使用 React、TypeScript、Vite、TanStack Router、TanStack Query、xterm.js 和 Zustand。

（2）桌面端提供多主机侧栏和可持久化 Pane 树；移动端提供会话抽屉、特殊键盘、文本发送和只读历史模式。

（3）实现 PWA Manifest 和 Service Worker，但终端连接本身必须在线。

#### 四、通信协议
##### 4.1 Agent 控制通道
（1）Agent 与 Hub 建立一条长期 mTLS WebSocket。

（2）WebSocket 内使用版本化 Protobuf envelope：

```protobuf
message Envelope {
  uint32 version = 1;
  string request_id = 2;
  oneof body {
    AgentHello hello = 10;
    Heartbeat heartbeat = 11;
    RpcRequest request = 12;
    RpcResponse response = 13;
    StreamOpen stream_open = 14;
    StreamData stream_data = 15;
    StreamClose stream_close = 16;
    AgentEvent event = 17;
  }
}
```

（3）一条连接复用控制 RPC、终端流和事件流；每条流使用 `stream_id` 区分并实施流量窗口。

（4）Agent 每 15 秒发送心跳；Hub 连续 45 秒未收到心跳则将设备标为离线并关闭关联终端流。

##### 4.2 浏览器终端通道
（1）浏览器先申请 30 至 60 秒有效、仅可兑换一次的终端票据。

```http
POST /api/v1/terminal-tickets
```

```json
{
  "deviceId": "device-id",
  "runtime": "tmux",
  "sessionId": "coding",
  "permission": "read-write",
  "cols": 120,
  "rows": 40
}
```

（2）浏览器随后连接：

```text
wss://hub.example.com/ws/v1/terminals/{ticket}
```

（3）终端流使用二进制帧；控制消息包括 `ready`、`resize`、`ping`、`pong`、`exit` 和 `error`。

（4）客户端断线后指数退避重连，重新申请票据并附着同一个 tmux 会话。

##### 4.3 REST API 边界
```text
/api/v1/auth/*
/api/v1/users/*
/api/v1/devices/*
/api/v1/devices/:id/sessions/*
/api/v1/devices/:id/files/*
/api/v1/terminal-tickets
/api/v1/pairing-codes
/api/v1/audit-events
/api/v1/settings
```

> tip：所有资源必须从已认证用户空间中查询，不允许通过客户端提交的 `userId` 或 `tenantId` 决定归属。

#### 五、安全设计
##### 5.1 身份和设备配对
（1）用户初期采用邮箱密码加 TOTP；增强阶段加入 OIDC 和 WebAuthn。

（2）Hub 生成 5 分钟有效的一次性配对码，数据库只保存哈希。

（3）Agent 本机生成 P-256 私钥和 CSR；Hub 验证配对码后签发 24 小时设备证书。

（4）设备证书 SAN 固定绑定 `userSpaceId` 与不可变 `deviceId`，Agent 自报字段不得覆盖身份。

（5）证书在寿命三分之二处轮换；设备删除后立即吊销证书并关闭连接。

##### 5.2 权限模型
```text
Owner    用户空间、成员、设备、策略、审计
Admin    设备、Agent、会话、文件管理
Operator 终端读写、创建会话、授权文件操作
Viewer   终端只读、文件只读
```

细粒度动作：

```text
session.list session.view session.input session.create
session.rename session.kill file.read file.write file.delete
device.manage audit.read
```

（1）每个 REST 请求和 WebSocket 消息均在服务端执行授权检查。

（2）终端输入、文件删除、Agent 升级和共享权限变更属于高风险动作。

##### 5.3 安全约束
（1）使用 TLS 1.3 和 mTLS，不自行设计密码算法。

（2）JWT 不写入 URL 或浏览器 `localStorage`；浏览器使用 `HttpOnly`、`Secure`、`SameSite` Cookie。

（3）WebSocket 校验 `Origin`、消息大小、频率、ticket 目标和操作权限。

（4）tmux 命令必须通过结构化参数执行，禁止 Shell 字符串拼接。

（5）文件访问必须限制在配置根目录内，并防止路径穿越、符号链接逃逸和 TOCTOU。

（6）审计日志记录身份、设备、目标、动作、结果和请求 ID，但不记录令牌、Cookie、私钥或完整终端输入。

#### 六、功能范围
##### 6.1 MVP
（1）账号注册、登录、TOTP 和用户空间。

（2）一次性配对码安装 Agent。

（3）Linux、macOS Agent 主动连接 Hub，并上报主机信息、版本和心跳。

（4）跨 NAT 的 tmux 会话列表、创建、重命名、关闭和实时终端。

（5）终端断线恢复、窗口尺寸同步、移动端特殊键输入。

（6）设备在线状态、最后在线时间和基础审计日志。

（7）Docker Compose 公网部署与 HTTPS。

##### 6.2 增强功能
（1）多 Pane 工作区、布局恢复和拖动调节。

（2）受限文件浏览、上传、下载和编辑。

（3）只读/读写分享、过期时间和撤销。

（4）自定义操作、延迟执行和重复任务。

（5）OIDC/WebAuthn、细粒度 RBAC 和 Agent 签名升级。

##### 6.3 扩展研究
（1）WebSocket 与 QUIC/WebTransport 在移动网络切换下的恢复时间对比。

（2）可选端到端加密终端流。

（3）Headscale 私网直连模式和 Hub 中继模式对比。

（4）Zellij 与 Windows 原生 ConPTY 支持。

#### 七、数据模型
##### 7.1 核心实体
```text
users
spaces
memberships
devices
device_certificates
pairing_codes
sessions
terminal_tickets
audit_events
user_settings
shares
```

##### 7.2 关键唯一性
（1）`devices` 使用服务端生成的 UUID，不以名称或 IP 作为身份。

（2）会话唯一键为 `device_id + runtime + runtime_ref`。

（3）票据保存哈希、目标和使用时间，兑换后原子标记为已使用。

（4）审计事件采用追加写，不允许业务 API 修改历史记录。

#### 八、代码结构
##### 8.1 Monorepo 规划
```text
SinthMux/
├── apps/
│   ├── hub/                 # Go Hub 入口
│   ├── agent/               # Go Agent 入口
│   └── web/                 # React PWA
├── internal/
│   ├── auth/
│   ├── devices/
│   ├── sessions/
│   ├── relay/
│   ├── files/
│   ├── audit/
│   └── persistence/
├── pkg/
│   ├── protocol/
│   └── terminal/
├── proto/
├── migrations/
├── deploy/
│   ├── docker-compose.yml
│   ├── Caddyfile
│   ├── systemd/
│   └── launchd/
├── docs/
│   ├── adr/
│   ├── threat-model.md
│   └── experiments.md
├── tests/
│   ├── integration/
│   └── e2e/
├── go.mod
└── Makefile
```

#### 九、实施里程碑
##### 9.1 第一阶段：协议验证（第 1 至 3 周）
（1）初始化 Go、React 和 Protobuf 工程。

（2）完成单用户 Hub、Agent 注册、心跳和内存主机表。

（3）打通浏览器到远程 Agent PTY 的最小数据流。

（4）验收：两台不同网络设备均无需开放入站端口，可在浏览器执行命令。

##### 9.2 第二阶段：tmux 产品闭环（第 4 至 7 周）
（1）完成 tmux 会话 CRUD、附着、窗口尺寸同步和断线恢复。

（2）完成多主机主页、终端页和移动端特殊键。

（3）加入短期 ticket、操作鉴权和基础审计。

（4）验收：手机切换网络后能恢复原 tmux 会话，任务不中断。

##### 9.3 第三阶段：公网安全与部署（第 8 至 10 周）
（1）完成一次性配对码、设备密钥、mTLS 和证书轮换。

（2）完成 Docker Compose、Caddy、systemd 和 launchd 配置。

（3）执行威胁建模、安全测试和故障注入。

##### 9.4 第四阶段：增强体验（第 11 至 14 周）
（1）完成多 Pane 工作区、文件浏览和分享。

（2）完成 PWA、移动端输入和通知。

（3）建立性能与可靠性指标面板。

##### 9.5 第五阶段：实验与论文（第 15 至 18 周）
（1）开展 NAT 接入成功率、断线恢复时间、终端交互延迟和并发资源占用实验。

（2）比较直接 SSH、固定重连和自适应重连方案。

（3）完成系统测试、威胁分析、实验章节和答辩演示。

#### 十、验收与实验
##### 10.1 产品验收
（1）家庭电脑、校园网络电脑和公网云服务器均可接入同一个 Hub。

（2）远程主机不开放任何新增入站端口。

（3）电脑、手机和平板均可登录、查看主机并操作 tmux。

（4）浏览器关闭后任务继续运行，再次连接能恢复现场。

（5）设备吊销后旧证书和既有连接立即失效。

##### 10.2 实验指标
```text
Agent 接入成功率
终端首屏时间
按键到回显 P50/P95 延迟
断网恢复时间
并发连接数与 Hub CPU/内存
消息丢失率与重复率
越权请求拦截率
设备吊销生效时间
```

##### 10.3 答辩演示
（1）一台 Mac、一台 Linux 云服务器和一台额外电脑连接公网 Hub。

（2）用手机创建 tmux 会话并启动持续任务。

（3）关闭手机页面，再由平板恢复同一会话。

（4）切断 Agent 网络并恢复，展示状态变化和自动重连。

（5）吊销一台设备，展示连接立即失效和审计记录。

（6）展示多 Pane 工作区同时观察日志、测试和系统指标。

#### 十一、最终决策
##### 11.1 ADR 摘要
（1）采用独立实现，不直接 Fork 现有平台。

（2）采用 Go Hub + Go Agent + React PWA。

（3）采用 Agent 主动出站的 mTLS WebSocket，不要求远程机器有公网 IP。

（4）首版以 tmux 为唯一持久终端运行时。

（5）PostgreSQL 是唯一持久数据库，Redis 是可选横向扩展组件。

（6）QUIC、Headscale 和终端端到端加密只作为后续研究扩展。

（7）第一阶段必须先证明远程链路，之后才投入复杂 UI。

> tip：下一步应先实现 `proto`、Hub 在线设备注册表和 Agent 最小长连接，不应从主页视觉设计开始。
