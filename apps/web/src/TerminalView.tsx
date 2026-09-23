import { FitAddon } from "@xterm/addon-fit";
import { Terminal as XTerm } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { ArrowLeft, Maximize2 } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { APIError, request } from "./api";

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
    let connecting = false;
    let connectedOnce = false;
    let retryCount = 0;
    let retryTimer: ReturnType<typeof setTimeout> | undefined;
    const resize = () => {
      fit.fit();
      if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
    };
    const observer = new ResizeObserver(resize);
    observer.observe(host.current);
    const onInput = term.onData((input) => {
      if (socket?.readyState === WebSocket.OPEN) socket.send(new TextEncoder().encode(input));
    });
    const scheduleRetry = () => {
      if (disposed || retryTimer) return;
      const delay = Math.min(1000 * 2 ** retryCount, 10000);
      retryCount++;
      setState(`连接已断开，${delay / 1000} 秒后重连…`);
      retryTimer = setTimeout(() => { retryTimer = undefined; void connect(); }, delay);
    };
    const connect = async () => {
      if (disposed || connecting) return;
      connecting = true;
      setState(connectedOnce ? "正在重新连接…" : "连接中…");
      try {
        const path = `/api/v1/devices/${encodeURIComponent(deviceId)}/sessions/${encodeURIComponent(session)}/ticket`;
        const { ticket } = await request<{ ticket: string }>(path, { method: "POST", signal: abort.signal });
        if (disposed) return;
        const scheme = location.protocol === "https:" ? "wss:" : "ws:";
        const nextSocket = new WebSocket(`${scheme}//${location.host}/ws/v1/terminal`, ["sinthmux.v1", `sinthmux.ticket.${ticket}`]);
        socket = nextSocket;
        nextSocket.binaryType = "arraybuffer";
        nextSocket.onopen = () => {
          if (disposed || socket !== nextSocket) return;
          connecting = false;
          if (connectedOnce) term.reset();
          connectedOnce = true;
          retryCount = 0;
          setState("已连接");
          resize();
          term.focus();
        };
        nextSocket.onmessage = (event: MessageEvent<ArrayBuffer>) => { if (socket === nextSocket && event.data instanceof ArrayBuffer) term.write(new Uint8Array(event.data)); };
        nextSocket.onclose = (event) => {
          if (disposed || socket !== nextSocket) return;
          connecting = false;
          socket = undefined;
          if (event.code === 1008 || (event.code === 1000 && /terminal exited|cannot open tmux session|invalid terminal request/.test(event.reason))) {
            setState(event.reason || "终端会话已结束");
            return;
          }
          scheduleRetry();
        };
        nextSocket.onerror = () => { if (!disposed && socket === nextSocket) setState("连接中断…"); };
      } catch (error) {
        connecting = false;
        if (disposed) return;
        if (error instanceof APIError && [400, 401, 403, 404].includes(error.status)) {
          setState(error.message);
          return;
        }
        scheduleRetry();
      }
    };
    const reconnectNow = () => {
      if (disposed || connecting || socket?.readyState === WebSocket.OPEN || socket?.readyState === WebSocket.CONNECTING) return;
      if (retryTimer) clearTimeout(retryTimer);
      retryTimer = undefined;
      void connect();
    };
    window.addEventListener("online", reconnectNow);
    void connect();
    return () => { disposed = true; abort.abort(); if (retryTimer) clearTimeout(retryTimer); window.removeEventListener("online", reconnectNow); socket?.close(); observer.disconnect(); onInput.dispose(); terminal.current = null; term.dispose(); };
  }, [deviceId, session]);

  return <section className="terminal-page" aria-label={`${session} 终端`}>
    <div className="terminal-toolbar"><button className="terminal-back" type="button" onClick={onBack}><ArrowLeft />返回会话</button><div className="terminal-heading"><strong>{session}</strong><span>{deviceName}</span></div><span className="terminal-state">{state}</span><button className="terminal-expand" type="button" title="聚焦终端" aria-label="聚焦终端" onClick={() => host.current?.querySelector<HTMLElement>(".xterm-helper-textarea")?.focus()}><Maximize2 /></button></div>
    <div className="terminal-surface" ref={host} />
  </section>;
}
