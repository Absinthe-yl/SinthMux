import { FitAddon } from "@xterm/addon-fit";
import { Terminal as XTerm } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { ArrowLeft, Maximize2 } from "lucide-react";
import { useEffect, useRef, useState } from "react";

export default function TerminalView({ deviceId, deviceName, session, theme, onBack }: { deviceId: string; deviceName: string; session: string; theme: "light" | "dark"; onBack: () => void }) {
  const host = useRef<HTMLDivElement>(null);
  const terminal = useRef<XTerm | null>(null);
  const [state, setState] = useState("连接中…");

  useEffect(() => {
    if (!terminal.current) return;
    const styles = getComputedStyle(document.documentElement);
    terminal.current.options.theme = { background: styles.getPropertyValue("--terminal-bg").trim(), foreground: styles.getPropertyValue("--terminal-fg").trim(), cursor: styles.getPropertyValue("--terminal-fg").trim() };
  }, [theme]);

  useEffect(() => {
    if (!host.current) return;
    const abort = new AbortController();
    const styles = getComputedStyle(document.documentElement);
    const term = new XTerm({ cursorBlink: true, fontSize: 13, fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace", theme: {
      background: styles.getPropertyValue("--terminal-bg").trim(),
      foreground: styles.getPropertyValue("--terminal-fg").trim(),
      cursor: styles.getPropertyValue("--terminal-fg").trim()
    } });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host.current);
    terminal.current = term;
    let socket: WebSocket | undefined;
    let disposed = false;
    const resize = () => {
      fit.fit();
      if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
    };
    const observer = new ResizeObserver(resize);
    observer.observe(host.current);
    const onInput = term.onData((input) => {
      if (socket?.readyState === WebSocket.OPEN) socket.send(new TextEncoder().encode(input));
    });
    const connect = async () => {
      try {
        const path = `/api/v1/devices/${encodeURIComponent(deviceId)}/sessions/${encodeURIComponent(session)}/ticket`;
        const response = await fetch(path, { method: "POST", signal: abort.signal });
        if (!response.ok) {
          const body = await response.json().catch(() => ({})) as { error?: string };
          throw new Error(body.error ?? `连接失败：${response.status}`);
        }
        const { ticket } = await response.json() as { ticket: string };
        if (disposed) return;
        const scheme = location.protocol === "https:" ? "wss:" : "ws:";
        socket = new WebSocket(`${scheme}//${location.host}/ws/v1/terminal`, ["sinthmux.v1", `sinthmux.ticket.${ticket}`]);
        socket.binaryType = "arraybuffer";
        socket.onopen = () => { setState("已连接"); resize(); term.focus(); };
        socket.onmessage = (event: MessageEvent<ArrayBuffer>) => { if (event.data instanceof ArrayBuffer) term.write(new Uint8Array(event.data)); };
        socket.onclose = (event) => { if (!disposed) setState(event.reason || "连接已断开"); };
        socket.onerror = () => { if (!disposed) setState("连接失败"); };
      } catch (error) {
        if (!disposed) setState(error instanceof Error ? error.message : "连接失败");
      }
    };
    void connect();
    return () => { disposed = true; abort.abort(); socket?.close(); observer.disconnect(); onInput.dispose(); terminal.current = null; term.dispose(); };
  }, [deviceId, session]);

  return <section className="terminal-page" aria-label={`${session} 终端`}>
    <div className="terminal-toolbar"><button className="terminal-back" type="button" onClick={onBack}><ArrowLeft />返回会话</button><div className="terminal-heading"><strong>{session}</strong><span>{deviceName}</span></div><span className="terminal-state">{state}</span><button className="terminal-expand" type="button" title="聚焦终端" aria-label="聚焦终端" onClick={() => host.current?.querySelector<HTMLElement>(".xterm-helper-textarea")?.focus()}><Maximize2 /></button></div>
    <div className="terminal-surface" ref={host} />
  </section>;
}
