import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { FormEvent, useState } from "react";
import { Pencil, Plus, Terminal, Trash2 } from "lucide-react";

type TmuxSession = { name: string; windows: number; attached: boolean; createdAt: number };
type Action = { kind: "create"; name: string } | { kind: "rename"; name: string; newName: string } | { kind: "close"; name: string };

const namePattern = /^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$/;
const nameHint = "名称只能使用字母、数字、下划线和连字符，且需以字母或数字开头（最多 64 字符）。";

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, { ...init, headers: { "Content-Type": "application/json", ...init?.headers } });
  if (!response.ok) {
    const body = await response.json().catch(() => ({})) as { error?: string };
    throw new Error(body.error ?? `请求失败：${response.status}`);
  }
  return response.status === 204 ? undefined as T : response.json();
}

export default function SessionPanel({ deviceId, online, canManage, onOpen }: { deviceId: string; online: boolean; canManage: boolean; onOpen: (session: string) => void }) {
  const queryClient = useQueryClient();
  const [creating, setCreating] = useState(false);
  const [newName, setNewName] = useState("");
  const [renaming, setRenaming] = useState<string | null>(null);
  const [renameName, setRenameName] = useState("");
  const [inputError, setInputError] = useState("");
  const base = `/api/v1/devices/${encodeURIComponent(deviceId)}/sessions`;
  const queryKey = ["sessions", deviceId];
  const sessions = useQuery({ queryKey, queryFn: () => request<{ sessions: TmuxSession[] }>(base), retry: false, refetchInterval: online ? 10000 : false });
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
      if (item.kind === "rename") setRenaming(null);
      void queryClient.invalidateQueries({ queryKey });
    }
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
  const close = (name: string) => {
    if (window.confirm(`确定关闭会话“${name}”？会话中的程序也会结束。`)) action.mutate({ kind: "close", name });
  };

  return <div className="sessions">
    <div className="sessions-header"><strong>会话 <span>{sessions.data?.sessions.length ?? "—"}</span></strong>{online && canManage && <button className="new-session-button" type="button" onClick={() => { setCreating(!creating); setInputError(""); action.reset(); }}><Plus />新建会话</button>}</div>
    {creating && <form className="session-form create-form" onSubmit={create}><label className="sr-only" htmlFor={`new-session-${deviceId}`}>新会话名称</label><input id={`new-session-${deviceId}`} autoFocus placeholder="会话名称" value={newName} onChange={(event) => setNewName(event.target.value)} maxLength={64} disabled={action.isPending} /><button type="submit" disabled={action.isPending}>创建</button><button type="button" className="subtle-button" onClick={() => setCreating(false)}>取消</button></form>}
    {!online && <p className="session-note">设备离线</p>}
    {online && !canManage && <p className="session-note">Agent 尚不支持会话管理</p>}
    {inputError && <div className="notice error">{inputError}</div>}
    {action.isError && <div className="notice error">操作失败：{action.error.message}</div>}
    {sessions.isPending ? <p className="session-note">正在读取会话…</p> : sessions.isError ? <div className="notice error">读取失败：{sessions.error.message} <button className="text-button" type="button" onClick={() => sessions.refetch()}>重试</button></div> : sessions.data.sessions.length === 0 ? <p className="session-note empty-sessions">暂无会话</p> : <ul className="session-list">{sessions.data.sessions.map((session) => <li className="session-row" key={session.name}>
      <Terminal className="terminal-icon" aria-hidden="true" /><button className="session-open" type="button" disabled={!online} onClick={() => onOpen(session.name)}><span className="session-info"><strong>{session.name}</strong><span>{session.windows} 个窗口{session.attached ? " · 已连接" : ""}</span></span><span className="session-enter">进入</span></button>
      {online && canManage && <div className="session-actions"><button type="button" title={`重命名 ${session.name}`} aria-label={`重命名 ${session.name}`} disabled={action.isPending} onClick={() => { setRenaming(session.name); setRenameName(session.name); setInputError(""); action.reset(); }}><Pencil /></button><button type="button" className="delete-button" title={`关闭 ${session.name}`} aria-label={`关闭 ${session.name}`} disabled={action.isPending} onClick={() => close(session.name)}><Trash2 /></button></div>}
      {renaming === session.name && <form className="session-form rename-form" onSubmit={rename}><label className="sr-only" htmlFor={`rename-${deviceId}-${session.name}`}>新的会话名称</label><input id={`rename-${deviceId}-${session.name}`} autoFocus value={renameName} maxLength={64} onChange={(event) => setRenameName(event.target.value)} disabled={action.isPending} /><button type="submit" disabled={action.isPending}>保存</button><button type="button" className="subtle-button" onClick={() => setRenaming(null)}>取消</button></form>}
    </li>)}</ul>}
  </div>;
}
