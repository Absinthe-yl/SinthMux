import { useCallback, useEffect, useState } from "react";

// Browser-local preferences, kept per signed-in user so two accounts in one
// browser never share pins or expanded devices.
const prefix = "sinthmux:v1:";

function read<T>(key: string, fallback: T): T {
  try {
    const raw = localStorage.getItem(prefix + key);
    return raw === null ? fallback : JSON.parse(raw) as T;
  } catch {
    return fallback;
  }
}

function write(key: string, value: unknown) {
  try { localStorage.setItem(prefix + key, JSON.stringify(value)); } catch { /* storage full or disabled */ }
}

export function usePref<T>(user: string | undefined, name: string, fallback: T): [T, (next: T | ((current: T) => T)) => void] {
  const key = `${user ?? "dev"}:${name}`;
  const [value, setValue] = useState<T>(() => read(key, fallback));
  // Reload when the user changes (login, logout, another account).
  useEffect(() => { setValue(read(key, fallback)); }, [key]); // eslint-disable-line react-hooks/exhaustive-deps
  const update = useCallback((next: T | ((current: T) => T)) => {
    setValue((current) => {
      const resolved = typeof next === "function" ? (next as (current: T) => T)(current) : next;
      write(key, resolved);
      return resolved;
    });
  }, [key]);
  return [value, update];
}

export const maxPins = 50;

export function pinKey(deviceId: string, session: string) { return `${deviceId}/${session}`; }

export function splitPin(key: string): { deviceId: string; session: string } {
  const slash = key.lastIndexOf("/");
  return { deviceId: key.slice(0, slash), session: key.slice(slash + 1) };
}
