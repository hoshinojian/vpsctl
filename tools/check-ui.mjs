#!/usr/bin/env node
// check-ui：管理台前端的最小无依赖冒烟检查。
// 目的：拦住"求值期/渲染期崩溃导致整页死掉"这一类故障（历史案例：
// $() 误用 # 选择器、重构丢失 key() 定义——两者症状都是 KPI 正常而列表空白）。
//
// 做法：从 internal/webui/static/index.html 抽出全部 <script> 块，在极简 DOM 桩里
// 用固定载荷完整执行，然后断言：脚本求值无异常、无未捕获异常、summary/KPI 已更新、
// 表格行数与载荷台数一致、地图 marker 数与有坐标节点数一致、脚本引用的静态 id 都存在；
// 另外核对云平台标签、价格两行文案（按量 / 包年包月 / 到期）、批量关机的台数确认、
// 创建弹窗的几处提示这些关键文案。
//
// 用法：node tools/check-ui.mjs [index.html 路径]（默认仓内页面）。
// 需要 node ≥18；无需任何第三方依赖。
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const pagePath = process.argv[2] ?? join(root, "internal/webui/static/index.html");
const html = readFileSync(pagePath, "utf8");

// ---- 固定载荷：覆盖 有坐标/无坐标/多状态/账号查询失败，以及三种计费方式 ----
// DO（按量、月价是官方标价）、腾讯云按量（以小时价为主）、腾讯云轻量与阿里云包年包月
// （续费价 + 到期 + 自动续费两种状态）、包年包月但问不到价的。
const inDays = n => new Date(Date.now() + n * 86400000).toISOString();
// 与页面的 fmtDay 同一条规则：今年内写"月/日"，跨年带年份
const dayOf = n => {
  const t = new Date(inDays(n)), md = (t.getMonth() + 1) + "/" + t.getDate();
  return t.getFullYear() === new Date().getFullYear() ? md : t.getFullYear() + "/" + md;
};
const FIXTURE_DROPLETS = {
  droplets: [
    { account: "do-1", id: "1", name: "do-1-node-a", provider: "digitalocean", billing: "hourly", status: "active", region: "ams3",
      lat: 52.37, lng: 4.9, ipv4_public: "203.0.113.10", price_monthly: 6, price_hourly: 0.00893, tags: ["nms-node"] },
    { account: "do-1", id: "2", name: "do-1-node-b", provider: "digitalocean", billing: "hourly", status: "off", region: "lon1",
      lat: 51.51, lng: -0.13, ipv4_public: "203.0.113.11", price_monthly: 6, price_hourly: 0.00893, tags: [] },
    { account: "do-2", id: "3", name: "do-2-node-c", provider: "digitalocean", billing: "hourly", status: "new", region: "unknown-region",
      ipv4_public: "", price_monthly: 5, price_hourly: 0.00595, tags: [] },
    { account: "tc-1", id: "ins-1", name: "tc-1-node-d", provider: "tencent-cvm", billing: "hourly", status: "stopping", region: "ap-beijing",
      lat: 39.9, lng: 116.41, ipv4_public: "203.0.113.12", price_monthly: 151.1, price_hourly: 0.207, currency: "CNY", tags: ["batch:t1"] },
    { account: "lh-1", id: "lhins-1", name: "lh-1-node-e", provider: "tencent-lighthouse", billing: "monthly", status: "active", region: "ap-guangzhou",
      lat: 23.13, lng: 113.26, ipv4_public: "203.0.113.13", price_monthly: 35, currency: "CNY", expires_at: inDays(40), auto_renew: true, tags: [] },
    { account: "al-1", id: "i-1", name: "al-1-node-f", provider: "aliyun-ecs", billing: "monthly", status: "active", region: "cn-qingdao",
      lat: 36.07, lng: 120.38, ipv4_public: "203.0.113.14", price_monthly: 1263.64, currency: "CNY", expires_at: inDays(15), auto_renew: false, tags: [] },
    { account: "al-1", id: "i-2", name: "al-1-node-g", provider: "aliyun-ecs", billing: "monthly", status: "active", region: "cn-qingdao",
      lat: 36.07, lng: 120.38, ipv4_public: "203.0.113.15", price_monthly: 0, currency: "CNY", expires_at: inDays(3), auto_renew: false, tags: [] },
  ],
  errors: [{ account: "do-3", error: "HTTP 401" }],
};
const ACCOUNTS = [
  { name: "do-1", provider: "digitalocean", ssh_user: "root", has_password: true },
  { name: "do-2", provider: "digitalocean", ssh_user: "root", has_password: true },
  { name: "tc-1", provider: "tencent-cvm", ssh_user: "", has_password: false },
  { name: "lh-1", provider: "tencent-lighthouse", ssh_user: "", has_password: false },
  { name: "al-1", provider: "aliyun-ecs", ssh_user: "", has_password: false },
];
const FIXTURES = {
  "/api/droplets": FIXTURE_DROPLETS,
  "/api/catalog": { accounts: ACCOUNTS.map(a => a.name) },
  // 轻量账号的目录：套餐自带月价；一台 Ubuntu、一台 Debian 的系统镜像
  "/api/catalog?account=lh-1": {
    regions: [{ slug: "ap-guangzhou", name: "华南地区(广州)" }],
    sizes: [{ slug: "bundle_starter_mc_med2_03", vcpus: 2, memory_mb: 2048, disk_gb: 40, price_monthly: 35, currency: "CNY" }],
    images: [
      { slug: "ubuntu-24.04", distribution: "Ubuntu", name: "Ubuntu Server 24.04 LTS 64bit" },
      { slug: "debian-12", distribution: "Debian", name: "Debian 12.0 64bit" },
    ],
    keys: [],
  },
  "/api/accounts": { accounts: ACCOUNTS,
    providers: ["aliyun-ecs", "digitalocean", "tencent-cvm", "tencent-lighthouse"],
    key_providers: ["aliyun-ecs", "tencent-cvm", "tencent-lighthouse"] },
  // 默认账号（排在第一个的 do-1）的目录：下面让它故意慢一点返回，复现"打开弹窗立刻切账号"的竞态
  "/api/catalog?account=do-1": {
    regions: [{ slug: "sgp1", name: "Singapore 1" }],
    sizes: [{ slug: "s-1vcpu-1gb", vcpus: 1, memory_mb: 1024, disk_gb: 25, price_monthly: 6 }],
    images: [{ slug: "ubuntu-24-04-x64", distribution: "Ubuntu", name: "24.04 x64" }],
    keys: [],
  },
  "/api/operations": { operations: [
    { time: "2026-09-09T03:00:00Z", kind: "power", detail: "power_off 1 台", ok: 1, fail: 0 },
  ] },
};
// 地图上同坐标的节点聚成一个 marker
const markerCount = new Set(FIXTURE_DROPLETS.droplets.filter(d => d.lat || d.lng).map(d => d.lat + "," + d.lng)).size;

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
  // 模仿 DOM：textContent 赋值一律转字符串（页面常直接赋数字，如 KPI 计数），
  // 且会清掉子节点（下拉框、表格都是先清空再重建）
  Object.defineProperty(el, "textContent", {
    get() { return this._text; },
    set(v) { this._text = String(v); this.children = []; },
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
const posts = [];                  // 记录发往写接口的请求，断言"该拦的没发出去"
globalThis.fetch = async (url, init = {}) => {
  const full = new URL(url, "http://check-ui");
  // 带账号的目录查询按账号找载荷，找不到再退回不带参数的那份
  const key = full.searchParams.has("account") ? full.pathname + "?account=" + full.searchParams.get("account") : full.pathname;
  if (init.method === "POST") {
    posts.push(full.pathname + " " + String(init.body));
    // 建机接口的回应由页面驱动脚本指定（要模拟"一台都没建出来"）；别的写接口一律回空结果
    const resp = full.pathname === "/api/create" && globalThis.__createResponse ? globalThis.__createResponse : { results: [] };
    globalThis.__createResponse = null;
    return { ok: true, status: 200, json: async () => structuredClone(resp) };
  }
  if (!(key in FIXTURES)) throw new Error("无载荷 fixture: " + key);
  // 默认账号的目录慢：真实环境里阿里云要查 33 个地域，比别的账号晚到
  if (key === "/api/catalog?account=do-1") await new Promise(r => setTimeout(r, 80));
  return { ok: true, json: async () => structuredClone(FIXTURES[key]) };
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
process.on("unhandledRejection", (e) => uncaught.push(String(e && e.message ? e.message : e)));

// 页面作用域内驱动：等 load 完成后依次看 列表 → 详情 → 批量关机 → 创建弹窗，把结果回传
const driver = `
;(async () => {
  try {
    await new Promise(r => setTimeout(r, 150));
    setView("map");
    // 桩的 textContent 不含子节点，自己拼
    const text = el => { let s = el._text || ""; for (const k of el.children) s += (s ? "|" : "") + text(k); return s; };
    const cells = tr => tr.children;
    const rows = {};
    for (const tr of document.getElementById("rows").children) {
      const tds = cells(tr);
      const price = tds.find(td => td.className === "price");
      rows[tds[1]._text] = {
        cloud: text(tds.find(td => td.className === "cloudcell")),
        price: text(price),
        tone: price.children[1] ? price.children[1].className.replace("p-sub", "").trim() : "",
      };
    }
    const out = {
      summary: document.getElementById("summary").textContent,
      rowsCount: document.getElementById("rows").children.length,
      markers: (document.getElementById("map-svg").innerHTML.match(/marker-group/g) || []).length,
      kpiTotal: document.getElementById("kpi-total").textContent,
      kpiCost: document.getElementById("kpi-cost").textContent,
      kpiCostSub: document.getElementById("kpi-cost-sub").textContent,
      cloudOptions: [...document.getElementById("f-provider").children].map(o => o.text),
      rows,
    };
    // 云平台筛选：只看腾讯云（云服务器 + 轻量都算）
    document.getElementById("f-provider").value = "tencent";
    render();
    out.filteredTencent = [...document.getElementById("rows").children].map(tr => cells(tr)[1]._text);
    document.getElementById("f-provider").value = "";
    render();

    // 详情：阿里云包年包月那台
    openDetails(droplets.find(d => d.id === "i-1"));
    out.detail = text(document.getElementById("dt-grid"));

    // 批量关机：选两台 → 弹台数确认；台数没输对时确认按钮不可点，也不会发请求
    selected.clear();
    selected.add(key(droplets[0])); selected.add(key(droplets[1]));
    await power("power_off");
    out.offModalOpen = document.getElementById("modal-off").hidden === false;
    out.offCount = document.getElementById("off-count").textContent;
    out.offConfirmDisabledBefore = document.getElementById("off-confirm").disabled;
    document.getElementById("off-input").value = "1";
    document.getElementById("off-input").oninput({ target: document.getElementById("off-input") });
    out.offConfirmDisabledWrong = document.getElementById("off-confirm").disabled;
    document.getElementById("off-input").value = "2";
    document.getElementById("off-input").oninput({ target: document.getElementById("off-input") });
    out.offConfirmDisabledRight = document.getElementById("off-confirm").disabled;
    out.postsBeforeConfirm = posts.length;
    document.getElementById("modal-off").hidden = true;
    selected.clear();

    // 创建弹窗的竞态：弹窗一打开就为默认账号（do-1）去拉目录，那个请求慢；用户立刻切到轻量账号，
    // 轻量的目录先到。慢请求后到时不能把当前账号的选项盖掉——真机上出过"账号是腾讯云、
    // 区域 / 机型 / 镜像却是阿里云的"
    (() => { openCreate().catch(() => {}); })();
    await new Promise(r => setTimeout(r, 20));
    const acctSel = document.getElementById("cr-account");
    // 真实浏览器里下拉填入选项后会自动选中第一个，页面模型不会——手动把默认账号选中，
    // 让它的（慢）目录请求先发出去，这才是真机上出事时的状态
    acctSel.value = acctSel.children[0] ? acctSel.children[0].value : "";
    acctSel.onchange();
    await new Promise(r => setTimeout(r, 20));   // 默认账号的目录还在路上（它要 80ms）
    out.raceSubmitDisabledWhileLoading = document.getElementById("cr-submit").disabled;
    await new Promise(r => setTimeout(r, 20));
    acctSel.value = "lh-1";
    acctSel.onchange();   // 页面模型没有 dispatchEvent，直接调页面挂的切换处理
    await new Promise(r => setTimeout(r, 300));   // 等慢请求也回来了
    out.raceAccount = acctSel.value;
    out.raceSizeOptions = [...document.getElementById("cr-size").children].map(o => o.value);
    out.raceRegionOptions = [...document.getElementById("cr-region").children].map(o => o.value);
    document.getElementById("modal-create").hidden = true;
    await new Promise(r => setTimeout(r, 150));

    // 创建弹窗：账号名带云平台；轻量账号说明"付一个月"；轻量 + Ubuntu 提示不能重装
    await openCreate();
    out.createAccountOptions = [...document.getElementById("cr-account").children].map(o => o.text);
    document.getElementById("cr-account").value = "lh-1";
    await loadCatalogFor("lh-1");
    document.getElementById("cr-size").value = "bundle_starter_mc_med2_03";
    document.getElementById("cr-image").value = "ubuntu-24.04";
    updateEstimate();
    out.lightEstimate = document.getElementById("cr-est").textContent;
    out.lightWarnUbuntu = document.getElementById("cr-warn").textContent;
    document.getElementById("cr-image").value = "debian-12";
    updateEstimate();
    out.lightWarnDebian = document.getElementById("cr-warn").textContent;

    // 建机一台都没建出来（预检没过）：服务端给的是 created: null + errors。界面要把原因亮出来，
    // 不能读 null 的长度崩掉把原因吞了——真机上出过，用户只看到弹窗消失、没有任何提示
    globalThis.__createResponse = { batch: "b", requested: { "lh-1": 1 }, created: null,
      errors: [{ account: "lh-1", name: "vps-lh-1-ap-guangzhou-01", index: 1,
        error: "建机预检（DryRun）未通过：ResourceUnavailable.InstanceType: 机型在当前可用区没有货" }] };
    const bannerEl = document.getElementById("banner");
    bannerEl.className = ""; bannerEl.textContent = "";
    await document.getElementById("cr-submit").onclick();
    out.createAllFailedBanner = bannerEl.textContent;
    out.createAllFailedShown = bannerEl.className.includes("show");
    out.createAllFailedModalHidden = document.getElementById("modal-create").hidden;
    bannerEl.className = ""; bannerEl.textContent = "";
    document.getElementById("modal-create").hidden = true;

    globalThis.__ui = out;
  } catch (e) { globalThis.__ui = { error: (e && e.stack) || String(e) }; }
})();
`;
const script = blocks.join("\n;\n") + driver;
try {
  new Function("posts", script)(posts);
} catch (e) {
  evalError = e.message;
}
await new Promise(r => setTimeout(r, 700));

// ---- 断言 ----
check("脚本求值无异常", evalError === null, evalError ?? "");
check("运行期无未捕获异常", uncaught.length === 0, uncaught.join("; "));
const ui = globalThis.__ui ?? {};
if (ui.error) failures.push("视图驱动失败: " + ui.error);
check("页面驱动完成", ui.error === undefined, ui.error ?? "");
const n = FIXTURE_DROPLETS.droplets.length;
check("summary 已渲染", (ui.summary ?? "").includes("个账号"), ui.summary ?? "(空)");
check(`表格行数 = ${n}`, ui.rowsCount === n, `got ${ui.rowsCount}`);
check("KPI 节点总数已更新", ui.kpiTotal === String(n), `got ${ui.kpiTotal}`);
check(`地图 marker = ${markerCount}（有坐标节点按坐标聚合）`, ui.markers === markerCount, `got ${ui.markers}`);

// 云平台：每行都有标签，筛选下拉按云归并（腾讯云两条产品线算一家）
const wantCloud = { "do-1-node-a": "DO", "tc-1-node-d": "腾讯云", "lh-1-node-e": "腾讯云", "al-1-node-f": "阿里云", "al-1-node-g": "阿里云" };
for (const [name, tag] of Object.entries(wantCloud)) {
  check(`云平台标签 ${name} = ${tag}`, ui.rows?.[name]?.cloud === tag, `got ${ui.rows?.[name]?.cloud}`);
}
check("云平台下拉 = 全部 / DO 3 台 / 腾讯云 2 台 / 阿里云 2 台",
  JSON.stringify(ui.cloudOptions) === JSON.stringify(["全部云平台", "DigitalOcean（3 台）", "腾讯云（2 台）", "阿里云（2 台）"]),
  JSON.stringify(ui.cloudOptions));
check("筛选腾讯云 → 两台（云服务器 + 轻量）",
  JSON.stringify(ui.filteredTencent) === JSON.stringify(["tc-1-node-d", "lh-1-node-e"]), JSON.stringify(ui.filteredTencent));

// 价格两行：主数字用真实的计费单位
const wantPrice = {
  "do-1-node-a": ["$6.00/月|$0.0089/时", ""],                 // DO：月价为主，小时价是官方值
  "tc-1-node-d": ["CNY 0.207/时|≈ CNY 151.10/月", ""],         // 国内云按量：小时价为主，月价是估算
  "lh-1-node-e": [`CNY 35.00/月|包年包月 · ${dayOf(40)} 到期 · 已开自动续费`, ""],   // 开了自动续费：不醒目
  "al-1-node-f": [`CNY 1263.64/月|包年包月 · ${dayOf(15)} 到期 · 未开自动续费`, "soon"],   // 半个月内到期
  "al-1-node-g": [`包年包月|包年包月 · ${dayOf(3)} 到期 · 未开自动续费`, "urgent"],      // 一周内到期、且问不到价
};
for (const [name, [text, tone]] of Object.entries(wantPrice)) {
  check(`价格 ${name}`, ui.rows?.[name]?.price === text && ui.rows?.[name]?.tone === tone,
    `got ${ui.rows?.[name]?.price} [${ui.rows?.[name]?.tone}]`);
}
check("合计把包年包月按续费价计入并注明",
  ui.kpiCost === "CNY 1449.74 + $17.00" && (ui.kpiCostSub ?? "").includes("含 3 台包年包月，按续费价"),
  `${ui.kpiCost} / ${ui.kpiCostSub}`);

// 详情：实例 ID 的标签、计费方式、到期与自动续费
const detail = ui.detail ?? "";
check("详情带 云平台 / 实例 ID / 计费方式 / 到期未自动续费",
  detail.includes("阿里云") && detail.includes("实例 ID") && !detail.includes("droplet id")
  && detail.includes("包年包月（按月付、到期续费") && detail.includes("未开自动续费"), detail.slice(0, 260));

// 批量关机：弹台数确认，没输对台数不能确认，期间不发请求
check("批量关机弹台数确认（2 台）", ui.offModalOpen === true && ui.offCount === "2", `${ui.offModalOpen} / ${ui.offCount}`);
check("台数没输对时不能确认", ui.offConfirmDisabledBefore === true && ui.offConfirmDisabledWrong === true,
  `${ui.offConfirmDisabledBefore} / ${ui.offConfirmDisabledWrong}`);
check("台数输对后才能确认", ui.offConfirmDisabledRight === false, `got ${ui.offConfirmDisabledRight}`);
check("关机弹窗期间没有发出关机请求", ui.postsBeforeConfirm === 0, `posts = ${JSON.stringify(posts)}`);

// 创建弹窗
check("快速切账号：默认账号的慢目录不会盖掉当前账号的选项",
  ui.raceAccount === "lh-1" && JSON.stringify(ui.raceSizeOptions) === JSON.stringify(["bundle_starter_mc_med2_03"])
  && JSON.stringify(ui.raceRegionOptions) === JSON.stringify(["ap-guangzhou"]),
  `account=${ui.raceAccount} sizes=${JSON.stringify(ui.raceSizeOptions)} regions=${JSON.stringify(ui.raceRegionOptions)}`);
check("目录还在加载时不能点「创建」", ui.raceSubmitDisabledWhileLoading === true, `disabled=${ui.raceSubmitDisabledWhileLoading}`);
check("创建弹窗的账号带云平台名",
  ["do-1 · DigitalOcean", "tc-1 · 腾讯云", "lh-1 · 腾讯云", "al-1 · 阿里云"].every(t => (ui.createAccountOptions ?? []).includes(t)),
  JSON.stringify(ui.createAccountOptions));
check("轻量账号说明「建一次付一个月」", (ui.lightEstimate ?? "").includes("建一次付一个月")
  && (ui.lightEstimate ?? "").includes("CNY 35.00") && (ui.lightEstimate ?? "").includes("自动续费"), ui.lightEstimate);
check("轻量 + Ubuntu 提示以后不能重装", (ui.lightWarnUbuntu ?? "").includes("不能重装"), ui.lightWarnUbuntu);
check("轻量 + Debian 没有这条提示", ui.lightWarnDebian === "", ui.lightWarnDebian);
check("建机全部失败：原因显示在页面上，而不是崩掉被吞",
  ui.createAllFailedShown === true && (ui.createAllFailedBanner ?? "").includes("机型在当前可用区没有货")
  && !(ui.createAllFailedBanner ?? "").includes("TypeError"), `shown=${ui.createAllFailedShown} banner=${ui.createAllFailedBanner}`);

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
