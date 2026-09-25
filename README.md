<div align="center">

<img src="apps/web/public/sinthmux-mark.png" alt="SinthMux" width="96" />

<h1>SinthMux</h1>

<p><strong>一个浏览器，接续每台设备上的终端工作</strong></p>

<p>把 Mac、Linux 主机和云服务器上的 tmux 会话汇聚到一个网页。终端任务留在原设备上运行，离开后随时回来接着做。</p>

<p><a href="#快速开始">快速开始</a> · <a href="#核心能力">核心能力</a> · <a href="#部署与运行">部署方式</a> · <a href="https://github.com/Absinthe-yl/SinthMux/issues">问题反馈</a></p>

</div>

SinthMux 是可自行部署的多设备终端工作台。无论是在笔记本上写代码、让 Codex 或 Claude Code 执行长任务，还是在云服务器上跑构建和测试，都可以从同一个页面查看设备、进入终端、管理会话。关闭网页不会中断 tmux 中的程序，换台设备打开浏览器，就能重新进入原来的工作现场。

## 核心能力

| | 能做什么 |
| --- | --- |
| 🕒 **终端持续运行** | 会话由目标机器上的 tmux 保存。网页关闭或短暂断网后，程序继续运行，回来重新进入即可。 |
| 🌐 **多设备集中管理** | 在一个页面查看设备在线状态和会话列表，创建、重命名、进入或关闭会话。 |
| ⚡ **一条命令接入设备** | 网页生成一次性接入命令；设备代理主动连接 Hub，不需要为目标机器开放入站端口。 |
| 👥 **个人与团队空间** | 用登录令牌或可选的 GitHub 登录进入，按空间组织设备和成员，并用角色权限控制操作。 |
| 💻 **保留原生工作方式** | 网页提供交互式终端；同一个 tmux 会话也能在目标机器的系统终端中使用。 |

## 快速开始

需要 Git、Docker Compose、curl 和 openssl。先在准备运行 Hub 的电脑上执行：

```bash
git clone https://github.com/Absinthe-yl/SinthMux.git
cd SinthMux
./scripts/docker-up.sh
```

打开 [http://127.0.0.1:5173](http://127.0.0.1:5173)。首次启动时，脚本会生成一个有效期为 24 小时的初始登录令牌。在登录页选择 **使用令牌登录**，并粘贴：

```bash
cat deploy/.bootstrap-token
```

登录后，打开 **登录令牌** 创建自己的常用令牌。然后点击 **添加设备**，输入设备名称，在目标机器的终端执行网页生成的命令。该机器需要 macOS 或 Linux（x86-64 / ARM64）、`tmux` 和 `curl`；无需安装 Go，也无需克隆本仓库。设备上线后，展开 **会话** 列表即可进入终端。

默认 Docker 配置只允许本机访问，因此首次体验请先把 **Hub 所在电脑**接入。要从另一台电脑接入设备或用手机访问，请将 Web 与 Hub 放在可访问的 HTTPS 地址下，并将 `SINTHMUX_PUBLIC_URL` 设置为该地址；设备代理通过出站连接接入，无需在设备上开放端口。

## 你可以这样使用

**让长任务继续运行**：在 tmux 会话里启动构建、测试、部署或 AI 编码工具。离开浏览器后，程序继续在目标机器上运行。

**在不同设备间接力**：在电脑上打开会话，换到另一台电脑或手机的浏览器后重新进入。会话保留在原来的机器上，终端重新附着即可。

**集中查看机器与会话**：从一个页面查看哪些设备在线、每台设备有哪些 tmux 会话，并按空间为团队成员分配操作权限。

## 它如何工作

```text
浏览器 ── HTTP / WebSocket ──▶ Hub ◀── 设备代理的出站连接 ──▶ tmux ──▶ 你的程序
                              │
                              └── PostgreSQL：用户、空间、设备与令牌
```

Hub 提供网页 API、设备管理和终端中继；设备代理运行在目标机器上，负责调用 tmux。浏览器进入终端时使用短期一次性票据，连接中断后重新取得票据并附着到原会话。Hub 是可信的中继组件，可以看到终端数据；SinthMux 当前不提供端到端加密。

## 部署与运行

| 方式 | 适合 | 入口 |
| --- | --- | --- |
| Docker Compose | 在本机启动 Web、Hub 和 PostgreSQL | `./scripts/docker-up.sh` |
| 源码运行 | 在单台电脑上试用或开发 | `make quickstart` |
| 公网多设备 | 从其他电脑和手机访问，接入不同网络的设备 | 自行配置 HTTPS 入口与 `SINTHMUX_PUBLIC_URL` |

源码运行需要 Go 1.25+、Node.js 20+、npm、tmux、curl、nc 和 make。全新克隆且未配置 `.env` 时，`make quickstart` 使用仅限本机的开发模式，网页无需登录；已有 `.env` 会由脚本加载。

再次运行 `./scripts/docker-up.sh` 可启动或更新 Docker 服务。停止服务且保留数据库卷：

```bash
docker compose --env-file deploy/.env.local -f deploy/docker-compose.yml down
```

设备代理在目标机器上独立运行。安装脚本会尝试配置 macOS LaunchAgent 或 Linux systemd 用户服务；如果没有用户服务管理器，会提示重启后如何手动启动。已有 tmux 会话不随浏览器或 Hub 的停止而结束。

### 公网部署提示

默认配置绑定 `127.0.0.1`。对外提供服务时，需要自行准备 DNS、HTTPS 证书和反向代理，并确保 `SINTHMUX_PUBLIC_URL` 是浏览器与设备都能访问的地址。设备代理使用配对后获得的 Bearer 凭据连接 Hub；请通过 HTTPS/WSS 暴露服务并妥善保护 Hub 和数据库。

## 常见问题

**关闭网页会结束终端里的任务吗？** 不会。任务在目标机器的 tmux 会话中运行；重新打开网页并进入该会话即可继续。显式关闭 tmux 会话会结束其中的程序。

**设备需要公网 IP 或开放 SSH 端口吗？** 不需要。设备代理主动连接可访问的 Hub；公网使用时 Hub 需要 HTTPS 入口。

**还能在本机终端使用原会话吗？** 可以。在运行 tmux 的设备上执行 `tmux attach -t '=会话名'`。

**支持哪些设备？** 设备代理安装脚本支持 macOS 和 Linux 的 x86-64、ARM64。浏览器端可从能够访问 Hub 的设备使用。

## 参与项目

欢迎通过 [Issues](https://github.com/Absinthe-yl/SinthMux/issues) 反馈问题、分享使用场景或提出建议。如果 SinthMux 帮你接续了跨设备的终端工作，也欢迎给仓库一个 Star。

想参与开发？Hub 与设备代理使用 Go，网页使用 React、TypeScript 和 xterm.js。模块入口与调用链见 [Harness](harness.md)。

```bash
make test   # Go 测试与 Web 类型检查
make build  # Go 与 Web 生产构建
```

设置 `SINTHMUX_TEST_DATABASE_URL` 后，测试还会运行 PostgreSQL 集成用例。
