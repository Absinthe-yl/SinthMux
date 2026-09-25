# Login Broker：可选集中 GitHub 登录

Login Broker 是独立服务，不是 Hub 运行的必需组件。`apps/login-broker/main.go` 读取公开 URL、GitHub OAuth 凭据和 Ed25519 签名种子，支持 `keygen` 命令；`internal/loginbroker/server.go` 实现 `/health`、`/start`、`/callback`、`/exchange`。

## 登录链路

1. 浏览器访问 Hub 的 `/api/v1/auth/github/start`；若配置了 Broker，`internal/auth/broker_http.go` 创建 state，跳转到 Broker `/start`。
2. Broker 跳转 GitHub OAuth；GitHub 回调后，Broker 读取用户身份，签发有有效期和目标 Hub 绑定信息的 Ed25519 票据，并把一次性兑换码送回 Hub 回调地址。
3. Hub 从 Broker `/exchange` 以兑换码取票据，通过配置的公钥、受众和 state 验证；随后在本地数据库建立或读取 GitHub 用户，并创建 Web session。

GitHub 访问令牌用于 Broker 向 GitHub 查询身份，不作为 Hub 的登录凭据。Hub 也保留直接配置 GitHub OAuth 的路径，以及始终可用的用户令牌登录路径。修改登录流程时检查两种 GitHub 路径及令牌登录是否仍可用。

## 配置和验证

- Broker：`SINTHMUX_BROKER_PUBLIC_URL`、`SINTHMUX_BROKER_SIGNING_SEED`、`SINTHMUX_GITHUB_CLIENT_ID`、`SINTHMUX_GITHUB_CLIENT_SECRET`；Compose 见 `deploy/login-broker.compose.yml`。
- Hub：`SINTHMUX_AUTH_BROKER_URL`、`SINTHMUX_AUTH_BROKER_PUBLIC_KEY`、`SINTHMUX_PUBLIC_URL`；具体示例见 `README.md`。
- 测试：`internal/loginbroker/server_test.go`、`internal/auth/auth_test.go` 的 Broker 路径。票据格式与验签在 `internal/auth/broker_ticket.go`。
