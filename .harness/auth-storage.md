# 身份、权限与持久化

## 何时启用

Hub 只有在 `SINTHMUX_DATABASE_URL` 非空时才创建 `auth.Server`。`internal/auth/store.go:Open` 连接 PostgreSQL 并执行建表迁移。无数据库的开发模式没有用户登录与空间授权，只能绑定 loopback；不要把该模式的行为当成正式权限模型。

## 数据与入口

| 数据或能力 | 主要位置 |
| --- | --- |
| 用户、个人/团队空间、成员角色 | `internal/auth/store.go`；HTTP 入口在 `internal/auth/http.go` |
| 初始 Owner 令牌 | `apps/hub/main.go` 的 `bootstrap-token` 命令；`Store.BootstrapOwner`，首次数据库初始化使用 |
| 恢复令牌 | `apps/hub/recovery.go` 的 `recovery-token [用户ID]` 命令（`scripts/recovery-token.sh` 包装 docker compose exec）；`Store.Users`、`Store.RecoveryToken` 为已有用户签发 24 小时令牌，不动其他凭据，写审计 `auth.recovery_token` |
| 删除空间 | `DELETE /api/v1/spaces/{id}`：`Server.deleteSpace` 仅 owner、仅团队空间（个人空间返回 409）；`Store.DeleteSpace` 事务内删除，成员/设备/配对码依赖 `ON DELETE CASCADE`，随后对每台设备调用 `OnDeviceRevoked` 断开在线连接 |
| 用户令牌、Web session 与 Cookie | `Store.NewToken`、`NewSession`、`Session`；`Server.Require`、`tokenLogin` |
| 设备、一次性配对码、撤销 | `Store.NewDevicePairing`、`RedeemDevicePairing`、`AuthenticateDevice`、`RevokeDevice` |
| 设备 CA、证书签发与持有证明 | `internal/devicecert`；`Store.DeviceAuthority`、`AuthenticateDeviceKey`、`BindDeviceKey`；HTTP 入口在 `internal/auth/device_proof.go` |
| 审计 | `Store.Audit`、`AuditEvents`；会话操作在 Hub 路由包装器中记录结果 |

表由 `Store.Migrate` 创建：`users`、`spaces`、`memberships`、`login_tokens`、`web_sessions`、`devices`、`device_pairings`、`audit_events`、`device_authority`。设备 CA 私钥保存在 `device_authority`，首次启动生成；数据库备份即包含 CA，需按密钥对待。`devices.public_key_hash` 登记设备公钥指纹，`secret_hash` 只在旧版令牌设备上存在，首次证书连接后清空。令牌和配对码以摘要形式保存。修改表或授权查询时，优先读对应 `Store` 方法及 `internal/auth/auth_test.go` 的数据库集成测试。

## 请求权限链

- `Server.Require` 从 `sinthmux_session` Cookie 取得 Web session；写操作还检查 Origin 与 `X-Sinthmux-CSRF`。前端 `api.ts` 从 `/api/v1/auth/me` 获取 CSRF 并在写请求带上。
- `Server.Device` 根据设备所属空间和成员角色检查操作权限。`Store.Allowed` 是动作与角色的集中矩阵：viewer 仅列会话；operator 可输入、创建和重命名；owner/admin 另可关闭会话、管理设备、查看审计。成员与空间管理另有 owner 权限检查。
- 终端连接不只依赖签发票据时的权限：`relay.TerminalHandler` 将票据绑定当前浏览器 session，并通过 `Store.TerminalAllowed` 在连接过程中复核可输入权限；撤权可终止现有连接。
- 设备代理使用与用户登录令牌不同的设备凭据：Hub 签发、URI SAN 绑定 `spaceId/deviceId` 的短期证书，加每次握手对一次性 nonce 的签名。签名内容包含用途、Hub 主机名和设备 ID，nonce 只能用一次。证书本身不足以认证：每次连接还要求数据库中设备未吊销且公钥指纹一致，所以吊销立即生效，不需要 CRL。配对码在事务中一次性消费；设备吊销后 Hub 也主动断开在线连接。

## 变更检查

改角色、成员、设备或终端权限时，同时检查 `Store.Allowed`、相关 HTTP handler、Hub 的 `wrap` 路由、`TerminalAllowed`、前端按钮显隐和 `auth_test.go`。前端限制只改善交互，服务端权限是实际边界。
