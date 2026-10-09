<div align="center">

<img src="apps/web/public/sinthmux-mark.png" alt="SinthMux" width="96" />

<h1>SinthMux</h1>

<p><strong>一个浏览器，接续每台设备上的终端工作</strong></p>

<p>把 Mac、Windows、Linux 主机和云服务器上的 tmux 会话汇聚到一个网页。终端任务留在原设备上运行，离开后随时回来接着做。</p>

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

打开 [http://127.0.0.1:5173](http://127.0.0.1:5173)。首次启动时，脚本会生成一个有效期为 24 小时的初始登录令牌。把它粘贴到登录页的输入框里：

```bash
cat deploy/.bootstrap-token
```

登录后，打开 **登录令牌** 创建自己的常用令牌（有效期 90 天，只显示一次，请存进密码管理器；登录页也会让浏览器记住它）。然后点击 **添加设备**，输入设备名称，按目标机器的系统选择命令：macOS / Linux 在终端执行，Windows 在 PowerShell 执行。无需安装 Go，也无需克隆本仓库。设备上线后，展开 **会话** 列表即可进入终端。

目标机器不需要预先安装 tmux。接入命令按以下顺序准备：先用系统已有的 tmux；没有就从 Hub 下载随附的 tmux（macOS 通用版、Linux 静态版、Windows 用兼容 tmux 的 [psmux](https://github.com/psmux/psmux)），并用 SHA-256 校验；Hub 未提供时再用 Homebrew、apt/dnf/yum/zypper/pacman/apk 或 winget 安装。随附的 tmux 只放在用户目录（`~/.local/bin/tmux` 或 `%LOCALAPPDATA%\SinthMux\tmux.exe`），不改动系统。

**更新或修复已接入的设备**：在该设备上重新执行原接入命令，会保留已有设备 ID 和设备密钥，不重复配对；旧版设备会自动换成设备证书。原命令不在手边时，也可以将下面的地址替换为自己的 Hub 地址后运行：

```bash
curl -fsSL 'https://hub.example.com/install/connector.sh' | bash -s -- --hub 'https://hub.example.com' --repair
```

修复命令只适用于已经配对到同一 Hub 的设备。完成后回到网页确认设备在线；macOS 和普通后台模式的运行日志在 `~/.config/sinthmux/connector.log`，systemd 用户服务可通过 `journalctl --user -u sinthmux-connector` 查看。

默认 Docker 配置只允许本机访问，因此首次体验请先把 **Hub 所在电脑**接入。要从另一台电脑接入设备或用手机访问，请将 Web 与 Hub 放在可访问的 HTTPS 地址下，并将 `SINTHMUX_PUBLIC_URL` 设置为该地址；设备代理通过出站连接接入，无需在设备上开放端口。

**同一 Wi-Fi 下临时用手机测试本地开发版**：在仓库根目录的 `.env` 中保留 `SINTHMUX_PUBLIC_URL=http://127.0.0.1:5173`，并加入以下两项（IP 换成运行 Hub 的电脑当前局域网地址），再重新运行 `./scripts/quickstart.sh`：

```dotenv
SINTHMUX_WEB_HOST=192.168.1.103
SINTHMUX_LAN_ORIGIN=http://192.168.1.103:5173
```

手机连接同一 Wi-Fi 后访问 `http://192.168.1.103:5173`。此方式仅用于可信局域网内的临时测试，登录令牌会通过未加密的 HTTP 传输；公网访问仍需 HTTPS。

## 你可以这样使用

**让长任务继续运行**：在 tmux 会话里启动构建、测试、部署或 AI 编码工具。离开浏览器后，程序继续在目标机器上运行。

**在不同设备间接力**：在电脑上打开会话，换到另一台电脑或手机的浏览器后重新进入。会话保留在原来的机器上，终端重新附着即可。

**集中查看机器与会话**：从一个页面查看哪些设备在线、每台设备有哪些 tmux 会话，并按空间为团队成员分配操作权限。

## 它如何工作

```text
浏览器 ── HTTP / WebSocket ──▶ Hub ◀── 设备代理的出站连接 ──▶ tmux ──▶ 你的程序
                              │
                              └── PostgreSQL：用户、空间、设备、证书与令牌
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

### 随附 tmux

Hub 镜像会打包 `deploy/bundles/` 中的 tmux。构建镜像前在一台 Mac 上运行（Linux 上运行时跳过 macOS 版本）：

```bash
./scripts/build-tmux-bundles.sh
```

脚本下载 Linux 静态版和 Windows 版 psmux，在本机编译 macOS 通用版，并生成 `SHA256SUMS`。目录为空时 Hub 照常运行，接入命令改用系统包管理器安装 tmux。

### 公网部署提示

默认配置绑定 `127.0.0.1`。对外提供服务时，需要自行准备 DNS、HTTPS 证书和反向代理，并确保 `SINTHMUX_PUBLIC_URL` 是浏览器与设备都能访问的地址。设备代理在本机生成私钥，配对后获得 Hub 签发的 24 小时设备证书，每次连接用私钥签名一次性挑战；证书可经过反向代理，无需透传 TLS。设备 CA 私钥保存在数据库中，请通过 HTTPS/WSS 暴露服务并妥善保护 Hub 和数据库及其备份。

## 常见问题

**关闭网页会结束终端里的任务吗？** 不会。任务在目标机器的 tmux 会话中运行；重新打开网页并进入该会话即可继续。显式关闭 tmux 会话会结束其中的程序。

**设备需要公网 IP 或开放 SSH 端口吗？** 不需要。设备代理主动连接可访问的 Hub；公网使用时 Hub 需要 HTTPS 入口。

**忘了登录令牌怎么办？** 先在浏览器或系统的密码管理器里搜索 “SinthMux”。还有一台已登录的设备时，在那里新建一个令牌即可。都没有时，由部署者在 Hub 所在机器的仓库目录运行 `./scripts/recovery-token.sh`（多个用户时先列出，再带上用户 ID），得到一个 24 小时有效的恢复令牌；已有的令牌、设备和会话不受影响。

**能删除空间吗？** 团队空间的 owner 可以在空间栏点击 **删除空间**，输入空间名称确认后，空间内的设备、成员和待用配对码一并删除，在线设备代理立即断开；设备上的 tmux 会话保留。每个用户的个人空间不能删除。

**还能在本机终端使用原会话吗？** 可以。在运行 tmux 的设备上执行 `tmux attach -t '=会话名'`。

**支持哪些设备？** macOS 11+、Linux 和 Windows 10 1809+ / Windows 11，x86-64 与 ARM64。Windows 上的会话由 psmux 保存，用 ConPTY 接入网页终端，可用 `tmux attach -t 会话名` 在本机 PowerShell 中接续。浏览器端可从能够访问 Hub 的任意设备使用。

## 参与项目

欢迎通过 [Issues](https://github.com/Absinthe-yl/SinthMux/issues) 反馈问题、分享使用场景或提出建议。如果 SinthMux 帮你接续了跨设备的终端工作，也欢迎给仓库一个 Star。

想参与开发？Hub 与设备代理使用 Go，网页使用 React、TypeScript 和 xterm.js。模块入口与调用链见 [Harness](harness.md)。

```bash
make test   # Go 测试与 Web 类型检查
make build  # Go 与 Web 生产构建
```

设置 `SINTHMUX_TEST_DATABASE_URL` 后，测试还会运行 PostgreSQL 集成用例。
