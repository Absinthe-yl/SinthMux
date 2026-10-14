import assert from "node:assert/strict";
import { test } from "node:test";
import { loadModule } from "./modules.mjs";

const { APIClient } = await loadModule("api.ts");
const deferred = () => { let resolve; const promise = new Promise((done) => { resolve = done; }); return { promise, resolve }; };

test("旧请求忽略 abort 仍无法交付 secret，新作用域只使用自己的 CSRF", async (t) => {
  const pending = deferred();
  const requests = [];
  t.mock.method(globalThis, "fetch", async (path, init) => {
    requests.push({ path, init });
    return path === "/old" ? pending.promise : Response.json({ token: "new-secret" });
  });
  const old = new APIClient();
  old.activate("csrf-A");
  const late = old.request("/old", { method: "POST" });
  const rejected = assert.rejects(late, { name: "AbortError" });
  old.dispose();
  const next = new APIClient();
  next.activate("csrf-B");
  assert.deepEqual(await next.request("/new", { method: "POST" }), { token: "new-secret" });
  pending.resolve(Response.json({ token: "old-secret" }));
  await rejected;
  assert.equal(requests[0].init.signal.aborted, true);
  assert.equal(requests[0].init.headers.get("X-Sinthmux-CSRF"), "csrf-A");
  assert.equal(requests[1].init.headers.get("X-Sinthmux-CSRF"), "csrf-B");
  await assert.rejects(old.request("/must-not-run"), { name: "AbortError" });
  assert.equal(requests.length, 2);
});

test("响应体解析期间失效不会交付旧 me 或覆盖新的 CSRF", async (t) => {
  const body = deferred();
  const parsing = deferred();
  t.mock.method(globalThis, "fetch", async () => ({ ok: true, status: 200, json: () => { parsing.resolve(); return body.promise; } }));
  const api = new APIClient();
  api.activate("old");
  const late = api.request("/api/v1/auth/me");
  const rejected = assert.rejects(late, { name: "AbortError" });
  await parsing.promise;
  api.dispose();
  api.activate("new");
  body.resolve({ csrf: "old" });
  await rejected;
  assert.equal(api.active, true);
});

test("旧身份迟到的 401 不会注销新身份；当前 401 立即触发失效", async (t) => {
  const pending = deferred();
  t.mock.method(globalThis, "fetch", async (path) => path === "/old" ? pending.promise : Response.json({ error: "expired" }, { status: 401 }));
  let invalidations = 0;
  const old = new APIClient();
  old.onUnauthorized = () => invalidations++;
  const late = old.request("/old");
  const rejected = assert.rejects(late, { name: "AbortError" });
  old.dispose();
  pending.resolve(Response.json({ error: "expired" }, { status: 401 }));
  await rejected;
  assert.equal(invalidations, 0);
  const next = new APIClient();
  next.onUnauthorized = () => { invalidations++; next.dispose(); };
  await assert.rejects(next.request("/current"), { name: "AbortError" });
  assert.equal(invalidations, 1);
  assert.equal(next.active, false);
});

test("查询取消信号阻止同一作用域中被替代的旧查询结果", async (t) => {
  const pending = deferred();
  t.mock.method(globalThis, "fetch", () => pending.promise);
  const api = new APIClient();
  const caller = new AbortController();
  const late = api.request("/me", { signal: caller.signal });
  const rejected = assert.rejects(late, { name: "AbortError" });
  caller.abort();
  pending.resolve(Response.json({ csrf: "old" }));
  await rejected;
  assert.equal(api.active, true);
});

test("作用域失效取消上传，旧 XHR 不能交付路径或继续报告进度", async (t) => {
  const previous = globalThis.XMLHttpRequest;
  let xhr;
  globalThis.XMLHttpRequest = class {
    headers = new Map();
    upload = {};
    constructor() { xhr = this; }
    open() {}
    setRequestHeader(key, value) { this.headers.set(key, value); }
    send() {}
    abort() { this.aborted = true; this.onabort(); this.onloadend(); }
  };
  t.after(() => { if (previous) globalThis.XMLHttpRequest = previous; else delete globalThis.XMLHttpRequest; });
  const api = new APIClient();
  api.activate("csrf-A");
  let progress = 0;
  const pending = api.uploadFile("device", new File(["content"], "test.txt"), () => progress++);
  const rejected = assert.rejects(pending, /上传已取消/);
  assert.equal(xhr.headers.get("X-Sinthmux-CSRF"), "csrf-A");
  api.dispose();
  await rejected;
  assert.equal(xhr.aborted, true);
  xhr.upload.onprogress({ lengthComputable: true, loaded: 7, total: 7 });
  xhr.status = 201;
  xhr.responseText = JSON.stringify({ path: "/old/path" });
  xhr.onload();
  assert.equal(progress, 0);
});

test("旧会话导出响应迟到不会触发浏览器下载", async (t) => {
  const body = deferred();
  const parsing = deferred();
  t.mock.method(globalThis, "fetch", async () => ({ ok: true, status: 200, blob: () => { parsing.resolve(); return body.promise; } }));
  let downloads = 0;
  t.mock.method(URL, "createObjectURL", () => { downloads++; return "blob:test"; });
  const api = new APIClient();
  const pending = api.downloadHistory("device", "设备A", "session", "all");
  const rejected = assert.rejects(pending, { name: "AbortError" });
  await parsing.promise;
  api.dispose();
  body.resolve(new Blob(["旧终端历史"]));
  await rejected;
  assert.equal(downloads, 0);
});
