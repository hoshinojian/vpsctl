#!/usr/bin/env node
// check-ui：管理台前端的最小无依赖冒烟检查。
// 目的：拦住"求值期/渲染期崩溃导致整页死掉"这一类故障（历史案例：
// $() 误用 # 选择器、重构丢失 key() 定义——两者症状都是 KPI 正常而列表空白）。
//
// 做法：从 internal/webui/static/index.html 抽出全部 <script> 块，在极简 DOM 桩里
// 用固定载荷完整执行，然后断言：脚本求值无异常、无未捕获异常、summary/KPI 已更新、
// 表格行数与载荷台数一致、地图 marker 数与有坐标节点数一致、脚本引用的静态 id 都存在。
//
// 用法：node tools/check-ui.mjs [index.html 路径]（默认仓内页面）。
// 需要 node ≥18；无需任何第三方依赖。
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const pagePath = process.argv[2] ?? join(root, "internal/webui/static/index.html");
const html = readFileSync(pagePath, "utf8");

// ---- 固定载荷：覆盖有坐标/无坐标/多状态/账号查询失败等分支 ----
const FIXTURE_DROPLETS = {
  droplets: [
    { account: "do-1", id: "1", name: "do-1-node-a", status: "active", region: "ams3",
      lat: 52.37, lng: 4.9, ipv4_public: "203.0.113.10", price_monthly: 6, tags: ["nms-node"] },
    { account: "do-1", id: "2", name: "do-1-node-b", status: "off", region: "lon1",
      lat: 51.51, lng: -0.13, ipv4_public: "203.0.113.11", price_monthly: 6, tags: [] },
    { account: "do-2", id: "3", name: "do-2-node-c", status: "new", region: "unknown-region",
      ipv4_public: "", price_monthly: 5, tags: [] },
  ],
  errors: [{ account: "do-3", error: "HTTP 401" }],
};
const FIXTURES = {
  "/api/droplets": FIXTURE_DROPLETS,
  "/api/catalog": { accounts: ["do-1", "do-2", "do-3"] },
  "/api/accounts": { accounts: [
    { name: "do-1", provider: "digitalocean", ssh_user: "root", has_password: true },
  ], providers: ["digitalocean"] },
  "/api/operations": { operations: [
    { time: "2026-09-09T03:00:00Z", kind: "power", detail: "power_off 1 台", ok: 1, fail: 0 },
  ] },
};
const locatedCount = FIXTURE_DROPLETS.droplets.filter(d => d.lat || d.lng).length;

// ---- 极简 DOM 桩 ----
function makeEl(tag = "div") {
  const el = {
    tag, children: [], style: {}, dataset: {}, value: "",
    className: "", innerHTML: "", hidden: false, disabled: false, checked: false,
    type: "", id: "", _text: "",
    append(...kids) { for (const k of kids) this.children.push(k); },
    focus() {}, click() {}, remove() {},
    addEventListener() {}, setAttribute() {}, querySelectorAll() { return [] },
  };
  // 模仿 DOM：textContent 赋值一律转字符串（页面常直接赋数字，如 KPI 计数）
  Object.defineProperty(el, "textContent", {
    get() { return this._text; },
    set(v) { this._text = String(v); },
  });
  return el;
}
const byId = new Map();
for (const m of html.matchAll(/id="([^"]+)"/g)) {
  if (!byId.has(m[1])) byId.set(m[1], makeEl());
}
globalThis.document = {
  getElementById: (id) => byId.get(id) ?? null,
  createElement: (tag) => makeEl(tag),
  createTextNode: (t) => ({ textContent: t, text: t }),
  body: makeEl("body"),
  querySelectorAll: () => [],
};
globalThis.Option = class { constructor(text, value) { this.text = text; this.value = value ?? text; } };
globalThis.Event = class { constructor(type) { this.type = type; } };
globalThis.confirm = () => true;
globalThis.alert = () => {};
globalThis.setInterval = () => 0; // 检查期不跑轮询
globalThis.fetch = async (url) => {
  const path = new URL(url, "http://check-ui").pathname;
  if (!(path in FIXTURES)) throw new Error("无载荷 fixture: " + path);
  return { ok: true, json: async () => structuredClone(FIXTURES[path]) };
};

// ---- 抽取脚本并执行 ----
const blocks = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map(m => m[1]);
const failures = [];
const check = (name, ok, detail = "") => {
  console.log(`${ok ? "✓" : "✗"} ${name}${detail ? " —— " + detail : ""}`);
  if (!ok) failures.push(name);
};
let evalError = null;
const uncaught = [];
process.on("uncaughtException", (e) => uncaught.push(e.message));

// 页面作用域内驱动：等 load 完成后切地图视图并回传结果
const driver = `
;setTimeout(() => {
  try {
    setView("map");
    const svg = document.getElementById("map-svg");
    globalThis.__ui = {
      summary: document.getElementById("summary").textContent,
      rows: document.getElementById("rows").children.length,
      markers: (svg.innerHTML.match(/marker-group/g) || []).length,
      kpiTotal: document.getElementById("kpi-total").textContent,
    };
  } catch (e) { globalThis.__ui = { error: e.message }; }
}, 200);
`;
const script = blocks.join("\n;\n") + driver;
try {
  new Function(script)();
} catch (e) {
  evalError = e.message;
}
await new Promise(r => setTimeout(r, 500));

// ---- 断言 ----
check("脚本求值无异常", evalError === null, evalError ?? "");
check("运行期无未捕获异常", uncaught.length === 0, uncaught.join("; "));
const ui = globalThis.__ui ?? {};
if (ui.error) failures.push("视图驱动失败: " + ui.error);
check("页面驱动完成", ui.error === undefined, ui.error ?? "");
const n = FIXTURE_DROPLETS.droplets.length;
check("summary 已渲染", (ui.summary ?? "").includes("个账号"), ui.summary ?? "(空)");
check(`表格行数 = ${n}`, ui.rows === n, `got ${ui.rows}`);
check("KPI 节点总数已更新", ui.kpiTotal === String(n), `got ${ui.kpiTotal}`);
check(`地图 marker = ${locatedCount}（有坐标节点）`, ui.markers === locatedCount, `got ${ui.markers}`);

// 静态 id 引用检查：脚本里 $('字面量') 的 id 必须在 HTML 中定义（动态 `#` 拼接另行靠上断言兜底）
const defined = new Set([...html.matchAll(/id="([^"]+)"/g)].map(m => m[1]));
const missing = new Set();
for (const m of html.matchAll(/\$\('([^']+)'\)/g)) {
  const id = m[1];
  if (id.startsWith("#")) missing.add(id + "（$ 不认选择器）");
  else if (!defined.has(id)) missing.add(id);
}
check("脚本静态引用的 id 均存在", missing.size === 0, [...missing].join(", "));

if (failures.length) {
  console.error(`\ncheck-ui FAIL（${failures.length} 项）：${failures.join("；")}`);
  process.exit(1);
}
console.log("\ncheck-ui PASS");
