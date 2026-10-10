import { FitAddon } from "@xterm/addon-fit";
import { Terminal as XTerm } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { ArrowLeft, CornerDownLeft, Download, Keyboard, Maximize2, Minus, Plus, RotateCw, Upload } from "lucide-react";
import { useEffect, useRef, useState, type FormEvent, type PointerEvent } from "react";
import { APIError, downloadHistory, uploadFile } from "./api";
import { insertPaths, pasteFiles } from "./paste";
import { stateText, TerminalConnection, type ConnectionState } from "./terminalConnection";
import { attachMobileInput } from "./mobileInput";
import { consumeModifiers, keySequence, modifyText, nextModifier, noModifiers, type Modifiers, type SpecialKey } from "./terminalKeys";

// Phones and tablets: touch input, or a narrow screen where a hardware keyboard is unlikely.
const touchDevice = typeof matchMedia === "function" && matchMedia("(pointer: coarse), (max-width: 600px)").matches;
const fontSizes = [11, 12, 13, 14, 15, 16, 18];
const defaultFontSize = touchDevice ? 13 : 15;

function savedFontSize() {
  const value = Number(readPref("sinthmux-terminal-font"));
  return fontSizes.includes(value) ? value : defaultFontSize;
}

// localStorage throws when the browser blocks site storage.
function readPref(key: string) { try { return localStorage.getItem(key); } catch { return null; } }
function writePref(key: string, value: string) { try { localStorage.setItem(key, value); } catch { /* storage disabled */ } }

// Buttons in the key bar must not take focus, or the phone keyboard closes.
const keepFocus = (event: PointerEvent) => event.preventDefault();

// Key bar. Every key here was checked against bash (--norc) and zsh (-f) in
// tmux: it either does what its label says in a stock shell, or is a key that
// full-screen programs (vim, less, htop, Claude Code / Codex prompts) need and
// phones cannot type. Keys that only work with custom shell bindings (Home,
// PgUp/PgDn, Delete, Ctrl+←/→) were dropped from the line-editing rows; Ctrl/Alt
// plus the soft keyboard covers the rest (e.g. Ctrl then "w").
type BarKey = { label: string; title: string } & ({ key: SpecialKey } | { data: string } | { text: string });

const keyGroups: { id: string; label: string; keys: BarKey[] }[] = [
  { id: "edit", label: "常用", keys: [
    { label: "Esc", key: "Escape", title: "Esc：退出 vim 插入模式、关闭补全菜单" },
    { label: "Tab", key: "Tab", title: "Tab：补全命令和路径" },
    { label: "↑", key: "Up", title: "↑：上一条命令" },
    { label: "↓", key: "Down", title: "↓：下一条命令" },
    { label: "←", key: "Left", title: "←：光标左移" },
    { label: "→", key: "Right", title: "→：光标右移" },
    { label: "^C", data: "\x03", title: "Ctrl+C：中断正在运行的程序" },
    { label: "^R", data: "\x12", title: "Ctrl+R：搜索历史命令" },
    { label: "^A", data: "\x01", title: "Ctrl+A：跳到行首" },
    { label: "^E", data: "\x05", title: "Ctrl+E：跳到行尾" },
    { label: "^W", data: "\x17", title: "Ctrl+W：删除前一个词" },
    { label: "^U", data: "\x15", title: "Ctrl+U：删除到行首" },
    { label: "^K", data: "\x0b", title: "Ctrl+K：删除到行尾" },
    { label: "⌥B", data: "\x1bb", title: "Alt+B：后退一个词" },
    { label: "⌥F", data: "\x1bf", title: "Alt+F：前进一个词" },
    { label: "⌥.", data: "\x1b.", title: "Alt+.：插入上一条命令的最后一个参数" },
    { label: "^L", data: "\x0c", title: "Ctrl+L：清屏" },
    { label: "^D", data: "\x04", title: "Ctrl+D：退出（空行时）" },
    { label: "^Z", data: "\x1a", title: "Ctrl+Z：挂起到后台，fg 恢复" }
  ] },
  { id: "symbols", label: "符号", keys: [..."|~/-_*&;<>$`'\"\\{}[]()#!=%^@:?"].map((text) => ({ label: text, text, title: `输入 ${text}` })) },
  { id: "tmux", label: "tmux", keys: [
    { label: "前缀", data: "\x02", title: "Ctrl+B：tmux 前缀键，之后再按一个键" },
    { label: "滚动", data: "\x02[", title: "进入复制/滚动模式，用 ↑↓ 或 PgUp/PgDn 查看历史输出，q 退出" },
    { label: "PgUp", key: "PageUp", title: "PgUp：上翻一页（tmux 滚动模式、less、vim）" },
    { label: "PgDn", key: "PageDown", title: "PgDn：下翻一页" },
    { label: "新窗口", data: "\x02c", title: "Ctrl+B c：新建窗口" },
    { label: "下一窗", data: "\x02n", title: "Ctrl+B n：切到下一个窗口" },
    { label: "上一窗", data: "\x02p", title: "Ctrl+B p：切到上一个窗口" },
    { label: "左右分屏", data: "\x02%", title: "Ctrl+B %：左右分屏" },
    { label: "上下分屏", data: "\x02\"", title: "Ctrl+B \"：上下分屏" },
    { label: "切窗格", data: "\x02o", title: "Ctrl+B o：切到下一个窗格" },
    { label: "放大", data: "\x02z", title: "Ctrl+B z：放大/还原当前窗格" },
    { label: "q", text: "q", title: "q：退出滚动模式、less、man" }
  ] },
  { id: "keys", label: "功能键", keys: [
    { label: "Home", key: "Home", title: "Home（vim、less 中有效）" },
    { label: "End", key: "End", title: "End" },
    { label: "Del", key: "Delete", title: "Delete：删除光标后的字符（vim 等）" },
    ...(["F1", "F2", "F3", "F4", "F5", "F6", "F7", "F8", "F9", "F10", "F11", "F12"] as const).map((key) => ({ label: key, key, title: `${key}（htop、mc 等程序使用）` }))
  ] }
];

export default function TerminalView({ deviceId, deviceName, session, theme, capabilities, onBack }: { deviceId: string; deviceName: string; session: string; theme: "light" | "dark"; capabilities: string[]; onBack: () => void }) {
  const host = useRef<HTMLDivElement>(null);
  const page = useRef<HTMLElement>(null);
  const terminal = useRef<XTerm | null>(null);
  const fitRef = useRef<() => void>(() => {});
  const sendRef = useRef<(data: string) => void>(() => {});
  const modsRef = useRef<Modifiers>(noModifiers);
  const [connection, setConnection] = useState<ConnectionState>({ kind: "connecting", reconnect: false });
  const connectionRef = useRef<TerminalConnection | null>(null);
  const transferBusy = useRef(false);
  const uploadRef = useRef<(files: File[]) => void>(() => {});
  const fileInput = useRef<HTMLInputElement>(null);
  const [transfer, setTransfer] = useState("");
  const [exportOpen, setExportOpen] = useState(false);
  const canUpload = capabilities.includes("file.upload.v1");
  const canExport = capabilities.includes("terminal.export.v1");
  // Upload/export messages replace the connection state for a few seconds.
  useEffect(() => {
    if (!transfer || transfer.startsWith("上传 ") || transfer.startsWith("正在导出")) return;
    const timer = setTimeout(() => setTransfer(""), 6000);
    return () => clearTimeout(timer);
  }, [transfer]);
  const [showKeys, setShowKeys] = useState(touchDevice);
  const [mods, setModsState] = useState<Modifiers>(noModifiers);
  const [fontSize, setFontSize] = useState(savedFontSize);
  const [command, setCommand] = useState("");
  const [keyGroup, setKeyGroup] = useState(() => readPref("sinthmux-key-group") ?? "edit");
  useEffect(() => { writePref("sinthmux-key-group", keyGroup); }, [keyGroup]);

  const setMods = (next: Modifiers) => { modsRef.current = next; setModsState(next); };

  useEffect(() => {
    if (!terminal.current) return;
    const styles = getComputedStyle(document.documentElement);
    terminal.current.options.theme = { background: styles.getPropertyValue("--terminal-bg").trim(), foreground: styles.getPropertyValue("--terminal-fg").trim(), cursor: styles.getPropertyValue("--terminal-fg").trim() };
  }, [theme]);

  useEffect(() => {
    writePref("sinthmux-terminal-font", String(fontSize));
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
    let detachMobileInput = () => {};
    const connection = new TerminalConnection(deviceId, session, {
      onState: (next) => { setConnection(next); if (next.kind !== "live") setTransfer(""); },
      onOutput: (data) => term.write(data),
      onReset: () => term.reset(),
      size: () => ({ cols: term.cols, rows: term.rows })
    });
    connectionRef.current = connection;
    const send = (data: string) => {
      if (!connection.send(data)) setTransfer("连接已断开，输入未发送");
    };
    sendRef.current = send;
    const resize = () => {
      fit.fit();
      connection.resize();
    };
    fitRef.current = resize;
    const observer = new ResizeObserver(resize);
    observer.observe(host.current);
    // Ctrl/Alt from the key bar apply to the next typed character.
    const typed = (input: string) => {
      const current = modsRef.current;
      const output = modifyText(input, current);
      if (output !== input) setMods(consumeModifiers(current));
      send(output);
    };
    const onInput = term.onData(typed);
    if (touchDevice && textarea) {
      detachMobileInput = attachMobileInput(host.current, textarea, {
        onText: (text) => { for (const char of text) typed(char); term.scrollToBottom(); },
        onKey: (sequence) => { send(sequence); term.scrollToBottom(); }
      });
    }
    // Files pasted or dropped onto the terminal are uploaded to the device and
    // their paths typed in. Capture phase runs before xterm's own paste handler.
    const surface = host.current;
    const onPaste = (event: ClipboardEvent) => {
      const files = pasteFiles(event.clipboardData);
      if (files.length === 0 || !canUpload) return;
      event.preventDefault();
      event.stopPropagation();
      uploadRef.current(files);
    };
    const onDragOver = (event: DragEvent) => { if (canUpload && event.dataTransfer?.types.includes("Files")) event.preventDefault(); };
    const onDrop = (event: DragEvent) => {
      const files = Array.from(event.dataTransfer?.files ?? []);
      if (files.length === 0 || !canUpload) return;
      event.preventDefault();
      uploadRef.current(files);
    };
    surface.addEventListener("paste", onPaste, true);
    surface.addEventListener("dragover", onDragOver);
    surface.addEventListener("drop", onDrop);
    connection.start();
    return () => {
      connection.dispose();
      connectionRef.current = null;
      surface.removeEventListener("paste", onPaste, true);
      surface.removeEventListener("dragover", onDragOver);
      surface.removeEventListener("drop", onDrop);
      observer.disconnect(); onInput.dispose(); detachMobileInput();
      sendRef.current = () => {}; fitRef.current = () => {}; terminal.current = null; term.dispose();
    };
  }, [deviceId, session, canUpload]);

  const uploadFiles = async (files: File[]) => {
    if (transferBusy.current) { setTransfer("上一个上传还没完成"); return; }
    transferBusy.current = true;
    const paths: string[] = [];
    try {
      for (const [index, file] of files.entries()) {
        const label = files.length > 1 ? `${file.name}（${index + 1}/${files.length}）` : file.name;
        setTransfer(`上传 ${label} 0%`);
        const result = await uploadFile(deviceId, file, (fraction) => setTransfer(`上传 ${label} ${Math.floor(fraction * 100)}%`));
        paths.push(result.path);
      }
      setTransfer(`已上传 ${paths.length} 个文件`);
    } catch (error) {
      setTransfer(error instanceof Error ? error.message : "上传失败");
    } finally {
      transferBusy.current = false;
      if (paths.length > 0) {
        sendRef.current(insertPaths(paths, terminal.current?.modes.bracketedPasteMode ?? false));
        terminal.current?.focus();
      }
    }
  };
  uploadRef.current = (files) => { void uploadFiles(files); };
  const exportHistory = async (lines: "1000" | "10000" | "all") => {
    setExportOpen(false);
    setTransfer("正在导出终端历史…");
    try {
      await downloadHistory(deviceId, deviceName, session, lines);
      setTransfer("终端历史已导出");
    } catch (error) {
      setTransfer(error instanceof APIError || error instanceof Error ? error.message : "导出失败");
    }
  };
  const pressKey = (key: SpecialKey) => {
    sendRef.current(keySequence(key, modsRef.current, terminal.current?.modes.applicationCursorKeysMode ?? false));
    setMods(consumeModifiers(modsRef.current));
  };
  const pressText = (text: string) => {
    sendRef.current(modifyText(text, modsRef.current));
    setMods(consumeModifiers(modsRef.current));
  };
  const pressBarKey = (item: BarKey) => {
    if ("key" in item) pressKey(item.key);
    else if ("text" in item) pressText(item.text);
    else { sendRef.current(item.data); setMods(noModifiers); }
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
      <span className="terminal-state" data-testid="terminal-state" data-state={connection.kind}>{transfer || stateText(connection)}</span>
      <div className="terminal-tools">
        {connection.kind !== "live" && connection.kind !== "connecting" && <button type="button" title="立即重连" aria-label="立即重连" data-testid="terminal-reconnect" onClick={() => { setTransfer(""); connectionRef.current?.reconnectNow(); }}><RotateCw /></button>}
        {canUpload && <button type="button" title="上传文件到设备（也可直接粘贴或拖入终端）" aria-label="上传文件" data-testid="terminal-upload" onClick={() => fileInput.current?.click()}><Upload /></button>}
        {canUpload && <input ref={fileInput} type="file" multiple hidden data-testid="terminal-upload-input" onChange={(event) => { const files = Array.from(event.target.files ?? []); event.target.value = ""; if (files.length) void uploadFiles(files); }} />}
        {canExport && <span className="export-menu"><button type="button" title="导出终端历史" aria-label="导出终端历史" aria-expanded={exportOpen} data-testid="terminal-export" onClick={() => setExportOpen((open) => !open)}><Download /></button>
          {exportOpen && <span className="export-options" role="menu">
            <button type="button" role="menuitem" data-testid="export-1000" onClick={() => void exportHistory("1000")}>最近 1000 行</button>
            <button type="button" role="menuitem" data-testid="export-10000" onClick={() => void exportHistory("10000")}>最近 10000 行</button>
            <button type="button" role="menuitem" data-testid="export-all" onClick={() => void exportHistory("all")}>全部历史</button>
          </span>}</span>}
        <button type="button" title="缩小字号" aria-label="缩小字号" onClick={() => changeFont(-1)} disabled={fontSize === fontSizes[0]}><Minus /></button>
        <button type="button" title="放大字号" aria-label="放大字号" onClick={() => changeFont(1)} disabled={fontSize === fontSizes[fontSizes.length - 1]}><Plus /></button>
        <button type="button" title="快捷键栏" aria-label="快捷键栏" aria-pressed={showKeys} className={showKeys ? "active" : ""} onClick={() => setShowKeys((value) => !value)}><Keyboard /></button>
        <button className="terminal-expand" type="button" title="聚焦终端" aria-label="聚焦终端" onClick={() => terminal.current?.focus()}><Maximize2 /></button>
      </div>
    </div>
    <div className="terminal-surface" ref={host} />
    {showKeys && <div className="terminal-keys" role="toolbar" aria-label="终端快捷键">
      <div className="key-tabs" role="tablist" aria-label="按键分组">
        {keyGroups.map((group) => <button key={group.id} type="button" role="tab" aria-selected={keyGroup === group.id} className={keyGroup === group.id ? "active" : ""} onPointerDown={keepFocus} onClick={() => setKeyGroup(group.id)}>{group.label}</button>)}
      </div>
      <div className="key-row">
        {(["ctrl", "alt"] as const).map((name) => <button key={name} type="button" className={`key modifier ${mods[name]}`} title={`${name === "ctrl" ? "Ctrl" : "Alt"}：点一次作用于下一个键（可配合手机键盘字母），点两次锁定`} aria-label={`${name === "ctrl" ? "Ctrl" : "Alt"}，${modifierLabel(name)}`} onPointerDown={keepFocus} onClick={() => toggle(name)}>{name === "ctrl" ? "Ctrl" : "Alt"}</button>)}
        <span className="key-divider" aria-hidden="true" />
        {(keyGroups.find((group) => group.id === keyGroup) ?? keyGroups[0]).keys.map((item) => <button key={item.label} type="button" className={`key${"data" in item ? " shortcut" : ""}`} title={item.title} aria-label={item.title} onPointerDown={keepFocus} onClick={() => pressBarKey(item)}>{item.label}</button>)}
      </div>
      <form className="command-line" onSubmit={sendCommand}>
        <input value={command} onChange={(event) => setCommand(event.target.value)} placeholder="输入命令，回车发送（可用输入法和粘贴）" aria-label="输入命令" autoCapitalize="off" autoCorrect="off" autoComplete="off" spellCheck={false} enterKeyHint="send" />
        <button type="submit" title="发送并回车" aria-label="发送并回车"><CornerDownLeft /></button>
      </form>
    </div>}
  </section>;
}
