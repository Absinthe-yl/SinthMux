export class APIError extends Error {
  constructor(message: string, public readonly status: number) { super(message); }
}

export type UploadResult = { path: string; fileName: string; size: number };
export const maxUploadBytes = 20 * 1024 * 1024;

// 每个登录/工作台作用域持有自己的 CSRF 和取消信号，不共享身份状态。
export class APIClient {
  private csrf = "";
  private controller = new AbortController();
  onUnauthorized?: () => void;

  get active() { return !this.controller.signal.aborted; }
  activate(csrf: string) {
    // React StrictMode 会清理后重新挂载 effect；旧请求仍持有已取消的信号。
    if (!this.active) this.controller = new AbortController();
    this.csrf = csrf;
  }
  dispose() { this.csrf = ""; this.controller.abort(); }

  private signal(signal?: AbortSignal | null) {
    return signal ? AbortSignal.any([this.controller.signal, signal]) : this.controller.signal;
  }

  private async response(path: string, init?: RequestInit): Promise<{ response: Response; signal: AbortSignal }> {
    const signal = this.signal(init?.signal);
    signal.throwIfAborted();
    const headers = new Headers(init?.headers);
    if (init?.body) headers.set("Content-Type", "application/json");
    if (init?.method && !["GET", "HEAD"].includes(init.method.toUpperCase()) && this.csrf) headers.set("X-Sinthmux-CSRF", this.csrf);
    const response = await fetch(path, { ...init, signal, headers, credentials: "same-origin" });
    signal.throwIfAborted();
    if (response.status === 401) this.onUnauthorized?.();
    if (!response.ok) {
      const body = await response.json().catch(() => ({})) as { error?: string };
      signal.throwIfAborted();
      throw new APIError(body.error ?? `请求失败：${response.status}`, response.status);
    }
    return { response, signal };
  }

  request = async <T>(path: string, init?: RequestInit): Promise<T> => {
    const { response, signal } = await this.response(path, init);
    const result = response.status === 204 ? undefined as T : await response.json() as T;
    // 即使底层未响应 abort，也不能交付旧身份的异步结果。
    signal.throwIfAborted();
    return result;
  };

  uploadFile = (deviceId: string, file: File, onProgress: (fraction: number) => void, callerSignal?: AbortSignal): Promise<UploadResult> => {
    return new Promise((resolve, reject) => {
      const signal = this.signal(callerSignal);
      signal.throwIfAborted();
      if (file.size === 0) return reject(new APIError(`${file.name || "文件"} 是空文件`, 400));
      if (file.size > maxUploadBytes) return reject(new APIError(`${file.name} 超过 20 MiB 上限`, 413));
      const xhr = new XMLHttpRequest();
      const abort = () => xhr.abort();
      xhr.open("POST", `/api/v1/devices/${encodeURIComponent(deviceId)}/uploads?name=${encodeURIComponent(file.name)}`);
      xhr.withCredentials = true;
      xhr.setRequestHeader("Content-Type", "application/octet-stream");
      if (this.csrf) xhr.setRequestHeader("X-Sinthmux-CSRF", this.csrf);
      xhr.upload.onprogress = (event) => { if (!signal.aborted && event.lengthComputable) onProgress(event.loaded / event.total); };
      xhr.onload = () => {
        if (signal.aborted) return reject(signal.reason);
        if (xhr.status === 401) this.onUnauthorized?.();
        if (signal.aborted) return reject(signal.reason);
        let body: { error?: string } & Partial<UploadResult> = {};
        try { body = JSON.parse(xhr.responseText); } catch { /* 非 JSON 响应 */ }
        if (xhr.status === 201 && body.path) resolve(body as UploadResult);
        else reject(new APIError(body.error ?? `上传失败：${xhr.status}`, xhr.status));
      };
      xhr.onerror = () => reject(new APIError("上传失败：网络错误", 0));
      xhr.onabort = () => reject(new APIError("上传已取消", 0));
      xhr.onloadend = () => signal.removeEventListener("abort", abort);
      signal.addEventListener("abort", abort, { once: true });
      xhr.send(file);
    });
  };

  downloadHistory = async (deviceId: string, deviceName: string, session: string, lines: "1000" | "10000" | "all") => {
    const { response, signal } = await this.response(`/api/v1/devices/${encodeURIComponent(deviceId)}/sessions/${encodeURIComponent(session)}/scrollback?lines=${lines}`);
    const blob = await response.blob();
    signal.throwIfAborted();
    // Content-Length 可能是压缩大小，因此核对 Hub 提供的解压后大小。
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
  };
}
