import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronDown, ChevronUp, Laptop, Moon, RefreshCw, Server, Sun } from "lucide-react";
import { FormEvent, lazy, Suspense, useLayoutEffect, useState } from "react";
import { request, setCSRF } from "./api";
import ActionDialog from "./ActionDialog";
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
type DialogAction =
  | { kind: "create-token" | "create-space" | "create-device" | "add-member" | "add-github-member" }
  | { kind: "change-role" | "remove-member"; member: Member }
  | { kind: "revoke-token"; token: LoginToken }
  | { kind: "revoke-device"; device: Device };
const memberRoles: Array<Exclude<Space["role"], "owner">> = ["admin", "operator", "viewer"];

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
      <img className="login-mark" src="/sinthmux-mark-transparent.png?v=3" alt="" />
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

function dialogText(action: DialogAction): { title: string; description?: string; confirmLabel: string; destructive?: boolean } {
  switch (action.kind) {
    case "create-token": return { title: "新建登录令牌", confirmLabel: "新建令牌" };
    case "create-space": return { title: "新建空间", confirmLabel: "创建空间" };
    case "create-device": return { title: "添加设备", description: "创建后会生成一次性接入命令。", confirmLabel: "生成接入命令" };
    case "add-member": return { title: "添加令牌成员", description: "创建后会显示一次性的初始登录令牌。", confirmLabel: "添加成员" };
    case "add-github-member": return { title: "添加 GitHub 成员", description: "对方需要先用 GitHub 登录过 SinthMux。", confirmLabel: "添加成员" };
    case "change-role": return { title: `修改 ${action.member.name} 的角色`, confirmLabel: "保存角色" };
    case "remove-member": return { title: "移除成员", description: `确定从当前空间移除 ${action.member.name}？`, confirmLabel: "移除成员", destructive: true };
    case "revoke-token": return { title: "撤销登录令牌", description: `撤销“${action.token.name}”后，使用它登录的会话将失效。`, confirmLabel: "撤销令牌", destructive: true };
    case "revoke-device": return { title: "移除设备", description: `移除“${action.device.name}”后，设备代理将立即断开。`, confirmLabel: "移除设备", destructive: true };
  }
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
  const [dialogAction, setDialogAction] = useState<DialogAction | null>(null);
  const [dialogName, setDialogName] = useState("");
  const [dialogGithubId, setDialogGithubId] = useState("");
  const [dialogRole, setDialogRole] = useState<Exclude<Space["role"], "owner">>("operator");
  const [dialogError, setDialogError] = useState("");
  const [dialogBusy, setDialogBusy] = useState(false);
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
  function openDialog(action: DialogAction) {
    setDialogAction(action);
    setDialogName(action.kind === "change-role" || action.kind === "remove-member" ? action.member.name : action.kind === "create-token" ? "我的登录令牌" : "");
    setDialogGithubId("");
    setDialogRole(action.kind === "change-role" && action.member.role !== "owner" ? action.member.role : "operator");
    setDialogError("");
  }
  function closeDialog() { if (!dialogBusy) setDialogAction(null); }
  async function submitDialog() {
    if (!dialogAction || dialogBusy) return;
    const name = dialogName.trim();
    const needsName = ["create-token", "create-space", "create-device", "add-member"].includes(dialogAction.kind);
    const needsSpace = ["create-device", "add-member", "add-github-member", "change-role", "remove-member", "revoke-device"].includes(dialogAction.kind);
    if (needsName && !name) { setDialogError("请输入名称"); return; }
    if (needsSpace && !activeSpace) { setDialogError("空间不可用，请刷新后重试"); return; }
    const spaceId = activeSpace?.id;
    setDialogBusy(true);
    setDialogError("");
    try {
      switch (dialogAction.kind) {
        case "create-token": {
          const result = await request<{ token: string }>("/api/v1/auth/tokens", { method: "POST", body: JSON.stringify({ name }) });
          setSecretCopied(false);
          setSecret({ label: "登录令牌（仅显示一次）", value: result.token });
          break;
        }
        case "create-space": {
          const space = await request<Space>("/api/v1/spaces", { method: "POST", body: JSON.stringify({ name }) });
          setSelectedSpace(space.id);
          break;
        }
        case "create-device": {
          const result = await request<{ code: string; expiresAt: string; hubUrl: string }>(`/api/v1/spaces/${spaceId}/device-pairings`, { method: "POST", body: JSON.stringify({ name }) });
          const hub = result.hubUrl.replace(/\/$/, "");
          const curlOptions = hub.startsWith("https://") ? " --proto '=https' --proto-redir '=https'" : "";
          const command = `curl -fsSL${curlOptions} ${shellQuote(`${hub}/install/connector.sh`)} | bash -s -- --hub ${shellQuote(hub)} --code ${shellQuote(result.code)}`;
          const localOnly = new URL(hub).hostname === "127.0.0.1" || new URL(hub).hostname === "localhost";
          setSecretCopied(false);
          setSecret({ label: `${name} 的接入命令`, value: command, hint: localOnly ? "配对码 5 分钟有效、只能用一次。当前 Hub 地址仅本机可访问；另一台电脑接入前需将 Hub 部署到可访问的 HTTPS 地址。目标电脑需要安装 tmux 和 curl。" : "配对码 5 分钟有效、只能用一次。在目标电脑终端执行；该电脑需要安装 tmux 和 curl。" });
          break;
        }
        case "add-member": {
          const result = await request<{ token: string }>(`/api/v1/spaces/${spaceId}/members`, { method: "POST", body: JSON.stringify({ name, role: dialogRole }) });
          setSecretCopied(false);
          setSecret({ label: `${name} 的初始登录令牌（仅显示一次）`, value: result.token });
          break;
        }
        case "add-github-member": {
          const githubId = Number(dialogGithubId);
          if (!Number.isSafeInteger(githubId) || githubId <= 0) { setDialogError("请输入有效的 GitHub 数字 ID"); return; }
          await request(`/api/v1/spaces/${spaceId}/members`, { method: "POST", body: JSON.stringify({ githubId, role: dialogRole }) });
          break;
        }
        case "change-role": {
          if (dialogRole !== dialogAction.member.role) await request(`/api/v1/spaces/${spaceId}/members/${dialogAction.member.userId}`, { method: "PATCH", body: JSON.stringify({ role: dialogRole }) });
          break;
        }
        case "remove-member":
          await request(`/api/v1/spaces/${spaceId}/members/${dialogAction.member.userId}`, { method: "DELETE" });
          break;
        case "revoke-token":
          await request(`/api/v1/auth/tokens/${dialogAction.token.id}`, { method: "DELETE" });
          break;
        case "revoke-device":
          await request(`/api/v1/spaces/${spaceId}/devices/${dialogAction.device.id}`, { method: "DELETE" });
          break;
      }
      setDialogAction(null);
      await queryClient.invalidateQueries();
    } catch (error) {
      setDialogError(error instanceof Error ? error.message : "操作失败");
    } finally {
      setDialogBusy(false);
    }
  }
  async function logout() { await runAction(async () => { await request("/api/v1/auth/logout", { method: "POST" }); setCSRF(""); setSecret(null); setActiveTerminal(null); }); }

  if (formal && me.isError) return <Login githubEnabled={status.data?.githubLoginEnabled ?? false} theme={theme} onToggleTheme={() => setTheme(theme === "dark" ? "light" : "dark")} onLogin={() => { void me.refetch(); }} />;

  return <div className="app-frame">
    <header className="topbar"><div className="topbar-inner">
      <a className="brand" href="#top" aria-label="SinthMux 首页"><img className="brand-mark" src="/sinthmux-mark-transparent.png?v=3" alt="" /><strong>SinthMux</strong></a>
      <div className="top-actions">{me.data && <span className="user-name">{me.data.user.name}</span>}<span className="hub-status"><span className={`status-dot${hubOnline ? " online" : ""}`} />Hub {hubOnline ? "在线" : status.isError ? "离线" : "连接中"}</span><button className="icon-button" type="button" title={theme === "dark" ? "切换浅色模式" : "切换深色模式"} aria-label={theme === "dark" ? "切换浅色模式" : "切换深色模式"} onClick={() => setTheme(theme === "dark" ? "light" : "dark")}>{theme === "dark" ? <Sun /> : <Moon />}</button></div>
    </div></header>

    <main className={`shell${activeTerminal ? " terminal-shell" : ""}`} id="top">
      {activeTerminal ? <Suspense fallback={<div className="session-note">正在打开终端…</div>}><TerminalView key={`${activeTerminal.deviceId}:${activeTerminal.session}`} {...activeTerminal} theme={theme} onBack={() => setActiveTerminal(null)} /></Suspense> : <>
      <div className="page-heading"><div><h1>设备 <span>{items.length}</span></h1></div><button className="refresh-button" type="button" onClick={() => { void status.refetch(); void devices.refetch(); }}><RefreshCw />刷新</button></div>
      {formal && me.data && <div className="workspace-bar"><label>空间 <select aria-label="当前空间" value={activeSpace?.id ?? ""} onChange={(event) => { setSelectedSpace(event.target.value); setSelectedDevice(null); }}><option value="" disabled>选择空间</option>{me.data.spaces.map((space) => <option value={space.id} key={space.id}>{space.name} · {space.role}</option>)}</select></label><button type="button" onClick={() => openDialog({ kind: "create-space" })}>新建空间</button>{activeSpace && (activeSpace.role === "owner" || activeSpace.role === "admin") && <button type="button" onClick={() => openDialog({ kind: "create-device" })}>添加设备</button>}{activeSpace && <button type="button" onClick={() => setShowMembers(!showMembers)}>成员</button>}<button type="button" onClick={() => setShowTokens(!showTokens)}>登录令牌</button><button type="button" onClick={() => void logout()}>退出</button></div>}
      {formal && showMembers && activeSpace && <div className="management-panel"><div className="management-heading"><strong>成员</strong>{me.data?.user.githubId && <span>我的 GitHub ID：{me.data.user.githubId}</span>}{activeSpace.role === "owner" && <><button type="button" onClick={() => openDialog({ kind: "add-member" })}>添加令牌成员</button><button type="button" onClick={() => openDialog({ kind: "add-github-member" })}>添加 GitHub 成员</button></>}</div>{members.data?.members.map((member) => <div className="management-row" key={member.userId}><span>{member.name} · {member.role}</span>{activeSpace.role === "owner" && member.role !== "owner" && <><button type="button" onClick={() => openDialog({ kind: "change-role", member })}>修改角色</button><button type="button" onClick={() => openDialog({ kind: "remove-member", member })}>移除</button></>}</div>)}</div>}
      {formal && showTokens && <div className="management-panel"><div className="management-heading"><strong>我的登录令牌</strong><button type="button" onClick={() => openDialog({ kind: "create-token" })}>新建令牌</button></div>{tokens.data?.tokens.map((token) => <div className="management-row" key={token.id}><span>{token.name} · {new Date(token.expiresAt).toLocaleDateString()}</span><button type="button" onClick={() => openDialog({ kind: "revoke-token", token })}>撤销</button></div>)}</div>}
      {secret && <div className="secret-panel"><strong>{secret.label}</strong><pre>{secret.value}</pre>{secret.hint && <p>{secret.hint}</p>}<button type="button" onClick={() => { void navigator.clipboard.writeText(secret.value).then(() => setSecretCopied(true)).catch(() => setNotice("复制失败，请手动选中内容复制。")); }}>{secretCopied ? "已复制" : "复制"}</button><button type="button" onClick={() => setSecret(null)}>关闭</button></div>}
      {notice && <div className="notice error">{notice}</div>}
      {devices.isError && <div className="notice error">无法读取设备列表，请确认 Hub 已启动。</div>}
      <section className="device-list" aria-label="设备列表">{items.length ? items.map((device) => <DeviceCard key={device.id} device={device} role={formal ? activeSpace?.role : undefined} onRevoke={formal && (activeSpace?.role === "owner" || activeSpace?.role === "admin") ? () => openDialog({ kind: "revoke-device", device }) : undefined} expanded={selectedDevice === device.id} onToggle={() => setSelectedDevice(selectedDevice === device.id ? null : device.id)} onOpen={(session) => setActiveTerminal({ deviceId: device.id, deviceName: device.name, session })} />) : !devices.isError && <div className="empty-state"><Server /><strong>{devices.isPending ? "正在加载设备" : "还没有设备"}</strong><span>{devices.isPending ? "" : "添加设备并运行设备代理后，设备会出现在这里。"}</span></div>}</section>
      </>}
    </main>
    {dialogAction && <ActionDialog key={dialogAction.kind} {...dialogText(dialogAction)} busy={dialogBusy} error={dialogError} onClose={closeDialog} onSubmit={() => void submitDialog()}>
      {["create-token", "create-space", "create-device", "add-member"].includes(dialogAction.kind) && <label className="dialog-field">
        <span>{dialogAction.kind === "create-token" ? "令牌名称" : dialogAction.kind === "create-space" ? "空间名称" : dialogAction.kind === "create-device" ? "设备名称" : "成员名称"}</span>
        <input value={dialogName} onChange={(event) => setDialogName(event.target.value)} maxLength={80} disabled={dialogBusy} />
      </label>}
      {dialogAction.kind === "add-github-member" && <label className="dialog-field">
        <span>GitHub 数字 ID</span>
        <input value={dialogGithubId} onChange={(event) => setDialogGithubId(event.target.value)} inputMode="numeric" disabled={dialogBusy} />
      </label>}
      {["add-member", "add-github-member", "change-role"].includes(dialogAction.kind) && <label className="dialog-field">
        <span>角色</span>
        <select value={dialogRole} onChange={(event) => setDialogRole(event.target.value as typeof dialogRole)} disabled={dialogBusy}>
          {memberRoles.map((role) => <option value={role} key={role}>{role}</option>)}
        </select>
      </label>}
    </ActionDialog>}
  </div>;
}
