import { slabOf, cylinder, boxCorners, fitProjection, depthOf } from "./anatomy/iso-kit.mjs";

const machines = {
  loop: ["整體機械 · 閉環輸送台", "既有交接是輸送路徑；回饋與驗收依原證據分開判讀。"],
  manager: ["功能組件 · 接口配線台", "每個插座對應原接口；同一物件不因被多個迴圈引用而複製。"],
  session: ["Session · 判斷操作台", "輸入、已提交的判斷與原輸出分開；未記錄的內部推論不補畫。"],
  prompt: ["Prompt · 載體轉換台", "兩端保存原資料型態；中間只展示已有交接，不宣稱語義等價。"],
  pclass: ["P-class · 條件選擇機構", "條件、斷言與原續行由原來源展開；載入不等於已遵循。"],
  cli: ["CLI · 轉移導軌", "原輸入經已記錄的條件走向輸出或拒絕；圖面不觸發轉移。"],
  observation: ["觀察 · 證據讀數台", "讀數只對應這筆來源的狀態；沒有量測就保留 UNKNOWN。"],
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
  let template = value.kind;
  if (object) {
    template = ["soodles.outer", "noodle.inner"].includes(value.namespace) ? "loop" : "manager";
  } else if (
    value.kind === "observation" &&
    [...inputs, ...outputs].some((a) => /prompt|input/.test(a.role))
  ) {
    template = "prompt";
  }
  const peers = new Set(
    [...new Set(edges.flatMap((e) => [e.from_node_id, e.to_node_id]))]
      .map((id) => graph.nodes.find((n) => n.id === id)?.object_id)
      .filter((id) => id && id !== (object ? value.id : value.object_id)),
  );
  return {
    value,
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
export function mechanismFigure(template, { apart = false } = {}) {
  const p = fitProjection(boxCorners({ x: -112, y: -75, w: 224, d: 150 }, -14, 110), 650, 410, {
    pad: 26,
    azimuth: 42,
    elevation: 35,
  });
  const parts = [];
  const add = ([x, y, w, d, z, h, cls = "", round = false]) => {
    const shape = round
      ? cylinder(x + w / 2, y + d / 2, w / 2, z, h, p, 28)
      : slabOf({ x, y, w, d, r: 1.5 }, z, h, p, 4, 0.6);
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
  } else if (template === "prompt" || template === "loop") {
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
    if (template === "loop") {
      for (const x of [-80, 72]) {
        add([x, -43, 8, 86, 8, 42, "li-rail"]);
      }
      add([-80, -43, 160, 8, 50, 7, "li-cap"]);
      add([-22, -43, 44, 8, 57, 12, "li-drawer"]);
    }
  } else if (template === "pclass" || template === "cli") {
    for (const x of [-72, 60]) {
      add([x, -40, 12, 80, 0, 12, "li-cap"]);
    }
    for (const y of [-32, 28]) {
      add([-60, y, 120, 4, 8, 4, "li-rail"]);
    }
    add([-28, -35, 56, 70, 12 + lift, 7, "li-drawer"]);
    const count = template === "pclass" ? 6 : 3;
    for (let i = 0; i < count; i++) {
      const x = -22 + i * (44 / count);
      add([x, -25, 5, 50, 19 + lift, 6, "li-cap"]);
      add([x, -12, 5, 14, 25 + lift, 14, "li-port"]);
    }
    add([60, -15, 12, 30, 12, 32, "li-rail"]);
  } else {
    add([-75, -44, 150, 88, 0, 9, "li-cap"]);
    for (let i = 0; i < 3; i++) {
      const x = -64 + i * 47;
      add([x, -32, 36, 36, 9 + lift, 9, "li-drawer", true]);
      add([x + 14, -22, 6, 18, 18 + lift, 1, "li-port"]);
      add([x + 8, 18, 20, 15, 9, 5, "li-drawer"]);
      for (let j = 0; j < 4; j++) {
        add([x + j * 8, 35, 4, 4, 9, 2, "li-port", true]);
      }
    }
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
  root.append(el("h4", model.title));
  const art = el("div", "");
  art.className = "li-anatomy-art";
  art.innerHTML = mechanismFigure(model.template);
  let apart = false,
    zoom = 1;
  const controls = el("div", "");
  controls.className = "li-toolbar";
  controls.append(
    button("拆解／組裝機構", () => {
      apart = !apart;
      art.innerHTML = mechanismFigure(model.template, { apart });
    }),
    button("詳圖 1×／4×", () => {
      zoom = zoom === 1 ? 4 : 1;
      art.style.setProperty("--anatomy-zoom", zoom);
    }),
  );
  root.append(
    art,
    controls,
    el("p", model.description),
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
