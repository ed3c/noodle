import {
  repositoryGroups,
  dirtyName,
  groupView,
  filterRepositories,
  branchName,
  worktreeKey,
} from "./loop-repository.mjs";
import {
  slabOf,
  iso,
  fitProjection,
  boxCorners,
  depthOf,
  planOutline,
} from "./anatomy/iso-kit.mjs";

// Low-detail silhouettes share the producer's functional classification with anatomy.
// These are mechanical symbols; none of their parts or contacts assert an execution.
const silhouettes = {
  outer: [
    [0, 0, 65, 8, 0, 5],
    [0, 36, 65, 8, 0, 5],
    [0, 8, 8, 28, 0, 12],
    [57, 8, 8, 28, 0, 12],
    [23, 10, 20, 25, 0, 9],
  ],
  inner: [
    [0, 0, 8, 44, 0, 28],
    [57, 0, 8, 44, 0, 28],
    [0, 0, 65, 8, 28, 6],
    [28, 4, 10, 8, 10, 18],
    [15, 16, 35, 25, 0, 7],
  ],
  router: [
    [0, 0, 65, 44, 0, 5],
    [5, 5, 12, 12, 5, 5],
    [26, 5, 12, 12, 5, 5],
    [47, 5, 12, 12, 5, 5],
    [5, 26, 12, 12, 5, 5],
    [26, 26, 12, 12, 5, 5],
    [47, 26, 12, 12, 5, 5],
  ],
  gauge: [
    [0, 0, 8, 40, 0, 25],
    [57, 0, 8, 40, 0, 25],
    [0, 0, 65, 8, 25, 6],
    [21, 5, 5, 8, 8, 17],
    [40, 5, 5, 8, 8, 17],
    [16, 18, 35, 20, 0, 5],
  ],
  testbench: [
    [0, 0, 65, 44, 0, 6],
    [5, 2, 55, 6, 6, 20],
    [8, 8, 6, 8, 10, 14],
    [30, 8, 6, 8, 10, 14],
    [52, 8, 6, 8, 10, 14],
    [10, 25, 45, 13, 6, 5],
  ],
  trigger: [
    [0, 0, 65, 44, 0, 7],
    [17, 10, 31, 24, 7, 9],
    [29, 18, 7, 7, 16, 17],
    [21, 13, 23, 17, 33, 4],
  ],
  clock: [
    [0, 0, 65, 44, 0, 7],
    [5, 4, 9, 34, 7, 8],
    [16, 4, 9, 34, 7, 16],
    [27, 4, 9, 34, 7, 24],
    [38, 4, 9, 34, 7, 16],
    [49, 4, 9, 34, 7, 8],
  ],
  reservoir: [
    [0, 0, 27, 44, 0, 30],
    [33, 3, 32, 8, 0, 8],
    [33, 18, 32, 8, 0, 8],
    [33, 33, 32, 8, 0, 8],
    [49, 1, 12, 12, 8, 7],
    [49, 16, 12, 12, 8, 7],
    [49, 31, 12, 12, 8, 7],
  ],
  index: [
    [0, 0, 65, 44, 0, 6],
    [0, 0, 65, 44, 8, 6],
    [0, 0, 65, 44, 16, 6],
    [0, 0, 65, 44, 24, 6],
  ],
  meter: [
    [0, 0, 65, 44, 0, 5],
    [4, 4, 16, 26, 5, 14],
    [25, 4, 16, 26, 5, 14],
    [46, 4, 16, 26, 5, 14],
  ],
  dock: [
    [0, 0, 23, 44, 0, 20],
    [42, 0, 23, 44, 0, 20],
    [23, 8, 19, 28, 0, 5],
    [6, 8, 11, 28, 20, 4],
    [48, 8, 11, 28, 20, 4],
  ],
  reader: [
    [0, 0, 7, 44, 0, 25],
    [58, 0, 7, 44, 0, 25],
    [0, 0, 65, 8, 25, 5],
    [8, 8, 49, 30, 4, 3],
  ],
};

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
export function projectConsole(
  graph,
  scope,
  { runId = null, sources = false, repositoryIds = null } = {},
) {
  let nodes = scope.nodes.filter(
    (n) =>
      runId === null ||
      n.run_id === runId ||
      scope.edges.some((e) => e.run_id === runId && [e.from_node_id, e.to_node_id].includes(n.id)),
  );
  let eligible = scope.edges.filter((e) => runId === null || e.run_id === runId);
  if (repositoryIds) {
    const own = new Set(
      nodes.filter((n) => repositoryIds.includes(n.repository_id)).map((n) => n.id),
    );
    eligible = eligible.filter((e) => own.has(e.from_node_id) || own.has(e.to_node_id));
    const related = new Set(eligible.flatMap((e) => [e.from_node_id, e.to_node_id]));
    nodes = nodes.filter((n) => own.has(n.id) || related.has(n.id));
  }
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
        scope.rows.some((r) => r.run_id === f.run_id) &&
        (runId === null || f.run_id === runId) &&
        (!repositoryIds ||
          f.node_ids.some((id) => nodes.some((n) => n.id === id)) ||
          nodes.some((n) => n.run_id === f.run_id)),
    ),
  };
}

/** Partition only supplied occurrences and edges; capability descriptions are not events. */
export function inspectMachine(scene, objectId) {
  const nodes = scene.nodes.filter((n) => n.object_id === objectId);
  const ids = new Set(nodes.map((n) => n.id));
  const incoming = [],
    internal = [],
    outgoing = [];
  for (const edge of scene.edges) {
    const from = ids.has(edge.from_node_id),
      to = ids.has(edge.to_node_id);
    if (from && to) {
      internal.push(edge);
    } else if (to) {
      incoming.push(edge);
    } else if (from) {
      outgoing.push(edge);
    }
  }
  const edges = [...incoming, ...outgoing, ...internal];
  return {
    nodes,
    incoming,
    outgoing,
    internal,
    edges,
    related: new Set([...ids, ...edges.flatMap((e) => [e.from_node_id, e.to_node_id])]),
    findings: scene.findings.filter((f) => f.node_ids.some((id) => ids.has(id))),
  };
}

const stateNames = {
  current: "目前交接證據完整",
  historical: "歷史記錄／目前效力未證實",
  unknown: "UNKNOWN／來源關係",
  failed: "失敗",
};

export function mountIsometric(
  root,
  { graph, scope, runs, onSelect, onFocus, onRun, onInspect, renderRunCard, view },
) {
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
  let machine, projection, sceneModel;
  state.anchor ??= null;
  state.onlyMachine ??= false;
  state.filter ??= { repo: "", branch: "", worktree: "", snapshot: "" };
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
    onRun?.(state.run);
  });
  const navigation = html("div", null, { class: "li-toolbar li-repository-nav" });
  const filters = {};
  for (const [key, label] of [
    ["repo", "Repo"],
    ["branch", "Branch"],
    ["worktree", "Worktree"],
    ["snapshot", "快照"],
  ]) {
    const select = html("select", null, { "aria-label": `來源${label}篩選` });
    select.addEventListener("change", () => {
      state.filter[key] = select.value;
      const order = ["repo", "branch", "worktree", "snapshot"];
      for (const child of order.slice(order.indexOf(key) + 1)) {
        state.filter[child] = "";
      }
      state.run = null;
      run.value = "";
      state.selection = null;
      state.anchor = null;
      state.returnView = null;
      render();
      onRun?.(null);
    });
    filters[key] = select;
    const field = html("label", `${label} `);
    field.append(select);
    navigation.append(field);
  }
  function updateFilters() {
    const available = graph.repositories.filter((r) =>
      scope.nodes.some((n) => n.repository_id === r.id),
    );
    const groups = repositoryGroups(available);
    const repos = state.filter.repo
      ? groups.find((g) => g.id === state.filter.repo)?.members || []
      : available;
    const branched = repos.filter(
      (r) => !state.filter.branch || branchName(r) === state.filter.branch,
    );
    const trees = branched.filter(
      (r) => !state.filter.worktree || worktreeKey(r) === state.filter.worktree,
    );
    const options = {
      repo: groups.map((g) => [g.id, `${g.label} · ${g.members.length} 來源快照`]),
      branch: [...new Set(repos.map((r) => branchName(r)))].map((b) => [b, b]),
      worktree: [
        ...new Map(
          branched.map((r) => [
            worktreeKey(r),
            [worktreeKey(r), `${r.root} · ${worktreeKey(r).slice(-8)}`],
          ]),
        ).values(),
      ],
      snapshot: trees.map((r) => [
        r.id,
        `${r.revision?.slice(0, 10) || "UNKNOWN"} · ${r.observation?.observed_at || "時間 UNKNOWN"} · ${dirtyName(r.observation?.dirty)}`,
      ]),
    };
    for (const [key, select] of Object.entries(filters)) {
      select.replaceChildren(html("option", "全部", { value: "" }));
      for (const [value, label] of options[key]) {
        select.append(html("option", label, { value }));
      }
      select.value = state.filter[key];
    }
  }
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
      Object.assign(state, {
        filter: { repo: "", branch: "", worktree: "", snapshot: "" },
        run: null,
        selection: null,
        zoom: 1,
        angle: 42,
        apart: false,
        anchor: null,
        returnView: null,
        onlyMachine: false,
      });
      run.value = "";
      render();
      onRun?.(null);
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
  root.append(heading, navigation, toolbar, body, readout, legend);

  function size() {
    drawing.style.width = `${state.zoom * 100}%`;
  }
  function selectItem(kind, id) {
    if (kind === "object" && state.anchor !== id) {
      state.anchor = id;
      state.onlyMachine = false;
      state.returnView = {
        run: state.run,
        sources: state.sources,
        apart: state.apart,
        angle: state.angle,
        zoom: state.zoom,
        filter: { ...state.filter },
      };
    }
    state.selection = { kind, id };
    render();
    onInspect?.(state.selection);
  }
  function expandRun(id) {
    state.run = id;
    run.value = id || "";
    state.selection = state.anchor ? { kind: "object", id: state.anchor } : null;
    render();
    onRun?.(id);
    onInspect?.(state.selection);
  }
  function returnToMachine() {
    if (state.returnView) {
      Object.assign(state, state.returnView, { filter: { ...state.returnView.filter } });
    }
    state.onlyMachine = false;
    state.selection = state.anchor ? { kind: "object", id: state.anchor } : null;
    run.value = state.run || "";
    source.checked = state.sources;
    render();
    onRun?.(state.run);
    onInspect?.(state.selection);
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
  function drawLabel(group, text, { point, cls = "", offset = 0 }) {
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
    if (!activeEdge && state.anchor && machine) {
      let nodeIds = machine.related;
      let edgeIds = new Set(machine.edges.map((e) => e.id));
      if (state.selection?.kind === "node") {
        const adjacent = sceneModel.edges.filter((e) =>
          [e.from_node_id, e.to_node_id].includes(state.selection.id),
        );
        edgeIds = new Set(adjacent.map((e) => e.id));
        nodeIds = new Set([
          state.selection.id,
          ...adjacent.flatMap((e) => [e.from_node_id, e.to_node_id]),
        ]);
      } else if (state.selection?.kind === "finding") {
        nodeIds = new Set(graph.findings.find((f) => f.id === state.selection.id)?.node_ids || []);
        edgeIds = new Set();
      }
      let related = true;
      if (kind === "node") {
        related = nodeIds.has(id);
      } else if (kind === "edge") {
        related = edgeIds.has(id);
      } else if (kind === "object") {
        related =
          id === state.anchor ||
          sceneModel.nodes.some((n) => nodeIds.has(n.id) && n.object_id === id);
      } else if (kind === "finding") {
        related = machine.findings.some((f) => f.id === id);
      }
      group.classList.toggle("li-dim", !related);
      group.classList.toggle("li-endpoint", related);
    }
    group.append(svg("title", title));
    action(group, () => selectItem(kind, id));
    return group;
  }
  function render() {
    source.disabled = !state.run;
    updateFilters();
    const filtered = Object.values(state.filter).some(Boolean);
    sceneModel = projectConsole(graph, scope, {
      runId: state.run,
      sources: state.sources,
      repositoryIds: filtered
        ? filterRepositories(graph.repositories, state.filter).map((r) => r.id)
        : null,
    });
    machine = state.anchor ? inspectMachine(sceneModel, state.anchor) : null;
    if (state.onlyMachine && state.run && machine) {
      sceneModel = {
        ...sceneModel,
        nodes: sceneModel.nodes.filter((n) => machine.related.has(n.id)),
        edges: machine.edges,
        findings: machine.findings,
      };
    }
    const repoGroups = repositoryGroups(sceneModel.repositories);
    const groupFor = new Map(repoGroups.flatMap((g) => g.members.map((r) => [r.id, g.id])));
    drawing.replaceChildren();
    explode.textContent = state.apart ? "收合資料載體" : "拆解資料載體";
    explode.setAttribute("aria-pressed", String(state.apart));
    const items = [];
    const placements = new Map();
    const objects = [...graph.objects].toSorted((a, b) => a.namespace.localeCompare(b.namespace));
    const cols = state.run ? 3 : 4;
    const rackRows = Math.ceil((state.run ? sceneModel.nodes.length : repoGroups.length) / cols);
    const machineDepth = Math.ceil(objects.length / 4) * 155 + 75;
    const worldDepth = machineDepth + Math.max(1, rackRows) * 160;
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
      const roof = 32;
      const portAnchor = [x + 65, y + 42, roof + 5];
      objectPoints.set(object.id, portAnchor);
      add([x + 65, y + 42, z], () => {
        const g = target("object", object.id, `物件 · ${object.label}`);
        solid(g, { plan: { x, y, w: 140, d: 86 }, z: 0, h: 8, cls: "li-foot" });
        solid(g, { plan: { x: x + 5, y: y + 5, w: 130, d: 76 }, z: 8, h: roof - 13, cls: "" });
        solid(g, { plan: { x: x + 3, y: y + 3, w: 134, d: 80 }, z: roof - 5, h: 5, cls: "li-cap" });
        const parts = silhouettes[object.mechanism?.template] || [];
        for (const [dx, dy, w, d, lift, h] of parts.toSorted(
          (a, b) =>
            depthOf([a[0] + a[2] / 2, a[1] + a[3] / 2, a[4] + a[5] / 2], projection) -
            depthOf([b[0] + b[2] / 2, b[1] + b[3] / 2, b[4] + b[5] / 2], projection),
        )) {
          solid(g, {
            plan: { x: x + 36 + dx, y: y + 17 + dy, w, d },
            z: roof + lift,
            h,
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
        drawLabel(g, object.label, { point: [x + 70, y + 45, roof + 56], cls: "", offset: 0 });
        drawLabel(g, `${scope.nodes.filter((n) => n.object_id === object.id).length} 節點`, {
          point: [x + 70, y + 45, roof + 56],
          cls: "li-small",
          offset: 15,
        });
        return g;
      });
    });
    const members = state.run ? sceneModel.nodes : repoGroups.map((g) => groupView(g));
    members.forEach((item, i) => {
      const x = (i % cols) * (760 / cols),
        y = machineDepth + Math.floor(i / cols) * 160;
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
        if (state.run) {
          const count = { session: 1, pclass: 5, cli: 3, observation: 2 }[item.kind];
          for (let j = 0; j < count; j++) {
            solid(g, {
              plan: {
                x: x + 20 + j * 20,
                y: y + 20,
                w: item.kind === "session" ? w - 40 : 12,
                d: item.kind === "session" ? 5 : 16,
              },
              z: top + 4,
              h: item.kind === "session" ? 20 : 8,
              cls: "li-port",
            });
          }
        }
        drawLabel(g, state.run ? item.kind : item.label, {
          point: [x + w / 2, y + 43, top + 8],
          cls: "",
          offset: 0,
        });
        drawLabel(
          g,
          state.run
            ? item.label.slice(0, 30)
            : `${sceneModel.nodes.filter((n) => groupFor.get(n.repository_id) === item.id).length} 節點 · ${item.revision?.slice(0, 8) || "版本未知"}`,
          { point: [x + w / 2, y + 43, top + 8], cls: "li-small", offset: 15 },
        );
        if (state.run) {
          drawLabel(
            g,
            `${graph.repositories.find((r) => r.id === item.repository_id)?.label || "Repo"} · ${item.status.execution}`,
            { point: [x + w / 2, y + 43, top + 8], cls: "li-small", offset: 29 },
          );
        } else {
          drawLabel(g, `${item.tracked_files.length} 個目錄索引`, {
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
        pair = [groupFor.get(from.repository_id), groupFor.get(to.repository_id)];
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
      if (selected("edge", edge.id)) {
        const typeNames = (ids) =>
          [
            ...new Set(
              ids.map(
                (id) => graph.artifacts.find((artifact) => artifact.id === id)?.role || "UNKNOWN",
              ),
            ),
          ].join(" + ") || "未記錄";
        g.append(
          svg(
            "text",
            `${typeNames(edge.input_artifact_ids)} → ${typeNames(edge.output_artifact_ids)}`,
            {
              x: (start[0] + end[0]) / 2,
              y: (start[1] + end[1]) / 2 - lift - 12,
              class: "li-label li-flow-type",
              "text-anchor": "middle",
            },
          ),
        );
      }
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
    readout.textContent = `${graph.objects.length} 唯一物件 · ${repoGroups.length} Repo 區域 · ${sceneModel.nodes.length} 路徑節點 · ${sceneModel.hidden} 來源節點按需展開 · ${sceneModel.edges.filter((e) => evidenceState(e, graph) === "current").length} 段目前交接證據完整`;
    size();
    inspect();
  }
  function machineInspector() {
    const owner = graph.objects.find((o) => o.id === state.anchor);
    if (!owner) {
      return;
    }
    const nav = html("nav", null, { "aria-label": "機械檢視導航" });
    nav.append(button(owner.label, () => selectItem("object", owner.id)));
    if (state.run) {
      nav.append(
        html("span", " → "),
        button(`Run ${state.run.slice(0, 12)}`, () => expandRun(state.run)),
      );
    }
    if (state.selection && state.selection.kind !== "object") {
      const record = (
        { node: graph.nodes, edge: graph.edges, finding: graph.findings }[state.selection.kind] ||
        []
      ).find((r) => r.id === state.selection.id);
      nav.append(html("span", ` → ${record?.label || record?.message || state.selection.kind}`));
    }
    nav.append(button("返回機械全景", returnToMachine));
    inspector.append(nav);
    const paths = html("section", null, { "aria-label": "此機械的閉環路徑與缺口" });
    paths.append(html("h5", "01 / 此機械的閉環路徑與缺口"));
    const runIds = new Set(machine.nodes.map((n) => n.run_id));
    for (const edge of machine.edges) {
      runIds.add(edge.run_id);
    }
    const rows = scope.rows.filter(
      (r) => runIds.has(r.run_id) && (!state.run || r.run_id === state.run),
    );
    for (const row of rows) {
      if (renderRunCard) {
        paths.append(renderRunCard(row, () => expandRun(row.run_id)));
      }
    }
    if (rows.length === 0) {
      paths.append(html("p", "此範圍沒有已記錄節點；能力契約不代表已執行。"));
    }
    inspector.append(paths);
    if (!state.run) {
      inspector.append(html("p", "請選一個 Run 展開節點與資料流；不混合不同執行。"));
      return;
    }
    const toggle = button(state.onlyMachine ? "顯示完整 Run 脈絡" : "只看此機械", () => {
      state.onlyMachine = !state.onlyMachine;
      render();
    });
    toggle.setAttribute("aria-pressed", String(state.onlyMachine));
    inspector.append(toggle, html("p", "跨 Run 端點僅作交接脈絡，不擴大 Goal 驗收範圍。"));
    const types = (ids) =>
      [
        ...new Set(
          ids.map((id) => {
            const a = graph.artifacts.find((artifact) => artifact.id === id);
            return a ? `${a.role} / ${a.format || "未知型態"}` : "未知來源";
          }),
        ),
      ].join(" + ") || "未記錄";
    for (const [label, values, kind] of [
      ["此機械的節點", machine.nodes, "node"],
      ["流入", machine.incoming, "edge"],
      ["流出", machine.outgoing, "edge"],
      ["內部轉換", machine.internal, "edge"],
      ["斷點", machine.findings, "finding"],
    ]) {
      const section = html("details", null, { "aria-label": label });
      section.open = true;
      section.append(html("summary", `${label} · ${values.length}`));
      for (const value of values) {
        const b = button(value.label || value.message, () => selectItem(kind, value.id));
        b.setAttribute("aria-pressed", String(selected(kind, value.id)));
        section.append(b);
        if (kind === "edge") {
          section.append(
            html(
              "p",
              `${types(value.input_artifact_ids)} → ${types(value.output_artifact_ids)} · ${stateNames[evidenceState(value, graph)]}`,
            ),
          );
        }
        if (kind === "node") {
          section.append(
            html(
              "p",
              `${value.kind} · ${value.status.execution} · Run ${value.run_id.slice(0, 12)}${value.run_id === state.run ? "" : " · 跨界脈絡"}`,
            ),
          );
        }
      }
      inspector.append(section);
    }
  }
  function inspect() {
    inspector.replaceChildren();
    const pick = state.selection;
    const collection = {
      object: graph.objects,
      node: graph.nodes,
      edge: graph.edges,
      repository: repositoryGroups(graph.repositories).map((g) => groupView(g)),
      finding: graph.findings,
    };
    const value = pick && collection[pick.kind]?.find((v) => v.id === pick.id);
    inspector.append(html("p", "INSPECT / 原來源讀回", { class: "li-kicker" }));
    machineInspector();
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
        const nodes = scope.nodes.filter((n) =>
          value.members.some((r) => r.id === n.repository_id),
        );
        inspector.append(
          html("p", `${value.members.length} 來源快照；Git 觀察不代表歷史執行版本。`),
          button("選取此 Repo", () => {
            state.filter = { repo: value.id, branch: "", worktree: "", snapshot: "" };
            render();
          }),
          button("定位目錄與區域 DAG", () =>
            onFocus({ node_ids: nodes.map((n) => n.id), edge_ids: [] }),
          ),
        );
        for (const repo of value.members) {
          const o = repo.observation;
          inspector.append(
            html(
              "p",
              `${branchName(repo)} · ${repo.revision || "UNKNOWN"} · ${dirtyName(o?.dirty)} · ${o?.observed_at || "觀察時間 UNKNOWN"}`,
            ),
          );
        }
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
        if (["node", "object"].includes(pick.kind)) {
          const panel = html("section", "載入立體詳圖…", { "aria-label": "節點 anatomy 詳圖" });
          inspector.append(
            button("詳圖放大／全景定位", () => {
              root.classList.toggle("li-detail-open");
              inspector.scrollTop = panel.offsetTop - inspector.offsetTop;
            }),
            panel,
          );
          import("./loop-anatomy.mjs")
            .then(({ anatomyModel, mountAnatomy }) => {
              if (!panel.isConnected) {
                return null;
              }
              const model = anatomyModel(graph, pick, sceneModel.nodes);
              if (model) {
                mountAnatomy(panel, model, {
                  onSelect: ({ kind, id }) => selectItem(kind, id),
                  onOriginal: (chosen) => openOriginal(chosen?.kind ? chosen : pick),
                });
              }
              return null;
            })
            .catch((error) => {
              if (panel.isConnected) {
                panel.textContent = `詳圖載入失敗：${error.message}`;
              }
            });
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
    if (state.anchor) {
      return;
    }
    const list = html("details");
    list.open = true;
    list.append(html("summary", `原始資料流 · ${sceneModel.edges.length}`));
    for (const edge of sceneModel.edges) {
      list.append(
        button(`${stateNames[evidenceState(edge, graph)]} · ${edge.label}`, () =>
          selectItem("edge", edge.id),
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
    setRun(id) {
      expandRun(id);
    },
    syncRun(id) {
      state.run = id;
      run.value = id || "";
      render();
    },
    setSelection(value) {
      if (value && ["object", "node", "edge", "finding"].includes(value.kind)) {
        if (value.kind === "object" && state.anchor !== value.id) {
          state.anchor = value.id;
          state.onlyMachine = false;
          state.returnView = {
            run: state.run,
            sources: state.sources,
            apart: state.apart,
            angle: state.angle,
            zoom: state.zoom,
            filter: { ...state.filter },
          };
        }
        state.selection = value;
        render();
      } else if (!value && state.selection) {
        state.selection = null;
        render();
      }
    },
  };
}
