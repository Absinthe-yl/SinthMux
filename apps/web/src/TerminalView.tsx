import { FitAddon } from "@xterm/addon-fit";
import { Terminal as XTerm } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { ArrowLeft, CornerDownLeft, Keyboard, Maximize2, Minus, Plus } from "lucide-react";
import { useEffect, useRef, useState, type FormEvent, type PointerEvent } from "react";
import { APIError, request } from "./api";
import { consumeModifiers, keySequence, modifyText, nextModifier, noModifiers, type Modifiers, type SpecialKey } from "./terminalKeys";

// Phones and tablets: touch input, or a narrow screen where a hardware keyboard is unlikely.
const touchDevice = typeof matchMedia === "function" && matchMedia("(pointer: coarse), (max-width: 600px)").matches;
const fontSizes = [11, 12, 13, 14, 15, 16, 18];
const defaultFontSize = touchDevice ? 13 : 15;

function savedFontSize() {
  const value = Number(localStorage.getItem("sinthmux-terminal-font"));
  return fontSizes.includes(value) ? value : defaultFontSize;
}

// Buttons in the key bar must not take focus, or the phone keyboard closes.
const keepFocus = (event: PointerEvent) => event.preventDefault();

const specialKeys: { label: string; key: SpecialKey; title: string }[] = [
  { label: "Esc", key: "Escape", title: "Esc" },
  { label: "Tab", key: "Tab", title: "Tab 补全" },
  { label: "↑", key: "Up", title: "上一条命令" },
  { label: "↓", key: "Down", title: "下一条命令" },
  { label: "←", key: "Left", title: "左移" },
  { label: "→", key: "Right", title: "右移" },
  { label: "Home", key: "Home", title: "行首" },
  { label: "End", key: "End", title: "行尾" },
  { label: "PgUp", key: "PageUp", title: "上翻页" },
  { label: "PgDn", key: "PageDown", title: "下翻页" }
];

const shortcuts: { label: string; data: string; title: string }[] = [
  { label: "^C", data: "\x03", title: "Ctrl+C 中断" },
  { label: "^D", data: "\x04", title: "Ctrl+D 退出" },
  { label: "^Z", data: "\x1a", title: "Ctrl+Z 挂起" },
  { label: "^L", data: "\x0c", title: "Ctrl+L 清屏" },
  { label: "^R", data: "\x12", title: "Ctrl+R 搜索历史" },
  { label: "^A", data: "\x01", title: "Ctrl+A 行首" },
  { label: "^E", data: "\x05", title: "Ctrl+E 行尾" },
  { label: "^U", data: "\x15", title: "Ctrl+U 删除到行首" },
  { label: "tmux", data: "\x02", title: "tmux 前缀 Ctrl+B" }
];

const symbols = ["|", "~", "/", "-", "_", "*", "&", ";", ">", "$", "`"];

export default function TerminalView({ deviceId, deviceName, session, theme, onBack }: { deviceId: string; deviceName: string; session: string; theme: "light" | "dark"; onBack: () => void }) {
  const host = useRef<HTMLDivElement>(null);
  const page = useRef<HTMLElement>(null);
  const terminal = useRef<XTerm | null>(null);
  const fitRef = useRef<() => void>(() => {});
  const sendRef = useRef<(data: string) => void>(() => {});
  const modsRef = useRef<Modifiers>(noModifiers);
  const [state, setState] = useState("连接中…");
  const [showKeys, setShowKeys] = useState(touchDevice);
  const [mods, setModsState] = useState<Modifiers>(noModifiers);
  const [fontSize, setFontSize] = useState(savedFontSize);
  const [command, setCommand] = useState("");

  const setMods = (next: Modifiers) => { modsRef.current = next; setModsState(next); };

  useEffect(() => {
    if (!terminal.current) return;
    const styles = getComputedStyle(document.documentElement);
    terminal.current.options.theme = { background: styles.getPropertyValue("--terminal-bg").trim(), foreground: styles.getPropertyValue("--terminal-fg").trim(), cursor: styles.getPropertyValue("--terminal-fg").trim() };
  }, [theme]);

  useEffect(() => {
    localStorage.setItem("sinthmux-terminal-font", String(fontSize));
    if (!terminal.current) return;
    terminal.current.options.fontSize = fontSize;
    fitRef.current();
  }, [fontSize]);

  // The key bar changes the terminal height, so refit after it opens or closes.
  useEffect(() => { fitRef.current(); }, [showKeys]);

  // Keep the terminal above the on-screen keyboard (iOS does not resize the layout viewport).
  useEffect(() => {
    const viewport = window.visualViewport;
    if (!viewport || !touchDevice) return;
    const update = () => {
      page.current?.style.setProperty("--terminal-viewport", `${viewport.height}px`);
      page.current?.style.setProperty("top", `${viewport.offsetTop}px`);
    };
    update();
    viewport.addEventListener("resize", update);
    viewport.addEventListener("scroll", update);
    return () => { viewport.removeEventListener("resize", update); viewport.removeEventListener("scroll", update); };
  }, []);

  useEffect(() => {
    if (!host.current) return;
    const abort = new AbortController();
    const styles = getComputedStyle(document.documentElement);
    const term = new XTerm({ cursorBlink: true, fontSize: savedFontSize(), minimumContrastRatio: 9, fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace", theme: {
      background: styles.getPropertyValue("--terminal-bg").trim(),
      foreground: styles.getPropertyValue("--terminal-fg").trim(),
      cursor: styles.getPropertyValue("--terminal-fg").trim()
    } });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host.current);
    terminal.current = term;
    const textarea = host.current.querySelector<HTMLTextAreaElement>(".xterm-helper-textarea");
    textarea?.setAttribute("autocapitalize", "off");
    textarea?.setAttribute("autocorrect", "off");
    textarea?.setAttribute("spellcheck", "false");
    let socket: WebSocket | undefined;
    let disposed = false;
    let connecting = false;
    let connectedOnce = false;
    let retryCount = 0;
    let retryTimer: ReturnType<typeof setTimeout> | undefined;
    const send = (data: string) => {
      if (socket?.readyState === WebSocket.OPEN) socket.send(new TextEncoder().encode(data));
    };
    sendRef.current = send;
    const resize = () => {
      fit.fit();
      if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
    };
    fitRef.current = resize;
    const observer = new ResizeObserver(resize);
    observer.observe(host.current);
    const onInput = term.onData((input) => {
      // Ctrl/Alt from the key bar apply to the next typed character.
      const current = modsRef.current;
      const output = modifyText(input, current);
      if (output !== input) setMods(consumeModifiers(current));
      send(output);
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
          if (!touchDevice) term.focus();
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
    return () => { disposed = true; abort.abort(); if (retryTimer) clearTimeout(retryTimer); window.removeEventListener("online", reconnectNow); socket?.close(); observer.disconnect(); onInput.dispose(); sendRef.current = () => {}; fitRef.current = () => {}; terminal.current = null; term.dispose(); };
  }, [deviceId, session]);

  const pressKey = (key: SpecialKey) => {
    sendRef.current(keySequence(key, modsRef.current, terminal.current?.modes.applicationCursorKeysMode ?? false));
    setMods(consumeModifiers(modsRef.current));
  };
  const pressText = (text: string) => {
    sendRef.current(modifyText(text, modsRef.current));
    setMods(consumeModifiers(modsRef.current));
  };
  const toggle = (name: keyof Modifiers) => setMods({ ...modsRef.current, [name]: nextModifier(modsRef.current[name]) });
  const sendCommand = (event: FormEvent) => {
    event.preventDefault();
    sendRef.current(`${command}\r`);
    setCommand("");
  };
  const modifierLabel = (name: keyof Modifiers) => mods[name] === "lock" ? "已锁定" : mods[name] === "once" ? "作用于下一个键" : "未启用";
  const changeFont = (step: number) => setFontSize((size) => fontSizes[Math.min(fontSizes.length - 1, Math.max(0, fontSizes.indexOf(size) + step))]);

  return <section className={`terminal-page${showKeys ? " with-keys" : ""}`} ref={page} aria-label={`${session} 终端`}>
    <div className="terminal-toolbar">
      <button className="terminal-back" type="button" onClick={onBack}><ArrowLeft />返回会话</button>
      <div className="terminal-heading"><strong>{session}</strong><span>{deviceName}</span></div>
      <span className="terminal-state">{state}</span>
      <div className="terminal-tools">
        <button type="button" title="缩小字号" aria-label="缩小字号" onClick={() => changeFont(-1)} disabled={fontSize === fontSizes[0]}><Minus /></button>
        <button type="button" title="放大字号" aria-label="放大字号" onClick={() => changeFont(1)} disabled={fontSize === fontSizes[fontSizes.length - 1]}><Plus /></button>
        <button type="button" title="快捷键栏" aria-label="快捷键栏" aria-pressed={showKeys} className={showKeys ? "active" : ""} onClick={() => setShowKeys((value) => !value)}><Keyboard /></button>
        <button className="terminal-expand" type="button" title="聚焦终端" aria-label="聚焦终端" onClick={() => terminal.current?.focus()}><Maximize2 /></button>
      </div>
    </div>
    <div className="terminal-surface" ref={host} />
    {showKeys && <div className="terminal-keys" role="toolbar" aria-label="终端快捷键">
      <div className="key-row">
        {(["ctrl", "alt"] as const).map((name) => <button key={name} type="button" className={`key modifier ${mods[name]}`} title={`${name === "ctrl" ? "Ctrl" : "Alt"}：点一次作用于下一个键，点两次锁定`} aria-label={`${name === "ctrl" ? "Ctrl" : "Alt"}，${modifierLabel(name)}`} onPointerDown={keepFocus} onClick={() => toggle(name)}>{name === "ctrl" ? "Ctrl" : "Alt"}</button>)}
        {specialKeys.map((item) => <button key={item.key} type="button" className="key" title={item.title} aria-label={item.title} onPointerDown={keepFocus} onClick={() => pressKey(item.key)}>{item.label}</button>)}
      </div>
      <div className="key-row">
        {shortcuts.map((item) => <button key={item.label} type="button" className="key shortcut" title={item.title} aria-label={item.title} onPointerDown={keepFocus} onClick={() => { sendRef.current(item.data); setMods(noModifiers); }}>{item.label}</button>)}
        {symbols.map((symbol) => <button key={symbol} type="button" className="key" aria-label={`输入 ${symbol}`} onPointerDown={keepFocus} onClick={() => pressText(symbol)}>{symbol}</button>)}
      </div>
      <form className="command-line" onSubmit={sendCommand}>
        <input value={command} onChange={(event) => setCommand(event.target.value)} placeholder="输入命令，回车发送（可用输入法和粘贴）" aria-label="输入命令" autoCapitalize="off" autoCorrect="off" autoComplete="off" spellCheck={false} enterKeyHint="send" />
        <button type="submit" title="发送并回车" aria-label="发送并回车"><CornerDownLeft /></button>
      </form>
    </div>}
  </section>;
}
