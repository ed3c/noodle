import { mechanismFigure } from "./loop-anatomy.mjs";

const names = {
  matched: "✓ 機械符合",
  deviated: "✕ 已證實偏離",
  unknown: "? 缺證據",
  not_applicable: "— 不適用",
};
const NS = "http://www.w3.org/2000/svg";
function el(tag, text, className = "") {
  const value = document.createElement(tag);
  if (text !== null) {
    value.textContent = text;
  }
  if (className) {
    value.className = className;
  }
  return value;
}
function svg(tag, attrs, text) {
  const value = document.createElementNS(NS, tag);
  for (const [key, item] of Object.entries(attrs)) {
    value.setAttribute(key, item);
  }
  if (text !== undefined) {
    value.textContent = text;
  }
  return value;
}
function button(text, fn) {
  const value = el("button", text);
  value.type = "button";
  value.addEventListener("click", fn);
  return value;
}
function activate(node, fn) {
  node.addEventListener("click", fn);
  node.addEventListener("keydown", (event) => {
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      fn();
    }
  });
}
function parse(artifact) {
  if (!artifact || !["available", "not_public"].includes(artifact.status)) {
    return null;
  }
  const content = artifact.public_content ?? artifact.content;
  if (typeof content !== "string") {
    return null;
  }
  try {
    return JSON.parse(content);
  } catch {
    return null;
  }
}

function unique(items) {
  return (
    items.every((v) => v && typeof v.id === "string") &&
    new Set(items.map((v) => v.id)).size === items.length
  );
}

export function comparisonModel(graph, runId) {
  const candidates = graph.artifacts
    .filter((a) => a.role === "loop_comparison")
    .map((a) => ({ artifact: a, value: parse(a) }))
    .filter((a) => a.value?.run_id === runId && a.value.version === 1);
  if (candidates.length !== 1) {
    return {
      status: "unknown",
      reason:
        candidates.length > 0
          ? "同一 Run 有多份比較；需原投影選定版本。"
          : "尚無原 Schema／LoopManager 比較投影；不從畫面猜預期。",
    };
  }
  const comparison = candidates[0].value;
  const artifact = graph.artifacts.find((a) => a.id === comparison.expectation_artifact_id);
  const expected = parse(artifact);
  if (!expected || expected.version !== 1 || expected.request_sha256 !== runId) {
    return { status: "unknown", reason: "預期來源尚未提供、不可讀或 Run 綁定不符。" };
  }
  const invalid = { status: "unknown", reason: "原比較投影的元件、路徑或引用不完整；保持未知。" };
  if (
    ![
      expected.steps,
      expected.components,
      expected.routes,
      expected.hypotheses,
      comparison.rows,
      comparison.semantic_review?.node_ids,
    ].every((items) => Array.isArray(items)) ||
    !comparison.step_matches ||
    !["pre_execution", "current_contract_review"].includes(expected.origin) ||
    !["current", "drift"].includes(comparison.source_status)
  ) {
    return invalid;
  }
  if (![expected.steps, expected.components, expected.routes].every((items) => unique(items))) {
    return invalid;
  }
  const componentIds = new Set(expected.components.map((c) => c.id));
  const stepIds = new Set(expected.steps.map((s) => s.id));
  if (
    expected.steps.some((s) => !componentIds.has(s.component) || typeof s.name !== "string") ||
    expected.routes.some(
      (r) => !stepIds.has(r.from) || !stepIds.has(r.to) || typeof r.applicable !== "boolean",
    ) ||
    comparison.rows.length !== expected.routes.length ||
    new Set(comparison.rows.map((r) => r?.route_id)).size !== expected.routes.length ||
    comparison.rows.some(
      (r) =>
        !r ||
        !expected.routes.some((v) => v.id === r.route_id) ||
        !Object.hasOwn(names, r.status) ||
        !Array.isArray(r.edge_ids),
    ) ||
    Object.entries(comparison.step_matches).some(
      ([step, ids]) =>
        !stepIds.has(step) ||
        !Array.isArray(ids) ||
        ids.some((id) => !graph.nodes?.some((n) => n.id === id && n.run_id === runId)),
    )
  ) {
    return invalid;
  }
  return { status: "available", expected, comparison, artifact };
}

/** Load only original comparison references attached to this Run, then its expectation. */
export async function loadComparison(graph, runId, readArtifact) {
  const refs = new Set(
    graph.nodes.filter((n) => n.run_id === runId).flatMap((n) => n.evidence_artifact_ids),
  );
  const contents = {};
  await Promise.all(
    graph.artifacts
      .filter((a) => a.role === "loop_comparison" && refs.has(a.id))
      .map(async (artifact) => {
        contents[artifact.id] = await readArtifact(artifact.id);
        const comparison = parse({ ...artifact, content: contents[artifact.id] });
        if (
          comparison?.run_id === runId &&
          graph.artifacts.some(
            (a) => a.id === comparison.expectation_artifact_id && a.role === "loop_expectation",
          )
        ) {
          contents[comparison.expectation_artifact_id] = await readArtifact(
            comparison.expectation_artifact_id,
          );
        }
      }),
  );
  return contents;
}

/** Two representations consume the same original projection; clicks never dispatch work. */
export function mountComparison(
  { panel, review },
  { graph, runId, pick, memory },
  { onSelect, onOriginal, readArtifact, onModel, unified = false },
) {
  panel.replaceChildren();
  review.replaceChildren();
  panel.append(el("h3", "契約預期與可推翻假說 / 原來源讀回"));
  review.append(el("h3", "Workspace / 預期 ↔ 實際交接審查"));
  const refs = new Set(
    graph.nodes.filter((n) => n.run_id === runId).flatMap((n) => n.evidence_artifact_ids),
  );
  const version = graph.artifacts
    .filter((a) => a.role === "loop_comparison" && refs.has(a.id))
    .map((a) => a.id + a.status)
    .toSorted()
    .join("|");
  if (memory.version !== version) {
    delete memory.contents;
    delete memory.loaded;
    delete memory.pending;
    delete memory.error;
    memory.version = version;
  }
  const ticket = Symbol("comparison-readback");
  panel.comparisonTicket = ticket;
  const hydrated = {
    ...graph,
    artifacts: graph.artifacts.map((a) =>
      memory.contents?.[a.id] ? { ...a, content: memory.contents[a.id] } : a,
    ),
  };
  const model = comparisonModel(hydrated, runId);
  onModel?.(model);
  if (model.status !== "available") {
    panel.append(el("p", runId ? model.reason : "選擇一個 Run，與同一版本的預期對照。"));
    review.append(el("p", "缺少原投影時，實際 DAG 仍可讀取；未知不視為偏離。"));
    if (memory.error) {
      panel.append(el("p", memory.error));
    }
    if (runId && readArtifact && !memory.loaded) {
      panel.append(el("p", "按需讀取原比較與預期…"));
      memory.pending ??= loadComparison(graph, runId, readArtifact)
        .then((contents) => {
          if (memory.version === version) {
            memory.contents = contents;
            memory.loaded = true;
          }
          return contents;
        })
        .catch((error) => {
          if (memory.version === version) {
            memory.error = String(error.message || error);
            memory.loaded = true;
          }
          return null;
        });
      return memory.pending.then(() => {
        if (panel.comparisonTicket === ticket) {
          rerender();
        }
        return null;
      });
    }
    return;
  }
  const { expected, comparison } = model;
  const steps = new Map(expected.steps.map((s) => [s.id, s]));
  const components = new Map(expected.components.map((c) => [c.id, c]));
  const objects = new Map(graph.objects.map((o) => [o.namespace, o]));
  panel.append(
    el(
      "p",
      expected.origin === "pre_execution" ? "執行前固定版本" : "依目前契約回顧 · 非事前預測",
      "lc-origin",
    ),
    el("p", `本次模式：${expected.mode}`),
    el(
      "p",
      `預期來源：${comparison.source_status === "current" ? "核對一致" : "來源已漂移，保留原版本"}`,
    ),
    button("讀取預期原內容", () => onOriginal({ kind: "artifact", id: model.artifact.id })),
  );
  const goal = el("details", null);
  goal.append(el("summary", "完整原 Goal／需求"), el("p", expected.goal));
  panel.append(goal);
  let chosenObject = null;
  if (pick?.kind === "object") {
    chosenObject = graph.objects.find((o) => o.id === pick.id);
  }
  if (pick?.kind === "node") {
    chosenObject = graph.objects.find(
      (o) => o.id === graph.nodes.find((n) => n.id === pick.id)?.object_id,
    );
  }
  const routes = expected.routes.filter((r) => r.applicable);
  const activeSteps = expected.steps.filter((s) =>
    routes.some((r) => [r.from, r.to].includes(s.id)),
  );
  const positions = new Map(
    activeSteps.map((s, i) => [s.id, { x: (i % 2) * 230 + 15, y: Math.floor(i / 2) * 190 + 20 }]),
  );
  const drawing = svg("svg", {
    viewBox: `0 0 470 ${Math.ceil(activeSteps.length / 2) * 190 + 20}`,
    role: "group",
    "aria-label": "預期機構資料流 DAG",
  });
  for (const route of routes) {
    const a = positions.get(route.from),
      b = positions.get(route.to);
    const row = comparison.rows.find((item) => item.route_id === route.id);
    const line = svg("g", {
      role: "button",
      tabindex: 0,
      "aria-label": `預期交接 · ${route.transformation}`,
      class: `lc-route lc-${row?.status || "unknown"}`,
    });
    const horizontal = a.y === b.y;
    const start = horizontal ? [a.x + 205, a.y + 75] : [a.x + 102, a.y + 150];
    const end = horizontal ? [b.x, b.y + 75] : [b.x + 102, b.y];
    const lane = b.y > a.y ? b.y - 20 : 5;
    let path = `M${start} v12 H5 V${end[1] - 12} H${end[0]} V${end[1]}`;
    if (horizontal) {
      path = `M${start} L${end}`;
    } else if (b.y > a.y) {
      path = `M${start} V${lane} H${end[0]} V${end[1]}`;
    }
    line.append(
      svg("path", { d: path, fill: "none", stroke: "transparent", "stroke-width": 12 }),
      svg("path", {
        d: path,
        fill: "none",
        stroke: "currentColor",
        "stroke-width": memory.route === route.id ? 4 : 2,
        "stroke-dasharray": "5 4",
      }),
      svg(
        "text",
        { x: end[0] - 4, y: end[1] + (horizontal ? 4 : 0), fill: "currentColor", "font-size": 10 },
        horizontal ? "▶" : "▼",
      ),
    );
    line.append(svg("title", {}, `${route.transformation} · ${names[row?.status] || "未知"}`));
    activate(line, () => chooseRoute(route.id));
    drawing.append(line);
  }
  for (const step of activeSteps) {
    const c = components.get(step.component),
      { x, y } = positions.get(step.id);
    const related = !chosenObject || c.owner === chosenObject.namespace;
    const card = svg("g", {
      role: "button",
      tabindex: 0,
      "aria-label": `預期節點 · ${step.name}`,
      class: `lc-step${related ? "" : " lc-dim"}`,
    });
    card.append(
      svg("rect", {
        x,
        y,
        width: 205,
        height: 150,
        rx: 8,
        fill: "#fffdf6",
        stroke: memory.step === step.id ? "#96763a" : "#9eaea6",
        "stroke-width": memory.step === step.id ? 3 : 1,
      }),
    );
    const art = svg("svg", { x: x + 30, y: y + 22, width: 145, height: 94 });
    art.innerHTML = mechanismFigure(step.mechanism || c.function);
    card.append(art);
    card.append(svg("text", { x: x + 8, y: y + 17, fill: "#253a42", "font-size": 11 }, step.name));
    card.append(svg("text", { x: x + 8, y: y + 130, fill: "#36565b", "font-size": 9 }, c.name));
    card.append(svg("text", { x: x + 8, y: y + 144, fill: "#36565b", "font-size": 9 }, step.role));
    activate(card, () => {
      memory.step = step.id;
      memory.route = null;
      memory.component = c.id;
      const actual = comparison.step_matches[step.id]?.find((id) =>
        graph.nodes.some((n) => n.id === id),
      );
      const owner = objects.get(c.owner);
      if (actual) {
        onSelect({ kind: "node", id: actual });
      } else if (owner) {
        onSelect({ kind: "object", id: owner.id });
      } else {
        rerender();
      }
    });
    drawing.append(card);
  }
  if (!unified) {
    panel.append(drawing);
  }
  const index = el("details", null);
  index.append(el("summary", `具名功能元件索引 · ${expected.components.length}`));
  for (const c of expected.components) {
    index.append(
      button(c.name, () => {
        memory.component = c.id;
        const owner = objects.get(c.owner);
        if (owner) {
          onSelect({ kind: "object", id: owner.id });
        } else {
          rerender();
        }
      }),
    );
  }
  panel.append(index);
  const selectedComponent =
    components.get(memory.component) || components.get(steps.get(memory.step)?.component);
  if (selectedComponent) {
    const c = selectedComponent;
    panel.append(
      el("h4", c.name),
      el("p", `輸入：${c.input} → 輸出：${c.output}`),
      el("p", `原入口：${c.entry}`),
      el("p", `讀回：${c.readback}`),
      el("p", `拒絕：${c.refusal}`),
      el("p", `來源：${c.source}`),
      el("p", "這是功能契約；沒有本次 invocation 就不宣稱元件已執行。"),
    );
  }
  const hypotheses = el("details", null);
  hypotheses.append(el("summary", `Session 假說／未決前提 · ${expected.hypotheses.length}`));
  for (const item of expected.hypotheses) {
    hypotheses.append(el("pre", typeof item === "string" ? item : JSON.stringify(item, null, 2)));
  }
  hypotheses.append(el("p", "原 Schema 宣告不是契約要求；人的採納不等於假說已驗證。"));
  panel.append(hypotheses);
  const harness = el("section", null, "lc-harness");
  harness.setAttribute("aria-label", "Harness 工具化缺口");
  harness.append(el("h4", "Harness：從敘述到工具"));
  const categories = {
    session_judgment: "需要 Session 語義判斷",
    tool_unwired: "已有工具但未接線",
    capability_missing: "缺少確定性能力",
    evidence_missing: "只有證據不足",
  };
  harness.append(
    el("p", "四類判斷分開保留。缺觀察不能推成缺工具；工具已註冊不代表已載入或已執行。"),
  );
  for (const row of comparison.rows.filter((r) => r.status === "unknown")) {
    harness.append(button(`證據不足 · ${row.route_id}`, () => chooseRoute(row.route_id)));
  }
  for (const record of comparison.harness_reviews || []) {
    const { assessment } = record;
    if (!assessment || !categories[assessment.category]) {
      continue;
    }
    const detail = el("details", null);
    detail.append(
      el("summary", `${categories[assessment.category]} · Session 提交／未認證`),
      el("p", assessment.claim),
      el("p", `前提：${assessment.premise}`),
      el("p", `推翻條件：${assessment.falsifier}`),
      el("p", `原工具：${assessment.tool || "未指定；不猜測命令"}`),
      button("檢視原判斷與來源版本", () =>
        onOriginal({ kind: "artifact", id: record.artifact_id }),
      ),
    );
    harness.append(detail);
  }
  review.append(harness);
  const table = el("table", null);
  const head = el("tr", null);
  for (const label of ["契約／型態轉換", "實際觀察", "機械對照"]) {
    head.append(el("th", label));
  }
  const thead = el("thead", null);
  thead.append(head);
  table.append(thead);
  const tbody = el("tbody", null);
  for (const route of expected.routes) {
    const row = comparison.rows.find((r) => r.route_id === route.id);
    const tr = el("tr", null, memory.route === route.id ? "lc-selected" : "");
    const expectedCell = el("td", null);
    expectedCell.append(
      button(`${steps.get(route.from).name} → ${steps.get(route.to).name}`, () =>
        chooseRoute(route.id),
      ),
      el("p", route.transformation),
      el("small", route.condition),
    );
    const actual = el("td", null);
    for (const id of row.edge_ids) {
      const edge = graph.edges.find((e) => e.id === id);
      if (edge) {
        actual.append(
          button(edge.label, () => onSelect({ kind: "edge", id })),
          button("同窗比較原內容", () => onOriginal({ kind: "edge", id })),
        );
      }
    }
    if (row.edge_ids.length === 0) {
      actual.append(el("p", "尚無對應的原交接證據"));
    }
    const status = el("td", null, `lc-${row.status}`);
    status.append(el("strong", names[row.status]), el("p", row.reason));
    tr.append(expectedCell, actual, status);
    tbody.append(tr);
  }
  table.append(tbody);
  review.append(table);
  if (memory.step) {
    review.append(el("h4", `節點對照 · ${steps.get(memory.step)?.name || memory.step}`));
    for (const id of comparison.step_matches[memory.step] || []) {
      const node = graph.nodes.find((n) => n.id === id);
      if (node) {
        review.append(
          button(`${node.label} · ${node.status.execution}`, () => onSelect({ kind: "node", id })),
        );
      }
    }
  }
  review.append(el("h4", "語義審查 · 獨立於機械對照"), el("p", comparison.semantic_review.reason));
  for (const id of comparison.semantic_review.node_ids) {
    review.append(button("讀取原語義／行為審查", () => onOriginal({ kind: "node", id })));
  }
  const delta = el("details", null);
  delta.append(
    el("summary", "原 Prompt Delta"),
    el("pre", expected.prompt_delta ?? "未提供；不由展示補寫。"),
  );
  review.append(delta);
  function chooseRoute(id) {
    memory.route = id;
    memory.step = null;
    const actual = comparison.rows
      .find((r) => r.route_id === id)
      ?.edge_ids.find((edgeId) => graph.edges.some((e) => e.id === edgeId));
    if (actual) {
      onSelect({ kind: "edge", id: actual });
    } else {
      rerender();
    }
  }
  function rerender() {
    mountComparison(
      { panel, review },
      { graph, runId, pick, memory },
      {
        onSelect,
        onOriginal,
        readArtifact,
        onModel,
        unified,
      },
    );
  }
}
