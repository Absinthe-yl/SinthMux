// Key sequences for the on-screen terminal key bar used on phones and tablets.

export type Modifier = "off" | "once" | "lock";
export type Modifiers = { ctrl: Modifier; alt: Modifier };
export type SpecialKey = "Escape" | "Tab" | "Up" | "Down" | "Left" | "Right" | "Home" | "End" | "PageUp" | "PageDown";

export const noModifiers: Modifiers = { ctrl: "off", alt: "off" };

const cursor: Record<string, string> = { Up: "A", Down: "B", Right: "C", Left: "D", Home: "H", End: "F" };
const tilde: Record<string, string> = { PageUp: "5", PageDown: "6" };

/** Next state when a modifier key is tapped: off → once → lock → off. */
export function nextModifier(value: Modifier): Modifier {
  return value === "off" ? "once" : value === "once" ? "lock" : "off";
}

/** Clears one-shot modifiers after a key has used them; locked modifiers stay. */
export function consumeModifiers(mods: Modifiers): Modifiers {
  return { ctrl: mods.ctrl === "once" ? "off" : mods.ctrl, alt: mods.alt === "once" ? "off" : mods.alt };
}

export function hasModifier(mods: Modifiers): boolean {
  return mods.ctrl !== "off" || mods.alt !== "off";
}

/** Applies Ctrl/Alt to one typed character, as a hardware keyboard would. */
export function modifyText(text: string, mods: Modifiers): string {
  if (text.length !== 1 || !hasModifier(mods)) return text;
  let result = text;
  if (mods.ctrl !== "off") {
    const upper = text.toUpperCase();
    const code = upper.charCodeAt(0);
    if (text === " " || text === "2" || text === "@") result = "\x00";
    else if (code >= 64 && code <= 95) result = String.fromCharCode(code & 0x1f);
    else if (text === "?") result = "\x7f";
  }
  return mods.alt !== "off" ? `\x1b${result}` : result;
}

/** xterm-compatible sequence for a special key. appCursor follows DECCKM. */
export function keySequence(key: SpecialKey, mods: Modifiers, appCursor: boolean): string {
  const param = 1 + (mods.alt !== "off" ? 2 : 0) + (mods.ctrl !== "off" ? 4 : 0);
  if (key === "Escape") return "\x1b";
  if (key === "Tab") return "\t";
  if (key in cursor) {
    if (param > 1) return `\x1b[1;${param}${cursor[key]}`;
    return `${appCursor ? "\x1bO" : "\x1b["}${cursor[key]}`;
  }
  return param > 1 ? `\x1b[${tilde[key]};${param}~` : `\x1b[${tilde[key]}~`;
}
