# SinthMux

**一个浏览器，管理多台机器上的持久终端。**

SinthMux 让每台机器上的 Agent 主动连接 Hub，再通过网页查看和操作 tmux 会话。关闭浏览器、切换设备或短暂断线后，tmux 中的任务仍会运行；回到网页即可继续使用。

> **当前为本机试用版本。** Hub、Web 和 PostgreSQL 可以用 Docker 启动；Agent 仍需在运行 tmux 的机器上原生运行。设备配对与 mTLS 尚未完成，当前配置仅监听本机，请勿直接开放到公网。

## 功能

- **多设备管理**：查看 Agent 在线状态、系统平台和 tmux 会话。
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

首次启动时，脚本把 24 小时有效的初始登录令牌写入 `deploy/.bootstrap-token`。查看令牌并在登录页输入：

```bash
cat deploy/.bootstrap-token
```

登录后，在网页的 **登录令牌** 页面创建自己的长期令牌。数据库密码保存在 `deploy/.env.local`，数据库内容保存在 Docker 卷中；请保留这份配置文件，以便下次连接同一个数据库。这两个文件均不会提交到 Git。

### 2. 连接本机 Agent

Agent 需要在 **运行 tmux 的电脑上**原生运行。当前 Docker 部署只监听本机，因此先让 Agent 与 Hub 运行在同一台电脑。准备 Go 1.25+、tmux 和 make：

1. 在 SinthMux 网页中点击 **添加设备**，复制页面显示的设备 ID 和设备令牌。
2. 在仓库根目录创建被 Git 忽略的 `.env`，写入刚才得到的值：

   ```dotenv
   SINTHMUX_AGENT_DEVICE_ID=<设备 ID>
   SINTHMUX_AGENT_DEVICE_TOKEN=<设备令牌>
   SINTHMUX_AGENT_HUB_URL=ws://127.0.0.1:8090/ws/v1/agents/connect
   ```

3. 启动 Agent：

   ```bash
   make dev-agent
   ```

设备上线后，展开它的 **会话** 列表即可创建会话或进入终端。离开网页不会关闭 tmux 会话；在系统终端也可以使用 `tmux attach -t '=会话名'` 接回同一会话。

### 停止与重启

再次运行 `./scripts/docker-up.sh` 可启动或更新服务。停止 Docker 服务运行：

```bash
docker compose --env-file deploy/.env.local -f deploy/docker-compose.yml down
```

此命令保留数据库卷。原生 Agent 在其终端按 Ctrl+C 停止，已有 tmux 会话仍会运行。

## 不使用 Docker：源码试用

如果只想在一台电脑上体验功能，可安装 Go 1.25+、Node.js 20+、npm、tmux、curl、nc 和 make，然后运行：

```bash
make quickstart
```

脚本会构建 Hub 和 Agent、安装 Web 依赖并启动三个服务。打开 **[http://127.0.0.1:5173](http://127.0.0.1:5173)**；日志位于 `.run/log/`，按 Ctrl+C 停止服务。全新克隆且未配置 `.env` 时，此方式使用仅限本机的开发模式，网页无需登录。若已有 `.env`，脚本会读取其中的数据库和设备配置。

## 当前范围

| 能力 | 状态 |
| --- | --- |
| Agent 主动连接、心跳与重连 | 已实现 |
| tmux 会话管理与浏览器终端 | 已实现 |
| 令牌登录、GitHub 登录、空间和角色权限 | 已实现；GitHub 登录需配置 OAuth |
| PostgreSQL 持久化与本机 Docker 启动 | 已实现；Docker 容器仍待实机验证 |
| Agent 安装包、设备配对、mTLS、公网 HTTPS 部署 | 待完成 |

目前 Hub 是受信任的控制面，项目不提供端到端加密。Docker Compose 将 Hub 和 Web 绑定在 `127.0.0.1`；其他电脑和手机无法直接访问当前默认部署。公网多主机使用仍需完成设备配对、mTLS 和 HTTPS 入口。

## 工作方式

```text
Browser ── HTTP / WebSocket ──▶ Hub ── Agent 出站连接 ──▶ Agent ──▶ tmux
                                  │
                                  └── PostgreSQL：用户、空间、设备和令牌
```

Hub 使用 Go，Web 使用 React 和 xterm.js。Agent 在目标机器上调用 tmux，并通过 Hub 转发终端数据。浏览器连接终端时使用短期一次性票据；tmux 进程与浏览器连接相互独立。

## 开发

```bash
make test   # Go 测试与 Web 类型检查
make build  # Go 构建与 Web 生产构建
make proto  # 验证 Protobuf 定义，需要 protoc
```

设置 `SINTHMUX_TEST_DATABASE_URL` 后，`make test` 还会运行 PostgreSQL 集成测试。当前通信使用 JSON envelope；[Protobuf 定义](proto/sinthmux/v1/agent.proto)尚未接入运行链路。

GitHub 登录需为 Hub 配置 `SINTHMUX_GITHUB_CLIENT_ID`、`SINTHMUX_GITHUB_CLIENT_SECRET` 和 `SINTHMUX_PUBLIC_URL`。OAuth 回调地址为 `<SINTHMUX_PUBLIC_URL>/api/v1/auth/github/callback`。本机无需配置 GitHub OAuth，也可使用 SinthMux 登录令牌。
