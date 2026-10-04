// Mobile soft keyboards deliver text through the IME pipeline (keydown with
// keyCode 229, then beforeinput/input), which xterm.js only half supports:
// spaces and punctuation commit without a composition and can be dropped, and
// Ctrl/Alt from our key bar never see the typed letter. On touch devices we
// take over text input on xterm's hidden textarea and send exactly what the
// keyboard committed.
//
// Desktop keyboards keep xterm's own handling (keydown has a real key there).

export type MobileInputOptions = {
  /** Sends committed text; the caller applies key-bar modifiers. */
  onText: (text: string) => void;
  /** Sends a raw sequence such as DEL for backspace or CR for enter. */
  onKey: (sequence: string) => void;
};

const DEL = "\x7f";
const CR = "\r";

/**
 * Installs capture-phase listeners on an ancestor of xterm's textarea. xterm
 * registers its own capture listeners on the textarea first, and listeners on
 * the same element run in registration order, so only an ancestor's capture
 * phase is guaranteed to run before xterm. Returns a cleanup function.
 */
export function attachMobileInput(container: HTMLElement, textarea: HTMLTextAreaElement, options: MobileInputOptions): () => void {
  // Hint keyboards not to autocorrect, capitalise or offer passwords, and keep
  // iOS from zooming when the hidden textarea gains focus.
  textarea.setAttribute("autocapitalize", "off");
  textarea.setAttribute("autocorrect", "off");
  textarea.setAttribute("autocomplete", "off");
  textarea.setAttribute("spellcheck", "false");
  textarea.setAttribute("enterkeyhint", "send");
  textarea.style.fontSize = "16px";

  let composing = false;
  // Text already sent for the current composition; a later commit that does
  // not extend it (autocorrect) erases it first.
  let sent = "";

  const stop = (event: Event) => {
    event.stopImmediatePropagation();
    if (event.cancelable) event.preventDefault();
  };
  const reset = () => { textarea.value = ""; };

  const sendDelta = (next: string) => {
    if (next.startsWith(sent)) {
      const delta = next.slice(sent.length);
      if (delta) options.onText(delta);
    } else {
      // The keyboard replaced what it had composed (autocorrect or a picked
      // suggestion): erase what we sent and send the replacement.
      options.onKey(DEL.repeat([...sent].length));
      if (next) options.onText(next);
    }
    sent = next;
  };

  const onBeforeInput = (event: InputEvent) => {
    switch (event.inputType) {
      case "insertText":
      case "insertReplacementText":
        if (composing) return; // handled by composition events
        stop(event);
        if (event.data) options.onText(event.data);
        reset();
        return;
      case "insertLineBreak":
      case "insertParagraph":
        stop(event);
        options.onKey(CR);
        reset();
        return;
      case "deleteContentBackward":
      case "deleteWordBackward":
      case "deleteSoftLineBackward":
        if (composing) return;
        stop(event);
        options.onKey(event.inputType === "deleteWordBackward" ? "\x17" : event.inputType === "deleteSoftLineBackward" ? "\x15" : DEL);
        reset();
        return;
      case "insertFromPaste":
      case "insertFromDrop":
        // xterm handles paste (including bracketed paste) itself.
        return;
      default:
        if (!composing && event.inputType.startsWith("insert")) {
          stop(event);
          if (event.data) options.onText(event.data);
          reset();
        }
    }
  };

  const onCompositionStart = (event: CompositionEvent) => {
    event.stopImmediatePropagation();
    composing = true;
    sent = "";
  };
  // Composed text is sent only when the keyboard commits it (compositionend):
  // pinyin such as "ni'hao" is ASCII too, so streaming updates would send it.
  // Latin keyboards end a composition at each space or suggestion tap, so
  // words appear after each word rather than letter by letter.
  const onCompositionUpdate = (event: CompositionEvent) => {
    event.stopImmediatePropagation();
  };
  const onCompositionEnd = (event: CompositionEvent) => {
    event.stopImmediatePropagation();
    composing = false;
    sendDelta(event.data ?? "");
    sent = "";
    reset();
  };
  // xterm also reads the textarea on input; once we handled it, keep it out.
  const onInput = (event: Event) => {
    if (!composing) {
      event.stopImmediatePropagation();
      reset();
    }
  };
  // Printable characters always come through beforeinput above. Some soft
  // keyboards (iOS English, for one) also send a real keydown/keypress for the
  // same character, so xterm would send it twice; keep those away from xterm.
  // Keys without text (Backspace, Enter, arrows, Tab, Esc) and Ctrl/Alt chords
  // from a Bluetooth keyboard stay with xterm, which maps them correctly.
  const printable = (event: KeyboardEvent) => event.key.length === 1 && !event.ctrlKey && !event.metaKey && !event.altKey;
  const onKeyDown = (event: KeyboardEvent) => {
    if (event.isComposing || event.keyCode === 229 || printable(event)) event.stopImmediatePropagation();
  };
  const onKeyPress = (event: KeyboardEvent) => {
    if (printable(event)) event.stopImmediatePropagation();
  };

  const listeners: [string, EventListener][] = [
    ["beforeinput", onBeforeInput as EventListener],
    ["compositionstart", onCompositionStart as EventListener],
    ["compositionupdate", onCompositionUpdate as EventListener],
    ["compositionend", onCompositionEnd as EventListener],
    ["input", onInput],
    ["keydown", onKeyDown as EventListener],
    ["keypress", onKeyPress as EventListener]
  ];
  // Only events aimed at xterm's textarea, not the key bar or command line.
  const scoped: [string, EventListener][] = listeners.map(([type, listener]) => [type, (event: Event) => { if (event.target === textarea) listener(event); }]);
  for (const [type, listener] of scoped) container.addEventListener(type, listener, true);
  return () => { for (const [type, listener] of scoped) container.removeEventListener(type, listener, true); };
}
