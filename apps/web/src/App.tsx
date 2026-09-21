import { useQuery } from "@tanstack/react-query";
import { Activity, Cloud, Laptop, Plus, RefreshCw, Server, ShieldCheck, TerminalSquare } from "lucide-react";
import { useState } from "react";
import SessionPanel from "./SessionPanel";

type Device = { id: string; name: string; platform: string; architecture: string; agentVersion: string; status: string; lastSeenAt: string; capabilities: string[] };
type Status = { status: string; version: string; connectedDevices: number };

async function getJSON<T>(path: string): Promise<T> {
  const response = await fetch(path);
  if (!response.ok) {
    const body = await response.json().catch(() => ({})) as { error?: string };
    throw new Error(body.error ?? `Request failed: ${response.status}`);
  }
  return response.json();
}

export default function App() {
  const [selectedDevice, setSelectedDevice] = useState<string | null>(null);
  const status = useQuery({ queryKey: ["status"], queryFn: () => getJSON<Status>("/api/v1/system/status"), refetchInterval: 5000 });
  const devices = useQuery({ queryKey: ["devices"], queryFn: () => getJSON<{ devices: Device[] }>("/api/v1/devices"), refetchInterval: 5000 });

  return <main className="shell">
    <header className="topbar"><div className="brand"><TerminalSquare /><div><strong>SinthMux</strong><span>终端相连，现场常在</span></div></div><div className="hub-state"><span className={status.data?.status === "ok" ? "dot online" : "dot"} />Hub {status.data?.status === "ok" ? "在线" : "连接中"}<code>{status.data?.version ?? "-"}</code></div></header>
    <section className="toolbar"><div><h1>所有设备</h1><p>通过 Agent 主动出站连接到这个 Hub</p></div><div className="commands"><button title="刷新" onClick={() => { status.refetch(); devices.refetch(); }}><RefreshCw /></button><button className="primary"><Plus />添加设备</button></div></section>
    <section className="summary"><div><Activity /><span>在线设备</span><strong>{devices.data?.devices.filter((item) => item.status === "online").length ?? 0}</strong></div><div><ShieldCheck /><span>连接模式</span><strong>Agent WSS</strong></div><div><Cloud /><span>公网入口</span><strong>待配置</strong></div></section>
    {devices.isError && <div className="error">Hub 暂不可用，请先运行 <code>make dev-hub</code>。</div>}
    <section className="device-list">{devices.data?.devices.length ? devices.data.devices.map((device) => <article className="device" key={device.id}><div className="device-icon">{device.platform === "darwin" ? <Laptop /> : <Server />}</div><div className="device-copy"><div><h2>{device.name}</h2><span className={`badge ${device.status}`}>{device.status === "online" ? "在线" : "离线"}</span></div><p>{device.platform} / {device.architecture} · Agent {device.agentVersion}</p><small>{device.capabilities.join(" · ")}</small></div><button className="open" onClick={() => setSelectedDevice(selectedDevice === device.id ? null : device.id)}>{selectedDevice === device.id ? "收起会话" : "查看会话"}</button>{selectedDevice === device.id && <SessionPanel deviceId={device.id} online={device.status === "online"} canManage={device.capabilities.includes("tmux.sessions.manage")} />}</article>) : <div className="empty"><Server /><h2>还没有设备</h2><p>在另一台机器运行 Agent 后，它会出现在这里。</p><code>make dev-agent</code></div>}</section>
  </main>;
}
