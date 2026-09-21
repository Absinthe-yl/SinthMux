import { useQuery } from "@tanstack/react-query";
import { ChevronDown, ChevronUp, Laptop, Moon, RefreshCw, Server, Sun } from "lucide-react";
import { useLayoutEffect, useState } from "react";
import SessionPanel from "./SessionPanel";

type Device = {
  id: string;
  name: string;
  platform: string;
  architecture: string;
  agentVersion: string;
  status: string;
  capabilities: string[];
};
type Status = { status: string; version: string; connectedDevices: number };
type Theme = "light" | "dark";

function initialTheme(): Theme {
  try {
    const saved = localStorage.getItem("sinthmux-theme");
    if (saved === "light" || saved === "dark") return saved;
  } catch { /* Storage may be unavailable. */ }
  return window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
}

async function getJSON<T>(path: string): Promise<T> {
  const response = await fetch(path);
  if (!response.ok) throw new Error(`请求失败：${response.status}`);
  return response.json();
}

function DeviceCard({ device, expanded, onToggle }: { device: Device; expanded: boolean; onToggle: () => void }) {
  const online = device.status === "online";
  return <article className={`device-card${expanded ? " expanded" : ""}`}>
    <div className="device-main">
      <div className="device-icon" aria-hidden="true">{device.platform === "darwin" ? <Laptop /> : <Server />}</div>
      <div className="device-info">
        <div className="device-title"><h2>{device.name}</h2><span className={`status-dot${online ? " online" : ""}`} /><span className="status-text">{online ? "在线" : "离线"}</span></div>
        <p>{device.platform} / {device.architecture} <span>·</span> Agent {device.agentVersion}</p>
      </div>
      <button className="device-toggle" type="button" aria-expanded={expanded} onClick={onToggle}>{expanded ? "收起" : "会话"}{expanded ? <ChevronUp /> : <ChevronDown />}</button>
    </div>
    {expanded && <SessionPanel deviceId={device.id} online={online} canManage={device.capabilities.includes("tmux.sessions.manage")} />}
  </article>;
}

export default function App() {
  const [theme, setTheme] = useState<Theme>(initialTheme);
  const [selectedDevice, setSelectedDevice] = useState<string | null>(null);
  useLayoutEffect(() => {
    document.documentElement.dataset.theme = theme;
    try { localStorage.setItem("sinthmux-theme", theme); } catch { /* Theme still works for this visit. */ }
  }, [theme]);

  const status = useQuery({ queryKey: ["status"], queryFn: () => getJSON<Status>("/api/v1/system/status"), refetchInterval: 5000 });
  const devices = useQuery({ queryKey: ["devices"], queryFn: () => getJSON<{ devices: Device[] }>("/api/v1/devices"), refetchInterval: 5000 });
  const items = devices.data?.devices ?? [];
  const hubOnline = !status.isError && status.data?.status === "ok";

  return <div className="app-frame">
    <header className="topbar"><div className="topbar-inner">
      <a className="brand" href="#top" aria-label="SinthMux 首页"><span className="brand-mark" aria-hidden="true">S/</span><strong>SinthMux</strong></a>
      <div className="top-actions"><span className="hub-status"><span className={`status-dot${hubOnline ? " online" : ""}`} />Hub {hubOnline ? "在线" : status.isError ? "离线" : "连接中"}</span><button className="icon-button" type="button" title={theme === "dark" ? "切换浅色模式" : "切换深色模式"} aria-label={theme === "dark" ? "切换浅色模式" : "切换深色模式"} onClick={() => setTheme(theme === "dark" ? "light" : "dark")}>{theme === "dark" ? <Sun /> : <Moon />}</button></div>
    </div></header>

    <main className="shell" id="top">
      <div className="page-heading"><div><h1>设备 <span>{items.length}</span></h1></div><button className="refresh-button" type="button" onClick={() => { void status.refetch(); void devices.refetch(); }}><RefreshCw />刷新</button></div>
      {devices.isError && <div className="notice error">无法读取设备列表，请确认 Hub 已启动。</div>}
      <section className="device-list" aria-label="设备列表">{items.length ? items.map((device) => <DeviceCard key={device.id} device={device} expanded={selectedDevice === device.id} onToggle={() => setSelectedDevice(selectedDevice === device.id ? null : device.id)} />) : !devices.isError && <div className="empty-state"><Server /><strong>{devices.isPending ? "正在加载设备" : "还没有设备"}</strong><span>{devices.isPending ? "" : "运行 Agent 后，设备会出现在这里。"}</span></div>}</section>
    </main>
  </div>;
}
