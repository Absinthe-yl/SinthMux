import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Bell, ChevronDown, ChevronUp, Laptop, Moon, Pin, PinOff, RefreshCw, Server, Sun } from "lucide-react";
import { FormEvent, lazy, Suspense, useEffect, useLayoutEffect, useRef, useState } from "react";
import { request, setCSRF } from "./api";
import { guessInstallOS, installCommand, installOSOptions, type InstallOS } from "./installCommand";
import ActionDialog from "./ActionDialog";
import SessionPanel, { useSessions, type SessionNotification } from "./SessionPanel";
import { maxPins, pinKey, splitPin, usePref } from "./prefs";

const TerminalView = lazy(() => import("./TerminalView"));

// Browsers without CSS field-sizing size a <select> to its longest option.
// Measure the selected label instead so the picker hugs the current space name.
function useFitSelect() {
  const ref = useRef<HTMLSelectElement>(null);
  // Runs after every render: the select only mounts once login data arrives,
  // and measuring one short label is cheap.
  useLayoutEffect(() => {
    const select = ref.current;
    if (!select || CSS.supports("field-sizing", "content")) return;
    const label = select.selectedOptions[0]?.text ?? "";
    const style = getComputedStyle(select);
    const context = document.createElement("canvas").getContext("2d");
    if (!context) return;
    context.font = `${style.fontWeight} ${style.fontSize} ${style.fontFamily}`;
    const extra = parseFloat(style.paddingLeft) + parseFloat(style.paddingRight) + parseFloat(style.borderLeftWidth) + parseFloat(style.borderRightWidth);
    const width = `${Math.ceil(context.measureText(label).width + extra) + 2}px`;
    if (select.style.width !== width) select.style.width = width;
  });
  return ref;
}

type Device = {
  id: string;
  spaceId?: string;
  name: string;
  platform: string;
  architecture: string;
  status: string;
  capabilities?: string[];
  notifications?: SessionNotification[];
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
  | { kind: "revoke-device"; device: Device }
  | { kind: "delete-space"; space: Space; deviceCount: number }
  | { kind: "logout"; hasSavedToken: boolean };
const memberRoles: Array<Exclude<Space["role"], "owner">> = ["admin", "operator", "viewer"];



function initialTheme(): Theme {
  try {
    const saved = localStorage.getItem("sinthmux-theme");
    if (saved === "light" || saved === "dark") return saved;
  } catch { /* Storage may be unavailable. */ }
  return window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
}

// Only a flag that this browser offered to save the login token in its
// password manager; the token itself is never stored by the page.
const tokenSavedKey = "sinthmux-token-saved";
function tokenSaved(): boolean {
  try { return localStorage.getItem(tokenSavedKey) === "1"; } catch { return false; }
}
function markTokenSaved() {
  try { localStorage.setItem(tokenSavedKey, "1"); } catch { /* ignore */ }
}

async function getJSON<T>(path: string): Promise<T> {
  return request<T>(path);
}

function Login({ githubEnabled, theme, onToggleTheme, onLogin }: { githubEnabled: boolean; theme: Theme; onToggleTheme: () => void; onLogin: () => void }) {
  const [showToken, setShowToken] = useState(true);
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [githubNotice, setGithubNotice] = useState(false);
  const [showHelp, setShowHelp] = useState(false);
  async function submit(event: FormEvent) {
    event.preventDefault(); setBusy(true); setError("");
    try {
      await request("/api/v1/auth/token", { method: "POST", body: JSON.stringify({ token: token.trim() }) });
      // A real form submit with username + current-password fields lets the
      // browser offer to save the token, and fill it in next time.
      if ("PasswordCredential" in window) {
        try { await navigator.credentials.store(new (window as unknown as { PasswordCredential: new (data: { id: string; password: string; name: string }) => Credential }).PasswordCredential({ id: `SinthMux · ${location.host}`, password: token.trim(), name: "SinthMux 登录令牌" })); } catch { /* the browser may decline */ }
      }
      markTokenSaved();
      setToken(""); onLogin();
    }
    catch (err) { setError(err instanceof Error && /invalid login token/.test(err.message) ? "令牌不对或已过期，请找管理员重新要一个。" : err instanceof Error ? err.message : "登录失败"); }
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
      <div className="login-divider">或</div>
      {showToken ? <form className="login-token-form" method="post" action="/api/v1/auth/token" autoComplete="on" onSubmit={(event) => void submit(event)}>
        {/* Hidden username so password managers file the token under this Hub. */}
        <input type="text" name="username" autoComplete="username" value={`SinthMux · ${location.host}`} readOnly hidden />
        <label htmlFor="login-token">粘贴管理员发给你的令牌</label>
        <input id="login-token" name="password" type="password" autoComplete="current-password" placeholder="smt_ 开头的一串字符" value={token} onChange={(event) => setToken(event.target.value)} required autoFocus />
        <button type="submit" disabled={busy}>{busy ? "登录中…" : "登录"}</button>
        {error && <div className="notice error">{error}</div>}
        <p className="login-hint">没有令牌？<strong>找管理员要一个</strong>，粘贴到上面就能登录。</p>
        <button className="login-help-toggle" type="button" aria-expanded={showHelp} onClick={() => setShowHelp(!showHelp)}>我是管理员</button>
        {showHelp && <div className="login-help">
          <div className="login-help-row"><span>邀请成员</span><p>成员 › 添加令牌成员</p></div>
          <div className="login-help-row"><span>重置令牌</span><p>在服务器执行</p></div>
          <pre>cd ~/SinthMux && ./scripts/recovery-token.sh</pre>
          <p className="login-help-note">多用户时追加用户 ID · 旧令牌不可找回</p>
        </div>}
      </form> : <button className="login-token-toggle" type="button" onClick={() => setShowToken(true)}>使用令牌登录</button>}
    </section>
  </main>;
}

type PinProps = { pinned: (deviceId: string, session: string) => boolean; onTogglePin: (deviceId: string, session: string) => void; onRenamed: (deviceId: string, from: string, to: string) => void; onClosed: (deviceId: string, session: string) => void };

function DeviceCard({ device, expanded, onToggle, onOpen, onRevoke, role, pins }: { device: Device; expanded: boolean; onToggle: () => void; onOpen: (session: string) => void; onRevoke?: () => void; role?: Space["role"]; pins: PinProps }) {
  const online = device.status === "online";
  const capabilities = device.capabilities ?? [];
  const notices = device.notifications ?? [];
  return <article className={`device-card${expanded ? " expanded" : ""}`} data-testid={`device-${device.name}`}>
    <div className="device-main">
      <div className="device-icon" aria-hidden="true">{device.platform === "darwin" || device.platform === "windows" ? <Laptop /> : <Server />}</div>
      <div className="device-info">
        <div className="device-title"><h2>{device.name}</h2><span className={`status-dot${online ? " online" : ""}`} /><span className="status-text">{online ? "在线" : "离线"}</span>{notices.length > 0 && <span className={`notify-badge ${notices[0].color}`} data-testid="notify-badge" title={notices.map((item) => `${item.session}：${item.message || "有新提醒"}`).join("\n")}><Bell aria-hidden="true" />{notices.length}</span>}</div>
        <p>{device.platform} / {device.architecture}</p>
      </div>
      {onRevoke && <button className="device-revoke" type="button" onClick={onRevoke}>移除</button>}<button className="device-toggle" type="button" aria-expanded={expanded} data-testid={`toggle-${device.name}`} onClick={onToggle}>{expanded ? "收起" : "会话"}{expanded ? <ChevronUp /> : <ChevronDown />}</button>
    </div>
    {expanded && <SessionPanel deviceId={device.id} platform={device.platform} online={online} canManage={capabilities.includes("tmux.sessions.manage") && role !== "viewer"} canClose={role === undefined || role === "owner" || role === "admin"} canOpen={role !== "viewer"} notifications={notices} canClearNotification={capabilities.includes("session.notify.v1") && role !== "viewer"} pinned={(session) => pins.pinned(device.id, session)} onTogglePin={(session) => pins.onTogglePin(device.id, session)} onRenamed={(from, to) => pins.onRenamed(device.id, from, to)} onClosed={(session) => pins.onClosed(device.id, session)} onOpen={onOpen} />}
  </article>;
}

// PinnedRow reads the device's cached session list to show whether the pinned session still exists.
function PinnedRow({ pin, device, canOpen, onOpen, onUnpin }: { pin: string; device?: Device; canOpen: boolean; onOpen: () => void; onUnpin: () => void }) {
  const { session } = splitPin(pin);
  const online = device?.status === "online";
  const sessions = useSessions(device?.id ?? "", online, !!device && online);
  const exists = sessions.data ? sessions.data.sessions.some((item) => item.name === session) : undefined;
  const notice = device?.notifications?.find((item) => item.session === session);
  const status = !device ? "设备不可见" : !online ? "设备离线" : exists === false ? "已不存在" : notice ? (notice.message || "有新提醒") : "在线";
  return <li className={`pinned-row${notice ? ` notified notify-${notice.color}` : ""}`} data-testid={`pinned-${session}`}>
    {notice ? <span className={`notify-dot ${notice.color}`} aria-hidden="true" /> : <Pin className="terminal-icon" aria-hidden="true" />}
    <span className="session-info"><strong>{session}</strong><span>{device?.name ?? "未知设备"} · {status}</span></span>
    <button type="button" className="pinned-open" data-testid={`pinned-open-${session}`} disabled={!online || !canOpen || exists === false} onClick={onOpen}>打开</button>
    <button type="button" className="icon-only" title="取消置顶" aria-label={`取消置顶 ${session}`} onClick={onUnpin}><PinOff /></button>
  </li>;
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
    case "delete-space": return { title: "删除空间", description: `将删除“${action.space.name}”及其中的${action.deviceCount ? ` ${action.deviceCount} 台设备、` : ""}成员和待用配对码，设备代理会立即断开，且无法恢复。设备上的 tmux 会话不受影响。请输入空间名称确认。`, confirmLabel: "删除空间", destructive: true };
    case "logout": return action.hasSavedToken
      ? { title: "退出登录", description: "退出后需要用登录令牌重新登录。如果浏览器已保存令牌，登录时会自动填入。", confirmLabel: "退出" }
      : { title: "退出前请确认能再登录", description: "重新登录需要登录令牌。令牌只在创建时显示一次，SinthMux 无法再次查看。如果没有保存，可以先新建一个令牌并保存到密码管理器或备忘录。", confirmLabel: "仍然退出", destructive: true };
  }
}

export default function App() {
  const queryClient = useQueryClient();
  const [theme, setTheme] = useState<Theme>(initialTheme);
  const [activeTerminal, setActiveTerminal] = useState<{ deviceId: string; deviceName: string; session: string; capabilities: string[] } | null>(null);
  const [notice, setNotice] = useState("");
  const [secret, setSecret] = useState<{ label: string; value: string; hint?: string; install?: { hub: string; code: string; os: InstallOS } } | null>(null);
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
  const userKey = formal ? me.data?.user.id : "dev";
  const [selectedSpace, setSelectedSpace] = usePref<string | null>(userKey, "space", null);
  const [expanded, setExpanded] = usePref<string[]>(userKey, "expanded", []);
  const [pins, setPins] = usePref<string[]>(userKey, "pinned", []);
  const activeSpace = me.data?.spaces.find((space) => space.id === selectedSpace) ?? me.data?.spaces[0];
  const spaceSelect = useFitSelect();
  const members = useQuery({ queryKey: ["members", activeSpace?.id], enabled: formal && showMembers && !!activeSpace, queryFn: () => request<{ members: Member[] }>(`/api/v1/spaces/${activeSpace!.id}/members`) });
  const tokens = useQuery({ queryKey: ["tokens"], enabled: formal && showTokens && !!me.data, queryFn: () => request<{ tokens: LoginToken[] }>("/api/v1/auth/tokens") });
  const items = (devices.data?.devices ?? []).filter((device) => !formal || device.spaceId === activeSpace?.id);
  const hubOnline = !status.isError && status.data?.status === "ok";
  const deviceById = new Map((devices.data?.devices ?? []).map((device) => [device.id, device]));
  const visiblePins = pins.filter((pin) => { const device = deviceById.get(splitPin(pin).deviceId); return !formal || !device || device.spaceId === activeSpace?.id; });
  const pinActions: PinProps = {
    pinned: (deviceId, session) => pins.includes(pinKey(deviceId, session)),
    onTogglePin: (deviceId, session) => {
      const key = pinKey(deviceId, session);
      setPins((current) => current.includes(key) ? current.filter((item) => item !== key) : [key, ...current].slice(0, maxPins));
    },
    onRenamed: (deviceId, from, to) => setPins((current) => current.map((item) => item === pinKey(deviceId, from) ? pinKey(deviceId, to) : item)),
    onClosed: (deviceId, session) => setPins((current) => current.filter((item) => item !== pinKey(deviceId, session)))
  };
  const openTerminal = (device: Device, session: string) => {
    setActiveTerminal({ deviceId: device.id, deviceName: device.name, session, capabilities: device.capabilities ?? [] });
    // The connector clears the notification when the session is opened; refresh soon after.
    if (device.notifications?.some((item) => item.session === session)) setTimeout(() => { void queryClient.invalidateQueries({ queryKey: ["devices"] }); }, 1500);
  };

  // Notifications: count in the tab title, and a desktop notification for new ones.
  const allNotices = (devices.data?.devices ?? []).flatMap((device) => (device.notifications ?? []).map((item) => ({ ...item, device })));
  const seenNotices = useRef<Map<string, number> | null>(null);
  const [desktopAllowed, setDesktopAllowed] = useState(() => typeof Notification !== "undefined" && Notification.permission === "granted");
  useEffect(() => {
    document.title = allNotices.length > 0 ? `(${allNotices.length}) SinthMux` : "SinthMux";
    const previous = seenNotices.current;
    const next = new Map(allNotices.map((item) => [`${item.device.id}/${item.session}`, item.at]));
    seenNotices.current = next;
    if (!previous || !desktopAllowed || document.visibilityState === "visible" && document.hasFocus()) return;
    for (const item of allNotices) {
      const key = `${item.device.id}/${item.session}`;
      if ((previous.get(key) ?? 0) < item.at) {
        const shown = new Notification(`${item.device.name} · ${item.session}`, { body: item.message || "有新提醒", tag: key });
        shown.onclick = () => { window.focus(); openTerminal(item.device, item.session); shown.close(); };
      }
    }
  }, [devices.data, desktopAllowed]); // eslint-disable-line react-hooks/exhaustive-deps
  const enableDesktop = () => { void Notification.requestPermission().then((result) => setDesktopAllowed(result === "granted")); };

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
    const needsName = ["create-token", "create-space", "create-device", "add-member", "delete-space"].includes(dialogAction.kind);
    const needsSpace = ["create-device", "add-member", "add-github-member", "change-role", "remove-member", "revoke-device"].includes(dialogAction.kind);
    if (needsName && !name) { setDialogError("请输入名称"); return; }
    if (dialogAction.kind === "delete-space" && name !== dialogAction.space.name) { setDialogError("名称不一致"); return; }
    if (needsSpace && !activeSpace) { setDialogError("空间不可用，请刷新后重试"); return; }
    const spaceId = activeSpace?.id;
    setDialogBusy(true);
    setDialogError("");
    try {
      switch (dialogAction.kind) {
        case "create-token": {
          const result = await request<{ token: string }>("/api/v1/auth/tokens", { method: "POST", body: JSON.stringify({ name }) });
          setSecretCopied(false);
          setSecret({ label: "登录令牌（仅显示一次）", value: result.token, hint: "SinthMux 只保存令牌的哈希，关闭后无法再次查看。请现在把它存进密码管理器或备忘录；下次在登录页输入时，浏览器也会提示保存。有效期 90 天。" });
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
          const os = guessInstallOS();
          const localOnly = new URL(hub).hostname === "127.0.0.1" || new URL(hub).hostname === "localhost";
          setSecretCopied(false);
          setSecret({ label: `${name} 的接入命令`, value: installCommand(os, hub, result.code), install: { hub, code: result.code, os }, hint: localOnly ? "配对码 5 分钟有效、只能用一次。当前 Hub 地址仅本机可访问；另一台电脑接入前需将 Hub 部署到可访问的 HTTPS 地址。" : "配对码 5 分钟有效、只能用一次。macOS / Linux 在终端执行，Windows 在 PowerShell 执行；没有 tmux 时会自动安装。" });
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
        case "delete-space":
          await request(`/api/v1/spaces/${dialogAction.space.id}`, { method: "DELETE" });
          setSelectedSpace(null);
          setShowMembers(false);
          break;
        case "logout":
          await request("/api/v1/auth/logout", { method: "POST" });
          setCSRF(""); setSecret(null); setActiveTerminal(null);
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
  function logout() { openDialog({ kind: "logout", hasSavedToken: tokenSaved() }); }

  if (formal && me.isError) return <Login githubEnabled={status.data?.githubLoginEnabled ?? false} theme={theme} onToggleTheme={() => setTheme(theme === "dark" ? "light" : "dark")} onLogin={() => { void me.refetch(); }} />;

  return <div className="app-frame">
    <header className="topbar"><div className="topbar-inner">
      <a className="brand" href="#top" aria-label="SinthMux 首页"><img className="brand-mark" src="/sinthmux-mark-transparent.png?v=3" alt="" /><strong>SinthMux</strong></a>
      <div className="top-actions">{me.data && <span className="user-name">{me.data.user.name}</span>}<span className="hub-status"><span className={`status-dot${hubOnline ? " online" : ""}`} />Hub {hubOnline ? "在线" : status.isError ? "离线" : "连接中"}</span><button className="icon-button" type="button" title={theme === "dark" ? "切换浅色模式" : "切换深色模式"} aria-label={theme === "dark" ? "切换浅色模式" : "切换深色模式"} onClick={() => setTheme(theme === "dark" ? "light" : "dark")}>{theme === "dark" ? <Sun /> : <Moon />}</button></div>
    </div></header>

    <main className={`shell${activeTerminal ? " terminal-shell" : ""}`} id="top">
      {activeTerminal ? <Suspense fallback={<div className="session-note">正在打开终端…</div>}><TerminalView key={`${activeTerminal.deviceId}:${activeTerminal.session}`} {...activeTerminal} theme={theme} onBack={() => setActiveTerminal(null)} /></Suspense> : <>
      <div className="page-heading"><div><h1>设备 <span>{items.length}</span></h1></div><button className="refresh-button" type="button" onClick={() => { void status.refetch(); void devices.refetch(); }}><RefreshCw />刷新</button></div>
      {formal && me.data && <div className="workspace-bar"><label>空间 <select ref={spaceSelect} aria-label="当前空间" value={activeSpace?.id ?? ""} onChange={(event) => setSelectedSpace(event.target.value)}><option value="" disabled>选择空间</option>{me.data.spaces.map((space) => <option value={space.id} key={space.id}>{space.name} · {space.role}</option>)}</select></label><button type="button" onClick={() => openDialog({ kind: "create-space" })}>新建空间</button>{activeSpace && (activeSpace.role === "owner" || activeSpace.role === "admin") && <button type="button" onClick={() => openDialog({ kind: "create-device" })}>添加设备</button>}{activeSpace && <button type="button" onClick={() => setShowMembers(!showMembers)}>成员</button>}{activeSpace?.kind === "team" && activeSpace.role === "owner" && <button type="button" className="danger-text" onClick={() => openDialog({ kind: "delete-space", space: activeSpace, deviceCount: items.length })}>删除空间</button>}<button type="button" onClick={() => setShowTokens(!showTokens)}>登录令牌</button><button type="button" onClick={logout}>退出</button></div>}
      {formal && showMembers && activeSpace && <div className="management-panel"><div className="management-heading"><strong>成员</strong>{me.data?.user.githubId && <span>我的 GitHub ID：{me.data.user.githubId}</span>}{activeSpace.role === "owner" && <><button type="button" onClick={() => openDialog({ kind: "add-member" })}>添加令牌成员</button><button type="button" onClick={() => openDialog({ kind: "add-github-member" })}>添加 GitHub 成员</button></>}</div>{members.data?.members.map((member) => <div className="management-row" key={member.userId}><span>{member.name} · {member.role}</span>{activeSpace.role === "owner" && member.role !== "owner" && <><button type="button" onClick={() => openDialog({ kind: "change-role", member })}>修改角色</button><button type="button" onClick={() => openDialog({ kind: "remove-member", member })}>移除</button></>}</div>)}</div>}
      {formal && showTokens && <div className="management-panel"><div className="management-heading"><strong>我的登录令牌</strong><button type="button" onClick={() => openDialog({ kind: "create-token" })}>新建令牌</button></div>{tokens.data?.tokens.map((token) => <div className="management-row" key={token.id}><span>{token.name} · {new Date(token.expiresAt).toLocaleDateString()}</span><button type="button" onClick={() => openDialog({ kind: "revoke-token", token })}>撤销</button></div>)}</div>}
      {secret && <div className="secret-panel"><strong>{secret.label}</strong>{secret.install && <div className="os-tabs" role="tablist">{installOSOptions.map((option) => <button key={option.id} type="button" role="tab" aria-selected={secret.install?.os === option.id} className={secret.install?.os === option.id ? "active" : ""} onClick={() => { const install = secret.install!; setSecretCopied(false); setSecret({ ...secret, value: installCommand(option.id, install.hub, install.code), install: { ...install, os: option.id } }); }}>{option.label}</button>)}</div>}<pre>{secret.value}</pre>{secret.hint && <p>{secret.hint}</p>}<button type="button" onClick={() => { void navigator.clipboard.writeText(secret.value).then(() => setSecretCopied(true)).catch(() => setNotice("复制失败，请手动选中内容复制。")); }}>{secretCopied ? "已复制" : "复制"}</button><button type="button" onClick={() => setSecret(null)}>关闭</button></div>}
      {notice && <div className="notice error">{notice}</div>}
      {devices.isError && <div className="notice error">无法读取设备列表，请确认 Hub 已启动。</div>}
      {(allNotices.length > 0 || visiblePins.length > 0) && typeof Notification !== "undefined" && Notification.permission === "default" && !desktopAllowed && <div className="notify-hint"><Bell aria-hidden="true" />会话提醒可以弹出桌面通知<button type="button" data-testid="enable-desktop-notify" onClick={enableDesktop}>开启桌面提醒</button></div>}
      {visiblePins.length > 0 && <section className="pinned-section" data-testid="pinned-section" aria-label="置顶会话"><h2><Pin aria-hidden="true" />置顶会话</h2><ul>{visiblePins.map((pin) => { const { deviceId, session } = splitPin(pin); const device = deviceById.get(deviceId); return <PinnedRow key={pin} pin={pin} device={device} canOpen={!formal || activeSpace?.role !== "viewer"} onOpen={() => device && openTerminal(device, session)} onUnpin={() => setPins((current) => current.filter((item) => item !== pin))} />; })}</ul></section>}
      <section className="device-list" aria-label="设备列表">{items.length ? items.map((device) => <DeviceCard key={device.id} device={device} role={formal ? activeSpace?.role : undefined} pins={pinActions} onRevoke={formal && (activeSpace?.role === "owner" || activeSpace?.role === "admin") ? () => openDialog({ kind: "revoke-device", device }) : undefined} expanded={expanded.includes(device.id)} onToggle={() => setExpanded((current) => current.includes(device.id) ? current.filter((id) => id !== device.id) : [...current, device.id])} onOpen={(session) => openTerminal(device, session)} />) : !devices.isError && <div className="empty-state"><Server /><strong>{devices.isPending ? "正在加载设备" : "还没有设备"}</strong><span>{devices.isPending ? "" : "添加设备并运行设备代理后，设备会出现在这里。"}</span></div>}</section>
      </>}
    </main>
    {dialogAction && <ActionDialog key={dialogAction.kind} {...dialogText(dialogAction)} busy={dialogBusy} error={dialogError} onClose={closeDialog} onSubmit={() => void submitDialog()}>
      {dialogAction.kind === "logout" && !dialogAction.hasSavedToken && <button type="button" className="dialog-secondary" disabled={dialogBusy} onClick={() => openDialog({ kind: "create-token" })}>先新建一个令牌并保存</button>}
      {["create-token", "create-space", "create-device", "add-member", "delete-space"].includes(dialogAction.kind) && <label className="dialog-field">
        <span>{dialogAction.kind === "create-token" ? "令牌名称" : dialogAction.kind === "create-space" ? "空间名称" : dialogAction.kind === "create-device" ? "设备名称" : dialogAction.kind === "delete-space" ? `输入“${dialogAction.space.name}”确认` : "成员名称"}</span>
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
