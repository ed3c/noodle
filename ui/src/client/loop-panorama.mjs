import { layoutGraph } from "./loop-graph-layout.mjs";
import { mountIsometric } from "./loop-isometric.mjs";

/** Read-only quotient of the original occurrence DAG. Never creates runtime edges. */
export function projectPanorama(graph, runIds = null) {
  const selected = runIds === null ? new Set(graph.nodes.map((n) => n.run_id)) : new Set(runIds);
  const edges = graph.edges.filter((e) => selected.has(e.run_id));
  const endpoints = new Set(edges.flatMap((e) => [e.from_node_id, e.to_node_id]));
  const nodes = graph.nodes.filter((n) => selected.has(n.run_id) || endpoints.has(n.id));
  const nodeMap = new Map(nodes.map((n) => [n.id, n]));
  const artifacts = new Map(graph.artifacts.map((a) => [a.id, a]));
  const present = (ids) =>
    ids.length > 0 &&
    ids.every((id) => ["available", "not_public"].includes(artifacts.get(id)?.status));
  const recorded = (e) =>
    e.kind === "handoff" &&
    e.status === "observed" &&
    e.current_validity !== "stale" &&
    present(e.evidence_artifact_ids) &&
    present(e.input_artifact_ids) &&
    present(e.output_artifact_ids) &&
    [e.from_node_id, e.to_node_id].every((id) => nodeMap.get(id)?.identity.status === "observed");
  const observed = (e) => recorded(e) && e.current_validity === "current";
  const relations = new Map();
  for (const edge of edges) {
    if (edge.kind !== "handoff") {
      continue;
    }
    const from = nodeMap.get(edge.from_node_id),
      to = nodeMap.get(edge.to_node_id);
    if (!from || !to) {
      throw new Error("Panorama endpoint missing");
    }
    const key = JSON.stringify([from.object_id, to.object_id]);
    if (!relations.has(key)) {
      relations.set(key, {
        id: key,
        from: from.object_id,
        to: to.object_id,
        edge_ids: [],
        current: 0,
      });
    }
    const relation = relations.get(key);
    relation.edge_ids.push(edge.id);
    if (observed(edge)) {
      relation.current++;
    }
  }
  const rows = [...selected].toSorted().map((run_id) => {
    const ownNodes = nodes.filter((n) => n.run_id === run_id);
    const ownEdges = edges.filter((e) => e.run_id === run_id);
    const current = ownEdges.filter((edge) => observed(edge));
    const consumed = current.filter((e) => {
      const target = nodeMap.get(e.to_node_id);
      return (
        target.status.transfer === "output_reviewed" &&
        target.status.acceptance === "original_scoped_review_approved"
      );
    });
    // The original producer only attaches this set after candidate/feedback/ACK/attempt checks.
    const feedback = ownEdges.filter((e) => {
      if (!recorded(e)) {
        return false;
      }
      const from = nodeMap.get(e.from_node_id),
        to = nodeMap.get(e.to_node_id);
      if (
        graph.objects.find((o) => o.id === from.object_id)?.namespace !== "soodles.loop" ||
        graph.objects.find((o) => o.id === to.object_id)?.namespace !== "noodle.inner" ||
        to.kind !== "cli"
      ) {
        return false;
      }
      const roles = new Set(e.evidence_artifact_ids.map((id) => artifacts.get(id)?.role));
      return ["review_candidate", "review_feedback", "control_ack", "native_spawn"].every((role) =>
        roles.has(role),
      );
    });
    const scopedReviews = ownNodes.filter(
      (n) =>
        n.identity.status === "observed" &&
        n.status.acceptance === "original_scoped_review_approved" &&
        present(n.evidence_artifact_ids),
    );
    return {
      run_id,
      node_ids: ownNodes.map((n) => n.id),
      edge_ids: ownEdges.map((e) => e.id),
      repository_ids: [...new Set(ownNodes.map((n) => n.repository_id))],
      kinds: [
        ...new Set(
          ownNodes
            .map((n) => graph.objects.find((o) => o.id === n.object_id)?.namespace)
            .filter((ns) => ns === "soodles.outer" || ns === "noodle.inner"),
        ),
      ],
      cross_repository: ownEdges.some((e) => e.scope === "cross_repository"),
      consumption: {
        status: consumed.length > 0 ? "observed" : "unknown",
        edge_ids: consumed.map((e) => e.id),
      },
      feedback: {
        status: feedback.length > 0 ? "observed" : "unknown",
        edge_ids: feedback.map((e) => e.id),
        current: feedback.length > 0 && feedback.every((edge) => observed(edge)),
      },
      goal: { status: "unknown", node_ids: scopedReviews.map((n) => n.id) },
      findings: graph.findings.filter((f) => f.run_id === run_id),
    };
  });
  return {
    nodes,
    edges,
    relations: [...relations.values()].toSorted((a, b) => a.id.localeCompare(b.id)),
    rows,
    context_run_ids: [
      ...new Set(nodes.filter((n) => !selected.has(n.run_id)).map((n) => n.run_id)),
    ],
  };
}

/** Presentation groups preserve native identity and never mint Session/P-class occurrences. */
export function projectRunDetail(graph, runId) {
  const scope = projectPanorama(graph, [runId]);
  const sessions = new Map();
  for (const node of scope.nodes) {
    const { identity } = node;
    const session = identity.status === "observed" ? identity.session_id : null;
    const key = JSON.stringify([node.run_id, node.repository_id, session || null]);
    if (!sessions.has(key)) {
      sessions.set(key, {
        run_id: node.run_id,
        repository_id: node.repository_id,
        session_id: session || null,
        nodes: [],
      });
    }
    sessions.get(key).nodes.push(node);
  }
  const references = new Map();
  for (const node of scope.nodes) {
    for (const id of [
      ...node.input_artifact_ids,
      ...node.output_artifact_ids,
      ...node.evidence_artifact_ids,
    ]) {
      if (!references.has(id)) {
        references.set(id, []);
      }
      references.get(id).push(node.id);
    }
  }
  return {
    ...scope,
    sessions: [...sessions.values()],
    pclass_nodes: scope.nodes.filter((n) => n.kind === "pclass"),
    instructions: graph.artifacts
      .filter((a) => a.role === "instruction" && references.has(a.id))
      .map((artifact) => ({ artifact, node_ids: [...new Set(references.get(artifact.id))] })),
  };
}

/** Resolve only a source-bound JSON pointer from already verified artifact content. */
export function readStructuredField(text, locator, sourcePath) {
  if (!locator || !sourcePath || !locator.startsWith(`${sourcePath}#/`)) {
    return null;
  }
  const pointer = locator.slice(sourcePath.length + 1);
  try {
    let value = JSON.parse(text);
    for (const token of pointer.slice(1).split("/")) {
      if (/~(?![01])/u.test(token)) {
        return { pointer, status: "unknown", text: "無效 JSON pointer" };
      }
      const key = token.replaceAll("~1", "/").replaceAll("~0", "~");
      if (value === null || typeof value !== "object" || !Object.hasOwn(value, key)) {
        return { pointer, status: "unknown", text: "原來源未提供此欄位" };
      }
      value = value[key];
    }
    return { pointer, status: "available", text: JSON.stringify(value, null, 2) };
  } catch {
    return { pointer, status: "unknown", text: "原來源不是可解析 JSON" };
  }
}

/** Source-only guidance is browseable without masquerading as runtime execution. */
export function projectDetailLayer(detail, includeSources = false) {
  const endpoints = new Set(
    detail.edges.filter((e) => e.kind === "handoff").flatMap((e) => [e.from_node_id, e.to_node_id]),
  );
  const nodes = includeSources
    ? detail.nodes
    : detail.nodes.filter(
        (n) =>
          endpoints.has(n.id) || !["available_reference", "selected"].includes(n.status.execution),
      );
  const ids = new Set(nodes.map((n) => n.id));
  return {
    nodes,
    edges: detail.edges.filter((e) => ids.has(e.from_node_id) && ids.has(e.to_node_id)),
  };
}

// View preferences only: no runtime state, evidence, or acceptance is stored here.
const views = new WeakMap();
const ns = "http://www.w3.org/2000/svg";
function el(tag, text, attrs = {}) {
  return decorate(document.createElement(tag), text, attrs);
}
function svgEl(tag, text, attrs = {}) {
  return decorate(document.createElementNS(ns, tag), text, attrs);
}
function decorate(node, text, attrs) {
  if (text !== null && text !== undefined) {
    node.textContent = text;
  }
  for (const [key, value] of Object.entries(attrs)) {
    node.setAttribute(key, String(value));
  }
  return node;
}
function button(text, action, attrs = {}) {
  const b = el("button", text, { type: "button", ...attrs });
  b.addEventListener("click", action);
  return b;
}
function activate(node, action) {
  node.addEventListener("click", action);
  node.addEventListener("keydown", (event) => {
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      action();
    }
  });
}
export function mountPanorama(
  root,
  {
    graph,
    runs,
    runIds = null,
    goal = "",
    onScope,
    onSelect: notifySelection,
    onFocus,
    onReset,
    readArtifact,
  },
) {
  const model = projectPanorama(graph, runIds ?? runs.map((r) => r.id));
  const scopeKey = JSON.stringify([goal, runIds]);
  const cachedView = views.get(root);
  const view =
    cachedView?.scope === scopeKey
      ? cachedView
      : { scope: scopeKey, run: null, selection: null, sources: false };
  views.set(root, view);
  const onSelect = (value) => {
    setSelection(value);
    notifySelection(value);
  };
  root.replaceChildren();
  root.classList.add("lp-panel");
  const header = el("header"),
    pageTitle = el("h2", goal || "全機總覽 · 尚未選定行為目標");
  header.append(
    el("p", "WORKSPACE / LOOP PANORAMA", { class: "lp-eyebrow" }),
    pageTitle,
    el(
      "p",
      "先找閉環缺口，再沿原交接深入 Session、P-class 與提示詞。每層引用同一來源；圖上有環不代表驗收完成。",
    ),
  );
  root.append(header);
  const scope = el("details", null, { class: "lp-scope" });
  scope.append(el("summary", `工作範圍 · ${runIds === null ? "全部" : runIds.length} Runs`));
  const label = el("label", "行為目標 ");
  const input = el("input", null, { "aria-label": "Workspace 行為目標", maxlength: 240 });
  input.value = goal;
  label.append(input);
  scope.append(label);
  const checkboxes = [];
  for (const run of runs) {
    const row = el("label", null, { class: "lp-run-choice" });
    const box = el("input", null, { type: "checkbox", value: run.id });
    box.checked = runIds === null || runIds.includes(run.id);
    checkboxes.push(box);
    row.append(box, el("span", `${run.repository} · ${run.mode} · ${run.id.slice(0, 12)}`));
    scope.append(row);
  }
  const error = el("p", "", { "aria-live": "polite" });
  scope.append(
    button("套用工作範圍", () => {
      const ids = checkboxes.filter((b) => b.checked).map((b) => b.value);
      if (!input.value.trim() || ids.length === 0) {
        error.textContent = "請命名行為目標並選取至少一個 Run。";
        return;
      }
      onScope({ goal: input.value.trim(), runIds: ids });
    }),
    button("全機總覽", () => onScope({ goal: "", runIds: null })),
    error,
  );
  root.append(scope);
  const consoleRoot = el("section", null, { "aria-label": "立體工程全景" });
  root.append(consoleRoot);
  const consoleView = mountIsometric(consoleRoot, {
    graph,
    readArtifact,
    scope: model,
    runs,
    onSelect,
    onFocus,
    onRun: (id) => {
      if (id) {
        openRun(id, { focus: true });
      } else {
        view.run = null;
        view.selection = null;
        detailPanel.open = false;
        updatePosition();
        onReset?.();
      }
    },
    onInspect: (value) => setSelection(value),
    renderRunCard: (row, expand) => renderRow(row, expand),
    view,
  });
  if (model.context_run_ids.length > 0) {
    root.append(
      el(
        "p",
        `另保留 ${model.context_run_ids.length} 個跨界來源 Run 作交接上下文；不納入此 Goal 驗收。`,
        { class: "lp-note" },
      ),
    );
  }
  const journey = el("section", null, { class: "lp-journey", "aria-label": "閉環路徑與缺口" });
  journey.append(el("h3", "01 / Goal 的閉環路徑與缺口"));
  const paths = el("div", null, { class: "lp-paths" });
  journey.append(paths);
  const breadcrumb = el("nav", null, {
    class: "lp-breadcrumb",
    "aria-label": "全景定位",
    "aria-live": "polite",
  });
  consoleView.setJourney(journey);
  root.append(breadcrumb);
  const architecture = el("details", null, { class: "lp-architecture" });
  architecture.append(el("summary", "責任物件與跨 Repo 關係總覽"));
  root.append(architecture);
  const objects = [...graph.objects].toSorted((a, b) => a.namespace.localeCompare(b.namespace));
  const width = Math.max(900, objects.length * 175 + 40);
  const height = 320;
  const scroll = el("div", null, { class: "lp-map" });
  const drawing = svgEl("svg", null, {
    viewBox: `0 0 ${width} ${height}`,
    role: "group",
    "aria-label": "唯一物件全景關係圖",
  });
  const defs = svgEl("defs", null, {}),
    marker = svgEl("marker", null, {
      id: "lp-arrow",
      viewBox: "0 0 10 10",
      refX: 9,
      refY: 5,
      markerWidth: 6,
      markerHeight: 6,
      orient: "auto",
    });
  marker.append(svgEl("path", null, { d: "M0 0 L10 5 L0 10z", fill: "currentColor" }));
  defs.append(marker);
  drawing.append(defs);
  const xs = new Map(objects.map((o, i) => [o.id, 30 + i * 175]));
  for (const [i, r] of model.relations.entries()) {
    const x1 = xs.get(r.from) + 75,
      x2 = xs.get(r.to) + 75;
    const arcY = 40 + (i % 5) * 24;
    const d =
      r.from === r.to
        ? `M${x1 - 24} 170 C${x1 - 120} ${arcY},${x1 + 120} ${arcY},${x1 + 24} 170`
        : `M${x1} 170 C${x1} ${arcY},${x2} ${arcY},${x2} 170`;
    const g = svgEl("g", null, {
      class: `lp-relation ${r.current === r.edge_ids.length ? "lp-observed" : "lp-unknown"}`,
      role: "button",
      tabindex: 0,
      "aria-label": `${objects.find((o) => o.id === r.from)?.label} → ${objects.find((o) => o.id === r.to)?.label} · ${r.edge_ids.length} 次交接`,
      "data-panorama-relation": r.id,
    });
    g.append(svgEl("path", null, { d, "marker-end": "url(#lp-arrow)" }));
    g.append(
      svgEl("text", `${r.current}/${r.edge_ids.length} 目前可證實`, {
        x: (x1 + x2) / 2,
        y: arcY + 50,
        "text-anchor": "middle",
      }),
    );
    activate(g, () => {
      const panel = relationPanels.get(r.id);
      panel.open = true;
      panel.scrollIntoView({ block: "nearest" });
    });
    drawing.append(g);
  }
  for (const o of objects) {
    const nodes = model.nodes.filter((n) => n.object_id === o.id),
      x = xs.get(o.id);
    const g = svgEl("g", null, {
      transform: `translate(${x},170)`,
      class: "lp-object",
      role: "button",
      tabindex: 0,
      "aria-label": o.label,
      "data-panorama-object": o.id,
    });
    g.append(
      svgEl("rect", null, { width: 150, height: 85, rx: 12 }),
      svgEl("text", o.label, { x: 12, y: 28 }),
      svgEl("text", `${nodes.length} 筆來源／執行節點`, { x: 12, y: 51 }),
    );
    activate(g, () => {
      onFocus({ node_ids: nodes.map((n) => n.id), edge_ids: [] });
      onSelect({ kind: "object", id: o.id });
    });
    drawing.append(g);
  }
  scroll.append(drawing);
  architecture.append(scroll);
  const drill = el("section", null, { "aria-label": "全景關係展開", class: "lp-drill" }),
    relationPanels = new Map();
  architecture.append(drill);
  drill.append(el("h3", "關係與原始交接"));
  for (const r of model.relations) {
    const detail = el("details");
    relationPanels.set(r.id, detail);
    const name = `${objects.find((o) => o.id === r.from)?.label} → ${objects.find((o) => o.id === r.to)?.label}`;
    detail.append(
      el("summary", `${name} · ${r.edge_ids.length} 次交接`),
      button("展開這組關係 DAG", () => onFocus({ node_ids: [], edge_ids: r.edge_ids })),
    );
    for (const id of r.edge_ids) {
      const e = graph.edges.find((edge) => edge.id === id);
      detail.append(
        button(`${e.label} · ${e.status}/${e.current_validity}`, () =>
          onSelect({ kind: "edge", id }),
        ),
      );
    }
    drill.append(detail);
  }
  if (model.relations.length === 0) {
    drill.append(el("p", "此範圍尚無已登記交接；不補造連線。"));
  }
  const detailPanel = el("details", null, {
    class: "lp-detail",
    "aria-label": "Session 與 P-class 細部全景",
  });
  detailPanel.append(el("summary", "深入 Session、提示詞型態與全部 P-class 節點"));
  const chooser = el("select", null, { "aria-label": "細部 Run" });
  for (const row of model.rows) {
    const run = runs.find((r) => r.id === row.run_id);
    chooser.append(
      el("option", `${run?.repository || "未知 Repo"} · ${row.run_id.slice(0, 12)}`, {
        value: row.run_id,
      }),
    );
  }
  const detailBody = el("div");
  const sourceLabel = el("label", "同時顯示可用／已選指引（來源層） ");
  const sourceToggle = el("input", null, { type: "checkbox", "aria-label": "顯示來源層" });
  sourceToggle.checked = view.sources;
  sourceLabel.append(sourceToggle);
  detailPanel.append(chooser, sourceLabel, detailBody);
  let renderedRun = null;
  const artifacts = new Map(graph.artifacts.map((a) => [a.id, a]));
  const types = (ids) =>
    [
      ...new Set(
        ids.map((id) => {
          const a = artifacts.get(id);
          return a ? `${a.role} (${a.format})` : "未知來源";
        }),
      ),
    ].join(" + ") || "未記錄";
  function sourceList(title, ids) {
    const section = el("details", null, { class: "lp-sources" });
    section.append(el("summary", `${title} · ${ids.length}`));
    for (const id of ids) {
      const a = artifacts.get(id);
      section.append(
        button(
          `${a?.role || "未知"} · ${a?.format || "未知型態"} · ${a?.status || "unknown"} · ${a?.source?.path || id}`,
          () => onSelect({ kind: "artifact", id }),
        ),
      );
    }
    return section;
  }
  function drawDetail() {
    if (!chooser.value || renderedRun === chooser.value) {
      return;
    }
    renderedRun = chooser.value;
    view.run = chooser.value;
    const detail = projectRunDetail(graph, chooser.value);
    const layer = projectDetailLayer(detail, view.sources);
    const hiddenCount = detail.nodes.length - layer.nodes.length;
    detailBody.replaceChildren();
    detailBody.append(
      el(
        "p",
        `${detail.sessions.filter((s) => s.session_id).length} 個原生 Session 分組 · ${detail.pclass_nodes.length} 個 P-class 來源／執行節點 · ${detail.instructions.length} 個綁定指引／附屬來源`,
        { class: "lp-note" },
      ),
      el(
        "p",
        "Session 分組依原身分建立，不是新增執行節點。指引綁定不等於載入或遵循；未提供的 Session 工程判斷仍為 UNKNOWN。跨界來源保留原 Run 標籤。",
        { class: "lp-note" },
      ),
    );
    detailBody.append(
      el(
        "p",
        `${layer.nodes.length} 個路徑節點 · ${hiddenCount} 個可用／已選指引留在來源層；全部 P-class 可由下方索引查閱。`,
        { class: "lp-note" },
      ),
    );
    const corridor = el("section", null, {
      class: "lp-corridor",
      "aria-label": "選定路徑的原始交接",
    });
    corridor.append(el("h4", "02 / 原始交接 → Session／attempt → 提示詞比較"));
    const handoffs = detail.edges.filter((e) => e.kind === "handoff");
    if (handoffs.length === 0) {
      corridor.append(el("p", "尚無原生交接證據；下方僅展示來源投影。"));
    }
    for (const edge of handoffs) {
      const from = detail.nodes.find((n) => n.id === edge.from_node_id);
      const to = detail.nodes.find((n) => n.id === edge.to_node_id);
      const item = el("article");
      item.append(
        button(`${edge.label} · ${edge.status}/${edge.current_validity}`, () =>
          onSelect({ kind: "edge", id: edge.id }),
        ),
        el("p", `${types(edge.input_artifact_ids)} → ${types(edge.output_artifact_ids)}`),
        el(
          "small",
          `${from.identity.session_id || "Session UNKNOWN"} → ${to.identity.session_id || "Session UNKNOWN"} · Attempt ${to.identity.attempt_id || "UNKNOWN"}`,
        ),
        button(`來源節點 · ${from.kind}`, () => onSelect({ kind: "node", id: from.id })),
        button(`接收節點 · ${to.kind}`, () => onSelect({ kind: "node", id: to.id })),
      );
      corridor.append(item);
    }
    detailBody.append(corridor);
    const geometry = layoutGraph({
      nodes: layer.nodes,
      edges: layer.edges
        .filter((e) => e.kind === "handoff")
        .map((e) => ({ from: e.from_node_id, to: e.to_node_id })),
    });
    const map = el("div", null, { class: "lp-map lp-detail-map" });
    const svg = svgEl("svg", null, {
      viewBox: `0 0 ${geometry.width} ${geometry.height}`,
      role: "group",
      "aria-label": "Session 與提示詞細部 DAG",
      style: `min-width:${geometry.width}px`,
    });
    const positions = new Map(geometry.nodes.map((n) => [n.id, n]));
    const nodeMap = new Map(detail.nodes.map((n) => [n.id, n]));
    const nodePanels = new Map();
    const edgePanels = new Map();
    for (const band of geometry.bands) {
      svg.append(
        svgEl("rect", null, { ...band, rx: 10, class: "lp-band" }),
        svgEl(
          "text",
          graph.repositories.find((r) => r.id === band.repository_id)?.label || "未知 Repo",
          { x: band.x + 12, y: band.y + 25, class: "lp-band-label" },
        ),
      );
    }
    for (const [index, edge] of layer.edges.entries()) {
      const from = positions.get(edge.from_node_id),
        to = positions.get(edge.to_node_id);
      const x1 = from.x + from.width;
      const y1 = from.y + from.height / 2;
      const x2 = to.x;
      const y2 = to.y + to.height / 2;
      const g = svgEl("g", null, {
        class: `lp-relation ${edge.current_validity === "current" && edge.status === "observed" ? "lp-observed" : "lp-unknown"}`,
        role: "button",
        tabindex: 0,
        "aria-label": `資料流 ${index + 1} · ${edge.label}`,
        "data-detail-edge": edge.id,
      });
      g.append(
        svgEl("path", null, {
          d: `M${x1} ${y1} C${x1 + 50} ${y1},${x2 - 50} ${y2},${x2} ${y2}`,
          "marker-end": "url(#lp-arrow)",
        }),
        svgEl("text", `#${index + 1}`, { x: (x1 + x2) / 2, y: (y1 + y2) / 2 - 8 }),
      );
      activate(g, () => {
        onSelect({ kind: "edge", id: edge.id });
      });
      svg.append(g);
    }
    for (const position of geometry.nodes) {
      const node = nodeMap.get(position.id);
      const g = svgEl("g", null, {
        transform: `translate(${position.x},${position.y})`,
        class: `lp-object ${node.status.execution === "failed" ? "lp-failed" : ""}`,
        role: "button",
        tabindex: 0,
        "data-detail-node": node.id,
        "aria-label": `細部 ${node.kind} · ${node.label}`,
      });
      const title = `${node.status.execution === "failed" ? "✕ " : ""}${node.kind} · ${node.label}`;
      g.append(
        svgEl("title", `${title} · ${node.identity.session_id || "Session 未綁定"}`, {}),
        svgEl("rect", null, { width: position.width, height: position.height, rx: 8 }),
        svgEl(
          "text",
          `${node.status.execution === "failed" ? "✕ " : ""}${node.kind.toUpperCase()}`,
          { x: 10, y: 19 },
        ),
        svgEl("text", node.label.length > 31 ? `${node.label.slice(0, 30)}…` : node.label, {
          x: 10,
          y: 42,
        }),
        svgEl("text", `${node.status.execution} · ${node.status.transfer}`, { x: 10, y: 64 }),
        svgEl("text", `Run ${node.run_id.slice(0, 12)}`, { x: 10, y: 81 }),
      );
      activate(g, () => {
        onSelect({ kind: "node", id: node.id });
      });
      svg.append(g);
    }
    map.append(svg);
    detailBody.append(map);
    const inventory = el("details");
    inventory.append(el("summary", `全部 P-class 來源／執行節點 · ${detail.pclass_nodes.length}`));
    for (const node of detail.pclass_nodes) {
      inventory.append(
        button(
          `${node.label} · ${node.status.execution} · ${node.identity.session_id || "Session UNKNOWN"}`,
          () => onSelect({ kind: "node", id: node.id }),
        ),
      );
    }
    detailBody.append(inventory);
    const referencePanels = el("details", null, { class: "lp-references" });
    referencePanels.append(el("summary", "可用／已選指引的來源紀錄"));
    detailBody.append(referencePanels);
    for (const session of detail.sessions) {
      const group = el("section", null, { class: "lp-session" });
      group.append(
        el(
          "h4",
          session.session_id ? `Session · ${session.session_id}` : "未綁定 Session 的原始節點",
        ),
        el(
          "small",
          `Run ${session.run_id} · ${graph.repositories.find((r) => r.id === session.repository_id)?.label || "未知 Repo"}`,
        ),
      );
      for (const node of session.nodes) {
        const panel = el("details", null, { "data-detail-occurrence": node.id });
        nodePanels.set(node.id, panel);
        panel.append(
          el(
            "summary",
            `${node.kind} · ${node.label} · ${node.status.execution} · ${node.identity.status}`,
          ),
          el(
            "p",
            `執行 ${node.status.execution} · 傳遞 ${node.status.transfer} · 驗收 ${node.status.acceptance}`,
          ),
          el(
            "p",
            `Attempt：${node.identity.attempt_id || "UNKNOWN"} · Invocation：${node.identity.invocation_id || "UNKNOWN"}`,
          ),
          sourceList("輸入表示", node.input_artifact_ids),
          sourceList("輸出表示", node.output_artifact_ids),
          sourceList("原生證據", node.evidence_artifact_ids),
          button("定位原節點與內容", () => {
            onSelect({ kind: "node", id: node.id });
          }),
        );
        if (["available_reference", "selected"].includes(node.status.execution)) {
          referencePanels.append(panel);
        } else {
          group.append(panel);
        }
      }
      detailBody.append(group);
    }
    const flows = el("section", null, { class: "lp-flows" });
    flows.append(el("h4", "連線上的資料型態變化"));
    for (const [index, edge] of detail.edges.entries()) {
      const panel = el("details", null, { "data-detail-flow": edge.id });
      edgePanels.set(edge.id, panel);
      panel.append(
        el("summary", `#${index + 1} ${edge.label} · ${edge.status}/${edge.current_validity}`),
        el("p", `${nodeMap.get(edge.from_node_id).kind} → ${nodeMap.get(edge.to_node_id).kind}`),
        el("p", `${types(edge.input_artifact_ids)} → ${types(edge.output_artifact_ids)}`, {
          class: "lp-types",
        }),
        sourceList("轉換前", edge.input_artifact_ids),
        sourceList("轉換後", edge.output_artifact_ids),
        sourceList("交接證據", edge.evidence_artifact_ids),
        button("同窗比較這次傳遞", () => onSelect({ kind: "edge", id: edge.id })),
      );
      flows.append(panel);
    }
    detailBody.append(flows);
    const sources = el("details", null, { class: "lp-instruction-index" });
    sources.append(
      el("summary", `P-class 指引及附屬來源完整索引 · ${detail.instructions.length}`),
      el(
        "p",
        "逐一列出原節點引用的 instruction 資料。包含 playbook 與輔助程式；此索引不宣告它們都是 P-class 執行節點。",
      ),
    );
    const search = el("input", null, {
      type: "search",
      placeholder: "依路徑篩選",
      "aria-label": "篩選指引來源",
    });
    const list = el("div");
    const drawSources = () => {
      const matching = detail.instructions.filter(({ artifact }) =>
        (artifact.source?.path || artifact.id).toLowerCase().includes(search.value.toLowerCase()),
      );
      list.replaceChildren(el("p", `${matching.length} / ${detail.instructions.length} 個來源`));
      for (const { artifact, node_ids } of matching) {
        const item = el("article");
        item.append(
          button(`${artifact.source?.path || artifact.id} · ${artifact.status}`, () =>
            onSelect({ kind: "artifact", id: artifact.id }),
          ),
          el("small", `綁定來源 · ${artifact.format} · 載入／遵循 UNKNOWN`),
          button(`定位 ${node_ids.length} 個引用節點`, () => onFocus({ node_ids, edge_ids: [] })),
        );
        list.append(item);
      }
    };
    search.addEventListener("input", drawSources);
    sources.append(search, list);
    drawSources();
    detailBody.append(sources);
    for (const f of graph.findings.filter((finding) => finding.run_id === chooser.value)) {
      detailBody.append(
        button(
          `${f.status === "failed" ? "✕" : "?"} ${f.message}`,
          () => onSelect({ kind: "finding", id: f.id }),
          { class: f.status === "failed" ? "lp-failed" : "lp-unknown" },
        ),
      );
    }
  }
  function updatePosition() {
    for (const card of paths.querySelectorAll("[data-panorama-run]")) {
      card.hidden = Boolean(view.run && card.dataset.panoramaRun !== view.run);
    }
    const run = runs.find((r) => r.id === view.run);
    const selected = view.selection;
    const record =
      selected &&
      (
        {
          node: graph.nodes,
          edge: graph.edges,
          finding: graph.findings,
          artifact: graph.artifacts,
          object: graph.objects,
        }[selected.kind] || []
      ).find((r) => r.id === selected.id);
    breadcrumb.replaceChildren(
      button("返回全局", () => {
        view.run = null;
        view.selection = null;
        detailPanel.open = false;
        consoleView.syncRun(null);
        updatePosition();
        onReset?.();
        root.querySelector(".lp-journey")?.scrollIntoView({ block: "start" });
      }),
      el("span", run ? `${run.repository} / ${run.id.slice(0, 12)}` : "Goal / 全部適用路徑"),
    );
    if (record) {
      breadcrumb.append(
        el(
          "strong",
          ` → ${selected.kind} / ${record.label || record.message || record.role || record.id}`,
        ),
      );
    }
    for (const node of root.querySelectorAll("[data-panorama-run]")) {
      node.classList.toggle("is-linked", node.dataset.panoramaRun === view.run);
    }
    for (const node of root.querySelectorAll("[data-detail-node], [data-detail-edge]")) {
      node.classList.toggle(
        "is-linked",
        Boolean(selected) && (node.dataset.detailNode || node.dataset.detailEdge) === selected.id,
      );
    }
  }
  function openRun(id, { focus = false } = {}) {
    if (![...chooser.options].some((o) => o.value === id)) {
      return;
    }
    chooser.value = id;
    view.run = id;
    consoleView.syncRun(id);
    detailPanel.open = true;
    drawDetail();
    updatePosition();
    if (focus) {
      const row = model.rows.find((candidate) => candidate.run_id === id);
      onFocus({ node_ids: row.node_ids, edge_ids: row.edge_ids, reveal: false });
    }
  }
  function setSelection(value) {
    consoleView.setSelection(value);
    if (!value) {
      view.selection = null;
      updatePosition();
      return;
    }
    const collection = {
      node: graph.nodes,
      edge: graph.edges,
      finding: graph.findings,
      artifact: graph.artifacts,
      object: graph.objects,
    }[value.kind];
    const record = collection?.find((r) => r.id === value.id);
    if (!record) {
      view.selection = null;
      updatePosition();
      return;
    }
    view.selection = value;
    if (
      record.run_id &&
      (!view.run || !projectRunDetail(graph, view.run).nodes.some((n) => n.id === record.id))
    ) {
      openRun(record.run_id, { focus: false });
    }
    if (
      value.kind === "node" &&
      ["available_reference", "selected"].includes(record.status.execution)
    ) {
      view.sources = true;
      sourceToggle.checked = true;
      renderedRun = null;
      drawDetail();
    }
    const selector = { node: "[data-detail-occurrence]", edge: "[data-detail-flow]" }[value.kind];
    if (selector) {
      for (const panel of detailBody.querySelectorAll(selector)) {
        const match = (panel.dataset.detailOccurrence || panel.dataset.detailFlow) === value.id;
        panel.open = match;
        if (match && panel.parentElement.tagName === "DETAILS") {
          panel.parentElement.open = true;
        }
      }
    }
    updatePosition();
  }
  sourceToggle.addEventListener("change", () => {
    view.sources = sourceToggle.checked;
    renderedRun = null;
    drawDetail();
    updatePosition();
  });
  chooser.addEventListener("change", () => {
    view.selection = null;
    openRun(chooser.value, { focus: true });
  });
  detailPanel.addEventListener("toggle", () => {
    if (detailPanel.open) {
      drawDetail();
    }
  });
  root.append(detailPanel);
  function renderRow(row, expand = null) {
    const run = runs.find((r) => r.id === row.run_id);
    const card = el("article", null, { class: "lp-loop", "data-panorama-run": row.run_id });
    card.append(
      el(
        "h4",
        `${run?.repository?.split("/").at(-1) || "未知 Repo"} · ${run?.mode || "未知模式"} · ${row.run_id.slice(0, 12)}`,
      ),
      el("p", `${row.kinds.join(" / ")}${row.cross_repository ? " · 跨 Repo 交接" : ""}`),
    );
    const facets = [
      ["資料送達並消費", row.consumption, "已見範圍內消費與審查"],
      [
        "回饋接收並續行",
        row.feedback,
        row.feedback.current
          ? "已見原回饋與後續 attempt（目前有效）"
          : "已見歷史續行；目前效力 UNKNOWN",
      ],
    ];
    for (const [name, facet, yes] of facets) {
      card.append(
        el(
          "p",
          `${facet.status === "observed" ? "✓" : "?"} ${name}：${facet.status === "observed" ? yes : "UNKNOWN"}`,
          { class: facet.status === "observed" ? "lp-observed" : "lp-unknown" },
        ),
      );
      for (const id of facet.edge_ids) {
        card.append(button("開啟交接證據", () => onSelect({ kind: "edge", id })));
      }
    }
    if (row.feedback.status === "unknown") {
      card.append(
        el("small", "原回饋角色／ACK／後續 attempt 綁定不足或已失效；不從邊的名稱推論續行。"),
      );
    }
    card.append(
      el(
        "p",
        `? 行為 Goal 驗收：UNKNOWN${row.goal.node_ids.length > 0 ? "（已有局部審查，不等於 Goal 完成）" : ""}`,
        { class: "lp-unknown" },
      ),
    );
    for (const id of row.goal.node_ids) {
      card.append(button("原局部審查", () => onSelect({ kind: "node", id })));
    }
    card.append(
      button("展開本次執行 DAG", () => {
        view.selection = null;
        if (expand) {
          expand(row.run_id);
        } else {
          consoleView.setRun(row.run_id);
        }
        consoleRoot.scrollIntoView({ block: "nearest" });
      }),
    );
    const failures = row.findings.filter((f) => f.status === "failed");
    for (const f of failures) {
      card.append(
        button(`✕ ${f.message}`, () => onSelect({ kind: "finding", id: f.id }), {
          class: "lp-failed",
        }),
      );
    }
    const gaps = row.findings.filter((f) => f.status === "unknown");
    if (gaps.length > 0) {
      const detail = el("details");
      detail.append(el("summary", `? ${gaps.length} 個證據缺口`));
      for (const f of gaps) {
        detail.append(button(f.message, () => onSelect({ kind: "finding", id: f.id })));
      }
      card.append(detail);
    }
    return card;
  }
  for (const row of model.rows.filter((candidate) => candidate.repository_ids.length > 0)) {
    paths.append(renderRow(row));
  }
  const regions = el("div", null, { class: "lp-regions" });
  for (const repo of graph.repositories) {
    const members = model.nodes.filter((n) => n.repository_id === repo.id);
    if (members.length === 0) {
      continue;
    }
    const region = el("section", null, { class: "lp-region", "data-panorama-repository": repo.id });
    const index = el("aside");
    index.append(
      el("h3", repo.label),
      el("p", repo.revision?.slice(0, 12) || "版本未知"),
      button("定位目錄與區域 DAG", () =>
        onFocus({ node_ids: members.map((n) => n.id), edge_ids: [] }),
      ),
      el("small", `${repo.tracked_files.length} 個 Git 索引項目 · ${repo.index_status}`),
    );
    const body = el("div", null, { class: "lp-region-runs" });
    for (const row of model.rows.filter((r) => r.repository_ids.includes(repo.id))) {
      body.append(
        button(`${row.run_id.slice(0, 12)} · 開啟路徑`, () => {
          view.selection = null;
          openRun(row.run_id, { focus: true });
          root.querySelector(".lp-detail")?.scrollIntoView({ block: "start" });
        }),
      );
    }
    if (body.children.length === 0) {
      body.append(el("p", "跨界來源上下文；不納入選定 Goal 的驗收。"));
    }
    region.append(index, body);
    regions.append(region);
  }
  const unlocated = model.rows.filter((row) => row.repository_ids.length === 0);
  if (unlocated.length > 0) {
    const section = el("section", null, {
      class: "lp-region-runs",
      "aria-label": "尚無 DAG 的選定 Runs",
    });
    section.append(el("h3", "尚無 DAG 的選定 Runs"));
    for (const row of unlocated) {
      section.append(renderRow(row));
    }
    journey.append(section);
  }
  architecture.append(
    regions,
    el(
      "p",
      "三欄各自對照原證據。現在的 v2 投影未提供完整 Goal 驗收分母；不以 completed、無紅 X 或圖上形成環推論通過。",
      { class: "lp-note" },
    ),
  );
  if (view.run && model.rows.some((r) => r.run_id === view.run)) {
    openRun(view.run);
  } else {
    view.run = null;
  }
  setSelection(view.selection);
  return { setSelection };
}
