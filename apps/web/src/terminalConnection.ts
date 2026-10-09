import { APIError, request } from "./api";

// Connection lifecycle for one terminal view: ticket → WebSocket handshake →
// first output → live. Every phase has a deadline, a live connection is probed
// with ping/pong, and only the newest attempt may touch the terminal.

export const timings = {
  ticket: 8000,
  handshake: 10000,
  firstByte: 10000,
  pingEvery: 15000,
  pongWithin: 6000,
  resumeProbe: 3000,
  maxBackoff: 10000
};

export type ConnectionState =
  | { kind: "connecting"; reconnect: boolean }
  | { kind: "live" }
  | { kind: "retrying"; seconds: number; reason: string }
  | { kind: "stopped"; reason: string };

type Handlers = {
  onState: (state: ConnectionState) => void;
  onOutput: (data: Uint8Array) => void;
  // Called when a fresh connection replaces an earlier one; tmux redraws the screen.
  onReset: () => void;
  size: () => { cols: number; rows: number };
};

// Close reasons from the Hub or connector that mean the session is really gone.
const finalReasons = /terminal exited|cannot open tmux session|invalid terminal request/;

export class TerminalConnection {
  private socket: WebSocket | undefined;
  private attempt = 0;
  private failures = 0;
  private connectedOnce = false;
  private disposed = false;
  private stopped = false;
  private timers = new Set<ReturnType<typeof setTimeout>>();
  private pingTimer: ReturnType<typeof setInterval> | undefined;
  private lastReceived = 0;
  private pingSeq = 0;
  private ticketAbort: AbortController | undefined;
  private live = false;
  private waitingRetry = false;

  constructor(private readonly deviceId: string, private readonly session: string, private readonly handlers: Handlers) {
    window.addEventListener("online", this.probeNow);
    window.addEventListener("pageshow", this.probeNow);
    document.addEventListener("visibilitychange", this.onVisibility);
  }

  start() { void this.connect(); }

  dispose() {
    this.disposed = true;
    this.clearTimers();
    this.ticketAbort?.abort();
    window.removeEventListener("online", this.probeNow);
    window.removeEventListener("pageshow", this.probeNow);
    document.removeEventListener("visibilitychange", this.onVisibility);
    this.socket?.close();
    this.socket = undefined;
  }

  get isLive() { return this.live; }

  send(data: string | Uint8Array): boolean {
    if (!this.live || this.socket?.readyState !== WebSocket.OPEN) return false;
    this.socket.send(typeof data === "string" ? new TextEncoder().encode(data) : data);
    return true;
  }

  resize() {
    if (this.socket?.readyState !== WebSocket.OPEN) return;
    const { cols, rows } = this.handlers.size();
    this.socket.send(JSON.stringify({ type: "resize", cols, rows }));
  }

  // Reconnect at once, also after the session was reported as ended.
  reconnectNow() {
    if (this.disposed) return;
    this.stopped = false;
    this.failures = 0;
    this.drop();
    void this.connect();
  }

  private timer(ms: number, run: () => void) {
    const id = setTimeout(() => { this.timers.delete(id); run(); }, ms);
    this.timers.add(id);
    return id;
  }

  private clearTimers() {
    for (const id of this.timers) clearTimeout(id);
    this.timers.clear();
    if (this.pingTimer) clearInterval(this.pingTimer);
    this.pingTimer = undefined;
  }

  private state(state: ConnectionState) { if (!this.disposed) this.handlers.onState(state); }

  // drop abandons the current attempt; late events from it are ignored.
  private drop() {
    this.attempt++;
    this.live = false;
    this.clearTimers();
    this.ticketAbort?.abort();
    const socket = this.socket;
    this.socket = undefined;
    if (socket && socket.readyState <= WebSocket.OPEN) socket.close(4000, "stale");
  }

  private stop(reason: string) {
    this.drop();
    this.stopped = true;
    this.state({ kind: "stopped", reason });
  }

  // fail retries with exponential backoff (1, 2, 4, 8, 10 s); a connection that
  // died after working retries immediately the first time.
  private fail(reason: string, immediate = false) {
    if (this.disposed || this.stopped) return;
    this.drop();
    const delay = immediate && this.failures === 0 ? 0 : Math.min(1000 * 2 ** this.failures, timings.maxBackoff);
    this.failures++;
    this.state({ kind: "retrying", seconds: Math.round(delay / 1000), reason });
    this.waitingRetry = true;
    this.timer(delay, () => void this.connect());
  }

  private async connect() {
    if (this.disposed || this.stopped) return;
    this.drop();
    this.waitingRetry = false;
    const attempt = this.attempt;
    const current = () => attempt === this.attempt && !this.disposed;
    this.state({ kind: "connecting", reconnect: this.connectedOnce });

    const abort = new AbortController();
    this.ticketAbort = abort;
    this.timer(timings.ticket, () => abort.abort());
    let ticket: string;
    try {
      const path = `/api/v1/devices/${encodeURIComponent(this.deviceId)}/sessions/${encodeURIComponent(this.session)}/ticket`;
      ({ ticket } = await request<{ ticket: string }>(path, { method: "POST", signal: abort.signal }));
    } catch (error) {
      if (!current()) return;
      if (error instanceof APIError && error.status === 401) return this.stop("登录已过期，请刷新页面重新登录");
      if (error instanceof APIError && [400, 403, 404].includes(error.status)) return this.stop(error.message);
      return this.fail(abort.signal.aborted ? "获取终端票据超时" : "获取终端票据失败");
    }
    if (!current()) return;
    this.clearTimers();

    const scheme = location.protocol === "https:" ? "wss:" : "ws:";
    const socket = new WebSocket(`${scheme}//${location.host}/ws/v1/terminal`, ["sinthmux.v1", `sinthmux.ticket.${ticket}`]);
    socket.binaryType = "arraybuffer";
    this.socket = socket;
    this.timer(timings.handshake, () => { if (current()) this.fail("连接终端超时"); });
    let gotOutput = false;

    socket.onopen = () => {
      if (!current()) return;
      this.clearTimers();
      this.resize();
      // tmux redraws the whole screen on attach, so silence means a stuck path.
      this.timer(timings.firstByte, () => { if (current() && !gotOutput) this.fail("终端没有响应"); });
    };
    socket.onmessage = (event: MessageEvent<ArrayBuffer | string>) => {
      if (!current()) return;
      this.lastReceived = Date.now();
      if (typeof event.data === "string") return; // pong
      if (!gotOutput) {
        gotOutput = true;
        this.clearTimers();
        if (this.connectedOnce) this.handlers.onReset();
        this.connectedOnce = true;
        this.failures = 0;
        this.live = true;
        this.state({ kind: "live" });
        this.startHeartbeat(current);
      }
      this.handlers.onOutput(new Uint8Array(event.data));
    };
    socket.onclose = (event) => {
      if (!current()) return;
      if (event.code === 1008) return this.stop(event.reason || "终端权限已变化");
      if (event.code === 1000 && finalReasons.test(event.reason)) return this.stop(event.reason || "终端会话已结束");
      this.fail("连接已断开", this.live);
    };
  }

  private startHeartbeat(current: () => boolean) {
    this.pingTimer = setInterval(() => {
      if (!current() || document.visibilityState !== "visible") return;
      this.probe(timings.pongWithin, current);
    }, timings.pingEvery);
  }

  // probe sends a ping and declares the connection dead if nothing arrives in time.
  private probe(within: number, current: () => boolean) {
    const socket = this.socket;
    if (!socket || socket.readyState !== WebSocket.OPEN) return;
    const sentAt = Date.now();
    socket.send(JSON.stringify({ type: "ping", id: String(++this.pingSeq) }));
    this.timer(within, () => {
      if (current() && this.live && this.lastReceived < sentAt) this.fail("连接无响应，正在重连", true);
    });
  }

  // Coming back to the page or the network: check the connection right away.
  private probeNow = () => {
    if (this.disposed || this.stopped) return;
    const attempt = this.attempt;
    if (this.live) this.probe(timings.resumeProbe, () => attempt === this.attempt && !this.disposed);
    // Waiting out a backoff: the network may be back, so try now.
    else if (this.waitingRetry) { this.failures = 0; void this.connect(); }
  };

  private onVisibility = () => { if (document.visibilityState === "visible") this.probeNow(); };
}

export function stateText(state: ConnectionState): string {
  switch (state.kind) {
    case "connecting": return state.reconnect ? "正在重新连接…" : "连接中…";
    case "live": return "已连接";
    case "retrying": return state.seconds > 0 ? `${state.reason}，${state.seconds} 秒后重连…` : `${state.reason}…`;
    case "stopped": return state.reason;
  }
}
