import {
  slabOf,
  iso,
  fitProjection,
  boxCorners,
  depthOf,
  planOutline,
} from "./anatomy/iso-kit.mjs";

const NS = "http://www.w3.org/2000/svg";
function html(tag, text, attrs = {}) {
  return decorate(document.createElement(tag), text, attrs);
}
function svg(tag, text, attrs = {}) {
  return decorate(document.createElementNS(NS, tag), text, attrs);
}
function decorate(el, text, attrs) {
  if (text !== null && text !== undefined) {
    el.textContent = text;
  }
  for (const [key, value] of Object.entries(attrs)) {
    el.setAttribute(key, String(value));
  }
  return el;
}
function action(el, fn) {
  el.addEventListener("click", fn);
  el.addEventListener("keydown", (e) => {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      fn();
    }
  });
}
function button(text, fn) {
  const b = html("button", text, { type: "button" });
  b.addEventListener("click", fn);
  return b;
}

/** Only original handoffs with complete bindings can be called currently evidenced. */
export function evidenceState(edge, graph) {
  if (edge.status === "failed") {
    return "failed";
  }
  const present = (ids) =>
    ids.length > 0 &&
    ids.every((id) =>
      graph.artifacts.some((a) => a.id === id && ["available", "not_public"].includes(a.status)),
    );
  const bound = [edge.from_node_id, edge.to_node_id].every((id) =>
    graph.nodes.some((n) => n.id === id && n.identity.status === "observed"),
  );
  if (
    edge.kind === "handoff" &&
    edge.status === "observed" &&
    bound &&
    present(edge.input_artifact_ids) &&
    present(edge.output_artifact_ids) &&
    present(edge.evidence_artifact_ids)
  ) {
    return edge.current_validity === "current" ? "current" : "historical";
  }
  return "unknown";
}

/** Geometry is a view of original IDs. It never adds an execution or a relationship. */
export function projectConsole(graph, scope, { runId = null, sources = false } = {}) {
  const nodes = scope.nodes.filter(
    (n) =>
      runId === null ||
      n.run_id === runId ||
      scope.edges.some((e) => e.run_id === runId && [e.from_node_id, e.to_node_id].includes(n.id)),
  );
  const eligible = scope.edges.filter((e) => runId === null || e.run_id === runId);
  const endpoints = new Set(
    eligible.filter((e) => e.kind === "handoff").flatMap((e) => [e.from_node_id, e.to_node_id]),
  );
  const visible = nodes.filter(
    (n) =>
      sources ||
      endpoints.has(n.id) ||
      !["available_reference", "selected"].includes(n.status.execution),
  );
  const ids = new Set(visible.map((n) => n.id));
  const edges = eligible.filter((e) => ids.has(e.from_node_id) && ids.has(e.to_node_id));
  const repositories = graph.repositories.filter((r) =>
    nodes.some((n) => n.repository_id === r.id),
  );
  return {
    nodes: visible,
    edges,
    repositories,
    hidden: nodes.length - visible.length,
    findings: graph.findings.filter(
      (f) =>
        scope.rows.some((r) => r.run_id === f.run_id) && (runId === null || f.run_id === runId),
    ),
  };
}

const stateNames = {
  current: "目前交接證據完整",
  historical: "歷史記錄／目前效力未證實",
  unknown: "UNKNOWN／來源關係",
  failed: "失敗",
};

export function mountIsometric(root, { graph, scope, runs, onSelect, onFocus, view }) {
  const state = (view.console ??= {
    run: null,
    sources: false,
    apart: false,
    angle: 42,
    zoom: 1,
    selection: null,
  });
  if (state.run && !scope.rows.some((r) => r.run_id === state.run)) {
    state.run = null;
  }
  let projection, sceneModel;
  root.addEventListener("keydown", (event) => {
    if (event.key === "Escape") {
      root.classList.remove("li-expanded");
    }
  });
  const openOriginal = (value) => {
    root.classList.remove("li-expanded");
    onSelect(value);
  };
  root.className = "li-console";
  const heading = html("header");
  heading.append(
    html("p", "FIG. 01 / EVIDENCE CONTROL ROOM", { class: "li-kicker" }),
    html("h3", "內外迴圈 · 立體工程全景"),
  );
  const toolbar = html("div", null, { class: "li-toolbar" });
  const run = html("select", null, { "aria-label": "立體視圖執行範圍" });
  run.append(html("option", "全局 · 唯一物件與跨 Repo", { value: "" }));
  for (const row of scope.rows) {
    const r = runs.find((candidate) => candidate.id === row.run_id);
    run.append(
      html("option", `${r?.repository?.split("/").at(-1) || "Repo"} · ${row.run_id.slice(0, 12)}`, {
        value: row.run_id,
      }),
    );
  }
  run.value = state.run || "";
  run.addEventListener("change", () => {
    state.run = run.value || null;
    state.selection = null;
    render();
  });
  const explode = button("拆解資料載體", () => {
    state.apart = !state.apart;
    render();
  });
  const sourceLabel = html("label", "全部 P-class／來源 ");
  const source = html("input", null, { type: "checkbox", "aria-label": "立體視圖全部來源" });
  source.checked = state.sources;
  source.addEventListener("change", () => {
    state.sources = source.checked;
    render();
  });
  sourceLabel.append(source);
  toolbar.append(
    run,
    button("專注／返回頁面", () => {
      root.classList.toggle("li-expanded");
    }),
    explode,
    button("左轉", () => {
      state.angle = Math.max(20, state.angle - 10);
      render();
    }),
    button("右轉", () => {
      state.angle = Math.min(70, state.angle + 10);
      render();
    }),
    button("放大", () => {
      state.zoom = Math.min(4, state.zoom + 0.5);
      size();
    }),
    button("縮小", () => {
      state.zoom = Math.max(1, state.zoom - 0.5);
      size();
    }),
    button("還原全景", () => {
      Object.assign(state, { run: null, selection: null, zoom: 1, angle: 42, apart: false });
      run.value = "";
      render();
    }),
    sourceLabel,
  );
  const body = html("div", null, { class: "li-body" });
  const viewport = html("div", null, {
    class: "li-viewport",
    tabindex: 0,
    "aria-label": "可縮放的立體工作台",
  });
  const drawing = svg("svg", null, {
    viewBox: "0 0 1100 760",
    role: "group",
    "aria-label": "內外迴圈立體資料流",
  });
  viewport.append(drawing);
  const inspector = html("aside", null, { class: "li-inspector", "aria-label": "立體選取讀回" });
  body.append(viewport, inspector);
  const readout = html("p", null, { class: "li-readout", "aria-live": "polite" });
  const legend = html(
    "p",
    "實線：目前交接證據完整　虛線：歷史／UNKNOWN　✕：已記錄失敗。連線與選取只讀回證據，不啟動任務。",
    { class: "li-legend" },
  );
  root.append(heading, toolbar, body, readout, legend);

  function size() {
    drawing.style.width = `${state.zoom * 100}%`;
  }
  function select(kind, id) {
    state.selection = { kind, id };
    render();
  }
  function selected(kind, id) {
    return state.selection?.kind === kind && state.selection.id === id;
  }
  function solid(group, { plan, z, h, cls = "" }) {
    const paths = slabOf({ r: 3, ...plan }, z, h, projection, 3, 0.8);
    const g = svg("g", null, { class: `li-solid ${cls}` });
    g.append(svg("path", null, { d: paths.fill, class: "li-fill" }));
    for (let i = 0; i < 4; i++) {
      g.append(svg("path", null, { d: paths.shades[i], class: `li-shade-${i}` }));
    }
    g.append(
      svg("path", null, { d: paths.top, class: "li-top" }),
      svg("path", null, { d: paths.outline, class: "li-outline" }),
      svg("path", null, { d: paths.bevel || "", class: "li-bevel" }),
    );
    group.append(g);
  }
  function label(group, text, { point, cls = "", offset = 0 }) {
    const [x, y] = iso(point, projection);
    group.append(
      svg("text", text, { x, y: y + offset, class: `li-label ${cls}`, "text-anchor": "middle" }),
    );
  }
  function target(kind, id, title) {
    const group = svg("g", null, {
      class: `li-target ${selected(kind, id) ? "is-selected" : ""}`,
      role: "button",
      tabindex: 0,
      "aria-label": title,
      [`data-iso-${kind}`]: id,
      ...(kind === "object" ? { "data-panorama-object": id } : {}),
      ...(kind === "node" ? { "data-detail-node": id } : {}),
    });
    const activeEdge =
      state.selection?.kind === "edge"
        ? graph.edges.find((e) => e.id === state.selection.id)
        : null;
    if (activeEdge) {
      const endpoints = graph.nodes.filter((n) =>
        [activeEdge.from_node_id, activeEdge.to_node_id].includes(n.id),
      );
      const related =
        kind === "edge"
          ? id === activeEdge.id
          : endpoints.some((n) => [n.id, n.object_id, n.repository_id].includes(id));
      group.classList.toggle("li-dim", !related);
      group.classList.toggle("li-endpoint", related && kind !== "edge");
    }
    group.append(svg("title", title));
    action(group, () => select(kind, id));
    return group;
  }
  function render() {
    source.disabled = !state.run;
    sceneModel = projectConsole(graph, scope, { runId: state.run, sources: state.sources });
    drawing.replaceChildren();
    explode.textContent = state.apart ? "收合資料載體" : "拆解資料載體";
    explode.setAttribute("aria-pressed", String(state.apart));
    const items = [];
    const placements = new Map();
    const objects = [...graph.objects].toSorted((a, b) => a.namespace.localeCompare(b.namespace));
    const cols = state.run ? 3 : 4;
    const rackRows = Math.ceil(
      (state.run ? sceneModel.nodes.length : sceneModel.repositories.length) / cols,
    );
    const worldDepth = 410 + Math.max(1, rackRows) * 160;
    projection = fitProjection(
      boxCorners({ x: -25, y: -20, w: 800, d: worldDepth, r: 4 }, -25, 140),
      1100,
      760,
      { pad: 40, azimuth: state.angle, elevation: 44 },
    );
    const add = (point, build) => items.push({ depth: depthOf(point, projection), build });
    const base = svg("g");
    solid(base, { plan: { x: -25, y: -20, w: 800, d: worldDepth }, z: -20, h: 12, cls: "li-base" });
    for (const x of [-10, 745]) {
      solid(base, { plan: { x, y: 0, w: 10, d: worldDepth - 40 }, z: -8, h: 7, cls: "li-rail" });
    }
    drawing.append(base);
    const objectPoints = new Map();
    objects.forEach((object, i) => {
      const x = (i % 4) * 190,
        y = Math.floor(i / 4) * 155,
        z = 0;
      const tower = ["soodles.outer", "noodle.inner"].includes(object.namespace);
      const roof = tower ? 106 : 63;
      const portAnchor = [x + 65, y + 42, roof + 5];
      objectPoints.set(object.id, portAnchor);
      add([x + 65, y + 42, z], () => {
        const g = target("object", object.id, `物件 · ${object.label}`);
        solid(g, { plan: { x, y, w: 140, d: 86 }, z: 0, h: 8, cls: "li-foot" });
        solid(g, { plan: { x: x + 5, y: y + 5, w: 130, d: 76 }, z: 8, h: roof - 13, cls: "" });
        solid(g, { plan: { x: x + 3, y: y + 3, w: 134, d: 80 }, z: roof - 5, h: 5, cls: "li-cap" });
        // Three drawers represent source, transition and evidence views, not executions.
        for (let j = 0; j < 3; j++) {
          solid(g, {
            plan: { x: x + 12, y: y + 69 + (state.apart ? 10 * j : 0), w: 112, d: 8 },
            z: 14 + j * 13,
            h: 8,
            cls: "li-drawer",
          });
        }
        for (const port of graph.ports.filter((p) => p.object_id === object.id)) {
          const px = x + (port.direction === "input" ? 16 : 115);
          solid(g, { plan: { x: px, y: y + 27, w: 9, d: 13 }, z: roof, h: 4, cls: "li-port" });
        }
        g.append(
          svg("path", null, {
            d: planOutline({ x: x + 12, y: y + 12, w: 112, d: 48, r: 2 }, roof, projection, 3),
            class: "li-engraving",
          }),
        );
        label(g, object.label, { point: [x + 70, y + 45, roof + 6], cls: "", offset: 0 });
        label(g, `${scope.nodes.filter((n) => n.object_id === object.id).length} 節點`, {
          point: [x + 70, y + 45, roof + 6],
          cls: "li-small",
          offset: 15,
        });
        return g;
      });
    });
    const members = state.run ? sceneModel.nodes : sceneModel.repositories;
    members.forEach((item, i) => {
      const x = (i % cols) * (760 / cols),
        y = 355 + Math.floor(i / cols) * 160;
      const w = 760 / cols - 28;
      const kind = state.run ? "node" : "repository";
      const top = state.apart ? 78 : 32;
      placements.set(item.id, [x + w / 2, y + 47, top + 10]);
      add([x + w / 2, y + 47, 0], () => {
        const g = target(kind, item.id, `${state.run ? item.kind : "Repo"} · ${item.label}`);
        solid(g, { plan: { x, y, w, d: 100 }, z: 0, h: 8, cls: "li-foot" });
        for (const dx of [7, w - 15]) {
          solid(g, {
            plan: { x: x + dx, y: y + 9, w: 8, d: 78 },
            z: 8,
            h: top - 8,
            cls: "li-rail",
          });
        }
        for (let j = 0; j < 3; j++) {
          solid(g, {
            plan: { x: x + 13, y: y + 13, w: w - 26, d: 72 },
            z: top - 12 + j * 6,
            h: 4,
            cls: "li-drawer",
          });
        }
        label(g, state.run ? item.kind : item.label, {
          point: [x + w / 2, y + 43, top + 8],
          cls: "",
          offset: 0,
        });
        label(
          g,
          state.run
            ? item.label.slice(0, 30)
            : `${sceneModel.nodes.filter((n) => n.repository_id === item.id).length} 節點 · ${item.revision?.slice(0, 8) || "版本未知"}`,
          { point: [x + w / 2, y + 43, top + 8], cls: "li-small", offset: 15 },
        );
        if (state.run) {
          label(
            g,
            `${graph.repositories.find((r) => r.id === item.repository_id)?.label || "Repo"} · ${item.status.execution}`,
            { point: [x + w / 2, y + 43, top + 8], cls: "li-small", offset: 29 },
          );
        } else {
          label(g, `${item.tracked_files.length} 個目錄索引`, {
            point: [x + w / 2, y + 43, top + 8],
            cls: "li-small",
            offset: 29,
          });
        }
        return g;
      });
    });
    items.sort((a, b) => a.depth - b.depth);
    for (const item of items) {
      drawing.append(item.build());
    }
    // Data routes are a schematic overlay. They are not physical pipes through equipment.
    const routeLayer = svg("g", null, { class: "li-routes" });
    const visibleEdges = state.run
      ? sceneModel.edges
      : sceneModel.edges.filter((e) => e.kind === "handoff");
    const groups = new Map();
    for (const edge of visibleEdges) {
      const from = graph.nodes.find((n) => n.id === edge.from_node_id),
        to = graph.nodes.find((n) => n.id === edge.to_node_id);
      let pair = [from.object_id, to.object_id];
      if (state.run) {
        pair = [from.id, to.id];
      } else if (edge.scope === "cross_repository") {
        pair = [from.repository_id, to.repository_id];
      }
      const key = JSON.stringify(pair);
      if (!groups.has(key)) {
        groups.set(key, { pair, edges: [] });
      }
      groups.get(key).edges.push(edge);
    }
    let lane = 0;
    for (const { pair, edges } of groups.values()) {
      const a = placements.get(pair[0]) || objectPoints.get(pair[0]),
        b = placements.get(pair[1]) || objectPoints.get(pair[1]);
      if (!a || !b) {
        continue;
      }
      const edge = edges.find((e) => selected("edge", e.id)) || edges[0];
      const start = iso(a, projection);
      const end = iso(b, projection);
      const lift = 32 + (lane++ % 4) * 14;
      const d =
        pair[0] === pair[1]
          ? `M${start[0] - 18} ${start[1]} C${start[0] - 90} ${start[1] - lift - 65},${start[0] + 90} ${start[1] - lift - 65},${start[0] + 18} ${start[1]}`
          : `M${start} C${start[0]} ${start[1] - lift},${end[0]} ${end[1] - lift},${end}`;
      const g = target("edge", edge.id, `資料流 · ${edge.label} · ${edges.length} 段`);
      let status = "unknown";
      if (edges.every((e) => evidenceState(e, graph) === "current")) {
        status = "current";
      } else if (edges.some((e) => evidenceState(e, graph) === "failed")) {
        status = "failed";
      }
      g.setAttribute("class", `${g.getAttribute("class")} li-route li-${status}`);
      g.append(
        svg("path", null, { d, class: "li-route-hit" }),
        svg("path", null, { d, class: "li-route-casing" }),
        svg("path", null, { d, class: "li-route-line" }),
      );
      const tip = pair[0] === pair[1] ? [start[0] + 18, start[1]] : end;
      g.append(
        svg("path", null, {
          d: `M${tip[0] - 4} ${tip[1] - 8} L${tip[0]} ${tip[1]} L${tip[0] + 4} ${tip[1] - 8}`,
          class: "li-arrow",
        }),
      );
      g.append(svg("circle", null, { cx: start[0], cy: start[1], r: 3, class: "li-terminal" }));
      g.append(
        svg("text", `${edges.length > 1 ? `${edges.length} × ` : ""}→`, {
          x: (start[0] + end[0]) / 2,
          y: (start[1] + end[1]) / 2 - lift * 0.75,
          class: "li-route-count",
        }),
      );
      routeLayer.append(g);
    }
    drawing.append(routeLayer);
    for (const finding of sceneModel.findings.filter((f) => f.status === "failed")) {
      const anchor = finding.node_ids.map((id) => placements.get(id)).find(Boolean);
      if (!anchor) {
        continue;
      }
      const [x, y] = iso(anchor, projection);
      const g = target("finding", finding.id, `斷點 · ${finding.message}`);
      g.append(svg("text", "✕", { x: x + 40, y: y - 25, class: "li-breakpoint" }));
      drawing.append(g);
    }
    readout.textContent = `${graph.objects.length} 唯一物件 · ${sceneModel.repositories.length} Repo 區域 · ${sceneModel.nodes.length} 路徑節點 · ${sceneModel.hidden} 來源節點按需展開 · ${sceneModel.edges.filter((e) => evidenceState(e, graph) === "current").length} 段目前交接證據完整`;
    size();
    inspect();
  }
  function inspect() {
    inspector.replaceChildren();
    const pick = state.selection;
    const collection = {
      object: graph.objects,
      node: graph.nodes,
      edge: graph.edges,
      repository: graph.repositories,
      finding: graph.findings,
    };
    const value = pick && collection[pick.kind]?.find((v) => v.id === pick.id);
    inspector.append(html("p", "INSPECT / 原來源讀回", { class: "li-kicker" }));
    if (value) {
      inspector.append(html("h4", value.label || value.message), html("code", value.id));
      if (pick.kind === "edge") {
        inspector.append(html("p", stateNames[evidenceState(value, graph)]));
        for (const [fieldName, ids] of [
          ["輸入", value.input_artifact_ids],
          ["輸出", value.output_artifact_ids],
        ]) {
          inspector.append(html("h5", fieldName));
          for (const id of ids) {
            const a = graph.artifacts.find((artifact) => artifact.id === id);
            inspector.append(
              html(
                "p",
                `${a?.role || "未知"} · ${a?.format || "未知"} · ${a?.status || "unknown"}`,
                { class: "li-type" },
              ),
            );
          }
        }
        inspector.append(button("比較原始輸入與輸出", () => openOriginal(pick)));
      } else if (pick.kind === "repository") {
        const nodes = scope.nodes.filter((n) => n.repository_id === value.id);
        inspector.append(
          html("p", `${value.tracked_files.length} 個 Git 索引項目 · ${value.index_status}`),
          button("定位目錄與區域 DAG", () =>
            onFocus({ node_ids: nodes.map((n) => n.id), edge_ids: [] }),
          ),
        );
        for (const id of new Set(nodes.map((n) => n.run_id))) {
          inspector.append(
            button(`拆解 Run ${id.slice(0, 12)}`, () => {
              state.run = id;
              run.value = id;
              state.selection = null;
              render();
            }),
          );
        }
      } else {
        if (pick.kind === "node") {
          inspector.append(
            html("p", `${value.kind} · ${value.status.execution} · ${value.status.transfer}`),
            html("p", `Session ${value.identity.session_id || "UNKNOWN"}`),
          );
        }
        if (pick.kind === "object") {
          const nodes = scope.nodes.filter((n) => n.object_id === value.id);
          inspector.append(
            html(
              "p",
              `${nodes.length} 筆來源／執行節點。${nodes.length > 0 ? "依原節點狀態判讀。" : "此範圍未觀察到執行。"}`,
            ),
          );
          for (const p of graph.ports.filter((port) => port.object_id === value.id)) {
            inspector.append(html("p", `${p.direction} · ${p.name}`, { class: "li-type" }));
          }
        }
        inspector.append(button("開啟原節點與證據", () => openOriginal(pick)));
      }
    } else {
      inspector.append(
        html("h4", "選一個設備或資料流"),
        html(
          "p",
          "設備只出現一次。選 Run 展開 Session、Prompt 與 P-class；拆解只改變視角，沒有啟動或重新執行。",
        ),
      );
    }
    const list = html("details");
    list.open = true;
    list.append(html("summary", `原始資料流 · ${sceneModel.edges.length}`));
    for (const edge of sceneModel.edges) {
      list.append(
        button(`${stateNames[evidenceState(edge, graph)]} · ${edge.label}`, () =>
          select("edge", edge.id),
        ),
      );
    }
    inspector.append(list);
    const failures = sceneModel.findings.filter((f) => f.status === "failed");
    if (failures.length > 0) {
      const box = html("details");
      box.open = true;
      box.append(html("summary", `✕ ${failures.length} 個已記錄斷點`));
      for (const f of failures) {
        box.append(button(`✕ ${f.message}`, () => openOriginal({ kind: "finding", id: f.id })));
      }
      inspector.append(box);
    }
  }
  render();
  return {
    setSelection(value) {
      if (value && ["object", "node", "edge", "finding"].includes(value.kind)) {
        state.selection = value;
        render();
      } else if (!value && state.selection) {
        state.selection = null;
        render();
      }
    },
  };
}
