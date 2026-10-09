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

export type UploadResult = { path: string; fileName: string; size: number };

export const maxUploadBytes = 20 * 1024 * 1024;

// uploadFile posts the raw file to the device and reports progress (0–1).
export function uploadFile(deviceId: string, file: File, onProgress: (fraction: number) => void, signal?: AbortSignal): Promise<UploadResult> {
  return new Promise((resolve, reject) => {
    if (file.size === 0) return reject(new APIError(`${file.name || "文件"} 是空文件`, 400));
    if (file.size > maxUploadBytes) return reject(new APIError(`${file.name} 超过 20 MiB 上限`, 413));
    const xhr = new XMLHttpRequest();
    xhr.open("POST", `/api/v1/devices/${encodeURIComponent(deviceId)}/uploads?name=${encodeURIComponent(file.name)}`);
    xhr.withCredentials = true;
    xhr.setRequestHeader("Content-Type", "application/octet-stream");
    if (csrf) xhr.setRequestHeader("X-Sinthmux-CSRF", csrf);
    xhr.upload.onprogress = (event) => { if (event.lengthComputable) onProgress(event.loaded / event.total); };
    xhr.onload = () => {
      let body: { error?: string } & Partial<UploadResult> = {};
      try { body = JSON.parse(xhr.responseText); } catch { /* not JSON */ }
      if (xhr.status === 201 && body.path) resolve(body as UploadResult);
      else reject(new APIError(body.error ?? `上传失败：${xhr.status}`, xhr.status));
    };
    xhr.onerror = () => reject(new APIError("上传失败：网络错误", 0));
    xhr.onabort = () => reject(new APIError("上传已取消", 0));
    signal?.addEventListener("abort", () => xhr.abort(), { once: true });
    xhr.send(file);
  });
}

// downloadHistory fetches a session's terminal history and saves it as a .txt file.
export async function downloadHistory(deviceId: string, deviceName: string, session: string, lines: "1000" | "10000" | "all") {
  const response = await fetch(`/api/v1/devices/${encodeURIComponent(deviceId)}/sessions/${encodeURIComponent(session)}/scrollback?lines=${lines}`, { credentials: "same-origin" });
  if (!response.ok) {
    const body = await response.json().catch(() => ({})) as { error?: string };
    throw new APIError(body.error ?? `导出失败：${response.status}`, response.status);
  }
  const blob = await response.blob();
  // Content-Length is the compressed size when the proxy compresses; compare
  // the decoded body with the size the Hub reports separately.
  const expected = Number(response.headers.get("X-Sinthmux-Export-Size"));
  if (expected && blob.size !== expected) throw new APIError("导出中断，请重试", 502);
  const stamp = new Date().toISOString().replace(/[-:]/g, "").replace("T", "-").slice(0, 15);
  const safeDevice = deviceName.replace(/[^\p{L}\p{N}_-]+/gu, "_").slice(0, 40) || "device";
  const link = document.createElement("a");
  link.href = URL.createObjectURL(blob);
  link.download = `${safeDevice}-${session}-${stamp}.txt`;
  link.dataset.testid = "history-download";
  document.body.appendChild(link);
  link.click();
  setTimeout(() => { URL.revokeObjectURL(link.href); link.remove(); }, 60000);
}
