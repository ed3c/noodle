import { slabOf, boxCorners, fitProjection, depthOf } from "./anatomy/iso-kit.mjs";

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
  const inputs = artifacts("input_artifact_ids"),
    outputs = artifacts("output_artifact_ids");
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

/** Mechanical analogy only. Parts count is geometry, never an execution metric. */
export function mechanismFigure(selectedTemplate, { apart = false } = {}) {
  const template = Object.hasOwn(machines, selectedTemplate) ? selectedTemplate : "unknown";
  const p = fitProjection(boxCorners({ x: -112, y: -75, w: 224, d: 150 }, -14, 110), 650, 410, {
    pad: 26,
    azimuth: 42,
    elevation: 35,
  });
  const parts = [];
  const add = ([x, y, w, d, z, h, cls = ""]) => {
    const shape = slabOf({ x, y, w, d, r: 1.5 }, z, h, p, 4, 0.6);
    parts.push({ depth: depthOf([x + w / 2, y + d / 2, z + h / 2], p), shape, cls });
  };
  // Bench: four paired feet, base, guide rails, mounting screws and end stops.
  for (const x of [-98, 86]) {
    for (const y of [-61, 49]) {
      add([x, y, 12, 12, -14, 4, "li-foot", true]);
      add([x + 2, y + 2, 8, 8, -10, 5, "", true]);
    }
  }
  add([-110, -73, 220, 146, -5, 5, "li-base"]);
  for (const y of [-51, 47]) {
    add([-99, y, 198, 4, 0, 5, "li-rail"]);
    for (const x of [-99, 93]) {
      add([x, y - 2, 6, 8, 5, 5, "li-cap"]);
    }
  }
  for (const x of [-104, 100]) {
    for (const y of [-67, 63]) {
      add([x, y, 3, 3, 0, 1, "li-port", true]);
    }
  }
  // Twelve electrical terminals are paired on the original input/output sides.
  for (const x of [-103, 89]) {
    for (let i = 0; i < 6; i++) {
      add([x, -38 + i * 13, 14, 8, 0, 5, "li-cap"]);
      add([x + 4, -36 + i * 13, 5, 4, 5, 2, "li-port"]);
    }
  }
  const lift = apart ? 20 : 0;
  if (template === "session") {
    add([-61, -45, 122, 15, 0, 64, "li-cap"]);
    add([-53, -28, 106, 3, 20, 36, "li-drawer"]);
    add([-62, 4, 124, 38, 0, 5, "li-cap"]);
    for (let r = 0; r < 3; r++) {
      for (let c = 0; c < 10; c++) {
        add([-55 + c * 11, 8 + r * 10, 8, 7, 5, 3, "li-drawer"]);
      }
    }
  } else if (template === "carrier" || template === "outer") {
    add([-78, -40, 156, 80, 0, 8, "li-cap"]);
    for (let i = 0; i < 8; i++) {
      add([-70 + i * 19, -36, 12, 72, 8, 5, "li-rail"]);
    }
    for (let i = 0; i < 3; i++) {
      add([-62 + i * 48, -25, 38, 50, 13 + lift, 5, i === 1 ? "li-port" : "li-drawer"]);
      for (let j = 0; j < 3; j++) {
        add([-57 + i * 48, -18 + j * 12, 28, 2, 18 + lift, 0.7, "li-rail"]);
      }
    }
    if (template === "outer") {
      for (const x of [-80, 72]) {
        add([x, -43, 8, 86, 8, 42, "li-rail"]);
      }
      add([-80, -43, 160, 8, 50, 7, "li-cap"]);
      add([-22, -43, 44, 8, 57, 12, "li-drawer"]);
    }
  } else if (template === "selector" || template === "interlock") {
    for (const x of [-72, 60]) {
      add([x, -40, 12, 80, 0, 12, "li-cap"]);
    }
    for (const y of [-32, 28]) {
      add([-60, y, 120, 4, 8, 4, "li-rail"]);
    }
    add([-28, -35, 56, 70, 12 + lift, 7, "li-drawer"]);
    const count = template === "selector" ? 6 : 3;
    for (let i = 0; i < count; i++) {
      const x = -22 + i * (44 / count);
      add([x, -25, 5, 50, 19 + lift, 6, "li-cap"]);
      add([x, -12, 5, 14, 25 + lift, 14, "li-port"]);
    }
    add([60, -15, 12, 30, 12, 32, "li-rail"]);
  } else if (template === "inner" || template === "workcell") {
    // Gantry, vertical tool slide, isolated work holder and three feed jaws.
    for (const x of [-74, 62]) {
      add([x, -38, 12, 76, 0, 65, "li-rail"]);
    }
    add([-74, -38, 148, 18, 65 + lift, 12, "li-cap"]);
    add([-20, -34, 40, 14, 25 + lift, 40, "li-drawer"]);
    add([-6, -27, 12, 7, 15 + lift, 10, "li-port"]);
    add([-57, -13, 114, 55, 0, 9, "li-cap"]);
    for (const x of [-49, -9, 31]) {
      add([x, 2, 24, 24, 9, 6, "li-drawer"]);
    }
  } else if (template === "router") {
    // Crosspoint board: rows are ports, not fabricated runtime connections.
    add([-77, -43, 154, 86, 0, 10, "li-cap"]);
    for (let r = 0; r < 4; r++) {
      for (let c = 0; c < 5; c++) {
        add([-67 + c * 29, -33 + r * 21, 17, 12, 10 + lift, 6, "li-drawer"]);
        add([-63 + c * 29, -30 + r * 21, 9, 6, 16 + lift, 3, "li-port"]);
      }
    }
  } else if (template === "gauge" || template === "testbench") {
    add([-70, -40, 140, 80, 0, 8, "li-cap"]);
    for (const x of [-65, 53]) {
      add([x, -34, 12, 12, 8, 52, "li-rail"]);
    }
    add([-65, -34, 130, 12, 60 + lift, 8, "li-cap"]);
    for (const x of template === "gauge" ? [-34, 28] : [-46, -4, 38]) {
      add([x, -30, 8, 8, 28 + lift, 32, "li-drawer"]);
      add([x + 2, -28, 4, 4, 18 + lift, 10, "li-port"]);
      add([x - 6, -18, 20, 33, 8, 7, "li-cap"]);
    }
    add([-56, 26, 112, 8, 8, 9, "li-drawer"]);
  } else if (template === "trigger" || template === "clock") {
    add([-60, -40, 120, 80, 0, 12, "li-cap"]);
    if (template === "trigger") {
      add([-36, -18, 72, 36, 12, 14, "li-drawer"]);
      add([-6, -6, 12, 12, 26 + lift, 35, "li-rail"]);
      add([-24, -15, 48, 30, 61 + lift, 7, "li-port"]);
    } else {
      // Stepped program drum and discrete contact followers.
      for (let i = 0; i < 7; i++) {
        add([-49 + i * 14, -25, 10, 50, 12 + lift, 12 + (i % 3) * 9, "li-drawer"]);
        add([-47 + i * 14, 29, 6, 9, 12, 9, "li-port"]);
      }
    }
  } else if (template === "index" || template === "reader") {
    const shelves = template === "index" ? 5 : 1;
    for (const x of [-69, 61]) {
      add([x, -40, 8, 80, 0, 72, "li-cap"]);
    }
    for (let i = 0; i < shelves; i++) {
      add([-61, -38 + (apart ? i * 5 : 0), 122, 72, i * 13 + 5, 8, "li-drawer"]);
      add([-12, 32 + (apart ? i * 5 : 0), 24, 5, i * 13 + 7, 4, "li-port"]);
    }
    if (template === "reader") {
      add([-61, -38, 122, 10, 45, 10, "li-cap"]);
      add([-54, -26, 108, 48, 17 + lift, 3, "li-port"]);
    }
  } else if (template === "reservoir") {
    add([-69, -43, 138, 86, 0, 8, "li-cap"]);
    add([-62, -35, 55, 70, 8 + lift, 48, "li-drawer"]);
    for (let i = 0; i < 4; i++) {
      add([7, -33 + i * 18, 52, 10, 8, 12, "li-rail"]);
      add([39, -35 + i * 18, 16, 14, 20 + lift, 9, "li-port"]);
    }
  } else if (template === "dock") {
    for (const x of [-70, 30]) {
      add([x, -36, 40, 72, 0, 22, "li-cap"]);
      for (let i = 0; i < 5; i++) {
        add([x + 9, -28 + i * 13, 22, 7, 22 + lift, 7, "li-port"]);
      }
    }
    add([-30, -30, 60, 60, 0, 7, "li-rail"]);
  } else if (template === "meter" || template === "recorder") {
    add([-75, -44, 150, 88, 0, 9, "li-cap"]);
    for (let i = 0; i < 3; i++) {
      const x = -64 + i * 47;
      add([x, -32, 36, 36, 9 + lift, 12, "li-drawer"]);
      for (let j = 0; j < 5; j++) {
        add([x + 4 + j * 6, -25, 2, 22, 21 + lift, 1, "li-rail"]);
      }
      add([x + 14, -19, 6, 15, 22 + lift, 2, "li-port"]);
    }
    if (template === "recorder") {
      add([-60, 10, 120, 30, 9 + lift, 4, "li-port"]);
    }
  } else {
    add([-48, -28, 96, 56, 0, 5, "li-drawer"]);
  }

  parts.sort((a, b) => a.depth - b.depth);
  const paths = parts
    .map(
      ({ shape: s, cls }) =>
        `<g class="li-solid ${cls}"><path d="${s.fill}" class="li-fill"/>${s.shades.map((d, i) => `<path d="${d}" class="li-shade-${i}"/>`).join("")}<path d="${s.top}" class="li-top"/><path d="${s.outline}" class="li-outline"/><path d="${s.bevel || ""}" class="li-bevel"/></g>`,
    )
    .join("");
  return `<svg viewBox="0 0 650 410" role="img" aria-label="${machines[template][0]}" data-anatomy-template="${template}" data-parts="${parts.length}">${paths}</svg>`;
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
  let apart = false,
    zoom = 1;
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
