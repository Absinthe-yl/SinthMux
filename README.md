<div align="center">

<img src="apps/web/public/sinthmux-mark.png" alt="SinthMux" width="96" />

<h1>SinthMux</h1>

<p><strong>一个浏览器，接续每台设备上的终端工作</strong></p>

<p>把 Mac、Windows、Linux 和云服务器上的 tmux 会话收进一个网页。任务留在原设备上运行，关掉网页也能随时回来接着做。</p>

<p><a href="#快速开始">快速开始</a> · <a href="#部署方式">部署方式</a> · <a href="OPERATIONS.md">运维与细节</a> · <a href="harness.md">开发文档</a> · <a href="https://github.com/Absinthe-yl/SinthMux/issues">问题反馈</a></p>

</div>

## 核心能力

| | 能做什么 |
| --- | --- |
| 🕒 **任务不中断** | 会话由目标机器上的 tmux 保存，关闭网页或短暂断网都不影响程序运行。 |
| 🌐 **多设备一个页面** | 查看设备在线状态和会话列表，创建、重命名、进入或关闭会话。 |
| ⚡ **一条命令接入** | 网页生成一次性接入命令，设备代理主动外连 Hub，无需开放入站端口。 |
| 👥 **空间与权限** | 用登录令牌或 GitHub 登录，按空间组织设备和成员，用角色控制操作。 |
| 💻 **不改变原有习惯** | 网页里是完整交互式终端；同一个会话也能在目标机器的系统终端中接续。 |

## 快速开始

需要 Git、Docker Compose、curl 和 openssl：

```bash
git clone https://github.com/Absinthe-yl/SinthMux.git
cd SinthMux
./scripts/docker-up.sh
```

1. 打开 [http://127.0.0.1:5173](http://127.0.0.1:5173)
2. 用初始令牌登录：`cat deploy/.bootstrap-token`（24 小时有效）
3. 点击 **添加设备**，按目标机器的系统执行生成的接入命令
4. 设备上线后，展开 **会话** 进入终端

登录后建议在 **登录令牌** 里创建自己的常用令牌（90 天有效，只显示一次）。默认配置只允许本机访问；要从其他设备或公网使用，见 [OPERATIONS.md](OPERATIONS.md)。

## 它如何工作

```text
浏览器 ── HTTP / WebSocket ──▶ Hub ◀── 设备代理的出站连接 ──▶ tmux ──▶ 你的程序
                              │
                              └── PostgreSQL：用户、空间、设备、证书与令牌
```

Hub 提供网页 API、设备管理和终端中继，设备代理运行在目标机器上调用 tmux。进入终端使用短期一次性票据，断线后重新取票附着到原会话。Hub 是可信中继，可以看到终端数据，当前不提供端到端加密。

## 部署方式

| 方式 | 适合 |
| --- | --- |
| Docker Compose | 本机启动 Web、Hub 和 PostgreSQL：`./scripts/docker-up.sh` |
| 源码运行 | 单机试用或开发：`make quickstart`（需 Go 1.25+、Node.js 20+） |
| 公网多设备 | 自行配置 HTTPS 入口并设置 `SINTHMUX_PUBLIC_URL` |

## 常见问题

**关闭网页会结束终端里的任务吗？** 不会。任务在目标机器的 tmux 会话里运行，重新进入即可继续。

**设备需要公网 IP 或开放端口吗？** 不需要。设备代理主动连接 Hub，公网使用只需 Hub 有 HTTPS 入口。

**忘了登录令牌怎么办？** 先找密码管理器里保存的令牌；都不在时，由部署者运行 `./scripts/recovery-token.sh` 签发一个 24 小时的恢复令牌。

**支持哪些系统？** macOS 11+、Linux、Windows 10 1809+ / Windows 11，x86-64 与 ARM64。Windows 使用 psmux，需要 3.4.0 或更新版本。

更多问题（删除空间、在本机接续会话、更新已接入设备）见 [OPERATIONS.md](OPERATIONS.md)。

## 参与开发

Hub 与设备代理使用 Go，网页使用 React、TypeScript 和 xterm.js。模块划分与调用链见 [harness.md](harness.md)。

```bash
make test   # Go 测试与 Web 类型检查
make build  # Go 与 Web 生产构建
```

设置 `SINTHMUX_TEST_DATABASE_URL` 后，测试还会运行 PostgreSQL 集成用例。欢迎通过 [Issues](https://github.com/Absinthe-yl/SinthMux/issues) 反馈问题或提出建议。
