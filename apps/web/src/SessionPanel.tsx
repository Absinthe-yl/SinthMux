import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { FormEvent, useState } from "react";
import { TerminalSquare } from "lucide-react";

type TmuxSession = { name: string; windows: number; attached: boolean; createdAt: number };
type Action = { kind: "create"; name: string } | { kind: "rename"; name: string; newName: string } | { kind: "close"; name: string };

const namePattern = /^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$/;

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, { ...init, headers: { "Content-Type": "application/json", ...init?.headers } });
  if (!response.ok) {
    const body = await response.json().catch(() => ({})) as { error?: string };
    throw new Error(body.error ?? `请求失败：${response.status}`);
  }
  return response.status === 204 ? undefined as T : response.json();
}

export default function SessionPanel({ deviceId, online, canManage }: { deviceId: string; online: boolean; canManage: boolean }) {
  const queryClient = useQueryClient();
  const [newName, setNewName] = useState("");
  const [renaming, setRenaming] = useState<string | null>(null);
  const [renameName, setRenameName] = useState("");
  const [inputError, setInputError] = useState("");
  const base = `/api/v1/devices/${encodeURIComponent(deviceId)}/sessions`;
  const queryKey = ["sessions", deviceId];
  const sessions = useQuery({ queryKey, queryFn: () => request<{ sessions: TmuxSession[] }>(base), retry: false });
  const action = useMutation({
    mutationFn: (item: Action) => {
      if (item.kind === "create") return request(`${base}`, { method: "POST", body: JSON.stringify({ name: item.name }) });
      const path = `${base}/${encodeURIComponent(item.name)}`;
      if (item.kind === "rename") return request(path, { method: "PATCH", body: JSON.stringify({ name: item.newName }) });
      return request(path, { method: "DELETE" });
    },
    onSuccess: (_result, item) => {
      setInputError("");
      if (item.kind === "create") setNewName("");
      if (item.kind === "rename") setRenaming(null);
      void queryClient.invalidateQueries({ queryKey });
    }
  });

  const create = (event: FormEvent) => {
    event.preventDefault();
    const name = newName.trim();
    if (!namePattern.test(name)) { setInputError("名称需以字母或数字开头，只能使用字母、数字、下划线和连字符，最长 64 字符。"); return; }
    setInputError("");
    action.mutate({ kind: "create", name });
  };
  const rename = (event: FormEvent) => {
    event.preventDefault();
    if (renaming === null) return;
    const name = renameName.trim();
    if (!namePattern.test(name)) { setInputError("名称需以字母或数字开头，只能使用字母、数字、下划线和连字符，最长 64 字符。"); return; }
    if (name === renaming) { setRenaming(null); return; }
    setInputError("");
    action.mutate({ kind: "rename", name: renaming, newName: name });
  };
  const close = (name: string) => {
    if (window.confirm(`确定关闭会话“${name}”？会话中的程序也会结束。`)) action.mutate({ kind: "close", name });
  };

  return <div className="sessions">
    {online && canManage && <form className="session-form" onSubmit={create}><input aria-label="新会话名称" placeholder="新会话名称" value={newName} onChange={(event) => setNewName(event.target.value)} maxLength={64} disabled={action.isPending} /><button type="submit" disabled={action.isPending}>创建会话</button></form>}
    {!online && <p>设备离线，暂时无法管理会话。</p>}{online && !canManage && <p>当前 Agent 尚不支持会话管理，请更新 Agent 后再试。</p>}
    {inputError && <div className="error">{inputError}</div>}
    {action.isError && <div className="error">操作失败：{action.error.message}</div>}
    {sessions.isPending || sessions.isFetching ? <p>正在读取会话…</p> : sessions.isError ? <div className="error">读取失败：{sessions.error.message} <button onClick={() => sessions.refetch()}>重试</button></div> : sessions.data.sessions.length === 0 ? <p>这台设备还没有 tmux 会话。</p> : <ul>{sessions.data.sessions.map((session) => <li key={session.name}>
      <TerminalSquare /><div className="session-info"><strong>{session.name}</strong><span>{session.windows} 个窗口 · {session.attached ? "已连接" : "未连接"}</span></div>
      {online && canManage && <div className="session-actions"><button type="button" disabled={action.isPending} onClick={() => { setRenaming(session.name); setRenameName(session.name); setInputError(""); action.reset(); }}>重命名</button><button type="button" className="danger" disabled={action.isPending} onClick={() => close(session.name)}>关闭</button></div>}
      {renaming === session.name && <form className="session-form rename-form" onSubmit={rename}><input aria-label="新的会话名称" value={renameName} maxLength={64} onChange={(event) => setRenameName(event.target.value)} disabled={action.isPending} /><button type="submit" disabled={action.isPending}>保存</button><button type="button" onClick={() => setRenaming(null)}>取消</button></form>}
    </li>)}</ul>}
  </div>;
}
