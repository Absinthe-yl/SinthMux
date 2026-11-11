# 运维与细节

README 之外的操作说明。快速开始和架构概览见 [README](README.md)，模块划分见 [harness.md](harness.md)。

## 公网部署

默认配置只绑定 `127.0.0.1`。对外提供服务时，需要自行准备 DNS、HTTPS 证书和反向代理，并把 `SINTHMUX_PUBLIC_URL` 设成浏览器和设备都能访问的地址。

设备代理在本机生成私钥，配对后获得 Hub 签发的 24 小时设备证书，每次连接用私钥签名一次性挑战。证书校验不依赖 TLS，因此可以经过反向代理，无需透传 TLS。设备 CA 私钥保存在数据库中——请只通过 HTTPS/WSS 暴露服务，并妥善保护 Hub、数据库及其备份。

## 局域网临时测试（手机）

同一 Wi-Fi 下用手机试本地开发版，在仓库根目录 `.env` 中保留 `SINTHMUX_PUBLIC_URL=http://127.0.0.1:5173`，并加入下面两项（IP 换成运行 Hub 的电脑当前局域网地址），然后重新运行 `./scripts/quickstart.sh`：

```dotenv
SINTHMUX_WEB_HOST=192.168.1.103
SINTHMUX_LAN_ORIGIN=http://192.168.1.103:5173
```

手机连接同一 Wi-Fi 后访问 `http://192.168.1.103:5173`。此方式仅用于可信局域网内的临时测试，登录令牌会通过未加密的 HTTP 传输。

## 更新或修复已接入的设备

在该设备上重新执行原接入命令即可：会保留已有设备 ID 和设备密钥，不重复配对，旧版设备自动换成设备证书。原命令不在手边时，把地址换成自己的 Hub 后运行：

```bash
curl -fsSL 'https://hub.example.com/install/connector.sh' | bash -s -- --hub 'https://hub.example.com' --repair
```

修复命令只适用于已经配对到同一 Hub 的设备。完成后回网页确认设备在线。日志位置：

- macOS 与普通后台模式：`~/.config/sinthmux/connector.log`
- Linux systemd 用户服务：`journalctl --user -u sinthmux-connector`

## 目标机器上的 tmux

接入命令按顺序准备 tmux：先用系统已有的；没有就从 Hub 下载随附版本（macOS 通用版、Linux 静态版、Windows 用兼容 tmux 的 [psmux](https://github.com/psmux/psmux)），并做 SHA-256 校验；Hub 未提供时再用 Homebrew、apt/dnf/yum/zypper/pacman/apk 或 winget 安装。

随附版本只放在用户目录（`~/.local/bin/tmux` 或 `%LOCALAPPDATA%\SinthMux\tmux.exe`），不改动系统。系统里自行安装的 psmux 不会被覆盖，需要按它的安装方式更新。

### 构建随附的 tmux bundle

Hub 镜像会打包 `deploy/bundles/` 中的 tmux。在一台 Mac 上运行（在 Linux/Windows 上运行会跳过 macOS 版本）：

```bash
./scripts/build-tmux-bundles.sh
```

脚本下载 Linux 静态版和 Windows 版 psmux、在本机编译 macOS 通用版，并生成 `SHA256SUMS`。GitHub 直连不通时可加 `SINTHMUX_GITHUB_MIRROR=https://gh-proxy.com/`。目录为空时 Hub 照常运行，接入命令改用系统包管理器安装 tmux。

## 代理后的登录限流

Docker 启动脚本会自动生成 `SINTHMUX_PROXY_SECRET`，让 Caddy 和 Hub 共享同一份随机密钥。Caddy 覆盖客户端传入的私有代理头，用实际连接 IP 作为限流依据；Hub 不直接信任 `X-Forwarded-For` 等公开头。

密钥只用于代理身份断言，不是用户或设备凭据，不应公开，也不要记录到访问日志。

已有部署直接用 Compose 更新时，先执行迁移（环境文件换成部署实际使用的那份）：

```bash
./scripts/ensure-proxy-secret.sh deploy/.env.local
```

迁移只在缺失时追加密钥，不替换数据库密码；Compose 缺少该配置时会拒绝启动并提示迁移。随后用原来的 Compose 文件组合重建 **Hub 和 Web**——公网部署必须保留 `docker-compose.public.yml`，不要改用仅本机的启动命令。

源码直连 Hub 可以不设置密钥，此时所有转发 IP 头均被忽略。自配代理需与 Hub 共享 `openssl rand -hex 32` 生成的值，并覆盖 `X-Sinthmux-Proxy-Secret` 和 `X-Sinthmux-Client-IP`；后者必须使用代理的实际连接 IP，不能照抄客户端请求头。多层代理/CDN 的真实 IP 信任链需另行配置，不会自动信任。

## 日常操作

**停止服务并保留数据库卷**

```bash
docker compose --env-file deploy/.env.local -f deploy/docker-compose.yml down
```

**恢复登录令牌**：部署者在 Hub 所在机器的仓库目录运行 `./scripts/recovery-token.sh`（多个用户时先列出，再带上用户 ID），得到 24 小时有效的恢复令牌。已有令牌、设备和会话不受影响。

**删除空间**：团队空间的 owner 在空间栏点击 **删除空间**，输入空间名称确认后，空间内的设备、成员和待用配对码一并删除，在线设备代理立即断开；设备上的 tmux 会话保留。个人空间不能删除。

**在本机终端接续原会话**：在运行 tmux 的设备上执行 `tmux attach -t '=会话名'`。

**设备代理的运行方式**：安装脚本会尝试配置 macOS LaunchAgent 或 Linux systemd 用户服务；没有用户服务管理器时会提示重启后如何手动启动。已有 tmux 会话不随浏览器或 Hub 的停止而结束。
