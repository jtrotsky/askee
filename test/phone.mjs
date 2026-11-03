// phone.mjs — does the dashboard hold still on a phone? A phone's address bar slides in and out
// as the reader scrolls, which changes the window's height; the page must not rebuild or move
// under their thumb when that happens, nor when a refresh rebuilds the group they are reading.
// Drives a headless Chrome at phone size over the DevTools protocol, like perf.mjs.
//
//   node test/phone.mjs http://127.0.0.1:8095/
import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const BASE = process.argv[2] || "http://127.0.0.1:8095/";
const CHROME = process.env.CHROME || "chrome";
const PORT = 9300 + Math.floor(Math.random() * 500);
const W = 390, H = 844, BAR = 84;   // an iPhone-sized screen; the address bar's height

// A fresh Chromium profile per run, removed on exit: one was left in /tmp on every run until
// 2026-11-04 (19 of them, ~1.4 MB each, in two days).
const PROFILE = mkdtempSync(join(tmpdir(), "askee-phone-"));
const sleep = ms => new Promise(r => setTimeout(r, ms));
const chrome = spawn(CHROME, [
  "--headless=new", "--no-sandbox", "--disable-gpu", "--hide-scrollbars",
  `--remote-debugging-port=${PORT}`, `--user-data-dir=${PROFILE}`,
  `--window-size=${W},${H}`, "about:blank",
], { stdio: "ignore", detached: true }); // its own process group, so exit can end every child
process.on("exit", () => { try { process.kill(-chrome.pid, "SIGKILL"); } catch {} try { rmSync(PROFILE, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 }); } catch {} });

let target;
for (let i = 0; i < 50 && !target; i++) {
  await sleep(200);
  try { target = (await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json()).find(t => t.type === "page"); } catch {}
}
if (!target) { console.error("chrome did not start"); process.exit(2); }
const ws = new WebSocket(target.webSocketDebuggerUrl);
await new Promise(r => ws.addEventListener("open", r));
let id = 0; const waiting = new Map();
ws.addEventListener("message", e => { const m = JSON.parse(e.data); if (waiting.has(m.id)) { waiting.get(m.id)(m); waiting.delete(m.id); } });
const send = (method, params = {}) => new Promise(r => { const n = ++id; waiting.set(n, r); ws.send(JSON.stringify({ id: n, method, params })); });
const evaluate = async expr => (await send("Runtime.evaluate", { expression: expr, awaitPromise: true, returnByValue: true })).result?.result?.value;
const until = async (expr, ms = 10000) => {
  for (const end = Date.now() + ms; Date.now() < end; await sleep(20)) { const v = await evaluate(expr); if (v) return v; }
  return null;
};
const size = h => send("Emulation.setDeviceMetricsOverride", { width: W, height: h, deviceScaleFactor: 3, mobile: true });

await send("Runtime.enable");
await size(H);
await send("Page.navigate", { url: BASE + "?theme=newsprint" });
if (!await until("window.askeePerf")) { console.error("the page never rendered: " + await evaluate("document.body.innerText.slice(0, 200)")); process.exit(1); }

const fails = [];
// pick the group with the most tiles, so there is something to scroll through
const g = await evaluate("META.groups.reduce((a, b) => b.n > a.n ? b : a).id");
await evaluate(`location.hash = "${g}/24h"; location.reload()`);
await sleep(300);
if (!await until("window.askeePerf")) { console.error(`group ${g} never rendered`); process.exit(1); }
const room = await evaluate("document.documentElement.scrollHeight - innerHeight");
if (room < 300) { console.error(`group ${g} is too short to scroll (${room}px)`); process.exit(2); }
const Y = Math.min(600, room - 50);
await evaluate(`scrollTo(0, ${Y}); window.__tile = document.querySelector(".tile"); true`);
await sleep(100);

// 1. the address bar hides and shows again
for (const h of [H - BAR, H, H - BAR]) {
  await size(h);
  await sleep(400);   // past the 150 ms resize debounce
  const same = await evaluate("window.__tile === document.querySelector('.tile')"), y = await evaluate("scrollY");
  if (!same) fails.push(`window height ${h}: the page was rebuilt`);
  if (Math.abs(y - Y) > 2) fails.push(`window height ${h}: scrolled from ${Y} to ${y}`);
}
// 2. a refresh that changes the layout rebuilds the group in place
await evaluate("shown.sig = ''; render(); true");
await sleep(100);
const y2 = await evaluate("scrollY");
if (Math.abs(y2 - Y) > 2) fails.push(`rebuild on refresh: scrolled from ${Y} to ${y2}`);
// 3. a rotation is a real change of width: that may rebuild, but must not throw an error
await size(W * 2);
await sleep(400);
await size(H);

console.log(`group ${g}: scrolled to ${Y}px of ${room}px, address bar ${BAR}px`);
if (fails.length) { console.log("FAIL\n  " + fails.join("\n  ")); process.exit(1); }
console.log("PASS: no rebuild and no jump when the address bar moves, scroll kept across a rebuild");
process.exit(0);
