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
    if (edge.kind !== "handoff") continue;
    const from = nodeMap.get(edge.from_node_id),
      to = nodeMap.get(edge.to_node_id);
    if (!from || !to) throw new Error("Panorama endpoint missing");
    const key = JSON.stringify([from.object_id, to.object_id]);
    if (!relations.has(key))
      relations.set(key, {
        id: key,
        from: from.object_id,
        to: to.object_id,
        edge_ids: [],
        current: 0,
      });
    const relation = relations.get(key);
    relation.edge_ids.push(edge.id);
    if (observed(edge)) relation.current++;
  }
  const rows = [...selected].sort().map((run_id) => {
    const ownNodes = nodes.filter((n) => n.run_id === run_id);
    const ownEdges = edges.filter((e) => e.run_id === run_id);
    const current = ownEdges.filter(observed);
    const consumed = current.filter((e) => {
      const target = nodeMap.get(e.to_node_id);
      return (
        target.status.transfer === "output_reviewed" &&
        target.status.acceptance === "original_scoped_review_approved"
      );
    });
    // The original producer only attaches this set after candidate/feedback/ACK/attempt checks.
    const feedback = ownEdges.filter((e) => {
      if (!recorded(e)) return false;
      const from = nodeMap.get(e.from_node_id),
        to = nodeMap.get(e.to_node_id);
      if (
        graph.objects.find((o) => o.id === from.object_id)?.namespace !== "soodles.loop" ||
        graph.objects.find((o) => o.id === to.object_id)?.namespace !== "noodle.inner" ||
        to.kind !== "cli"
      )
        return false;
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
        status: consumed.length ? "observed" : "unknown",
        edge_ids: consumed.map((e) => e.id),
      },
      feedback: {
        status: feedback.length ? "observed" : "unknown",
        edge_ids: feedback.map((e) => e.id),
        current: feedback.length > 0 && feedback.every(observed),
      },
      goal: { status: "unknown", node_ids: scopedReviews.map((n) => n.id) },
      findings: graph.findings.filter((f) => f.run_id === run_id),
    };
  });
  return {
    nodes,
    edges,
    relations: [...relations.values()].sort((a, b) => a.id.localeCompare(b.id)),
    rows,
    context_run_ids: [
      ...new Set(nodes.filter((n) => !selected.has(n.run_id)).map((n) => n.run_id)),
    ],
  };
}

const ns = "http://www.w3.org/2000/svg";
function el(tag, text, attrs = {}, svg = false) {
  const node = svg ? document.createElementNS(ns, tag) : document.createElement(tag);
  if (text != null) node.textContent = text;
  for (const [key, value] of Object.entries(attrs)) node.setAttribute(key, String(value));
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
  { graph, runs, runIds = null, goal = "", onScope, onSelect, onFocus },
) {
  const model = projectPanorama(graph, runIds ?? runs.map((r) => r.id));
  root.replaceChildren();
  root.classList.add("lp-panel");
  const header = el("header"),
    title = el("h2", goal || "全機總覽 · 尚未選定行為目標");
  header.append(
    el("p", "WORKSPACE / LOOP PANORAMA", { class: "lp-eyebrow" }),
    title,
    el("p", "上層看責任物件與回饋關係，下層展開原始執行 DAG。彙總圖中的環不代表同一次執行已閉環。"),
  );
  root.append(header);
  const scope = el("details", null, { class: "lp-scope" });
  scope.append(el("summary", `工作範圍 · ${runIds === null ? "全部" : runIds.length} Runs`));
  const label = el("label", "行為目標 "),
    input = el("input", null, { "aria-label": "Workspace 行為目標", maxlength: 240 });
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
      if (!input.value.trim() || !ids.length) {
        error.textContent = "請命名行為目標並選取至少一個 Run。";
        return;
      }
      onScope({ goal: input.value.trim(), runIds: ids });
    }),
    button("全機總覽", () => onScope({ goal: "", runIds: null })),
    error,
  );
  root.append(scope);
  if (model.context_run_ids.length)
    root.append(
      el(
        "p",
        `另保留 ${model.context_run_ids.length} 個跨界來源 Run 作交接上下文；不納入此 Goal 驗收。`,
        { class: "lp-note" },
      ),
    );
  const objects = [...graph.objects].sort((a, b) => a.namespace.localeCompare(b.namespace));
  const width = Math.max(900, objects.length * 175 + 40),
    height = 320;
  const scroll = el("div", null, { class: "lp-map" });
  const drawing = el(
    "svg",
    null,
    { viewBox: `0 0 ${width} ${height}`, role: "group", "aria-label": "唯一物件全景關係圖" },
    true,
  );
  const defs = el("defs", null, {}, true),
    marker = el(
      "marker",
      null,
      {
        id: "lp-arrow",
        viewBox: "0 0 10 10",
        refX: 9,
        refY: 5,
        markerWidth: 6,
        markerHeight: 6,
        orient: "auto",
      },
      true,
    );
  marker.append(el("path", null, { d: "M0 0 L10 5 L0 10z", fill: "currentColor" }, true));
  defs.append(marker);
  drawing.append(defs);
  const xs = new Map(objects.map((o, i) => [o.id, 30 + i * 175]));
  model.relations.forEach((r, i) => {
    const x1 = xs.get(r.from) + 75,
      x2 = xs.get(r.to) + 75;
    const arcY = 40 + (i % 5) * 24;
    const d =
      r.from === r.to
        ? `M${x1 - 24} 170 C${x1 - 120} ${arcY},${x1 + 120} ${arcY},${x1 + 24} 170`
        : `M${x1} 170 C${x1} ${arcY},${x2} ${arcY},${x2} 170`;
    const g = el(
      "g",
      null,
      {
        class: `lp-relation ${r.current === r.edge_ids.length ? "lp-observed" : "lp-unknown"}`,
        role: "button",
        tabindex: 0,
        "aria-label": `${objects.find((o) => o.id === r.from)?.label} → ${objects.find((o) => o.id === r.to)?.label} · ${r.edge_ids.length} 次交接`,
        "data-panorama-relation": r.id,
      },
      true,
    );
    g.append(el("path", null, { d, "marker-end": "url(#lp-arrow)" }, true));
    g.append(
      el(
        "text",
        `${r.current}/${r.edge_ids.length} 目前可證實`,
        { x: (x1 + x2) / 2, y: arcY + 50, "text-anchor": "middle" },
        true,
      ),
    );
    activate(g, () => {
      const panel = relationPanels.get(r.id);
      panel.open = true;
      panel.scrollIntoView({ block: "nearest" });
    });
    drawing.append(g);
  });
  objects.forEach((o) => {
    const nodes = model.nodes.filter((n) => n.object_id === o.id),
      x = xs.get(o.id);
    const g = el(
      "g",
      null,
      {
        transform: `translate(${x},170)`,
        class: "lp-object",
        role: "button",
        tabindex: 0,
        "aria-label": o.label,
        "data-panorama-object": o.id,
      },
      true,
    );
    g.append(
      el("rect", null, { width: 150, height: 85, rx: 12 }, true),
      el("text", o.label, { x: 12, y: 28 }, true),
      el("text", `${nodes.length} 次已記錄發生`, { x: 12, y: 51 }, true),
    );
    activate(g, () => {
      onFocus({ node_ids: nodes.map((n) => n.id), edge_ids: [] });
      onSelect({ kind: "object", id: o.id });
    });
    drawing.append(g);
  });
  scroll.append(drawing);
  root.append(scroll);
  const drill = el("section", null, { "aria-label": "全景關係展開", class: "lp-drill" }),
    relationPanels = new Map();
  root.append(drill);
  drill.append(el("h3", "關係與原始交接"));
  model.relations.forEach((r) => {
    const detail = el("details");
    relationPanels.set(r.id, detail);
    const name = `${objects.find((o) => o.id === r.from)?.label} → ${objects.find((o) => o.id === r.to)?.label}`;
    detail.append(
      el("summary", `${name} · ${r.edge_ids.length} 次交接`),
      button("展開這組關係 DAG", () => onFocus({ node_ids: [], edge_ids: r.edge_ids })),
    );
    r.edge_ids.forEach((id) => {
      const e = graph.edges.find((e) => e.id === id);
      detail.append(
        button(`${e.label} · ${e.status}/${e.current_validity}`, () =>
          onSelect({ kind: "edge", id }),
        ),
      );
    });
    drill.append(detail);
  });
  if (!model.relations.length) drill.append(el("p", "此範圍尚無已登記交接；不補造連線。"));
  function renderRow(row) {
    const run = runs.find((r) => r.id === row.run_id),
      card = el("article", null, { class: "lp-loop", "data-panorama-run": row.run_id });
    card.append(
      el("h4", `${run?.mode || "未知模式"} · ${row.run_id.slice(0, 12)}`),
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
      facet.edge_ids.forEach((id) =>
        card.append(button("開啟交接證據", () => onSelect({ kind: "edge", id }))),
      );
    }
    if (row.feedback.status === "unknown")
      card.append(
        el("small", "原回饋角色／ACK／後續 attempt 綁定不足或已失效；不從邊的名稱推論續行。"),
      );
    card.append(
      el(
        "p",
        `? 行為 Goal 驗收：UNKNOWN${row.goal.node_ids.length ? "（已有局部審查，不等於 Goal 完成）" : ""}`,
        { class: "lp-unknown" },
      ),
    );
    row.goal.node_ids.forEach((id) =>
      card.append(button("原局部審查", () => onSelect({ kind: "node", id }))),
    );
    card.append(
      button("展開本次執行 DAG", () => onFocus({ node_ids: row.node_ids, edge_ids: row.edge_ids })),
    );
    const failures = row.findings.filter((f) => f.status === "failed");
    failures.forEach((f) =>
      card.append(
        button(`✕ ${f.message}`, () => onSelect({ kind: "finding", id: f.id }), {
          class: "lp-failed",
        }),
      ),
    );
    const gaps = row.findings.filter((f) => f.status === "unknown");
    if (gaps.length) {
      const detail = el("details");
      detail.append(el("summary", `? ${gaps.length} 個證據缺口`));
      gaps.forEach((f) =>
        detail.append(button(f.message, () => onSelect({ kind: "finding", id: f.id }))),
      );
      card.append(detail);
    }
    return card;
  }
  const regions = el("div", null, { class: "lp-regions" });
  for (const repo of graph.repositories) {
    const members = model.nodes.filter((n) => n.repository_id === repo.id);
    if (!members.length) continue;
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
      body.append(renderRow(row));
    }
    if (!body.children.length) body.append(el("p", "跨界來源上下文；不納入選定 Goal 的驗收。"));
    region.append(index, body);
    regions.append(region);
  }
  const unlocated = model.rows.filter((row) => !row.repository_ids.length);
  if (unlocated.length) {
    const section = el("section", null, {
      class: "lp-region-runs",
      "aria-label": "尚無 DAG 的選定 Runs",
    });
    section.append(el("h3", "尚無 DAG 的選定 Runs"));
    unlocated.forEach((row) => section.append(renderRow(row)));
    regions.append(section);
  }
  root.append(
    regions,
    el(
      "p",
      "三欄各自對照原證據。現在的 v2 投影未提供完整 Goal 驗收分母；不以 completed、無紅 X 或圖上形成環推論通過。",
      { class: "lp-note" },
    ),
  );
}
