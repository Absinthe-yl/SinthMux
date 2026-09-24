# SinthMux

<img src="apps/web/public/sinthmux-mark.png" alt="SinthMux logo" width="72" />

**一个浏览器，管理多台机器上的持久终端。**

SinthMux 让每台机器上的设备代理主动连接 Hub，再通过网页查看和操作 tmux 会话。Claude Code、Codex 等 AI 编码 Agent 可以在 tmux 中持续运行；关闭浏览器、切换设备或短暂断线后，回到网页即可继续使用同一会话。

“设备代理”是连接 Hub、操作 tmux 的 SinthMux 后台程序，代码目录名为 `connector`；“AI 编码 Agent”是用户在终端中运行的 Claude Code、Codex 等程序。两者职责不同。

> **当前默认只供本机试用。** Hub、Web 和 PostgreSQL 可以用 Docker 启动；设备代理在运行 tmux 的机器上原生运行。一次性设备配对已实现，但 mTLS 尚未完成；公网部署需先配置 HTTPS 入口与可访问的 `SINTHMUX_PUBLIC_URL`。

## 功能

- **多设备管理**：查看设备代理在线状态、系统平台和 tmux 会话。
- **会话操作**：创建、重命名、关闭会话，或在浏览器中进入终端。
- **持久终端**：浏览器断线后自动重连；刷新页面后可重新进入原 tmux 会话。
- **用户与空间**：支持 SinthMux 登录令牌、可选 GitHub 登录、个人及团队空间和角色权限。

## 快速开始

### 1. 启动 Hub、Web 和数据库

准备好 Git、Docker Compose、curl 和 openssl，然后运行：

```bash
git clone https://github.com/Absinthe-yl/SinthMux.git
cd SinthMux
./scripts/docker-up.sh
```

脚本会构建 Web 和 Hub，启动 PostgreSQL，并将服务限制在本机。PostgreSQL 运行在容器中，无需在电脑上单独安装。打开 **[http://127.0.0.1:5173](http://127.0.0.1:5173)**。

首次启动时，脚本把 24 小时有效的初始**用户登录令牌**写入 `deploy/.bootstrap-token`。在登录页点击 **使用令牌登录**，粘贴令牌：

```bash
cat deploy/.bootstrap-token
```

登录后，在网页的 **登录令牌** 页面创建自己的长期用户令牌，以后可直接在登录页使用。数据库密码保存在 `deploy/.env.local`，数据库内容保存在 Docker 卷中；请保留这份配置文件，以便下次连接同一个数据库。这两个文件均不会提交到 Git。

### 2. 一条命令接入设备

登录网页，点击 **添加设备**、填写名称，再将页面生成的命令粘贴到要接入的电脑终端执行。目标电脑需先有 `tmux`、`curl`，运行 macOS 或 Linux（x86-64 / ARM64）；无需克隆仓库或安装 Go。命令会下载设备代理、用 5 分钟有效的一次性配对码领取设备凭据，并设置后台启动。如果该用户尚无 tmux 会话，还会创建名为 `sinthmux` 的默认会话，供网页直接打开。凭据只保存在该电脑的用户配置目录中。

默认 Docker 部署只监听 `127.0.0.1`，所以当前命令只能在 **Hub 所在电脑** 执行。要接入第二台电脑，先将 Hub 与 Web 部署到第二台电脑可访问的 HTTPS 地址，并设置 `SINTHMUX_PUBLIC_URL` 为该地址；网页生成的命令会使用它。设备代理只主动向 Hub 发起出站连接，不需要在第二台电脑开放入站端口。

设备上线后，展开它的 **会话** 列表即可创建会话或进入终端。离开网页不会关闭 tmux 会话；在系统终端也可以使用 `tmux attach -t '=会话名'` 接回同一会话。

### 停止与重启

再次运行 `./scripts/docker-up.sh` 可启动或更新服务。停止 Docker 服务运行：

```bash
docker compose --env-file deploy/.env.local -f deploy/docker-compose.yml down
```

此命令保留数据库卷。设备代理由 macOS LaunchAgent 或 Linux systemd 用户服务保持运行；没有用户服务管理器时，安装脚本会在后台启动，并提示重启后的手动启动方式。已有 tmux 会话不会随浏览器关闭。

## 不使用 Docker：源码试用

如果只想在一台电脑上体验功能，可安装 Go 1.25+、Node.js 20+、npm、tmux、curl、nc 和 make，然后运行：

```bash
make quickstart
```

脚本会构建 Hub 和设备代理、安装 Web 依赖并启动三个服务。打开 **[http://127.0.0.1:5173](http://127.0.0.1:5173)**；日志位于 `.run/log/`，按 Ctrl+C 停止服务。全新克隆且未配置 `.env` 时，此方式使用仅限本机的开发模式，网页无需登录。若已有 `.env`，脚本会读取其中的数据库和设备配置。

## 当前范围

| 能力 | 状态 |
| --- | --- |
| 设备代理主动连接、心跳与重连 | 已实现 |
| tmux 会话管理与浏览器终端 | 已实现 |
| 令牌登录、GitHub 登录、空间和角色权限 | 已实现；GitHub 登录需配置 OAuth |
| PostgreSQL 持久化与本机 Docker 启动 | 已实现；Docker 容器仍待实机验证 |
| 一条命令安装设备代理、一次性设备配对 | 已实现；需目标机器安装 tmux、curl |
| mTLS、公网 HTTPS 部署 | 待完成 |

目前 Hub 是受信任的控制面，项目不提供端到端加密。Docker Compose 将 Hub 和 Web 绑定在 `127.0.0.1`；其他电脑和手机无法直接访问当前默认部署。公网多主机使用仍需配置可访问的 HTTPS 入口；mTLS 尚未完成。

## 工作方式

```text
Browser ── HTTP / WebSocket ──▶ Hub ◀── 出站连接 ── 设备代理 ──▶ tmux ──▶ AI 编码 Agent
                                  │
                                  └── PostgreSQL：用户、空间、设备和令牌
```

Hub 使用 Go，Web 使用 React 和 xterm.js。设备代理在目标机器上调用 tmux，并通过 Hub 转发终端数据。Claude Code、Codex 等 AI 编码 Agent 运行在 tmux 会话中；浏览器连接终端时使用短期一次性票据，tmux 进程与浏览器连接相互独立。

## 开发

```bash
make test   # Go 测试与 Web 类型检查
make build  # Go 构建与 Web 生产构建
make proto  # 验证 Protobuf 定义，需要 protoc
```

设置 `SINTHMUX_TEST_DATABASE_URL` 后，`make test` 还会运行 PostgreSQL 集成测试。当前通信使用 JSON envelope；[Protobuf 定义](proto/sinthmux/v1/connector.proto)尚未接入运行链路。

### 统一 GitHub 登录服务

登录服务只需部署一份，集中保存 GitHub OAuth App 的 Client ID 和 Client Secret。每个 Hub 配置登录服务地址及 Ed25519 公钥后，即可显示 **使用 GitHub 登录**；网页登录也始终保留 **使用令牌登录**。登录服务尚未部署到正式域名，当前克隆项目可先用令牌登录。

部署登录服务时，在 GitHub 创建 [OAuth App](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/creating-an-oauth-app)，回调地址设为 `https://login.example.com/callback`。运行 `go run ./apps/login-broker keygen` 生成签名密钥。将私钥种子和 GitHub 凭据写入被 Git 忽略的 `deploy/.broker.env.local`：

```dotenv
SINTHMUX_BROKER_PUBLIC_URL=https://login.example.com
SINTHMUX_BROKER_SIGNING_SEED=<keygen 输出的私钥种子>
SINTHMUX_GITHUB_CLIENT_ID=<GitHub Client ID>
SINTHMUX_GITHUB_CLIENT_SECRET=<GitHub Client Secret>
```

启动登录服务：

```bash
docker compose --env-file deploy/.broker.env.local -f deploy/login-broker.compose.yml up -d --build
```

将 HTTPS 反向代理指向本机 `127.0.0.1:8091`。每个 Hub 在 `deploy/.env.local` 中配置：

```dotenv
SINTHMUX_AUTH_BROKER_URL=https://login.example.com
SINTHMUX_AUTH_BROKER_PUBLIC_KEY=<keygen 输出的公钥>
```

Hub 的 `SINTHMUX_PUBLIC_URL` 必须是浏览器能打开的地址。公网 Hub 使用 HTTPS；本机联调可使用 `http://127.0.0.1:5173`。登录服务收到 GitHub 回调后只向 Hub 传一次性兑换码；Hub 经后端交换并验签，GitHub 访问令牌不会进入 Hub 或浏览器地址。若要在单个 Hub 上直接配置 GitHub OAuth，也可继续设置 `SINTHMUX_GITHUB_CLIENT_ID`、`SINTHMUX_GITHUB_CLIENT_SECRET` 和 `SINTHMUX_PUBLIC_URL`，回调地址为 `<SINTHMUX_PUBLIC_URL>/api/v1/auth/github/callback`。
