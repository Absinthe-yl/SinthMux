// 浏览器内的内存服务故意忽略 AbortSignal，以验证前端自己的失效防护。
export function installServer(initial = {}) {
  localStorage.setItem("sinthmux-theme", "dark");
  const server = window.__server = {
    user: "A", session: 1, authenticated: false, github: true,
    requests: [], holds: [], pending: [], sockets: [], meFailure: null,
    ...initial,
    release() { for (const done of this.pending.splice(0)) done(); this.holds = []; }
  };
  window.fetch = async (path, init = {}) => {
    const method = init.method ?? "GET";
    const key = `${method} ${path}`;
    const headers = new Headers(init.headers);
    server.requests.push({ path, method, csrf: headers.get("X-Sinthmux-CSRF"), user: server.user });
    if (path === "/api/v1/auth/me" && server.meFailure) {
      if (server.meFailure === "network") throw new TypeError("Failed to fetch");
      return Response.json({ error: "模拟临时服务故障" }, { status: server.meFailure });
    }
    let status = 200;
    let data;
    const user = server.user;
    if (path === "/api/v1/system/status") data = { status: "ok", version: "test", authMode: "formal", connectedDevices: 1, githubLoginEnabled: server.github };
    else if (path === "/api/v1/auth/token") {
      const token = JSON.parse(init.body).token;
      if (token === "invalid") { status = 401; data = { error: "invalid login token" }; }
      else { server.user = token; server.session++; server.authenticated = true; data = {}; }
    } else if (!server.authenticated) { status = 401; data = { error: "unauthorized" }; }
    else if (path === "/api/v1/auth/me") data = { user: { id: user, name: `用户${user}`, githubId: 123 }, spaces: [
      { id: `space-${user}`, name: `空间${user}`, kind: "team", role: "owner" },
      { id: `extra-${user}`, name: `额外空间${user}`, kind: "team", role: "owner" }
    ], csrf: `csrf-${user}-${server.session}` };
    else if (path === "/api/v1/auth/logout") { server.authenticated = false; status = 204; }
    else if (path === "/api/v1/devices") data = { devices: [{ id: "shared-device", spaceId: `space-${user}`, name: `设备${user}`, platform: "darwin", architecture: "arm64", status: "online", capabilities: ["tmux.sessions.manage"] }] };
    else if (path === "/api/v1/auth/tokens" && method === "GET") data = { tokens: [{ id: "current-token", name: `令牌${user}`, expiresAt: "2099-01-01" }] };
    else if (path === "/api/v1/auth/tokens" && method === "POST") data = { token: `secret-${user}-${server.session}` };
    else if (path === "/api/v1/auth/tokens/current-token" && method === "DELETE") { server.authenticated = false; status = 204; }
    else if (path === "/api/v1/spaces" && method === "POST") data = { id: `extra-${user}`, name: `额外空间${user}`, kind: "team", role: "owner" };
    else if (path.endsWith("/members")) data = { members: [{ userId: user, name: `成员${user}`, role: "owner" }] };
    else if (path.endsWith("/sessions")) data = { sessions: [{ name: `会话${user}`, windows: 1, attached: false, createdAt: 1 }] };
    else if (path.endsWith("/ticket")) data = { ticket: "offline-test" };
    else throw new Error(`未模拟的请求：${key}`);
    const body = JSON.stringify(data);
    if (server.holds.includes(key)) await new Promise((resolve) => server.pending.push(resolve));
    return new Response(status === 204 ? null : body, { status, headers: { "Content-Type": "application/json" } });
  };
  class Socket {
    static OPEN = 1;
    readyState = 0;
    frames = [];
    constructor() {
      server.sockets.push(this);
      setTimeout(() => {
        if (this.readyState !== 0) return;
        this.readyState = 1;
        this.onopen?.();
        this.onmessage?.({ data: new TextEncoder().encode("\x1b[?2004h离线终端\r\n").buffer });
      }, 0);
    }
    send(data) {
      if (this.readyState !== 1) throw new Error("连接未打开");
      this.frames.push(typeof data === "string" ? data : Array.from(data));
    }
    close() { this.readyState = 3; }
  }
  window.WebSocket = Socket;
  Object.defineProperty(window, "PasswordCredential", { value: undefined, configurable: true });
}
