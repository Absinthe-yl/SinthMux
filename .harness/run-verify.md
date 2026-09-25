# 运行、部署与验证入口

## 本机源码运行

仓库根目录运行 `make quickstart`：`scripts/quickstart.sh` 检查 Go、Node、npm、tmux、curl、nc 和端口，构建 Hub/Connector，安装 Web 依赖并启动三个进程。默认页面是 `http://127.0.0.1:5173`，Hub 监听 `127.0.0.1:8090`，日志写入 `.run/log/`。已有 `.env` 会被脚本加载；环境变量含义以 `internal/config/config.go` 为准。

## Docker 运行

`make docker-up` 调用 `scripts/docker-up.sh`，使用 `deploy/docker-compose.yml` 构建 PostgreSQL、Hub 和 Web。`deploy/Caddyfile` 将 `/api`、`/ws`、`/health`、安装与下载路径转给 Hub，其余路径作为 Web 静态资源。默认 Web 与 Hub 端口只绑定本机。首次初始化会把临时 Owner 用户令牌写入被 Git 忽略的 `deploy/.bootstrap-token`；数据库密码在 `deploy/.env.local`。设备代理不在 Compose 中，应在运行 tmux 的机器上安装。

公网接入需能访问的 HTTPS 入口和正确的 `SINTHMUX_PUBLIC_URL`，详见 `README.md` 与 `docs/DEPLOYMENT_SINTHE_TOP.md`。部署记录描述某次环境状态，改部署前以当前 Compose、Caddyfile 和运行环境为准。Login Broker 使用独立的 `deploy/login-broker.compose.yml`。

## 当前开发机的 GitHub 推送

这台 Mac 的系统 SOCKS 代理当前为 `127.0.0.1:7897`。遇到直接连接的 DNS 故障或 HTTP 代理的 TLS 中断时，已验证下面的命令可推送；`socks5h` 让域名由代理端解析，本次配合 `HTTP/1.1` 设置后推送成功：

```bash
git -c http.proxy=socks5h://127.0.0.1:7897 -c http.version=HTTP/1.1 push origin main
```

代理端口可能变化，重用前先用 `scutil --proxy` 查看 `SOCKSProxy`、`SOCKSPort` 和 `SOCKSEnable`。此设置仅作用于单次 Git 命令，不修改仓库或全局 Git 配置。

## 验证矩阵

| 变更范围 | 优先命令或证据 |
| --- | --- |
| Go 模块 | `go test ./...`；相关包的 `*_test.go` |
| Web 类型 | `npm --prefix apps/web run typecheck` |
| Go 与 Web 构建 | `make build` |
| 全仓基本检查 | `make test`，运行 Go 测试和 Web 类型检查 |
| PostgreSQL 认证链路 | 设置 `SINTHMUX_TEST_DATABASE_URL` 后运行 `go test ./internal/auth -run TestFormalStoreIntegration`；未设置时该测试会跳过 |
| tmux 命令链路 | `go test ./apps/connector`；本机未安装 tmux 时生命周期测试会跳过 |
| Protobuf 定义 | `make proto`，需 `protoc`；不验证当前 JSON 运行协议 |
| 浏览器到设备的端到端行为 | 启动 Hub、Web、Connector 后实际创建、打开、断线重连和关闭 tmux 会话；当前无 Web E2E 自动化套件 |

## 协作检查

改动前按 `harness.md` 找到目标模块及调用链两端；改动后运行与变更范围匹配的检查，并记录跳过或缺失的外部依赖。新增模块、入口、协议消息或运行约束时更新相应 `.harness/` 文件和根索引。不要把 `docs/IMPLEMENTATION_PLAN.md` 中尚未落地的设计写成当前行为。
