import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronDown, ChevronUp, Laptop, Moon, RefreshCw, Server, Sun } from "lucide-react";
import { FormEvent, lazy, Suspense, useLayoutEffect, useState } from "react";
import { request, setCSRF } from "./api";
import SessionPanel from "./SessionPanel";

const TerminalView = lazy(() => import("./TerminalView"));

type Device = {
  id: string;
  spaceId?: string;
  name: string;
  platform: string;
  architecture: string;
  connectorVersion: string;
  status: string;
  capabilities?: string[];
};
type Status = { status: string; version: string; connectedDevices: number; authMode: "formal" | "development"; githubLoginEnabled: boolean };
type Space = { id: string; name: string; kind: string; role: "owner" | "admin" | "operator" | "viewer" };
type Me = { user: { id: string; name: string; githubId?: number }; spaces: Space[]; csrf: string };
type Member = { userId: string; name: string; role: Space["role"] };
type LoginToken = { id: string; name: string; expiresAt: string; lastUsedAt?: string };
type Theme = "light" | "dark";

function shellQuote(value: string): string { return "'" + value.replaceAll("'", "'\\''") + "'"; }

function initialTheme(): Theme {
  try {
    const saved = localStorage.getItem("sinthmux-theme");
    if (saved === "light" || saved === "dark") return saved;
  } catch { /* Storage may be unavailable. */ }
  return window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
}

async function getJSON<T>(path: string): Promise<T> {
  return request<T>(path);
}

function Login({ githubEnabled, theme, onToggleTheme, onLogin }: { githubEnabled: boolean; theme: Theme; onToggleTheme: () => void; onLogin: () => void }) {
  const [showToken, setShowToken] = useState(false);
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [githubNotice, setGithubNotice] = useState(false);
  async function submit(event: FormEvent) {
    event.preventDefault(); setBusy(true); setError("");
    try { await request("/api/v1/auth/token", { method: "POST", body: JSON.stringify({ token }) }); setToken(""); onLogin(); }
    catch (err) { setError(err instanceof Error ? err.message : "登录失败"); }
    finally { setBusy(false); }
  }
  return <main className="login-shell">
    <button className="login-theme icon-button" type="button" title={theme === "dark" ? "切换浅色模式" : "切换深色模式"} aria-label={theme === "dark" ? "切换浅色模式" : "切换深色模式"} onClick={onToggleTheme}>{theme === "dark" ? <Sun /> : <Moon />}</button>
    <section className="login-card">
      <img className="login-mark" src="/sinthmux-mark.png" alt="" />
      <h1>SinthMux</h1>
      <p>终端继续运行，回来接着用。</p>
      {githubEnabled
        ? <a className="github-login" href="/api/v1/auth/github/start">使用 GitHub 登录</a>
        : <button className="github-login" type="button" onClick={() => setGithubNotice(true)}>使用 GitHub 登录</button>}
      {githubNotice && !githubEnabled && <div className="notice">当前服务尚未配置 GitHub 登录。请由部署者配置 OAuth 或统一登录服务。</div>}
      {showToken ? <form className="login-token-form" onSubmit={(event) => void submit(event)}>
        <label htmlFor="login-token">用户令牌</label>
        <input id="login-token" type="password" autoComplete="off" value={token} onChange={(event) => setToken(event.target.value)} required autoFocus />
        <p className="login-token-help">首次使用？在部署机器的仓库目录运行 <code>./scripts/docker-up.sh</code>，再用 <code>cat deploy/.bootstrap-token</code> 查看初始令牌。</p>
        <button type="submit" disabled={busy}>{busy ? "登录中…" : "使用令牌登录"}</button>
        {error && <div className="notice error">{error}</div>}
      </form> : <button className="login-token-toggle" type="button" onClick={() => setShowToken(true)}>使用令牌登录</button>}
    </section>
  </main>;
}

function DeviceCard({ device, expanded, onToggle, onOpen, onRevoke, role }: { device: Device; expanded: boolean; onToggle: () => void; onOpen: (session: string) => void; onRevoke?: () => void; role?: Space["role"] }) {
  const online = device.status === "online";
  return <article className={`device-card${expanded ? " expanded" : ""}`}>
    <div className="device-main">
      <div className="device-icon" aria-hidden="true">{device.platform === "darwin" ? <Laptop /> : <Server />}</div>
      <div className="device-info">
        <div className="device-title"><h2>{device.name}</h2><span className={`status-dot${online ? " online" : ""}`} /><span className="status-text">{online ? "在线" : "离线"}</span></div>
        <p>{device.platform} / {device.architecture} <span>·</span> 设备代理 {device.connectorVersion}</p>
      </div>
      {onRevoke && <button className="device-revoke" type="button" onClick={onRevoke}>移除</button>}<button className="device-toggle" type="button" aria-expanded={expanded} onClick={onToggle}>{expanded ? "收起" : "会话"}{expanded ? <ChevronUp /> : <ChevronDown />}</button>
    </div>
    {expanded && <SessionPanel deviceId={device.id} online={online} canManage={(device.capabilities ?? []).includes("tmux.sessions.manage") && role !== "viewer"} canClose={role === undefined || role === "owner" || role === "admin"} canOpen={role !== "viewer"} onOpen={onOpen} />}
  </article>;
}

export default function App() {
  const queryClient = useQueryClient();
  const [theme, setTheme] = useState<Theme>(initialTheme);
  const [selectedDevice, setSelectedDevice] = useState<string | null>(null);
  const [selectedSpace, setSelectedSpace] = useState<string | null>(null);
  const [activeTerminal, setActiveTerminal] = useState<{ deviceId: string; deviceName: string; session: string } | null>(null);
  const [notice, setNotice] = useState("");
  const [secret, setSecret] = useState<{ label: string; value: string; hint?: string } | null>(null);
  const [secretCopied, setSecretCopied] = useState(false);
  const [showMembers, setShowMembers] = useState(false);
  const [showTokens, setShowTokens] = useState(false);
  useLayoutEffect(() => {
    document.documentElement.dataset.theme = theme;
    try { localStorage.setItem("sinthmux-theme", theme); } catch { /* Theme still works for this visit. */ }
  }, [theme]);

  const status = useQuery({ queryKey: ["status"], queryFn: () => getJSON<Status>("/api/v1/system/status"), refetchInterval: 5000 });
  const formal = status.data?.authMode === "formal";
  const me = useQuery({ queryKey: ["me"], enabled: formal, retry: false, queryFn: async () => { const result = await getJSON<Me>("/api/v1/auth/me"); setCSRF(result.csrf); return result; } });
  const devices = useQuery({ queryKey: ["devices"], enabled: status.data?.authMode === "development" || (formal && !!me.data), queryFn: () => getJSON<{ devices: Device[] }>("/api/v1/devices"), refetchInterval: 5000 });
  const activeSpace = me.data?.spaces.find((space) => space.id === selectedSpace) ?? me.data?.spaces[0];
  const members = useQuery({ queryKey: ["members", activeSpace?.id], enabled: formal && showMembers && !!activeSpace, queryFn: () => request<{ members: Member[] }>(`/api/v1/spaces/${activeSpace!.id}/members`) });
  const tokens = useQuery({ queryKey: ["tokens"], enabled: formal && showTokens && !!me.data, queryFn: () => request<{ tokens: LoginToken[] }>("/api/v1/auth/tokens") });
  const items = (devices.data?.devices ?? []).filter((device) => !formal || device.spaceId === activeSpace?.id);
  const hubOnline = !status.isError && status.data?.status === "ok";

  async function runAction(action: () => Promise<void>) { setNotice(""); try { await action(); await queryClient.invalidateQueries(); } catch (error) { setNotice(error instanceof Error ? error.message : "操作失败"); } }
  async function createToken() { const name = window.prompt("令牌名称", "我的登录令牌"); if (!name) return; await runAction(async () => { const result = await request<{ token: string }>("/api/v1/auth/tokens", { method: "POST", body: JSON.stringify({ name }) }); setSecretCopied(false); setSecret({ label: "登录令牌（仅显示一次）", value: result.token }); }); }
  async function createSpace() { const name = window.prompt("空间名称"); if (!name) return; await runAction(async () => { const space = await request<Space>("/api/v1/spaces", { method: "POST", body: JSON.stringify({ name }) }); setSelectedSpace(space.id); }); }
  async function createDevice() { if (!activeSpace) return; const name = window.prompt("设备名称"); if (!name) return; await runAction(async () => { const result = await request<{ code: string; expiresAt: string; hubUrl: string }>(`/api/v1/spaces/${activeSpace.id}/device-pairings`, { method: "POST", body: JSON.stringify({ name }) }); const hub = result.hubUrl.replace(/\/$/, ""); const curlOptions = hub.startsWith("https://") ? " --proto '=https' --proto-redir '=https'" : ""; const command = `curl -fsSL${curlOptions} ${shellQuote(`${hub}/install/connector.sh`)} | bash -s -- --hub ${shellQuote(hub)} --code ${shellQuote(result.code)}`; const localOnly = new URL(hub).hostname === "127.0.0.1" || new URL(hub).hostname === "localhost"; setSecretCopied(false); setSecret({ label: `${name} 的接入命令`, value: command, hint: localOnly ? "配对码 5 分钟有效、只能用一次。当前 Hub 地址仅本机可访问；另一台电脑接入前需将 Hub 部署到可访问的 HTTPS 地址。目标电脑需要安装 tmux 和 curl。" : "配对码 5 分钟有效、只能用一次。在目标电脑终端执行；该电脑需要安装 tmux 和 curl。" }); }); }
  async function addMember() { if (!activeSpace) return; const name = window.prompt("成员名称"); if (!name) return; const role = window.prompt("角色：admin、operator 或 viewer", "operator")?.toLowerCase(); if (!role) return; await runAction(async () => { const result = await request<{ token: string }>(`/api/v1/spaces/${activeSpace.id}/members`, { method: "POST", body: JSON.stringify({ name, role }) }); setSecretCopied(false); setSecret({ label: `${name} 的初始登录令牌（仅显示一次）`, value: result.token }); }); }
  async function addGithubMember() { if (!activeSpace) return; const githubId = Number(window.prompt("对方的 GitHub 数字 ID（需先登录 SinthMux）")); if (!Number.isSafeInteger(githubId) || githubId <= 0) return; const role = window.prompt("角色：admin、operator 或 viewer", "operator")?.toLowerCase(); if (!role) return; await runAction(async () => { await request(`/api/v1/spaces/${activeSpace.id}/members`, { method: "POST", body: JSON.stringify({ githubId, role }) }); }); }
  async function changeRole(member: Member) { if (!activeSpace) return; const role = window.prompt(`设置 ${member.name} 的角色：admin、operator 或 viewer`, member.role)?.toLowerCase(); if (!role || role === member.role) return; await runAction(async () => { await request(`/api/v1/spaces/${activeSpace.id}/members/${member.userId}`, { method: "PATCH", body: JSON.stringify({ role }) }); }); }
  async function removeMember(member: Member) { if (!activeSpace || !window.confirm(`移除成员 ${member.name}？`)) return; await runAction(async () => { await request(`/api/v1/spaces/${activeSpace.id}/members/${member.userId}`, { method: "DELETE" }); }); }
  async function revokeToken(token: LoginToken) { if (!window.confirm(`撤销登录令牌“${token.name}”？`)) return; await runAction(async () => { await request(`/api/v1/auth/tokens/${token.id}`, { method: "DELETE" }); }); }
  async function revokeDevice(device: Device) { if (!activeSpace || !window.confirm(`移除设备“${device.name}”？设备代理将立即断开。`)) return; await runAction(async () => { await request(`/api/v1/spaces/${activeSpace.id}/devices/${device.id}`, { method: "DELETE" }); }); }
  async function logout() { await runAction(async () => { await request("/api/v1/auth/logout", { method: "POST" }); setCSRF(""); setSecret(null); setActiveTerminal(null); }); }

  if (formal && me.isError) return <Login githubEnabled={status.data?.githubLoginEnabled ?? false} theme={theme} onToggleTheme={() => setTheme(theme === "dark" ? "light" : "dark")} onLogin={() => { void me.refetch(); }} />;

  return <div className="app-frame">
    <header className="topbar"><div className="topbar-inner">
      <a className="brand" href="#top" aria-label="SinthMux 首页"><img className="brand-mark" src="/sinthmux-mark.png" alt="" /><strong>SinthMux</strong></a>
      <div className="top-actions">{me.data && <span className="user-name">{me.data.user.name}</span>}<span className="hub-status"><span className={`status-dot${hubOnline ? " online" : ""}`} />Hub {hubOnline ? "在线" : status.isError ? "离线" : "连接中"}</span><button className="icon-button" type="button" title={theme === "dark" ? "切换浅色模式" : "切换深色模式"} aria-label={theme === "dark" ? "切换浅色模式" : "切换深色模式"} onClick={() => setTheme(theme === "dark" ? "light" : "dark")}>{theme === "dark" ? <Sun /> : <Moon />}</button></div>
    </div></header>

    <main className={`shell${activeTerminal ? " terminal-shell" : ""}`} id="top">
      {activeTerminal ? <Suspense fallback={<div className="session-note">正在打开终端…</div>}><TerminalView key={`${activeTerminal.deviceId}:${activeTerminal.session}`} {...activeTerminal} theme={theme} onBack={() => setActiveTerminal(null)} /></Suspense> : <>
      <div className="page-heading"><div><h1>设备 <span>{items.length}</span></h1></div><button className="refresh-button" type="button" onClick={() => { void status.refetch(); void devices.refetch(); }}><RefreshCw />刷新</button></div>
      {formal && me.data && <div className="workspace-bar"><label>空间 <select aria-label="当前空间" value={activeSpace?.id ?? ""} onChange={(event) => { setSelectedSpace(event.target.value); setSelectedDevice(null); }}><option value="" disabled>选择空间</option>{me.data.spaces.map((space) => <option value={space.id} key={space.id}>{space.name} · {space.role}</option>)}</select></label><button type="button" onClick={() => void createSpace()}>新建空间</button>{activeSpace && (activeSpace.role === "owner" || activeSpace.role === "admin") && <button type="button" onClick={() => void createDevice()}>添加设备</button>}{activeSpace && <button type="button" onClick={() => setShowMembers(!showMembers)}>成员</button>}<button type="button" onClick={() => setShowTokens(!showTokens)}>登录令牌</button><button type="button" onClick={() => void logout()}>退出</button></div>}
      {formal && showMembers && activeSpace && <div className="management-panel"><div className="management-heading"><strong>成员</strong>{me.data?.user.githubId && <span>我的 GitHub ID：{me.data.user.githubId}</span>}{activeSpace.role === "owner" && <><button type="button" onClick={() => void addMember()}>添加令牌成员</button><button type="button" onClick={() => void addGithubMember()}>添加 GitHub 成员</button></>}</div>{members.data?.members.map((member) => <div className="management-row" key={member.userId}><span>{member.name} · {member.role}</span>{activeSpace.role === "owner" && member.role !== "owner" && <><button type="button" onClick={() => void changeRole(member)}>修改角色</button><button type="button" onClick={() => void removeMember(member)}>移除</button></>}</div>)}</div>}
      {formal && showTokens && <div className="management-panel"><div className="management-heading"><strong>我的登录令牌</strong><button type="button" onClick={() => void createToken()}>新建令牌</button></div>{tokens.data?.tokens.map((token) => <div className="management-row" key={token.id}><span>{token.name} · {new Date(token.expiresAt).toLocaleDateString()}</span><button type="button" onClick={() => void revokeToken(token)}>撤销</button></div>)}</div>}
      {secret && <div className="secret-panel"><strong>{secret.label}</strong><pre>{secret.value}</pre>{secret.hint && <p>{secret.hint}</p>}<button type="button" onClick={() => { void navigator.clipboard.writeText(secret.value).then(() => setSecretCopied(true)).catch(() => setNotice("复制失败，请手动选中内容复制。")); }}>{secretCopied ? "已复制" : "复制"}</button><button type="button" onClick={() => setSecret(null)}>关闭</button></div>}
      {notice && <div className="notice error">{notice}</div>}
      {devices.isError && <div className="notice error">无法读取设备列表，请确认 Hub 已启动。</div>}
      <section className="device-list" aria-label="设备列表">{items.length ? items.map((device) => <DeviceCard key={device.id} device={device} role={formal ? activeSpace?.role : undefined} onRevoke={formal && (activeSpace?.role === "owner" || activeSpace?.role === "admin") ? () => { void revokeDevice(device); } : undefined} expanded={selectedDevice === device.id} onToggle={() => setSelectedDevice(selectedDevice === device.id ? null : device.id)} onOpen={(session) => setActiveTerminal({ deviceId: device.id, deviceName: device.name, session })} />) : !devices.isError && <div className="empty-state"><Server /><strong>{devices.isPending ? "正在加载设备" : "还没有设备"}</strong><span>{devices.isPending ? "" : "添加设备并运行设备代理后，设备会出现在这里。"}</span></div>}</section>
      </>}
    </main>
  </div>;
}
