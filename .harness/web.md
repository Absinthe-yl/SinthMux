# Web：页面、API 与浏览器终端

Web 位于 `apps/web/`，使用 React 19、TypeScript、Vite、React Query 和 xterm.js。`src/main.tsx` 挂载应用，`src/App.tsx` 管理登录、空间、设备和页面级状态。当前没有独立路由库，终端由 App 的活动终端状态切换显示。

## 文件索引

| 文件 | 职责 |
| --- | --- |
| `src/App.tsx` | 登录页、主题、当前空间、设备列表、成员、令牌、配对命令和设备卡片 |
| `src/SessionPanel.tsx` | 会话查询、创建、重命名、关闭、打开；按在线状态、能力和角色显示操作 |
| `src/TerminalView.tsx` | xterm 实例、尺寸同步、票据申请、WebSocket 连接与退避重连 |
| `src/api.ts` | 同源 `fetch`、Cookie、CSRF header 与 API 错误包装 |
| `src/styles.css` | 页面和终端样式；主题变量 |
| `vite.config.ts` | 开发期将 `/api`、`/ws`、安装与下载路径代理到本机 Hub |

## 页面数据流

`App.tsx` 先读取 `/api/v1/system/status`，识别正式/开发模式。正式模式用 `/api/v1/auth/me` 取得用户、空间和 CSRF；设备列表按当前空间筛选。展开设备后 `SessionPanel` 查询会话，在线时定期刷新；操作成功后使 React Query 缓存失效。tmux 缺失或旧代理无法解析会话列表时，页面展示可复制的修复命令。

打开会话时 `TerminalView` 先 POST 申请票据，再建立终端 WebSocket。输入以二进制发送，resize 以 JSON 文本发送；断线后重新申请票据并连接，不能重复使用旧票据。修改这段流程时对照 `internal/relay/terminal.go`。

## 开发定位

- API 字段或权限改变：同步检查 `App.tsx`、`SessionPanel.tsx` 与 `src/api.ts`；服务端仍是权限真相。
- 终端断线、尺寸或输入问题：先看 `TerminalView.tsx`，再看 `relay/terminal.go` 和 Connector 的 `terminal.go`。
- 前端验证：`npm --prefix apps/web run typecheck`、`npm --prefix apps/web run build`。仓库当前没有 Web 自动化测试套件。
