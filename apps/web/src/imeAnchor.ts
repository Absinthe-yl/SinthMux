// Full-screen TUIs such as Claude Code hide the cursor and often leave it parked
// on the last row (where tmux draws its status bar). xterm.js draws IME preedit
// at that cursor, so Chinese input lands far from the app's input box. While the
// cursor is hidden, anchor composition to the last row that has content, which
// is where those apps draw their prompt.
import type { Terminal } from "@xterm/xterm";

const HIDE = [0x1b, 0x5b, 0x3f, 0x32, 0x35, 0x6c];
const SHOW = [0x1b, 0x5b, 0x3f, 0x32, 0x35, 0x68];

function contains(haystack: Uint8Array, needle: number[]): boolean {
  outer: for (let i = 0; i + needle.length <= haystack.length; i++) {
    for (let j = 0; j < needle.length; j++) {
      if (haystack[i + j] !== needle[j]) continue outer;
    }
    return true;
  }
  return false;
}

export function keepCompositionVisible(term: Terminal, host: HTMLElement): { trackOutput: (data: string | Uint8Array) => void; dispose: () => void } {
  let hidden = false;
  let frame = 0;
  // Where the app last drew its own cursor while it was still visible; that is
  // the input caret for TUIs which hide the cursor afterwards.
  let lastVisible = { x: 0, y: 0 };

  const trackOutput = (data: string | Uint8Array) => {
    const hid = typeof data === "string" ? data.includes("\x1b[?25l") : contains(data, HIDE);
    const shown = typeof data === "string" ? data.includes("\x1b[?25h") : contains(data, SHOW);
    if (hid) hidden = true;
    if (shown) hidden = false;
    if (!hidden) {
      const buffer = term.buffer.active;
      lastVisible = { x: buffer.cursorX, y: buffer.cursorY };
    }
  };

  const anchor = () => {
    const rows = term.rows;
    const cols = term.cols;
    const screen = host.querySelector<HTMLElement>(".xterm-screen");
    const view = host.querySelector<HTMLElement>(".composition-view");
    const area = host.querySelector<HTMLTextAreaElement>(".xterm-helper-textarea");
    if (!screen || !view || rows < 2) return;
    const box = screen.getBoundingClientRect();
    const cellHeight = box.height / rows;
    const cellWidth = box.width / cols;
    let target = lastVisible.y;
    let column = lastVisible.x;
    // Fold back to the last content row if the remembered spot is unusable.
    if (target >= rows - 1 || (target === 0 && lastVisible.x === 0)) {
      const buffer = term.buffer.active;
      for (let y = rows - 2; y >= 0; y--) {
        const line = buffer.getLine(buffer.baseY + y);
        if (line && line.translateToString(true).trim() !== "") { target = Math.min(y + 1, rows - 2); column = 0; break; }
      }
    }
    const top = `${Math.round(target * cellHeight)}px`;
    const left = `${Math.round(column * cellWidth)}px`;
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
  const onEnd = () => { cancelAnimationFrame(frame); frame = 0; };

  host.addEventListener("compositionstart", onStart, true);
  host.addEventListener("compositionend", onEnd, true);
  return {
    trackOutput,
    dispose: () => {
      cancelAnimationFrame(frame);
      frame = 0;
      host.removeEventListener("compositionstart", onStart, true);
      host.removeEventListener("compositionend", onEnd, true);
    }
  };
}
