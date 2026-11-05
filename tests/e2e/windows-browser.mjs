import fs from "node:fs";
import path from "node:path";
import crypto from "node:crypto";
import { pathToFileURL } from "node:url";

const moduleName = process.env.PLAYWRIGHT_MODULE ?? "playwright-core";
const { chromium } = await import(path.isAbsolute(moduleName) ? pathToFileURL(moduleName).href : moduleName);
const target = process.env.SINTHMUX_E2E_URL;
const token = process.env.SINTHMUX_E2E_TOKEN;
const deviceName = process.env.SINTHMUX_E2E_DEVICE;
const output = process.env.SINTHMUX_E2E_DIR;
if (!target || !token || !deviceName || !output) throw new Error("SINTHMUX_E2E_URL/TOKEN/DEVICE/DIR are required");
const url = new URL(target);
if (!/^https?:$/.test(url.protocol) || !["localhost", "127.0.0.1"].includes(url.hostname) || url.username || url.password || url.search || url.hash) {
  throw new Error("E2E target must be an HTTP(S) loopback URL without credentials, query or fragment");
}
const root = fs.realpathSync(path.resolve(".run"));
const directory = path.resolve(output);
const within = (parent, child) => {
  const relative = path.relative(parent, child);
  return relative === "" || !relative.startsWith(".." + path.sep) && relative !== ".." && !path.isAbsolute(relative);
};
if (!within(root, directory)) throw new Error("E2E output must be inside project .run");
fs.mkdirSync(directory, { recursive: true });
if (!within(root, fs.realpathSync(directory))) throw new Error("E2E output resolves outside project .run");
const marker = `wb-${Date.now().toString(36)}-${crypto.randomBytes(3).toString("hex")}`;
const session = `e2e-${marker}`;
const guard = `${session}-guard`;
const renamed = `${session}-renamed`;
const files = Object.fromEntries(["sequence", "long", "size", "background"].map(name => [name, path.join(directory, `${marker}.${name}`)]));
const quote = value => "'" + value.replaceAll("'", "''") + "'";
const utf8 = "([Text.UTF8Encoding]::new($false))";
const hash = value => crypto.createHash("sha256").update(value).digest("hex");
const result = { marker, session, guard, renamed, checks: {}, rounds: [], console: [], pageErrors: [], cleanup: [], screenshots: [] };
const owned = new Set();
let sessionsURL;
let timedOut = false;
const browser = await chromium.launch({ headless: true, ...(process.env.CHROME_BIN ? { executablePath: process.env.CHROME_BIN } : { channel: "chrome" }) });
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
const page = await context.newPage();
page.setDefaultTimeout(12000);
page.on("console", message => result.console.push({ type: message.type(), text: message.text() }));
page.on("pageerror", error => result.pageErrors.push(error.message));
await page.addInitScript(() => {
  window.__terminalFrames = [];
  const send = WebSocket.prototype.send;
  WebSocket.prototype.send = function(data) {
    if (data instanceof Uint8Array) window.__terminalFrames.push({ bytes: data.byteLength, head: Array.from(data.slice(0, 4)), tail: Array.from(data.slice(-4)) });
    if (typeof data === "string") {
      try { const control = JSON.parse(data); if (control.type === "resize") window.__terminalFrames.push(control); } catch {}
    }
    return send.call(this, data);
  };
});
const timer = setTimeout(() => { timedOut = true; void page.close(); }, 240000);
const check = (name, value) => { result.checks[name] = Boolean(value); if (!value) throw new Error(name + " failed"); };
async function screenshot(name) {
  const file = path.join(directory, `${marker}-${name}.png`);
  await page.screenshot({ path: file, fullPage: true });
  result.screenshots.push(file);
}
async function waitFile(file, predicate, timeout = 15000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline && !timedOut) {
    try { const value = fs.readFileSync(file); if (predicate(value)) return value; } catch (error) { if (error.code !== "ENOENT") throw error; }
    await new Promise(resolve => setTimeout(resolve, 75));
  }
  throw new Error("Timed out waiting for " + path.basename(file));
}
async function type(command) {
  await page.getByRole("textbox", { name: "Terminal input" }).focus();
  await page.keyboard.type(command, { delay: 0 });
  await page.keyboard.press("Enter");
}
async function paste(command) {
  await page.getByRole("textbox", { name: "Terminal input" }).focus();
  await page.locator(".xterm-helper-textarea").evaluate((element, text) => {
    const data = new DataTransfer();
    data.setData("text/plain", text);
    element.dispatchEvent(new ClipboardEvent("paste", { clipboardData: data, bubbles: true, cancelable: true }));
  }, command);
  await page.keyboard.press("Enter");
}
async function outputContains(text) {
  await page.waitForFunction(value => document.querySelector(".xterm-rows")?.innerText.includes(value), text);
}
async function create(name) {
  await page.getByRole("button", { name: "新建会话", exact: true }).click();
  await page.getByRole("textbox", { name: "新会话名称" }).fill(name);
  const pending = page.waitForResponse(response => response.request().method() === "POST" && /\/sessions$/.test(new URL(response.url()).pathname) && response.request().postDataJSON()?.name === name);
  await page.getByRole("button", { name: "创建", exact: true }).click();
  const response = await pending;
  if (response.status() !== 201) throw new Error("Create failed: HTTP " + response.status());
  sessionsURL = response.url();
  owned.add(name);
  await page.getByTestId(`session-${name}`).waitFor();
}
async function open(name) {
  await page.getByTestId(`session-${name}`).locator(".session-open").click();
  await page.locator('[data-testid="terminal-state"][data-state="live"]').waitFor();
}
try {
  await page.goto(url.href);
  await page.locator("#login-token").fill(token);
  await page.getByRole("button", { name: "登录", exact: true }).click();
  await page.getByTestId(`device-${deviceName}`).waitFor();
  await page.getByTestId(`toggle-${deviceName}`).click();
  await create(guard);
  await create(session);
  await page.getByRole("button", { name: `重命名 ${session}`, exact: true }).click();
  await page.getByRole("textbox", { name: "新的会话名称" }).fill(renamed);
  const renamePending = page.waitForResponse(response => response.request().method() === "PATCH" && response.url() === sessionsURL + "/" + encodeURIComponent(session));
  await page.getByRole("button", { name: "保存", exact: true }).click();
  const renameResponse = await renamePending;
  check("rename", renameResponse.status() === 200);
  owned.delete(session);
  owned.add(renamed);
  await page.getByTestId(`session-${renamed}`).waitFor();
  await open(renamed);
  await page.waitForFunction(() => /PS [\s\S]*?>[ \t]*(?:\n|$)/.test(document.querySelector(".xterm-rows")?.innerText ?? ""));
  await type("Write-Output ('ASCII-'+(6*7));Write-Output ('中文-'+(8*9))");
  await outputContains("ASCII-42");
  await outputContains("中文-72");
  check("asciiAndChinese", true);
  for (let round = 0; round < 3; round++) {
    fs.writeFileSync(files.sequence, "NOT_READY");
    const ready = `${marker}-READY-${round + 1}`;
    await type(`[IO.File]::WriteAllText(${quote(files.sequence)},'',${utf8});Write-Output (${quote(marker + "-READY-")}+(${round}+1))`);
    await waitFile(files.sequence, value => value.length === 0);
    await outputContains(ready);
    for (let index = 0; index < 30; index++) await type(`[IO.File]::AppendAllText(${quote(files.sequence)},('中'+(${index}*7)+'|'),${utf8})`);
    const expected = Array.from({ length: 30 }, (_, index) => "中" + index * 7 + "|").join("");
    let actual;
    try { actual = (await waitFile(files.sequence, value => value.toString("utf8") === expected)).toString("utf8"); }
    finally { result.rounds.push({ round, expected, actual: actual ?? fs.readFileSync(files.sequence, "utf8") }); }
    check(`sequenceRound${round + 1}`, actual === expected);
  }
  const long = "a".repeat(1024) + "中".repeat(8000);
  const frameStart = await page.evaluate(() => window.__terminalFrames.length);
  await paste(`[IO.File]::WriteAllText(${quote(files.long)},${quote(long)},${utf8});Write-Output ('LONG-'+(1024+8000))`);
  const longBytes = await waitFile(files.long, value => value.equals(Buffer.from(long)));
  await outputContains("LONG-9024");
  const frames = await page.evaluate(start => window.__terminalFrames.slice(start), frameStart);
  result.long = { expectedHash: hash(long), actualHash: hash(longBytes), frames };
  check("longPasteExact", hash(long) === hash(longBytes));
  check("longPasteChunked", frames.filter(frame => frame.bytes > 1).length >= 2 && frames.every(frame => !frame.bytes || frame.bytes <= 16384));
  const resizeStart = await page.evaluate(() => window.__terminalFrames.length);
  await page.setViewportSize({ width: 1000, height: 700 });
  await page.waitForFunction(start => window.__terminalFrames.slice(start).some(frame => frame.type === "resize"), resizeStart);
  const expectedSize = await page.evaluate(() => window.__terminalFrames.filter(frame => frame.type === "resize").at(-1));
  await type(`[IO.File]::WriteAllText(${quote(files.size)},($Host.UI.RawUI.WindowSize.Width.ToString()+'x'+$Host.UI.RawUI.WindowSize.Height.ToString()),${utf8})`);
  const expectedDimensions = `${expectedSize.cols}x${expectedSize.rows - 1}`;
  const actualSize = (await waitFile(files.size, value => value.toString("utf8") === expectedDimensions)).toString("utf8");
  result.size = { control: expectedSize, expected: expectedDimensions, actual: actualSize };
  check("resize", actualSize === expectedDimensions);
  await type(`[IO.File]::WriteAllText(${quote(files.background)},'READY|',${utf8});Write-Output ('DETACH-'+(5*9));Start-Sleep -Seconds 4;[IO.File]::AppendAllText(${quote(files.background)},'DONE|',${utf8});Write-Output ('BACKGROUND-'+(7*9))`);
  await waitFile(files.background, value => value.toString("utf8") === "READY|");
  await outputContains("DETACH-45");
  await page.getByRole("button", { name: "返回会话", exact: true }).click();
  await page.getByTestId(`session-${renamed}`).waitFor();
  const detachedAt = Date.now();
  await waitFile(files.background, value => value.toString("utf8") === "READY|DONE|");
  const completedAt = fs.statSync(files.background).mtimeMs;
  const reenteredAt = Date.now();
  result.background = { detachedAt, completedAt, reenteredAt };
  check("taskCompletedWhileDetached", completedAt >= detachedAt && completedAt <= reenteredAt);
  await open(renamed);
  await outputContains("BACKGROUND-63");
  check("reattachOutput", true);
  await screenshot("terminal");
  await page.getByRole("button", { name: "返回会话", exact: true }).click();
  await page.getByRole("button", { name: `关闭 ${renamed}`, exact: true }).click();
  const closePending = page.waitForResponse(response => response.request().method() === "DELETE" && response.url() === sessionsURL + "/" + encodeURIComponent(renamed));
  await page.getByRole("button", { name: "关闭会话", exact: true }).click();
  const closed = await closePending;
  check("closeHTTP", closed.status() === 204);
  owned.delete(renamed);
  await page.getByTestId(`session-${renamed}`).waitFor({ state: "detached" });
  check("closeExact", await page.getByTestId(`session-${guard}`).count() === 1);
  check("noPageErrors", result.pageErrors.length === 0);
  await screenshot("sessions");
  result.ok = true;
} catch (error) {
  result.ok = false;
  result.error = error.message;
  try { result.terminalText = await page.locator(".xterm-rows").innerText({ timeout: 1000 }); } catch {}
  try { result.inputFrames = await page.evaluate(() => window.__terminalFrames); } catch {}
  try { await screenshot("failure"); } catch {}
} finally {
  clearTimeout(timer);
  for (const name of owned) {
    try {
      const meResponse = await page.request.get(url.origin + "/api/v1/auth/me");
      if (!meResponse.ok()) throw new Error("Cleanup login expired");
      const me = await meResponse.json();
      const response = await page.request.delete(sessionsURL + "/" + encodeURIComponent(name), { headers: { Origin: url.origin, "X-Sinthmux-CSRF": me.csrf } });
      if (![204, 404].includes(response.status())) throw new Error("Cleanup HTTP " + response.status());
      const remainingResponse = await page.request.get(sessionsURL);
      if (!remainingResponse.ok() || (await remainingResponse.json()).sessions.some(item => item.name === name)) throw new Error("Owned session remains after cleanup");
      result.cleanup.push({ name, ok: true });
    } catch (error) { result.cleanup.push({ name, ok: false, error: error.message }); }
  }
  for (const file of Object.values(files)) {
    try { fs.rmSync(file, { force: true }); } catch (error) { result.cleanup.push({ file, ok: false, error: error.message }); }
  }
  result.ok = Boolean(result.ok && result.cleanup.every(item => item.ok) && !timedOut);
  fs.writeFileSync(path.join(directory, marker + ".result.json"), JSON.stringify(result, null, 2));
  await browser.close();
}
console.log(JSON.stringify({ ok: result.ok, error: result.error, checks: result.checks, result: path.join(directory, marker + ".result.json"), screenshots: result.screenshots }));
process.exitCode = result.ok ? 0 : 1;
