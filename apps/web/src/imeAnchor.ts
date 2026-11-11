// Full-screen TUIs such as Claude Code hide the cursor, and once it is hidden
// xterm.js keeps drawing IME preedit at the stale position — often the last row
// where tmux paints its status bar. Chinese input then appears far from the
// app's input box, and on Windows the inline preedit duplicates the IME's own
// composition window. While the cursor is hidden, anchor composition at the
// place the app last showed its caret.
import type { Terminal } from "@xterm/xterm";

const HIDE = [0x1b, 0x5b, 0x3f, 0x32, 0x35, 0x6c];
const SHOW = [0x1b, 0x5b, 0x3f, 0x32, 0x35, 0x68];
// Windows IMEs draw their own composition and candidate windows, so the
// terminal's inline preedit would be a second copy at a different spot.
const imeDrawsItself = typeof navigator !== "undefined" && /Windows/i.test(navigator.userAgent);

function contains(haystack: Uint8Array, needle: number[]): boolean {
  outer: for (let i = 0; i + needle.length <= haystack.length; i++) {
    for (let j = 0; j < needle.length; j++) {
      if (haystack[i + j] !== needle[j]) continue outer;
    }
    return true;
  }
  return false;
}

export function keepCompositionVisible(term: Terminal, host: HTMLElement): { trackOutput: (data: string | Uint8Array) => void; sample: () => void; dispose: () => void } {
  let hidden = false;
  let frame = 0;
  // Where the app last drew its own cursor while it was still visible; that is
  // the input caret for TUIs which hide the cursor afterwards.
  let lastVisible = { x: 0, y: 0, known: false };

  const trackOutput = (data: string | Uint8Array) => {
    const hid = typeof data === "string" ? data.includes("\x1b[?25l") : contains(data, HIDE);
    const shown = typeof data === "string" ? data.includes("\x1b[?25h") : contains(data, SHOW);
    if (hid) hidden = true;
    if (shown) hidden = false;
  };

  // Called once xterm has parsed a write, so the cursor is current.
  const sample = () => {
    if (hidden) return;
    const buffer = term.buffer.active;
    lastVisible = { x: buffer.cursorX, y: buffer.cursorY, known: true };
  };

  const anchor = () => {
    const rows = term.rows;
    const cols = term.cols;
    const screen = host.querySelector<HTMLElement>(".xterm-screen");
    const view = host.querySelector<HTMLElement>(".composition-view");
    const area = host.querySelector<HTMLTextAreaElement>(".xterm-helper-textarea");
    if (!screen || !view || rows < 2) return;
    const box = screen.getBoundingClientRect();
    let target = lastVisible.known ? lastVisible.y : -1;
    let column = lastVisible.x;
    // A remembered spot outside the screen is useless; fall back to the blank
    // row under the app's last line of content.
    if (target < 0 || target >= rows - 1) {
      const buffer = term.buffer.active;
      target = rows - 2;
      column = 0;
      for (let y = rows - 2; y >= 0; y--) {
        const line = buffer.getLine(buffer.baseY + y);
        if (line && line.translateToString(true).trim() !== "") { target = Math.min(y + 1, rows - 2); break; }
      }
    }
    const top = `${Math.round(target * (box.height / rows))}px`;
    const left = `${Math.round(column * (box.width / cols))}px`;
    if (view.style.display !== (imeDrawsItself ? "none" : "")) view.style.display = imeDrawsItself ? "none" : "";
    if (view.style.top !== top || view.style.left !== left) { view.style.top = top; view.style.left = left; }
    if (area && (area.style.top !== top || area.style.left !== left)) { area.style.top = top; area.style.left = left; }
  };

  // xterm repositions the preedit on every render, so hold our spot each frame
  // until the composition ends.
  const hold = () => { anchor(); frame = requestAnimationFrame(hold); };
  const onStart = (event: CompositionEvent) => {
    const target = event.target as HTMLElement | null;
    if (!hidden || !target?.classList.contains("xterm-helper-textarea")) return;
    cancelAnimationFrame(frame);
    hold();
  };
  const onEnd = () => {
    cancelAnimationFrame(frame);
    frame = 0;
    const view = host.querySelector<HTMLElement>(".composition-view");
    if (view) { view.style.display = ""; view.textContent = ""; }
    sample();
  };

  host.addEventListener("compositionstart", onStart, true);
  host.addEventListener("compositionend", onEnd, true);
  return {
    trackOutput,
    sample,
    dispose: () => {
      cancelAnimationFrame(frame);
      frame = 0;
      host.removeEventListener("compositionstart", onStart, true);
      host.removeEventListener("compositionend", onEnd, true);
    }
  };
}
