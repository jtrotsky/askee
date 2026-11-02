// perf.mjs — is askee still instant? Drives a real headless Chrome over the DevTools
// protocol (no dependencies: Node's own fetch and WebSocket) against a running askee, and
// times every group at every range: the API fetch, the canvas render, and an in-place
// repaint (what every refetch costs). Fails if any number is over budget.
//
//   node test/perf.mjs http://127.0.0.1:8095/        # or `make perf` style, from the repo
//
// Budgets are what "instant" means to a person: under 100 ms feels immediate, a frame is
// 16 ms. A cold first load gets more because it also fetches the page and /api/meta.
import { spawn } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const BASE = process.argv[2] || "http://127.0.0.1:8095/";
const CHROME = process.env.CHROME || "chrome";
const BUDGET = { firstMs: 800, fetchMs: 150, renderMs: 100, tickMs: 50 };
const PORT = 9300 + Math.floor(Math.random() * 500);

const sleep = ms => new Promise(r => setTimeout(r, ms));
const chrome = spawn(CHROME, [
  "--headless=new", "--no-sandbox", "--disable-gpu", "--hide-scrollbars",
  `--remote-debugging-port=${PORT}`, `--user-data-dir=${mkdtempSync(join(tmpdir(), "askee-perf-"))}`,
  "--window-size=1600,900", "about:blank",
], { stdio: "ignore" });
process.on("exit", () => chrome.kill());

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

const fails = [];
const check = (what, v, max) => { if (v > max) fails.push(`${what}: ${v.toFixed(0)} ms > ${max} ms`); return v.toFixed(1).padStart(7); };

await send("Runtime.enable");
const t0 = Date.now();
await send("Page.navigate", { url: BASE + "?theme=newsprint" });
const first = await until("window.askeePerf");
if (!first) { console.error("the page never rendered (window.askeePerf unset)"); process.exit(1); }
console.log(`first load ${check("first load", Date.now() - t0, BUDGET.firstMs)} ms   (page + meta + first group)`);

const meta = await evaluate("({groups: META.groups.map(g => g.id), ranges: META.ranges})");
console.log(`\n${"group".padEnd(9)}${"range".padEnd(7)}  fetch ms  render ms  repaint ms tiles`);
for (const [gi, g] of meta.groups.entries()) {
  for (const r of meta.ranges) {
    await evaluate("window.askeePerf = null");
    await evaluate(`range = ${JSON.stringify(r)}; cur = ${gi}; page = 0; load()`);
    const p = await until("window.askeePerf");
    if (!p) { fails.push(`${g} ${r}: never rendered`); continue; }
    // a refresh: the same data repainted in place, as every refetch does (no layout change)
    const tick = await evaluate(`(() => { const a = performance.now(); const ok = repaint(); return ok ? performance.now() - a : -1; })()`);
    if (tick < 0) fails.push(`${g} ${r}: a refresh would rebuild the page instead of repainting it`);
    console.log(`${g.padEnd(9)}${r.padEnd(7)}${check(`${g} ${r} fetch`, p.fetchMs, BUDGET.fetchMs)}   ${check(`${g} ${r} render`, p.renderMs, BUDGET.renderMs)}  ${check(`${g} ${r} tick`, tick, BUDGET.tickMs)}  ${String(p.tiles).padStart(5)}`);
  }
}
ws.close();
if (fails.length) { console.log(`\nOVER BUDGET (${fails.length}):\n  ` + fails.join("\n  ")); process.exit(1); }
console.log(`\nall within budget: fetch ≤ ${BUDGET.fetchMs} ms, render ≤ ${BUDGET.renderMs} ms, repaint ≤ ${BUDGET.tickMs} ms`);
process.exit(0);
