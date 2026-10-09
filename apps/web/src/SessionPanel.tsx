import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { FormEvent, useState } from "react";
import { BellOff, ListChecks, Pencil, Pin, PinOff, Plus, Terminal, Trash2 } from "lucide-react";
import ActionDialog from "./ActionDialog";
import { request } from "./api";
import { deviceInstallOS, installCommand } from "./installCommand";

export type TmuxSession = { name: string; windows: number; attached: boolean; createdAt: number };
export type SessionNotification = { session: string; color: "blue" | "green" | "yellow" | "red"; message: string; at: number };
type Action = { kind: "create"; name: string } | { kind: "rename"; name: string; newName: string } | { kind: "close"; name: string };

const namePattern = /^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$/;
const nameHint = "名称只能使用字母、数字、下划线和连字符，且需以字母或数字开头（最多 64 字符）。";

export function sessionsPath(deviceId: string) { return `/api/v1/devices/${encodeURIComponent(deviceId)}/sessions`; }

// useSessions shares one cached list per device between the device panel and the pinned section.
export function useSessions(deviceId: string, online: boolean, enabled = true) {
  return useQuery({ queryKey: ["sessions", deviceId], enabled, queryFn: () => request<{ sessions: TmuxSession[] }>(sessionsPath(deviceId)), retry: false, refetchInterval: online ? 10000 : false });
}

export function clearNotification(deviceId: string, session: string) {
  return request(`${sessionsPath(deviceId)}/${encodeURIComponent(session)}/notification`, { method: "DELETE" });
}

type Props = {
  deviceId: string;
  platform?: string;
  online: boolean;
  canManage: boolean;
  canClose: boolean;
  canOpen: boolean;
  notifications: SessionNotification[];
  canClearNotification: boolean;
  pinned: (session: string) => boolean;
  onTogglePin: (session: string) => void;
  onRenamed: (from: string, to: string) => void;
  onClosed: (session: string) => void;
  onOpen: (session: string) => void;
};

export default function SessionPanel({ deviceId, platform, online, canManage, canClose, canOpen, notifications, canClearNotification, pinned, onTogglePin, onRenamed, onClosed, onOpen }: Props) {
  const queryClient = useQueryClient();
  const [creating, setCreating] = useState(false);
  const [newName, setNewName] = useState("");
  const [renaming, setRenaming] = useState<string | null>(null);
  const [renameName, setRenameName] = useState("");
  const [inputError, setInputError] = useState("");
  const [repairCopied, setRepairCopied] = useState(false);
  const [closing, setClosing] = useState<string | null>(null);
  const [batch, setBatch] = useState(false);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [batchConfirm, setBatchConfirm] = useState(false);
  const [batchBusy, setBatchBusy] = useState(false);
  const [batchErrors, setBatchErrors] = useState<Record<string, string>>({});
  const base = sessionsPath(deviceId);
  const queryKey = ["sessions", deviceId];
  const sessions = useSessions(deviceId, online);
  const repairNeeded = sessions.isError && /tmux is not installed|设备代理找不到 tmux|invalid tmux session output/i.test(sessions.error.message);
  const hub = window.location.origin;
  const repairCommand = installCommand(deviceInstallOS(platform), hub);
  const noticeFor = (name: string) => notifications.find((item) => item.session === name);
  const action = useMutation({
    mutationFn: (item: Action) => {
      if (item.kind === "create") return request(base, { method: "POST", body: JSON.stringify({ name: item.name }) });
      const path = `${base}/${encodeURIComponent(item.name)}`;
      if (item.kind === "rename") return request(path, { method: "PATCH", body: JSON.stringify({ name: item.newName }) });
      return request(path, { method: "DELETE" });
    },
    onSuccess: (_result, item) => {
      setInputError("");
      if (item.kind === "create") { setNewName(""); setCreating(false); }
      if (item.kind === "rename") { setRenaming(null); onRenamed(item.name, item.newName); }
      if (item.kind === "close") { setClosing(null); onClosed(item.name); }
      void queryClient.invalidateQueries({ queryKey });
    }
  });
  const clear = useMutation({
    mutationFn: (name: string) => clearNotification(deviceId, name),
    onSuccess: () => { void queryClient.invalidateQueries({ queryKey: ["devices"] }); }
  });

  const create = (event: FormEvent) => {
    event.preventDefault();
    const name = newName.trim();
    if (!namePattern.test(name)) { setInputError(nameHint); return; }
    setInputError("");
    action.mutate({ kind: "create", name });
  };
  const rename = (event: FormEvent) => {
    event.preventDefault();
    if (renaming === null) return;
    const name = renameName.trim();
    if (!namePattern.test(name)) { setInputError(nameHint); return; }
    if (name === renaming) { setRenaming(null); return; }
    setInputError("");
    action.mutate({ kind: "rename", name: renaming, newName: name });
  };
  const names = sessions.data?.sessions.map((session) => session.name) ?? [];
  const toggleSelected = (name: string) => setSelected((current) => {
    const next = new Set(current);
    if (next.has(name)) next.delete(name); else next.add(name);
    return next;
  });
  // Close one at a time so each close is audited and a failure keeps its row selected.
  const closeSelected = async () => {
    setBatchBusy(true);
    const errors: Record<string, string> = {};
    for (const name of selected) {
      try {
        await request(`${base}/${encodeURIComponent(name)}`, { method: "DELETE" });
        onClosed(name);
      } catch (error) {
        errors[name] = error instanceof Error ? error.message : "关闭失败";
      }
    }
    setBatchErrors(errors);
    setSelected(new Set(Object.keys(errors)));
    setBatchBusy(false);
    setBatchConfirm(false);
    if (Object.keys(errors).length === 0) setBatch(false);
    void queryClient.invalidateQueries({ queryKey });
  };

  return <div className="sessions">
    <div className="sessions-header"><strong>会话 <span>{sessions.data?.sessions.length ?? "—"}</span></strong>
      <span className="sessions-header-actions">
        {online && canManage && canClose && names.length > 0 && <button className="new-session-button" type="button" data-testid="batch-toggle" aria-pressed={batch} onClick={() => { setBatch(!batch); setSelected(new Set()); setBatchErrors({}); }}><ListChecks />{batch ? "完成" : "批量管理"}</button>}
        {online && canManage && !batch && <button className="new-session-button" type="button" onClick={() => { setCreating(!creating); setInputError(""); action.reset(); }}><Plus />新建会话</button>}
      </span>
    </div>
    {batch && <div className="batch-bar">
      <label><input type="checkbox" data-testid="batch-select-all" checked={names.length > 0 && selected.size === names.length} onChange={(event) => setSelected(event.target.checked ? new Set(names) : new Set())} />全选</label>
      <button type="button" className="danger-text" data-testid="batch-close" disabled={selected.size === 0 || batchBusy} onClick={() => setBatchConfirm(true)}>关闭所选 ({selected.size})</button>
    </div>}
    {creating && <form className="session-form create-form" onSubmit={create}><label className="sr-only" htmlFor={`new-session-${deviceId}`}>新会话名称</label><input id={`new-session-${deviceId}`} autoFocus placeholder="会话名称" value={newName} onChange={(event) => setNewName(event.target.value)} maxLength={64} disabled={action.isPending} /><button type="submit" disabled={action.isPending}>创建</button><button type="button" className="subtle-button" onClick={() => setCreating(false)}>取消</button></form>}
    {!online && <p className="session-note">设备离线</p>}
    {online && !canManage && <p className="session-note">{canOpen ? "设备代理尚不支持会话管理" : "当前角色只能查看会话列表"}</p>}
    {inputError && <div className="notice error">{inputError}</div>}
    {action.isError && <div className="notice error">操作失败：{action.error.message}</div>}
    {clear.isError && <div className="notice error">清除提醒失败：{clear.error.message}</div>}
    {sessions.isPending ? <p className="session-note">正在读取会话…</p> : sessions.isError ? <div className="notice error">读取失败：{sessions.error.message} <button className="text-button" type="button" onClick={() => sessions.refetch()}>重试</button>{repairNeeded && <div className="session-repair"><p>在这台设备上运行修复命令，会自动安装 tmux 并更新设备代理{platform === "windows" ? "（在 PowerShell 中运行）" : ""}：</p><pre>{repairCommand}</pre><button className="text-button" type="button" onClick={() => { void navigator.clipboard.writeText(repairCommand).then(() => setRepairCopied(true)).catch(() => setRepairCopied(false)); }}>{repairCopied ? "已复制" : "复制修复命令"}</button></div>}</div> : sessions.data.sessions.length === 0 ? <p className="session-note empty-sessions">暂无会话</p> : <ul className="session-list">{sessions.data.sessions.map((session) => {
      const notice = noticeFor(session.name);
      const isPinned = pinned(session.name);
      return <li className={`session-row${notice ? ` notified notify-${notice.color}` : ""}`} key={session.name} data-testid={`session-${session.name}`}>
        {batch ? <input type="checkbox" className="batch-check" aria-label={`选择 ${session.name}`} data-testid={`batch-${session.name}`} checked={selected.has(session.name)} onChange={() => toggleSelected(session.name)} /> : <Terminal className="terminal-icon" aria-hidden="true" />}
        <button className="session-open" type="button" disabled={!online || !canOpen || batch} onClick={() => onOpen(session.name)}><span className="session-info"><strong>{notice && <span className={`notify-dot ${notice.color}`} aria-hidden="true" />}{session.name}</strong><span>{notice ? <span className="notify-message" data-testid="session-notify" data-color={notice.color}>{notice.message || "有新提醒"}</span> : <>{session.windows} 个窗口{session.attached ? " · 已连接" : ""}</>}</span>{batchErrors[session.name] && <span className="batch-error">{batchErrors[session.name]}</span>}</span><span className="session-enter">{canOpen ? "进入" : "只读暂未开放"}</span></button>
        {!batch && <div className="session-actions">
          {notice && canClearNotification && <button type="button" title="清除提醒" aria-label={`清除 ${session.name} 的提醒`} data-testid={`clear-notify-${session.name}`} disabled={clear.isPending} onClick={() => clear.mutate(session.name)}><BellOff /></button>}
          <button type="button" className={isPinned ? "active" : ""} title={isPinned ? "取消置顶" : "置顶"} aria-label={`${isPinned ? "取消置顶" : "置顶"} ${session.name}`} aria-pressed={isPinned} data-testid={`pin-${session.name}`} onClick={() => onTogglePin(session.name)}>{isPinned ? <PinOff /> : <Pin />}</button>
          {online && canManage && <><button type="button" title={`重命名 ${session.name}`} aria-label={`重命名 ${session.name}`} disabled={action.isPending} onClick={() => { setRenaming(session.name); setRenameName(session.name); setInputError(""); action.reset(); }}><Pencil /></button>{canClose && <button type="button" className="delete-button" title={`关闭 ${session.name}`} aria-label={`关闭 ${session.name}`} disabled={action.isPending} onClick={() => { action.reset(); setClosing(session.name); }}><Trash2 /></button>}</>}
        </div>}
        {renaming === session.name && <form className="session-form rename-form" onSubmit={rename}><label className="sr-only" htmlFor={`rename-${deviceId}-${session.name}`}>新的会话名称</label><input id={`rename-${deviceId}-${session.name}`} autoFocus value={renameName} maxLength={64} onChange={(event) => setRenameName(event.target.value)} disabled={action.isPending} /><button type="submit" disabled={action.isPending}>保存</button><button type="button" className="subtle-button" onClick={() => setRenaming(null)}>取消</button></form>}
      </li>;
    })}</ul>}
    {closing && <ActionDialog title="关闭会话" description={`确定关闭“${closing}”？会话中的程序也会结束。`} confirmLabel="关闭会话" destructive busy={action.isPending} error={action.isError ? action.error.message : undefined} onClose={() => { setClosing(null); action.reset(); }} onSubmit={() => action.mutate({ kind: "close", name: closing })} />}
    {batchConfirm && <ActionDialog title={`关闭 ${selected.size} 个会话`} description={`将关闭：${[...selected].join("、")}。这些会话中的程序都会结束。`} confirmLabel="全部关闭" destructive busy={batchBusy} onClose={() => { if (!batchBusy) setBatchConfirm(false); }} onSubmit={() => void closeSelected()} />}
  </div>;
}
