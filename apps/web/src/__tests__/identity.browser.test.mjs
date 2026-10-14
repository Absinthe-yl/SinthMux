import assert from "node:assert/strict";
import { test, before, after } from "node:test";
import { build } from "esbuild";
import { root } from "./modules.mjs";
import { installServer } from "./browser-fixture.mjs";

// 复用现有 Playwright/Chrome；不下载依赖，不连接真实网络或后端。
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE ?? "playwright-core");
let browser;
let bundle;
let styles;
before(async () => {
  const result = await build({
    absWorkingDir: root, bundle: true, write: false, format: "iife", jsx: "automatic", outdir: "test-memory",
    define: { "process.env.NODE_ENV": '"development"' },
    stdin: { resolveDir: root, loader: "tsx", contents: `
      import { StrictMode } from "react";
      import { createRoot } from "react-dom/client";
      import { QueryClient } from "@tanstack/react-query";
      import App from "./src/App";
      import "./src/styles.css";
      window.__clients = [];
      const mount = QueryClient.prototype.mount;
      QueryClient.prototype.mount = function() {
        if (!window.__clients.includes(this)) window.__clients.push(this);
        return mount.call(this);
      };
      window.__refetchMe = () => Promise.all(window.__clients.map(client => client.refetchQueries({ queryKey: ["me"], type: "active" })));
      window.__root = createRoot(document.getElementById("root"));
      window.__root.render(<StrictMode><App /></StrictMode>);
    ` }
  });
  bundle = result.outputFiles.find((file) => file.path.endsWith(".js")).text;
  styles = result.outputFiles.find((file) => file.path.endsWith(".css"))?.text ?? "";
  browser = await chromium.launch({ headless: true, ...(process.env.CHROME_BIN ? { executablePath: process.env.CHROME_BIN } : { channel: "chrome" }) });
});
after(async () => { await browser?.close(); });

async function pageFor(t, initial = {}) {
  const context = await browser.newContext();
  t.after(() => context.close());
  const page = await context.newPage();
  page.setDefaultTimeout(5000);
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("console", (message) => { if (message.type() === "error") t.diagnostic(message.text()); });
  t.after(() => assert.deepEqual(errors, [], "浏览器不得出现未处理错误"));
  await page.addInitScript(installServer, initial);
  await page.route("**/*", async (route) => {
    const { pathname, hostname } = new URL(route.request().url());
    if (hostname !== "sinthmux.test") throw new Error("禁止访问外部网络");
    if (pathname === "/") return route.fulfill({ contentType: "text/html", body: '<!doctype html><html><head><link rel="stylesheet" href="/app.css"></head><body><div id="root"></div><script src="/app.js"></script></body></html>' });
    if (pathname === "/app.js") return route.fulfill({ contentType: "text/javascript", body: bundle });
    if (pathname === "/app.css") return route.fulfill({ contentType: "text/css", body: styles });
    return route.fulfill({ status: 204 });
  });
  await page.goto("http://sinthmux.test");
  if (initial.authenticated) return page;
  try { await page.locator("#login-token").waitFor(); }
  catch (error) {
    t.diagnostic(JSON.stringify(errors));
    t.diagnostic(JSON.stringify(await page.evaluate(() => ({ html: document.body.outerHTML, root: Object.keys(window.__root ?? {}), requests: window.__server.requests, queries: window.__clients.map((client) => client.getQueryCache().getAll().map((query) => ({ key: query.queryKey, state: query.state }))) }))));
    throw error;
  }
  return page;
}

async function login(page, user) {
  await page.locator("#login-token").fill(user);
  await page.getByRole("button", { name: "登录", exact: true }).click();
  await page.getByTestId(`device-设备${user}`).waitFor();
}

async function createSecret(page) {
  if (!await page.getByRole("button", { name: "新建令牌", exact: true }).isVisible()) await page.getByRole("button", { name: "登录令牌", exact: true }).click();
  await page.getByRole("button", { name: "新建令牌", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "新建令牌", exact: true }).click();
  await page.locator(".secret-panel").waitFor();
  await page.getByRole("dialog").waitFor({ state: "hidden" });
}

async function expire(page) {
  await page.evaluate(() => { window.__server.authenticated = false; return window.__refetchMe(); });
  await page.locator("#login-token").waitFor();
  assert.equal(await page.locator(".secret-panel, .terminal-page, .management-panel, .workspace-bar").count(), 0);
}

async function noOldCache(page, marker) {
  const snapshot = await page.evaluate(() => window.__clients.map((client) => ({
    queries: client.getQueryCache().getAll().map((query) => query.state.data),
    mutations: client.getMutationCache().getAll().map((mutation) => mutation.state.data)
  })));
  assert(!JSON.stringify(snapshot).includes(marker), `旧账号数据仍在缓存中：${marker}`);
}

test("401 me 立即卸载含 secret 的工作台；跨账号缓存、偏好、主题和 GitHub 入口隔离", async (t) => {
  const page = await pageFor(t);
  await page.getByRole("button", { name: "切换浅色模式" }).click();
  await login(page, "A");
  await page.getByRole("button", { name: "成员", exact: true }).click();
  await page.getByText("成员A · owner", { exact: true }).waitFor();
  await page.getByTestId("toggle-设备A").click();
  await page.getByTestId("session-会话A").waitFor();
  await page.getByTestId("pin-会话A").click();
  await createSecret(page);
  assert.match(await page.locator(".secret-panel").innerText(), /secret-A-/);
  await expire(page);
  await noOldCache(page, "成员A");
  await noOldCache(page, "会话A");
  await noOldCache(page, "设备A");
  await noOldCache(page, "令牌A");
  assert.equal(await page.getByRole("link", { name: "使用 GitHub 登录" }).getAttribute("href"), "/api/v1/auth/github/start");
  assert.equal(await page.locator("html").getAttribute("data-theme"), "light");
  await login(page, "B");
  assert.equal(await page.locator(".secret-panel, .management-panel, .pinned-section, .sessions").count(), 0);
  await page.getByRole("button", { name: "登录令牌", exact: true }).click();
  await page.getByText(/令牌B ·/).waitFor();
  await noOldCache(page, "令牌A");
  await expire(page);
  await login(page, "A");
  await page.getByTestId("pinned-会话A").waitFor();
  await page.getByTestId("session-会话A").waitFor();
  assert.equal(await page.locator(".secret-panel").count(), 0, "偏好可恢复，但 secret 不得恢复");
});

test("失效后的旧 action 和 devices 查询即使迟到也不能注入新账号或污染 CSRF", async (t) => {
  const page = await pageFor(t);
  await login(page, "A");
  await page.evaluate(() => {
    window.__server.holds = ["POST /api/v1/auth/tokens", "GET /api/v1/devices"];
    for (const client of window.__clients) void client.refetchQueries({ queryKey: ["devices"], type: "active" });
  });
  await page.getByRole("button", { name: "登录令牌", exact: true }).click();
  await page.getByRole("button", { name: "新建令牌", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "新建令牌", exact: true }).click();
  await page.waitForFunction(() => window.__server.pending.length === 2);
  await expire(page);
  await page.evaluate(() => { window.__server.holds = []; });
  await login(page, "B");
  await page.evaluate(() => window.__server.release());
  await createSecret(page);
  assert.match(await page.locator(".secret-panel").innerText(), /secret-B-/);
  assert.doesNotMatch(await page.locator("body").innerText(), /secret-A-|设备A/);
  await noOldCache(page, "设备A");
  const csrf = await page.evaluate(() => window.__server.requests.filter((item) => item.method === "POST" && item.path === "/api/v1/auth/tokens" && item.user === "B").at(-1).csrf);
  assert.match(csrf, /^csrf-B-/);
});

test("同账号重新登录及主动退出均生成新生命周期，不恢复 secret", async (t) => {
  const page = await pageFor(t);
  await login(page, "A");
  await createSecret(page);
  await expire(page);
  await login(page, "A");
  assert.equal(await page.locator(".secret-panel").count(), 0);
  await createSecret(page);
  await page.getByRole("button", { name: "退出", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "退出", exact: true }).click();
  await page.locator("#login-token").waitFor();
  await noOldCache(page, "令牌A");
  await login(page, "A");
  assert.equal(await page.locator(".secret-panel").count(), 0);
});

test("撤销当前令牌会重新核验 me 并卸载工作台，重新登录无残留", async (t) => {
  const page = await pageFor(t);
  await login(page, "A");
  await createSecret(page);
  await page.getByRole("button", { name: "撤销", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "撤销令牌", exact: true }).click();
  await page.locator("#login-token").waitFor();
  assert.equal(await page.locator(".secret-panel").count(), 0);
  await noOldCache(page, "令牌A");
  await login(page, "B");
  assert.equal(await page.locator(".secret-panel").count(), 0);
});

test("终端失效卸载会关闭 WebSocket；旧 me 迟到不能替换新账号身份", async (t) => {
  const page = await pageFor(t);
  await login(page, "A");
  await page.getByTestId("toggle-设备A").click();
  await page.getByTestId("session-会话A").locator(".session-open").click();
  await page.waitForFunction(() => document.querySelector('[data-testid="terminal-state"]')?.dataset.state === "live");
  await page.evaluate(() => {
    window.__server.holds = ["GET /api/v1/auth/me"];
    void window.__refetchMe();
  });
  await page.waitForFunction(() => window.__server.pending.length > 0);
  // devices 的 401 使正在等待旧 me 的整个作用域失效。
  await page.evaluate(() => {
    window.__server.authenticated = false;
    for (const client of window.__clients) void client.refetchQueries({ queryKey: ["devices"], type: "active" });
  });
  await page.locator("#login-token").waitFor();
  assert.equal(await page.locator(".terminal-page").count(), 0);
  assert(await page.evaluate(() => window.__server.sockets.every((socket) => socket.readyState === 3)));
  await page.evaluate(() => { window.__server.holds = []; });
  await login(page, "B");
  await page.evaluate(() => window.__server.release());
  await createSecret(page);
  assert.match(await page.locator(".secret-panel").innerText(), /secret-B-/);
  assert.equal(await page.locator(".user-name").innerText(), "用户B");
  await noOldCache(page, "用户A");
});

test("me 发现同账号的新会话 CSRF 会重建工作台；空间变更显式刷新外层 me", async (t) => {
  const page = await pageFor(t);
  await login(page, "A");
  await createSecret(page);
  await page.evaluate(() => { window.__server.session++; return window.__refetchMe(); });
  await page.waitForFunction(() => !document.querySelector(".secret-panel"));
  assert.equal(await page.locator(".management-panel").count(), 0);
  await page.getByRole("button", { name: "新建空间", exact: true }).click();
  await page.getByRole("dialog").getByRole("textbox").fill("额外空间A");
  const before = await page.evaluate(() => window.__server.requests.filter((item) => item.path === "/api/v1/auth/me").length);
  await page.getByRole("dialog").getByRole("button", { name: "创建空间", exact: true }).click();
  await page.getByRole("dialog").waitFor({ state: "hidden" });
  await page.waitForFunction((count) => window.__server.requests.filter((item) => item.path === "/api/v1/auth/me").length > count, before);
  assert.equal(await page.getByRole("combobox", { name: "当前空间" }).inputValue(), "extra-A");
});

test("无效令牌可重试，status 刷新仍保留未配置 GitHub 的提示入口", async (t) => {
  const page = await pageFor(t);
  await page.evaluate(() => {
    window.__server.github = false;
    return Promise.all(window.__clients.map((client) => client.refetchQueries({ queryKey: ["status"], type: "active" })));
  });
  await page.getByRole("button", { name: "使用 GitHub 登录", exact: true }).click();
  await page.getByText("当前服务尚未配置 GitHub 登录。请由部署者配置 OAuth 或统一登录服务。", { exact: true }).waitFor();
  await page.locator("#login-token").fill("invalid");
  await page.getByRole("button", { name: "登录", exact: true }).click();
  await page.getByText("令牌不对或已过期，请找管理员重新要一个。", { exact: true }).waitFor();
  await login(page, "A");
  assert.equal(await page.locator(".user-name").innerText(), "用户A");
});

for (const view of ["secret", "terminal"]) {
  test(`me 临时503/网络故障保留 ${view} 与同会话，自动恢复后401仍清理`, async (t) => {
    const page = await pageFor(t);
    await login(page, "A");
    await createSecret(page);
    const secret = await page.locator(".secret-panel pre").innerText();
    if (view === "terminal") {
      await page.getByTestId("toggle-设备A").click();
      await page.getByTestId("session-会话A").locator(".session-open").click();
      await page.waitForFunction(() => document.querySelector('[data-testid="terminal-state"]')?.dataset.state === "live");
    }
    await page.evaluate(() => {
      window.__retainedView = document.querySelector(".terminal-page, .secret-panel");
      window.__retainedSocket = window.__server.sockets.at(-1);
      window.__retainedClient = window.__clients.find((client) => client.getQueryCache().find({ queryKey: ["devices"] }));
      window.__retainedMe = window.__clients.flatMap((client) => client.getQueryCache().getAll()).find((query) => query.queryKey[0] === "me");
      window.__retainedCSRF = window.__retainedMe.state.data.csrf;
    });
    for (const failure of [503, "network"]) {
      await page.evaluate((value) => { window.__server.meFailure = value; return window.__refetchMe(); }, failure);
      await page.waitForFunction(() => window.__retainedMe.state.status === "error");
      assert.equal(await page.locator("#login-token").count(), 0);
      assert(await page.evaluate(() => window.__retainedView.isConnected), "故障不得卸载原组件");
      assert(await page.evaluate(() => !!window.__retainedClient.getQueryData(["devices"])), "故障不得清空账号缓存");
      if (view === "terminal") {
        assert.equal(await page.getByTestId("terminal-state").getAttribute("data-state"), "live");
        assert(await page.evaluate(() => window.__retainedSocket.readyState === 1 && window.__server.sockets.at(-1) === window.__retainedSocket));
      } else assert.equal(await page.locator(".secret-panel pre").innerText(), secret);
      // 不手动 refetch：等待真实的5秒轮询恢复，证明 error 后查询仍 enabled。
      await page.evaluate(() => { window.__server.meFailure = null; });
      await page.waitForFunction(() => window.__retainedMe.state.status === "success", undefined, { timeout: 8000 });
      assert(await page.evaluate(() => window.__retainedMe.state.data.csrf === window.__retainedCSRF && window.__retainedView.isConnected));
      if (view === "terminal") assert(await page.evaluate(() => window.__server.sockets.at(-1) === window.__retainedSocket && window.__retainedSocket.readyState === 1));
    }
    if (view === "terminal") {
      await page.getByRole("button", { name: "返回会话", exact: true }).click();
      assert.equal(await page.locator(".secret-panel pre").innerText(), secret, "终端故障恢复后工作台 secret 状态仍保留");
      await page.getByTestId("session-会话A").locator(".session-open").click();
      await page.waitForFunction(() => document.querySelector('[data-testid="terminal-state"]')?.dataset.state === "live");
    }
    await expire(page);
    await noOldCache(page, "设备A");
    await noOldCache(page, "用户A");
    assert(await page.evaluate(() => window.__server.sockets.every((socket) => socket.readyState === 3)));
    await login(page, "B");
    assert.equal(await page.locator(".secret-panel, .terminal-page").count(), 0);
  });
}

for (const failure of [503, "network"]) {
  test(`首次 me 无数据遇到 ${failure} 保留 Pending，轮询恢复且无需重新登录`, async (t) => {
    const page = await pageFor(t, { authenticated: true, meFailure: failure });
    await page.waitForFunction(() => window.__clients.some((client) => client.getQueryState(["me"])?.status === "error"));
    await page.getByText("正在加载设备", { exact: true }).waitFor();
    assert.equal(await page.locator("#login-token, .workspace-bar").count(), 0);
    const before = await page.evaluate(() => window.__server.requests.filter((request) => request.path === "/api/v1/auth/me").length);
    await page.evaluate(() => { window.__server.meFailure = null; });
    await page.getByTestId("device-设备A").waitFor({ timeout: 8000 });
    assert(await page.evaluate((count) => window.__server.requests.filter((request) => request.path === "/api/v1/auth/me").length > count, before));
    assert.equal(await page.evaluate(() => window.__server.requests.filter((request) => request.path === "/api/v1/auth/token").length), 0);
    await expire(page);
    await noOldCache(page, "设备A");
  });
}
