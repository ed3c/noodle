import {
  repositoryGroups,
  dirtyName,
  groupView,
  filterRepositories,
  branchName,
  worktreeKey,
} from "./loop-repository.mjs";
import { mechanismFigure } from "./loop-anatomy.mjs";
import { mountComparison } from "./loop-comparison.mjs";

let viewSequence = 0;
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
  const incoming = [];
  const internal = [];
  const outgoing = [];
  for (const edge of scene.edges) {
    const from = ids.has(edge.from_node_id);
    const to = ids.has(edge.to_node_id);
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

/** A native identity plus an execution observation is participation, never acceptance. */
export function observedParticipation(node, runId) {
  return (
    node.run_id === runId &&
    node.identity.status === "observed" &&
    [
      "observed",
      "completed",
      "failed",
      "refused",
      "locally_integrated",
      "recorded",
      "input_recorded",
      "prepared",
      "condition_fixed",
    ].includes(node.status.execution)
  );
}

/** Follow supplied directional relations, retaining their evidence status. No root-cause claim. */
export function traceFinding(scene, finding) {
  const { edges } = scene;
  const seeds = new Set(finding.node_ids);
  for (const edge of edges.filter((candidate) => finding.edge_ids.includes(candidate.id))) {
    seeds.add(edge.from_node_id);
    seeds.add(edge.to_node_id);
  }
  const walk = (reverse) => {
    const nodes = new Set(seeds);
    const found = new Set();
    const queue = [...seeds];
    while (queue.length > 0) {
      const id = queue.shift();
      for (const e of edges) {
        if ((reverse ? e.to_node_id : e.from_node_id) !== id) {
          continue;
        }
        found.add(e.id);
        const next = reverse ? e.from_node_id : e.to_node_id;
        if (!nodes.has(next)) {
          nodes.add(next);
          queue.push(next);
        }
      }
    }
    return { nodes, edges: found };
  };
  const upstream = walk(true);
  const downstream = walk(false);
  return {
    seeds,
    upstream,
    downstream,
    nodes: new Set([...upstream.nodes, ...downstream.nodes]),
    edges: new Set([...upstream.edges, ...downstream.edges]),
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
  { graph, scope, runs, onSelect, onFocus, onRun, onInspect, renderRunCard, view, readArtifact },
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
  let machine;
  let sceneModel;
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
  const readback = html("aside", null, {
    class: "li-readback",
    "aria-label": "Workspace Goal 與執行讀回",
  });
  const journeySlot = html("div", null, { class: "li-journey-slot" });
  readback.append(journeySlot, inspector);
  viewport.prepend(html("h3", "左 / 原執行證據", { class: "lc-actual-title" }));
  const expectedPanel = html("aside", null, { class: "lc-panel", "aria-label": "契約預期與假說" });
  const comparisonReview = html("section", null, {
    class: "lc-review",
    "aria-label": "預期實際比較",
  });
  const reviewArea = html("section", null, { class: "lc-review-area" });
  reviewArea.append(readback, comparisonReview);
  body.append(viewport, expectedPanel);
  state.comparison ??= {};
  const readout = html("p", null, { class: "li-readout", "aria-live": "polite" });
  const legend = html(
    "p",
    "青綠亮起：本次已觀察參與（不等於成功）　金框：目前選取　✕：原失敗　?：缺證據。連線是原資料關係，不是機械管線。",
    { class: "li-legend" },
  );
  root.append(heading, navigation, toolbar, body, readout, legend, reviewArea);

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
    const finding =
      state.selection?.kind === "finding"
        ? sceneModel.findings.find((f) => f.id === state.selection.id)
        : null;
    if (finding) {
      const trace = traceFinding(sceneModel, finding);
      let related = true;
      if (kind === "node") {
        related = trace.nodes.has(id);
      }
      if (kind === "edge") {
        related = trace.edges.has(id);
      }
      if (kind === "object") {
        related = sceneModel.nodes.some((n) => n.object_id === id && trace.nodes.has(n.id));
      }
      group.classList.toggle("li-dim", !related);
      group.classList.toggle("li-endpoint", related);
    }
    if (
      !activeEdge &&
      !finding &&
      ((state.anchor && machine) || state.selection?.kind === "node")
    ) {
      let nodeIds = machine?.related || new Set();
      let edgeIds = new Set((machine?.edges || []).map((e) => e.id));
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
        related = (machine?.findings || []).some((f) => f.id === id);
      }
      group.classList.toggle("li-dim", !related);
      group.classList.toggle("li-endpoint", related);
    }
    if (state.run && ["node", "object"].includes(kind)) {
      const participating = sceneModel.nodes.some(
        (n) =>
          (kind === "node" ? n.id === id : n.object_id === id) &&
          observedParticipation(n, state.run),
      );
      group.classList.toggle("li-participating", participating);
      group.classList.toggle("li-unobserved", !participating);
      group.dataset.participation = participating ? "observed" : "not_observed";
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
    const placements = new Map();
    const objectPoints = new Map();
    const loops = graph.objects.filter((o) => ["outer", "inner"].includes(o.mechanism?.template));
    const shared = graph.objects.filter((o) => !loops.includes(o));
    const label = ([parent, text, x, y, cls = ""]) =>
      parent.append(svg("text", text, { x, y, class: `li-label ${cls}` }));
    const frame = ([x, y, w, h, title, cls = ""]) => {
      const g = svg("g", null, { class: `li-region ${cls}` });
      g.append(svg("rect", null, { x, y, width: w, height: h, rx: 12, class: "li-region-frame" }));
      label([g, title, x + 18, y + 26]);
      drawing.append(g);
      return g;
    };
    const definitions = svg("defs");
    drawing.append(definitions);
    const symbolIds = new Map();
    const art = ([parent, template, x, y, w, h]) => {
      const name = template || "unknown";
      if (!symbolIds.has(name)) {
        const holder = svg("g");
        holder.innerHTML = mechanismFigure(name, { apart: state.apart, angle: state.angle });
        const figure = holder.firstElementChild;
        const id = `loop-machine-${++viewSequence}`;
        const symbol = svg("symbol", null, { id, viewBox: figure.getAttribute("viewBox") });
        symbol.append(...figure.childNodes);
        definitions.append(symbol);
        symbolIds.set(name, id);
      }
      parent.append(
        svg("use", null, {
          href: `#${symbolIds.get(name)}`,
          x,
          y,
          width: w,
          height: h,
          class: "li-machine-art",
          "aria-hidden": "true",
          "data-anatomy-template": name,
        }),
      );
    };
    const machineCard = ([o, x, y, w, h]) => {
      const g = target("object", o.id, `物件 · ${o.label}`);
      g.append(svg("rect", null, { x, y, width: w, height: h, rx: 8, class: "li-card-outline" }));
      art([g, o.mechanism?.template, x + 10, y + 18, w - 20, h - 43]);
      label([g, o.label, x + 12, y + 18, "li-machine-title"]);
      const own = sceneModel.nodes.filter((n) => n.object_id === o.id);
      const observed = own.filter((n) => observedParticipation(n, state.run)).length;
      label([
        g,
        state.run ? `${observed} 本次觀察 / ${own.length} 節點` : `${own.length} 範圍內節點`,
        x + 12,
        y + h - 10,
        "li-small",
      ]);
      objectPoints.set(o.id, [x + w / 2, y + h]);
      drawing.append(g);
    };
    frame([10, 10, 1080, 180, "整體機械 / 原 outer 與 inner Owner", "li-assemblies"]);
    loops.forEach((o, i) => machineCard([o, 25 + i * 530, 48, 515, 132]));
    const sharedRows = Math.ceil(shared.length / 5);
    const sharedHeight = sharedRows * 130 + 40;
    frame([10, 202, 1080, sharedHeight, "共用接口帶 / 唯一 Manager · 契約存在不代表已執行"]);
    shared.forEach((o, i) =>
      machineCard([o, 25 + (i % 5) * 212, 240 + Math.floor(i / 5) * 130, 202, 122]),
    );
    let cursor = 214 + sharedHeight;
    for (const repo of repoGroups) {
      const regionY = cursor;
      const members = sceneModel.nodes.filter((n) => groupFor.get(n.repository_id) === repo.id);
      const rows = state.run ? Math.ceil(members.length / 4) : 1;
      const regionHeight = rows * 178 + 64;
      const g = frame([
        10,
        cursor,
        1080,
        regionHeight,
        `${repo.label} / ${repo.members.length} 來源快照`,
        "li-repo-region",
      ]);
      g.dataset.repoRegion = repo.id;
      if (state.run === null) {
        const r = groupView(repo);
        const targetRepo = target("repository", r.id, `Repo · ${r.label}`);
        targetRepo.append(
          svg("rect", null, {
            x: 24,
            y: cursor + 44,
            width: 1052,
            height: 160,
            rx: 8,
            class: "li-card-outline",
          }),
        );
        label([
          targetRepo,
          `${members.length} 路徑節點 · 選擇 Run 後展開原資料流`,
          45,
          cursor + 85,
        ]);
        label([
          targetRepo,
          `${r.tracked_files.length} 個目錄索引 · 點選區域讀回來源`,
          45,
          cursor + 115,
          "li-small",
        ]);
        drawing.append(targetRepo);
        placements.set(repo.id, [550, cursor + 130]);
      } else {
        members.forEach((n, i) => {
          const x = 26 + (i % 4) * 265;
          const y = regionY + 48 + Math.floor(i / 4) * 178;
          const w = 248;
          const h = 158;
          const node = target("node", n.id, `${n.kind} · ${n.label}`);
          node.append(
            svg("rect", null, { x, y, width: w, height: h, rx: 8, class: "li-card-outline" }),
          );
          art([node, n.mechanism, x + 24, y + 22, w - 48, 105]);
          label([node, n.kind, x + 10, y + 16, "li-machine-title"]);
          label([
            node,
            n.label.length > 32 ? `${n.label.slice(0, 31)}…` : n.label,
            x + 10,
            y + 132,
            "li-small",
          ]);
          label([
            node,
            `${n.status.execution}${n.run_id === state.run ? "" : " · 跨 Run 脈絡"}`,
            x + 10,
            y + 148,
            "li-small",
          ]);
          placements.set(n.id, [x + w / 2, y]);
          drawing.append(node);
        });
      }
      cursor += regionHeight + 16;
    }
    drawing.setAttribute("viewBox", `0 0 1100 ${Math.max(760, cursor + 20)}`);
    if (state.run && state.selection) {
      const selectedNodes = sceneModel.nodes.filter(
        (n) =>
          (state.selection.kind === "object" && n.object_id === state.selection.id) ||
          (state.selection.kind === "node" && n.id === state.selection.id),
      );
      const membership = svg("g", null, {
        class: "li-membership",
        "aria-label": "所屬接口（不是執行交接）",
      });
      for (const node of selectedNodes) {
        const from = objectPoints.get(node.object_id),
          to = placements.get(node.id);
        if (!from || !to) {
          continue;
        }
        membership.append(
          svg("path", null, {
            d: `M${from} C8 ${from[1]},8 ${to[1]},${to}`,
            class: "li-owner-link",
          }),
        );
      }
      drawing.append(membership);
    }
    const routes = svg("g", null, { class: "li-routes" });
    const visible = state.run
      ? sceneModel.edges
      : sceneModel.edges.filter((e) => e.kind === "handoff");
    let lane = 0;
    for (const edge of visible) {
      const from = graph.nodes.find((n) => n.id === edge.from_node_id);
      const to = graph.nodes.find((n) => n.id === edge.to_node_id);
      const a = state.run ? placements.get(from.id) : objectPoints.get(from.object_id);
      const b = state.run ? placements.get(to.id) : objectPoints.get(to.object_id);
      if (!a || !b) {
        continue;
      }
      const start = [a[0], state.run ? a[1] + 158 : a[1]];
      const end = b;
      const side = 18 + (lane++ % 5) * 5;
      const sameRow = Math.abs(a[1] - b[1]) < 5;
      const direction = b[0] >= a[0] ? 1 : -1;
      const tip = sameRow ? [b[0] - direction * 124, b[1] + 80] : end;
      const d = sameRow
        ? `M${a[0] + direction * 124} ${a[1] + 80} C${a[0] + direction * 150} ${a[1] - side},${b[0] - direction * 150} ${b[1] - side},${tip}`
        : `M${start} C${start[0]} ${start[1] + side},${end[0]} ${end[1] - side},${end}`;
      const targetEdge = target("edge", edge.id, `資料流 · ${edge.label}`);
      targetEdge.setAttribute(
        "class",
        `${targetEdge.getAttribute("class")} li-route li-${evidenceState(edge, graph)}`,
      );
      targetEdge.append(
        svg("path", null, { d, class: "li-route-hit" }),
        svg("path", null, { d, class: "li-route-casing" }),
        svg("path", null, { d, class: "li-route-line" }),
      );
      targetEdge.append(
        svg("circle", null, { cx: tip[0], cy: tip[1], r: 3, class: "li-terminal" }),
        svg("path", null, {
          d: sameRow
            ? `M${tip[0] - direction * 7} ${tip[1] - 4} L${tip} L${tip[0] - direction * 7} ${tip[1] + 4}`
            : `M${tip[0] - 4} ${tip[1] - 7} L${tip} L${tip[0] + 4} ${tip[1] - 7}`,
          class: "li-arrow",
        }),
      );
      if (selected("edge", edge.id)) {
        const types = (ids) =>
          [
            ...new Set(
              ids.map(
                (id) => graph.artifacts.find((artifact) => artifact.id === id)?.role || "UNKNOWN",
              ),
            ),
          ].join(" + ") || "未記錄";
        label([
          targetEdge,
          `${types(edge.input_artifact_ids)} → ${types(edge.output_artifact_ids)}`,
          Math.min(a[0], b[0]),
          Math.min(a[1], b[1]) - 8,
          "li-flow-type",
        ]);
      }
      routes.append(targetEdge);
    }
    drawing.append(routes);
    for (const f of sceneModel.findings) {
      const ids = [
        ...f.node_ids,
        ...sceneModel.edges
          .filter((e) => f.edge_ids.includes(e.id))
          .flatMap((e) => [e.from_node_id, e.to_node_id]),
      ];
      const at = ids.map((id) => placements.get(id)).find(Boolean);
      if (!at) {
        continue;
      }
      const marker = target("finding", f.id, `斷點 · ${f.message}`);
      marker.append(
        svg("circle", null, { cx: at[0] + 112, cy: at[1] + 16, r: 11, class: "li-gap-badge" }),
      );
      label([
        marker,
        f.status === "failed" ? "✕" : "?",
        at[0] + 106,
        at[1] + 21,
        f.status === "failed" ? "li-breakpoint" : "li-gap",
      ]);
      drawing.append(marker);
    }
    readout.textContent = `${graph.objects.length} 唯一物件 · ${repoGroups.length} Repo 區域 · ${sceneModel.nodes.length} 路徑節點 · ${sceneModel.hidden} 來源節點按需展開 · ${sceneModel.edges.filter((e) => evidenceState(e, graph) === "current").length} 段目前交接證據完整`;
    size();
    inspect();
    mountComparison(
      { panel: expectedPanel, review: comparisonReview },
      {
        graph,
        runId: state.run,
        pick: state.selection,
        memory: (state.comparison[state.run || "global"] ??= {}),
      },
      {
        onSelect: (pick) => selectItem(pick.kind, pick.id),
        onOriginal: openOriginal,
        readArtifact,
      },
    );
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
    const paths = html("details", null, { "aria-label": "此機械的閉環路徑與缺口" });
    paths.open = !state.run;
    paths.append(html("summary", "此機械的閉環路徑與缺口"));
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
      section.open = state.selection?.kind === "object";
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
    const runHeader = html("details", null, {
      role: "region",
      class: "li-run-summary",
      "aria-label": "本次執行與斷點",
    });
    runHeader.open = !pick;
    runHeader.append(
      html(
        "summary",
        state.run
          ? `本次執行 · ${state.run.slice(0, 12)} · ${sceneModel.findings.length} 個斷點／缺口`
          : "全景 · 請選擇執行",
      ),
    );
    if (state.run) {
      const selectedRun = runs.find((r) => r.id === state.run);
      runHeader.append(
        html(
          "p",
          `${selectedRun?.repository || "Repo UNKNOWN"} · ${selectedRun?.mode || "模式 UNKNOWN"}`,
        ),
      );
      for (const repo of sceneModel.repositories) {
        runHeader.append(
          html(
            "p",
            `${repo.label} · ${branchName(repo)} · ${repo.root} · ${repo.revision || "版本 UNKNOWN"} · ${dirtyName(repo.observation?.dirty)}`,
          ),
        );
      }
      runHeader.append(html("h5", "斷點與缺證據路徑"));
      for (const f of sceneModel.findings) {
        runHeader.append(
          button(`${f.status === "failed" ? "✕" : "?"} ${f.message}`, () =>
            selectItem("finding", f.id),
          ),
        );
      }
      if (sceneModel.findings.length === 0) {
        runHeader.append(html("p", "未記錄斷點；不等於 Goal 已驗收。"));
      }
    }
    inspector.append(runHeader);
    machineInspector();
    if (value) {
      inspector.append(html("h4", value.label || value.message), html("code", value.id));
      if (pick.kind === "finding") {
        const trace = traceFinding(sceneModel, value);
        inspector.append(
          html(
            "p",
            `${value.status} · 上游 ${trace.upstream.nodes.size - trace.seeds.size} 節點 / 下游 ${trace.downstream.nodes.size - trace.seeds.size} 節點。依已記錄關係定位，不宣稱全局根因。`,
          ),
        );
        inspector.append(
          html(
            "p",
            `原續行：${value.original_next ? JSON.stringify(value.original_next) : "未提供；不另造執行命令"}`,
          ),
        );
        for (const id of value.artifact_ids) {
          const a = graph.artifacts.find((artifact) => artifact.id === id);
          inspector.append(html("p", `來源：${a?.source?.path || id} · ${a?.status || "unknown"}`));
        }
        if (trace.seeds.size === 0) {
          inspector.append(
            html("p", "原來源未綁定節點或連線；缺口保留在 Run 層，不能猜一個機械掛上去。"),
          );
        }
        inspector.append(button("讀回原斷點、來源與續行", () => openOriginal(pick)));
      } else if (pick.kind === "edge") {
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
    setJourney(element) {
      journeySlot.replaceChildren(element);
    },
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
