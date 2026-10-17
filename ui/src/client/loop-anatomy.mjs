import { geometryOrders } from "./anatomy/geometry-orders.mjs";
import {
  slabOf,
  cylinder,
  extrude,
  radialTicks,
  segment,
  boxCorners,
  fitProjection,
  depthOf,
} from "./anatomy/iso-kit.mjs";

const machines = {
  outer: ["Outer · 審查回送分流機", "分開審查、回饋與驗收；回送路徑仍須原續行證據。"],
  inner: ["Inner · 隔離加工單元", "Stage 輸入進入隔離 worktree；原生輸出不等於 Goal 驗收。"],
  router: [
    "LoopManager · 接口交換台",
    "原 request 經所選接口交原 owner；配線不建立第二份任務狀態。",
  ],
  gauge: ["Schema · 校驗量規", "量規檢查已知資料與綁定，不能測出語義判斷是否正確。"],
  testbench: ["Test · 可換探頭測試台", "選定控制量測原行為，由獨立 Oracle 判定。"],
  trigger: ["Hook · 事件觸發開關", "原生事件與專用 binding 才能觸發；查看機構不發送事件。"],
  clock: ["Cron · 授權定時接點", "原 heartbeat 喚醒目前 Session；此物件不是額外 daemon。"],
  reservoir: ["Swap · 資源分配歧管", "全機額度與各 context 租約分開；最後租約交原 Host 釋放。"],
  index: ["Code Intel · 來源索引抽屜", "依 worktree 與來源版本查結構；結構有效不代表測試通過。"],
  meter: ["Cost · 觀察儀表台", "同源讀數保留時間與缺口；不同成本區間不能直接相加。"],
  dock: ["Provider · 交付對接座", "效果由原 Provider 執行並讀回；局部 PASS 不授權交付。"],
  reader: ["Knowledge · 來源讀取槽", "讀取原 Bundle 與 Cards；知識資料不提供執行權限。"],
  session: ["Session · 判斷操作台", "呈現已提交的判斷、輸入與輸出；不補寫未記錄的思考。"],
  carrier: ["Prompt · 載體轉換台", "保留兩端型態與原內容；連線不自行證明語義等價。"],
  selector: ["P-class／選測 · 條件選擇器", "條件選擇是原功能的一部分；不把指引當成執行授權。"],
  interlock: ["CLI · 互鎖轉移導軌", "依原狀態檢查合法轉移；保留拒絕與未知效果。"],
  workcell: ["Worker · 隔離加工單元", "依原 admission 處理已綁定 Stage。"],
  recorder: ["觀察 · 證據記錄台", "只顯示來源已有的讀回；缺少 invocation 不補畫因果邊。"],
  unknown: ["UNKNOWN · 機制尚未分類", "缺少版本化功能映射；保留原資料，不從名稱或狀態猜造型。"],
};

export function anatomyModel(graph, pick, scopeNodes = graph.nodes) {
  const object = pick.kind === "object";
  const value = (object ? graph.objects : graph.nodes).find((v) => v.id === pick.id);
  if (!value) {
    return null;
  }
  const nodes = object ? scopeNodes.filter((n) => n.object_id === value.id) : [value];
  const ids = new Set(nodes.map((n) => n.id));
  const edges = graph.edges.filter((e) => ids.has(e.from_node_id) || ids.has(e.to_node_id));
  const artifacts = (field) =>
    [...new Set(nodes.flatMap((n) => n[field]))]
      .map((id) => graph.artifacts.find((a) => a.id === id))
      .filter(Boolean);
  const inputs = artifacts("input_artifact_ids");
  const outputs = artifacts("output_artifact_ids");
  const selected = object ? value.mechanism?.template : value.mechanism;
  const template = Object.hasOwn(machines, selected) ? selected : "unknown";
  const peers = new Set(
    [...new Set(edges.flatMap((e) => [e.from_node_id, e.to_node_id]))]
      .map((id) => graph.nodes.find((n) => n.id === id)?.object_id)
      .filter((id) => id && id !== (object ? value.id : value.object_id)),
  );
  return {
    value,
    capabilities: object ? value.mechanism?.capabilities || [] : [],
    template,
    title: machines[template][0],
    description: machines[template][1],
    nodes,
    edges,
    inputs,
    outputs,
    evidence: artifacts("evidence_artifact_ids"),
    peers: graph.objects.filter((o) => peers.has(o.id)),
    findings: graph.findings.filter(
      (f) =>
        f.node_ids.some((id) => ids.has(id)) ||
        f.edge_ids.some((id) => edges.some((e) => e.id === id)),
    ),
    ports: graph.ports.filter((p) => p.object_id === (object ? value.id : value.object_id)),
  };
}

// Cached geometry is a mechanical analogy, never execution state or an instrument reading.
const line = (d, cls = "li-engraving") => `<path d="${d}" class="${cls}"/>`;
export function geometryFingerprint(parts) {
  let value = 2_166_136_261;
  for (const part of parts) {
    for (const character of part.shape.fill) {
      value = Math.imul(value ^ character.codePointAt(0), 16_777_619);
    }
  }
  return value < 0 ? value + 4_294_967_296 : value;
}
const figures = new Map();
export function mechanismFigure(selectedTemplate, { apart = false, angle = 42 } = {}) {
  const template = Object.hasOwn(machines, selectedTemplate) ? selectedTemplate : "unknown";
  const key = `${template}:${apart}:${angle}`;
  if (figures.has(key)) {
    return figures.get(key);
  }
  const p = fitProjection(boxCorners({ x: -112, y: -75, w: 224, d: 150 }, -14, 110), 650, 410, {
    pad: 20,
    azimuth: angle,
    elevation: 40,
  });
  const parts = [];
  const put = ([shape, center, cls = "", detail = ""]) => {
    parts.push({ depth: depthOf(center, p), shape, cls, detail });
  };
  const box = ([x, y, w, d, z, h, cls = ""]) =>
    put([slabOf({ x, y, w, d, r: 1.5 }, z, h, p, 4, 0.6), [x + w / 2, y + d / 2, z + h / 2], cls]);
  const round = ([x, y, r, z, h, cls = "", detail = ""]) =>
    put([cylinder(x, y, r, z, h, p, 40, 0.3), [x, y, z + h / 2], cls, detail]);

  const bolt = (x, y, z) =>
    round([x, y, 1.8, z, 1, "li-cap", line(segment([x - 1, y, z + 1], [x + 1, y, z + 1], p))]);
  const dial = ([x, y, r, z, clock = false]) => {
    round([x, y, r, z, 5, "li-cap"]);
    const ticks = radialTicks(x, y, r - 3, clock ? 60 : 30, 5, z + 6, [1.8, 4], p);
    const hands = clock
      ? [
          [x, y - r * 0.64],
          [x + r * 0.45, y + r * 0.15],
        ]
      : [[x - r * 0.5, y - r * 0.32]];
    round([
      x,
      y,
      r - 2,
      z + 5,
      1,
      "li-dial",
      line(ticks.minor) +
        line(ticks.major) +
        hands.map((end) => line(segment([x, y, z + 6], [...end, z + 6], p), "li-needle")).join(""),
    ]);
    round([x, y, 2, z + 6, 1, "li-port"]);
  };
  const knob = ([x, y, z, r = 6]) => {
    round([x, y, r, z, 6, "li-cap"]);
    round([
      x,
      y,
      r - 1,
      z + 6,
      1,
      "li-port",
      line(segment([x, y, z + 7], [x + r - 2, y, z + 7], p)),
    ]);
  };
  const travel = template === "gauge" ? 42 : 22;
  const lift = apart ? travel : 0;
  for (const x of [-96, 96]) {
    for (const y of [-60, 60]) {
      round([x, y, 7, -12, 4, "li-foot"]);
      round([x, y, 4, -8, 4, "li-rail"]);
    }
  }
  box([-110, -73, 220, 146, -4, 4, "li-base"]);
  for (const x of [-101, 101]) {
    for (const y of [-64, 64]) {
      bolt(x, y, 0);
    }
  }
  // Bound terminals belong to the explanatory fixture, not invented runtime ports.
  for (const x of [-99, 99]) {
    for (const y of [-30, 0, 30]) {
      round([x, y, 5, 0, 3, "li-cap"]);
      round([x, y, 2.7, 3, 1, "li-port"]);
    }
  }
  // The blank serial plate identifies the fixture; it carries no runtime measurement.
  box([-60, 60, 120, 8, 0, 1, "li-drawer"]);
  bolt(-54, 64, 1);
  bolt(54, 64, 1);
  if (template === "clock") {
    box([-72, -47, 114, 94, 0, 14, "li-cap"]);
    dial([-18, 0, 42, 14 + lift, true]);
    round([65, -22, 18, 0, 8, "li-drawer"]);
    // Eccentric cam and follower are exposed beside the clock, not a live time reading.
    round([69, -22, 12, 8, 4, "li-port"]);
    round([65, -22, 3, 12, 3, "li-cap"]);
    box([54, 0, 22, 35, 0, 5, "li-rail"]);
    box([59, 0, 12, 20, 5, 8, "li-drawer"]);
    round([65, 1, 6, 13, 4, "li-cap"]);
    knob([-18, -56, 0, 7]);
  } else if (template === "outer" || template === "carrier") {
    box([-80, -40, 160, 80, 0, 6, "li-cap"]);
    for (const y of [-36, 28]) {
      box([-77, y, 154, 8, 6, 8, "li-rail"]);
    }
    // Round roller ends identify a conveyor; the centre diverter belongs only to outer.
    for (let x = -68; x <= 68; x += 17) {
      box([x - 3, -26, 6, 52, 6, 4, "li-drawer"]);
      round([x, -32, 4, 14, 2, "li-cap"]);
      round([x, 32, 4, 14, 2, "li-cap"]);
    }
    for (const x of [-48, 40]) {
      box([x - 18, -23, 36, 46, 10 + lift, 2, "li-paper-sheet"]);
      for (let y = -14; y < 19; y += 8) {
        box([x - 12, y, 24, 1, 12 + lift, 0.5, "li-rail"]);
      }
    }
    if (template === "outer") {
      round([0, 0, 10, 10, 5, "li-cap"]);
      put([
        extrude(
          [
            [0, -4],
            [48, 17],
            [45, 24],
            [-3, 4],
          ],
          15,
          5,
          p,
        ),
        [20, 10, 17],
        "li-port",
      ]);
      box([-10, -48, 20, 8, 0, 42, "li-rail"]);
      dial([0, -44, 10, 42]);
    } else {
      box([-6, -47, 12, 7, 0, 42, "li-rail"]);
      box([-6, 40, 12, 7, 0, 42, "li-rail"]);
      box([-19, -40, 38, 80, 42 + lift, 4, "li-cap"]);
    }
  } else if (template === "inner" || template === "workcell") {
    box([-72, -46, 144, 92, 0, 8, "li-cap"]);
    for (const x of [-68, 58]) {
      box([x, -42, 10, 84, 8, 6, "li-rail"]);
      round([x + 5, -30, 4, 14, 54, "li-rail"]);
    }
    box([-68, -36, 136, 12, 68 + lift, 9, "li-cap"]);
    box([-18, -33, 36, 10, 43 + lift, 25, "li-drawer"]);
    round([0, -28, 6, 31 + lift, 12, "li-port"]);
    box([-48, -5, 96, 42, 8, 6, "li-drawer"]);
    for (const x of [-42, 30]) {
      box([x, 1, 12, 30, 14, 10, "li-cap"]);
    }
    for (const y of [4, 12, 20, 28]) {
      box([-28, y, 56, 2, 14, 0.8, "li-rail"]);
    }
    knob([57, 30, 14]);
    dial([-57, 34, 9, 14]);
  } else if (template === "router") {
    for (const x of [-78, 70]) {
      box([x, -45, 8, 90, 0, 40, "li-rail"]);
    }
    for (let r = 0; r < 3; r++) {
      box([-70, -40 + r * 29, 140, 24, 6 + r * 12 + lift, 5, "li-cap"]);
      for (let c = 0; c < 5; c++) {
        const x = -56 + c * 28;
        const y = -28 + r * 29;
        const z = 11 + r * 12 + lift;
        round([x, y, 7, z, 3, "li-drawer"]);
        round([x, y, 3.5, z + 3, 1, "li-port"]);
      }
    }
  } else if (template === "gauge" || template === "testbench") {
    box([-78, -45, 156, 90, 0, 8, "li-cap"]);
    round([-58, -25, 7, 8, 5, "li-cap"]);
    round([-58, -25, 4, 13, 51, "li-rail"]);
    box([-65, -31, 96, 12, 64, 7, "li-cap"]);
    knob([-58, -25, 71]);
    if (template === "gauge") {
      dial([17, -25, 24, 71 + lift]);
      round([17, -25, 3, 34 + lift, 30, "li-port"]);
      box([-12, -38, 58, 33, 8, 7, "li-drawer"]);
      for (const x of [-10, 34]) {
        box([x, -36, 10, 29, 15, 11, "li-cap"]);
      }
      box([-40, 18, 108, 7, 8, 4, "li-rail"]);
      box([-40, 18, 10, 23, 12, 5, "li-cap"]);
      box([34, 18, 10, 23, 12, 5, "li-drawer"]);
      knob([39, 25, 17, 4]);
    } else {
      for (const x of [-12, 16, 44]) {
        round([x, 14, 6, 8, 5, "li-cap"]);
        round([x, 14, 2, 13, 22 + lift, "li-port"]);
      }
      dial([48, -26, 17, 8]);
      box([-62, 26, 110, 16, 8, 5, "li-drawer"]);
      for (const x of [-48, -20, 8]) {
        knob([x, 34, 13, 4]);
      }
    }
  } else if (template === "trigger") {
    box([-56, -35, 112, 70, 0, 16, "li-cap"]);
    box([-43, -25, 86, 50, 16, 8, "li-drawer"]);
    round([-27, 0, 8, 24, 5, "li-cap"]);
    put([
      extrude(
        [
          [-29, -5],
          [47, 14],
          [45, 23],
          [-31, 5],
        ],
        29 + lift,
        4,
        p,
      ),
      [8, 8, 31 + lift],
      "li-rail",
    ]);
    round([45, 18, 11, 33 + lift, 8, "li-port"]);
    for (const x of [-24, 0, 24]) {
      round([x, 35, 5, 16, 3, "li-cap"]);
      bolt(x, 35, 19);
    }
    box([8, -16, 10, 12, 24, 4, "li-port"]);
    box([26, -16, 10, 12, 24, 4, "li-port"]);
  } else if (template === "selector" || template === "interlock") {
    if (template === "selector") {
      round([0, 0, 48, 0, 9, "li-cap"]);
      dial([0, 0, 43, 9 + lift]);
      knob([0, 0, 16 + lift, 12]);
      for (let i = 0; i < 6; i++) {
        const a = (i * Math.PI) / 3;
        round([Math.cos(a) * 57, Math.sin(a) * 57, 5, 0, 5, "li-port"]);
      }
    } else {
      box([-70, -35, 140, 70, 0, 8, "li-cap"]);
      for (const y of [-27, 23]) {
        box([-62, y, 124, 4, 8, 8, "li-rail"]);
      }
      box([-45, -20, 44, 40, 8 + lift, 8, "li-drawer"]);
      for (const x of [4, 26, 48]) {
        box([x, -18, 12, 36, 8, 6, "li-cap"]);
        round([x + 6, 0, 3, 14, 15, "li-port"]);
      }
      knob([-23, 0, 16 + lift, 9]);
      box([65, -19, 8, 38, 8, 24, "li-cap"]);
    }
  } else if (template === "session") {
    box([-68, -42, 136, 18, 0, 65, "li-cap"]);
    box([-59, -24, 118, 4, 22, 34, "li-screen"]);
    box([-66, -7, 132, 47, 0, 7, "li-drawer"]);
    for (let row = 0; row < 3; row++) {
      for (let col = 0; col < 10; col++) {
        box([-57 + col * 11, 1 + row * 10, 8, 7, 7, 3, "li-cap"]);
      }
    }
    knob([53, 29, 10, 5]);
  } else if (template === "reservoir") {
    round([-35, 0, 29, 0, 7, "li-cap"]);
    round([-35, 0, 25, 7, 54, "li-drawer"]);
    round([-35, 0, 29, 61 + lift, 5, "li-cap"]);
    dial([-35, 0, 16, 66 + lift]);
    for (const y of [-31, 0, 31]) {
      box([7, y - 7, 62, 14, 0, 10, "li-rail"]);
      knob([48, y, 10, 8]);
      round([74, y, 5, 0, 10, "li-port"]);
    }
  } else if (template === "index" || template === "reader") {
    for (const x of [-65, 57]) {
      box([x, -40, 8, 80, 0, 72, "li-cap"]);
    }
    const count = template === "index" ? 5 : 1;
    for (let i = 0; i < count; i++) {
      box([-57, -38 + (apart ? i * 4 : 0), 114, 72, 4 + i * 13, 8, "li-drawer"]);
      box([-12, 34 + (apart ? i * 4 : 0), 24, 4, 7 + i * 13, 3, "li-port"]);
    }
    if (template === "reader") {
      box([-57, -40, 114, 12, 52 + lift, 7, "li-cap"]);
      box([-50, -26, 100, 55, 12, 2, "li-paper-sheet"]);
      for (let x = -42; x < 45; x += 7) {
        box([x, -18, 3, 35, 14, 0.5, "li-rail"]);
      }
    }
  } else if (template === "dock") {
    for (const x of [-75, 25]) {
      box([x, -36, 50, 72, 0, 20, "li-cap"]);
      for (const y of [-22, 0, 22]) {
        round([x + 25, y, 8, 20 + lift, 4, "li-drawer"]);
        round([x + 25, y, 3, 24 + lift, 3, "li-port"]);
      }
    }
    box([-25, -28, 50, 56, 0, 6, "li-rail"]);
  } else if (template === "meter" || template === "recorder") {
    box([-78, -43, 156, 86, 0, 12, "li-cap"]);
    if (template === "meter") {
      for (const x of [-48, 0, 48]) {
        dial([x, -7, 20, 12 + lift]);
      }
    } else {
      for (const x of [-48, 48]) {
        round([x, -12, 23, 12, 6, "li-drawer"]);
      }
      box([-38, -30, 76, 36, 18 + lift, 1, "li-paper-sheet"]);
      box([-61, 20, 122, 13, 12, 5, "li-cap"]);
      box([-3, -22, 6, 42, 19 + lift, 3, "li-port"]);
    }
    for (const x of [-58, -29, 0, 29, 58]) {
      knob([x, 29, template === "recorder" ? 17 : 12, 4]);
    }
  } else {
    box([-48, -28, 96, 56, 0, 5, "li-drawer"]);
    // Empty mounting points communicate an unclassified mechanism, not a guessed machine.
    for (const x of [-36, -12, 12, 36]) {
      for (const y of [-18, 18]) {
        round([x, y, 4, 5, 1, "li-cap"]);
        bolt(x, y, 6);
      }
    }
  }
  const settled = geometryOrders[key];
  if (
    !settled ||
    settled.fingerprint !== geometryFingerprint(parts) ||
    settled.order.length !== parts.length
  ) {
    throw new Error(`Anatomy geometry changed for ${key}; regenerate the audited drawing order.`);
  }
  const orderedParts = settled.order.map((index) => parts[index]);
  const paths = orderedParts
    .map(
      ({ shape: s, cls, detail }) =>
        `<g class="li-solid ${cls}"><path d="${s.fill}" class="li-fill"/>${s.shades.map((d, i) => `<path d="${d}" class="li-shade-${i}"/>`).join("")}<path d="${s.top}" class="li-top"/>${detail}<path d="${s.outline}" class="li-outline"/><path d="${s.bevel || ""}" class="li-bevel"/></g>`,
    )
    .join("");
  // Kit solids use projected M/L pairs. Fit the actual drawn silhouette, including apart parts.
  const points = parts.flatMap(({ shape }) =>
    [...shape.fill.matchAll(/[ML](-?[\d.]+) (-?[\d.]+)/g)].map((m) => [Number(m[1]), Number(m[2])]),
  );
  const left = Math.min(...points.map((point) => point[0])) - 12;
  const top = Math.min(...points.map((point) => point[1])) - 12;
  const width = Math.max(...points.map((point) => point[0])) - left + 12;
  const height = Math.max(...points.map((point) => point[1])) - top + 12;
  const result = `<svg viewBox="${left} ${top} ${width} ${height}" role="img" aria-label="${machines[template][0]}" data-anatomy-template="${template}" data-parts="${parts.length}">${paths}</svg>`;
  figures.set(key, result);
  return result;
}

function el(tag, text) {
  const n = document.createElement(tag);
  n.textContent = text;
  return n;
}

export function mountAnatomy(root, model, { onSelect, onOriginal }) {
  const button = (text, action) => {
    const n = el("button", text);
    n.type = "button";
    n.addEventListener("click", action);
    return n;
  };
  root.replaceChildren();
  root.className = "li-anatomy";
  const heading = el("h4", model.title);
  const description = el("p", model.description);
  root.append(heading);
  const art = el("div", "");
  art.className = "li-anatomy-art";
  art.innerHTML = mechanismFigure(model.template);
  let activeTemplate = model.template;
  let apart = false;
  let zoom = 1;
  const controls = el("div", "");
  controls.className = "li-toolbar";
  controls.append(
    button("拆解／組裝機構", () => {
      apart = !apart;
      art.innerHTML = mechanismFigure(activeTemplate, { apart });
    }),
    button("詳圖 1×／4×", () => {
      zoom = zoom === 1 ? 4 : 1;
      art.style.setProperty("--anatomy-zoom", zoom);
    }),
  );
  root.append(
    art,
    controls,
    description,
    el("p", "機構是解說模型，位置與零件數不代表進度或執行次數。以下讀數來自同一筆原來源。"),
  );
  const fields = el("div", "");
  fields.className = "li-anatomy-fields";
  for (const [name, values] of [
    ["輸入", model.inputs],
    ["輸出", model.outputs],
    ["證據", model.evidence],
  ]) {
    const section = el("details", "");
    section.append(el("summary", `${name} · ${values.length}`));
    if (values.length === 0) {
      section.append(el("p", "UNKNOWN · 未記錄"));
    }
    section.addEventListener("toggle", () => {
      if (!section.open || section.dataset.loaded) {
        return;
      }
      section.dataset.loaded = "true";
      for (const a of values) {
        section.append(
          button(`${a.role} · ${a.format} · ${a.status}`, () =>
            onOriginal({ kind: "artifact", id: a.id }),
          ),
        );
      }
    });
    fields.append(section);
  }
  root.append(fields);
  if (model.capabilities.length > 0) {
    const capabilities = el("details", "");
    capabilities.open = true;
    capabilities.append(el("summary", `原物件內的功能機構 · ${model.capabilities.length}`));
    capabilities.append(
      el("p", "以下為版本化接口契約。存在接口不等於本次已執行；按鈕只切換解說機構。"),
    );
    const capabilityButton = (c) =>
      button("查看此功能機構", () => {
        activeTemplate = Object.hasOwn(machines, c.function) ? c.function : "unknown";
        const [title, detail] = machines[activeTemplate];
        heading.textContent = `${model.value.label} / ${c.id} · ${title}`;
        description.textContent = detail;
        art.innerHTML = mechanismFigure(activeTemplate, { apart });
      });
    for (const c of model.capabilities) {
      const section = el("details", "");
      section.append(el("summary", c.id));
      section.append(capabilityButton(c));
      for (const [name, key] of [
        ["原入口", "entry"],
        ["輸入", "input"],
        ["輸出", "output"],
        ["讀回", "readback"],
        ["拒絕／未知", "refusal"],
        ["來源", "source"],
      ]) {
        section.append(el("p", `${name} · ${c[key]}`));
      }
      capabilities.append(section);
    }
    root.append(capabilities);
  }
  const details = el("details", "");
  details.open = true;
  details.append(el("summary", "條件／斷言／原續行與拒絕"));
  const conditions = model.nodes.filter((n) =>
    /#\/(?:outer\/thinking|.*assertions(?:\/|$)|.*next(?:\/|$))/.test(n.identity.locator || ""),
  );
  if (conditions.length === 0 && model.findings.length === 0) {
    details.append(el("p", "UNKNOWN · 此節點沒有結構化條件或拒絕記錄。"));
  }
  for (const n of conditions) {
    details.append(
      button(`${n.label} · ${n.status.execution}`, () => onSelect({ kind: "node", id: n.id })),
    );
  }
  for (const f of model.findings) {
    details.append(
      button(`${f.status === "failed" ? "✕" : "?"} ${f.message}`, () =>
        onOriginal({ kind: "finding", id: f.id }),
      ),
    );
  }
  root.append(details);
  const list = el("details", "");
  list.append(el("summary", `已記錄節點 · ${model.nodes.length}`));
  for (const n of model.nodes) {
    list.append(
      button(`${n.kind} · ${n.label} · ${n.status.execution}`, () =>
        onSelect({ kind: "node", id: n.id }),
      ),
    );
  }
  root.append(list);
  for (const port of model.ports) {
    root.append(el("p", `${port.direction} · ${port.name} · ${port.schema}`));
  }
  if (model.peers.length > 0) {
    root.append(el("h5", "沿原交接連到唯一物件"));
  }
  for (const o of model.peers) {
    root.append(button(o.label, () => onSelect({ kind: "object", id: o.id })));
  }
  root.append(button("開啟原內容與同窗比較", onOriginal));
}
