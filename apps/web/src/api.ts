let csrf = "";

export function setCSRF(value: string) { csrf = value; }

export class APIError extends Error {
  constructor(message: string, public readonly status: number) { super(message); }
}

export async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers);
  if (init?.body) headers.set("Content-Type", "application/json");
  if (init?.method && !["GET", "HEAD"].includes(init.method.toUpperCase()) && csrf) headers.set("X-Sinthmux-CSRF", csrf);
  const response = await fetch(path, { ...init, headers, credentials: "same-origin" });
  if (!response.ok) {
    const body = await response.json().catch(() => ({})) as { error?: string };
    throw new APIError(body.error ?? `请求失败：${response.status}`, response.status);
  }
  return response.status === 204 ? undefined as T : response.json() as Promise<T>;
}
