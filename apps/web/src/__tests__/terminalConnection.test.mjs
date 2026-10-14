import assert from "node:assert/strict";
import { test } from "node:test";
import { loadModule } from "./modules.mjs";

const { TerminalConnection, maxInputFrameBytes } = await loadModule("terminalConnection.ts");
const encode = (text) => new TextEncoder().encode(text);

class Socket {
  static OPEN = 1;
  static instances = [];
  readyState = 0;
  frames = [];
  constructor() { Socket.instances.push(this); }
  send(data) { this.frames.push(typeof data === "string" ? data : new Uint8Array(data)); }
  close() { this.readyState = 3; }
}

async function connection(t) {
  globalThis.window = new EventTarget();
  globalThis.document = Object.assign(new EventTarget(), { visibilityState: "visible" });
  globalThis.location = { protocol: "http:", host: "sinthmux.test" };
  globalThis.WebSocket = Socket;
  const states = [];
  const conn = new TerminalConnection("device", "session", {
    onState: (state) => states.push(state), onOutput() {}, onReset() {}, size: () => ({ cols: 80, rows: 24 })
  }, async () => ({ ticket: "offline-test" }));
  t.after(() => conn.dispose());
  conn.start();
  await Promise.resolve();
  const socket = Socket.instances.at(-1);
  socket.readyState = Socket.OPEN;
  socket.onopen();
  socket.onmessage({ data: encode("ready").buffer });
  socket.frames.length = 0;
  assert.equal(conn.isLive, true);
  return { conn, socket, states };
}

for (const size of [0, 1, 16383, 16384, 16385, 32768, 32769, 100003]) {
  test(`二进制 ${size} 字节按 16KiB 分帧且重组不变`, async (t) => {
    const { conn, socket } = await connection(t);
    const backing = Uint8Array.from({ length: size + 31 }, (_, i) => (i * 37) % 256);
    const input = backing.subarray(17, 17 + size);
    assert.equal(conn.send(input), "sent");
    assert.equal(maxInputFrameBytes, 16384);
    assert.equal(socket.frames.length, Math.ceil(size / 16384));
    assert(socket.frames.every((frame) => frame.length > 0 && frame.length <= 16384));
    assert.deepEqual(Buffer.concat(socket.frames), Buffer.from(input));
  });
}

test("大段中文与四字节字符跨帧，bracketed paste 仅包裹整段一次", async (t) => {
  const { conn, socket } = await connection(t);
  const text = "\x1b[200~" + "中".repeat(6000) + "𠮷文\r\n".repeat(7000) + "\x1b[201~";
  assert.equal(conn.send(text), "sent");
  assert(socket.frames.every((frame) => frame.byteLength <= 16384));
  assert.deepEqual(Buffer.concat(socket.frames), Buffer.from(encode(text)));
  const decoder = new TextDecoder("utf-8", { fatal: true });
  const decoded = socket.frames.map((frame) => decoder.decode(frame, { stream: true })).join("") + decoder.decode();
  assert.equal(decoded, text);
  assert.equal(decoded.split("\x1b[200~").length, 2);
  assert.equal(decoded.split("\x1b[201~").length, 2);
});

test("断线和已销毁连接拒绝输入，重连不会重放或补发", async (t) => {
  const { conn, socket } = await connection(t);
  socket.readyState = 3;
  assert.equal(conn.send("不能排队"), "rejected");
  assert.equal(socket.frames.length, 0);
  conn.reconnectNow();
  assert.equal(conn.send("连接中也不排队"), "rejected");
  await Promise.resolve();
  const next = Socket.instances.at(-1);
  next.readyState = 1;
  next.onopen();
  next.onmessage({ data: encode("ready").buffer });
  assert.equal(next.frames.filter((frame) => typeof frame !== "string").length, 0);
  conn.dispose();
  assert.equal(conn.isLive, false);
  assert.equal(conn.send("销毁后"), "rejected");
});

test("中途 send 抛错返回 partial，停止分块且不重放前缀或后缀", async (t) => {
  const { conn, socket, states } = await connection(t);
  socket.send = (data) => {
    if (socket.frames.length === 1) throw new Error("模拟发送失败");
    socket.frames.push(new Uint8Array(data));
  };
  assert.equal(conn.send("中".repeat(20000)), "partial");
  assert.equal(socket.frames.length, 1);
  assert.equal(socket.frames[0].length, 16384);
  assert.equal(conn.isLive, false);
  assert.equal(states.at(-1).kind, "retrying");
  assert.equal(conn.send("不可继续发送"), "rejected");
  conn.reconnectNow();
  await Promise.resolve();
  const next = Socket.instances.at(-1);
  next.readyState = 1;
  next.onopen();
  next.onmessage({ data: encode("ready").buffer });
  assert.equal(next.frames.filter((frame) => typeof frame !== "string").length, 0);
});

test("首次 send 抛错也保守报告不确定，避免调用方自动重放", async (t) => {
  const { conn, socket } = await connection(t);
  socket.send = () => { throw new Error("模拟发送失败"); };
  assert.equal(conn.send("input"), "partial");
  assert.equal(conn.isLive, false);
});

test("分块之间关闭 socket 不继续发送", async (t) => {
  const { conn, socket } = await connection(t);
  socket.send = (data) => { socket.frames.push(new Uint8Array(data)); socket.readyState = 3; };
  assert.equal(conn.send(new Uint8Array(50000)), "partial");
  assert.equal(socket.frames.length, 1);
});
