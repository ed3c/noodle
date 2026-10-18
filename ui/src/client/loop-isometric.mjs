import {
  repositoryGroups,
  dirtyName,
  groupView,
  filterRepositories,
  branchName,
  worktreeKey,
} from "./loop-repository.mjs";
import { layoutGraph } from "./loop-graph-layout.mjs";
import { mechanismFigure } from "./loop-anatomy.mjs";
import { contractFacets, expectedScene, principleFigure } from "./loop-contract.mjs";
import { focusExpectations, partitionMachines, focusedLayout } from "./loop-scene-focus.mjs";
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

/** Evidence of transport is separate from failure, freshness and semantic acceptance. */
export function observedFlow(edge, graph) {
  if (!["observed", "failed"].includes(edge.status)) {
    return false;
  }
  return ["current", "historical"].includes(evidenceState({ ...edge, status: "observed" }, graph));
}

function isNativeTool(graph, node) {
  return node?.output_artifact_ids?.some((id) =>
    graph.artifacts.some(
      (a) => a.id === id && ["native_tool_call", "native_tool_result"].includes(a.role),
    ),
  );
}

/** Geometry is a view of original IDs. It never adds an execution or a relationship. */
export function projectConsole(
  graph,
  scope,
  { runId = null, sources = false, events = false, selection = null, repositoryIds = null } = {},
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
  const selectedIds = new Set(
    selection?.kind === "finding"
      ? graph.findings.find((f) => f.id === selection.id)?.node_ids || []
      : [],
  );
  if (selection?.kind === "node") {
    selectedIds.add(selection.id);
  }
  if (selection?.kind === "edge") {
    const edge = eligible.find((e) => e.id === selection.id);
    if (edge) {
      selectedIds.add(edge.from_node_id);
      selectedIds.add(edge.to_node_id);
    }
  }
  const revealed = new Set(selectedIds);
  for (const edge of eligible) {
    if (
      edge.kind === "handoff" &&
      (selectedIds.has(edge.from_node_id) || selectedIds.has(edge.to_node_id))
    ) {
      revealed.add(edge.from_node_id);
      revealed.add(edge.to_node_id);
    }
  }
  const visible = nodes.filter(
    (n) =>
      (events || revealed.has(n.id) || !isNativeTool(graph, n)) &&
      (sources ||
        endpoints.has(n.id) ||
        !["available_reference", "selected"].includes(n.status.execution)),
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
  {
    graph,
    goal,
    scope,
    runs,
    onSelect,
    onFocus,
    onRun,
    onInspect,
    renderRunCard,
    view,
    readArtifact,
    onReviewSelection,
  },
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
  state.events ??= false;
  state.anchor ??= null;
  state.onlyMachine ??= false;
  state.fullOverlay ??= false;
  state.fullArchitecture ??= false;
  state.sceneHeight ??= 360;
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
    state.events = false;
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
  const overlayToggle = button("完整疊圖", () => {
    state.fullOverlay = !state.fullOverlay;
    render();
  });
  const architectureToggle = button("完整架構", () => {
    state.fullArchitecture = !state.fullArchitecture;
    render();
  });
  const clearPick = button("返回本次路徑", () => {
    state.selection = null;
    state.anchor = null;
    state.onlyMachine = false;
    render();
    onInspect?.(null);
  });
  const heightControl = html("input", null, {
    type: "range",
    min: 220,
    max: 620,
    step: 20,
    "aria-label": "立體圖高度",
  });
  heightControl.value = state.sceneHeight;
  heightControl.addEventListener("input", () => {
    state.sceneHeight = Number(heightControl.value);
    root.style.setProperty("--li-scene-height", `${state.sceneHeight}px`);
  });
  const heightLabel = html("label", "圖高度 ", { class: "li-height-control" });
  heightLabel.append(heightControl);
  toolbar.append(
    run,
    overlayToggle,
    architectureToggle,
    clearPick,
    heightLabel,
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
  const viewOptions = html("details", null, { class: "li-view-options" });
  const viewControls = html("div", null, { class: "li-toolbar" });
  viewOptions.append(html("summary", "視角與來源"), viewControls);
  for (const control of [...toolbar.children].slice(6)) {
    viewControls.append(control);
  }
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
  viewport.prepend(
    html("h3", "同源全景 / 原執行證據 ＋ 契約預期 ＋ 可推翻假說", { class: "lc-actual-title" }),
  );
  const expectedPanel = html("aside", null, { class: "lc-panel", "aria-label": "契約預期與假說" });
  const comparisonReview = html("section", null, {
    class: "lc-review",
    "aria-label": "預期實際比較",
  });
  const reviewArea = html("section", null, { class: "lc-review-area" });
  const sourceDetails = html("details", null, { class: "li-expectation-sources" });
  sourceDetails.append(html("summary", "契約與假說來源索引"), expectedPanel);
  readback.append(comparisonReview, sourceDetails);
  reviewArea.append(readback);
  body.append(viewport, reviewArea);
  state.comparison ??= {};
  const readout = html("p", null, { class: "li-readout", "aria-live": "polite" });
  const legend = html(
    "p",
    "綠線：有原交接證據（不等於驗收通過） · 黃實線：契約預期 · 黃虛線：可推翻假說 · 白框：選取 · ✕：已確認斷點 · ?：缺證據。舊證據另標歷史。",
    { class: "li-legend" },
  );
  const location = html("p", null, { class: "li-position", "aria-live": "polite" });
  const compareSize = button("放大／返回審查", () => root.classList.toggle("li-review-expanded"));
  const positionBar = html("div", null, { class: "li-position-bar" });
  positionBar.append(location, compareSize);
  root.append(heading, navigation, toolbar, viewOptions, positionBar, body, readout, legend);
  root.style.setProperty("--li-scene-height", `${state.sceneHeight}px`);
  let expectedModel = null;
  let expectedKey = "";
  let expectedRun = state.run;

  function size() {
    drawing.style.width = `${state.zoom * 100}%`;
  }
  function selectItem(kind, id) {
    if (
      (kind === "object" || kind === "principle") &&
      state.anchor !== (kind === "principle" ? id.split("/")[0] : id)
    ) {
      state.anchor = kind === "principle" ? id.split("/")[0] : id;
      state.onlyMachine = false;
      state.returnView = {
        run: state.run,
        sources: state.sources,
        events: state.events,
        apart: state.apart,
        angle: state.angle,
        zoom: state.zoom,
        fullOverlay: state.fullOverlay,
        fullArchitecture: state.fullArchitecture,
        scrollTop: viewport.scrollTop,
        scrollLeft: viewport.scrollLeft,
        filter: { ...state.filter },
      };
    }
    state.selection = { kind, id };
    if (kind === "principle" && readArtifact) {
      const owner = graph.objects.find((o) => o.id === id.split("/")[0]);
      state.facetContents ??= {};
      state.facetErrors ??= {};
      const refs = graph.artifacts.filter(
        (a) =>
          owner?.source_artifact_ids?.includes(a.id) &&
          a.role === "mechanism_contract" &&
          !a.content &&
          !a.public_content &&
          !Object.hasOwn(state.facetContents, a.id) &&
          !state.facetErrors[a.id],
      );
      if (refs.length > 0) {
        Promise.all(
          refs.map(async (a) => {
            try {
              state.facetContents[a.id] = await readArtifact(a.id);
            } catch (error) {
              state.facetErrors[a.id] = String(error.message || error);
            }
          }),
        )
          .then(() => {
            if (root.isConnected && state.selection?.kind === kind && state.selection?.id === id) {
              render();
            }
            return null;
          })
          .catch((error) => {
            inspector.append(html("p", `分類讀回失敗：${error.message}`));
          });
      }
    }
    render();
    readback.scrollTop = inspector.offsetTop - readback.offsetTop;
    if (!["principle", "expected", "expected-route"].includes(kind)) {
      onInspect?.(state.selection);
    }
  }
  function expandRun(id) {
    if (state.run !== id) {
      state.events = false;
    }
    state.run = id;
    expectedModel = null;
    expectedKey = "";
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
    viewport.scrollTop = state.returnView?.scrollTop || 0;
    viewport.scrollLeft = state.returnView?.scrollLeft || 0;
  }
  function facetGraph() {
    return {
      ...graph,
      artifacts: graph.artifacts.map((a) =>
        state.facetContents?.[a.id] ? { ...a, content: state.facetContents[a.id] } : a,
      ),
    };
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
    if (expectedRun !== state.run) {
      expectedModel = null;
      expectedKey = "";
      expectedRun = state.run;
    }
    source.disabled = !state.run;
    updateFilters();
    const filtered = Object.values(state.filter).some(Boolean);
    sceneModel = projectConsole(graph, scope, {
      runId: state.run,
      sources: state.sources,
      events: state.events,
      selection: state.selection,
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
    const label = ([parent, text, x, y, cls = ""]) =>
      parent.append(svg("text", text, { x, y, class: `li-label ${cls}` }));
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
      g.classList.add("li-station");
      g.append(
        svg("path", null, {
          d: `M${x},${y + h - 22} l${w - 24},-14 l24,14 l-${w - 24},14 Z`,
          class: "li-station-base li-card-outline",
        }),
      );
      art([g, o.mechanism?.template, x + 10, y + 24, w - 20, h - 48]);
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
    const allExpected = expectedScene(graph, expectedModel);
    const overlay = focusExpectations(allExpected, {
      full: state.fullOverlay,
      selection: state.selection,
      sourceStatus: expectedModel?.comparison?.source_status,
    });
    overlayToggle.textContent = state.fullOverlay
      ? "收合完整疊圖"
      : `完整疊圖 · ${overlay.hidden} 條收合`;
    overlayToggle.setAttribute("aria-pressed", String(state.fullOverlay));
    architectureToggle.textContent = state.fullArchitecture ? "本次參與優先" : "完整架構";
    architectureToggle.setAttribute("aria-pressed", String(state.fullArchitecture));
    const machines = partitionMachines(graph.objects, sceneModel, {
      overlay,
      run: state.run,
      full: state.fullArchitecture,
    });
    const ranks = state.run
      ? new Map(
          layoutGraph({
            nodes: sceneModel.nodes,
            edges: sceneModel.edges
              .filter((e) => e.kind === "handoff")
              .map((e) => ({ from: e.from_node_id, to: e.to_node_id })),
          }).nodes.map((n) => [n.id, n.rank]),
        )
      : new Map();
    const visibleNodes = state.run
      ? sceneModel.nodes.toSorted(
          (a, b) => ranks.get(a.id) - ranks.get(b.id) || a.id.localeCompare(b.id),
        )
      : [];
    const visibleIds = new Set(visibleNodes.map((n) => n.id));
    const unmatched = overlay.steps.filter(
      (step) =>
        !(step.matches.length === 1 && visibleIds.has(step.matches[0])) &&
        overlay.routes.some((r) => r.from === step.id || r.to === step.id),
    );
    const items = state.run ? visibleNodes : repoGroups;
    const layout = focusedLayout(
      machines.primary.length,
      machines.index.length,
      items.length + unmatched.length,
    );
    const floor = svg("g", null, { class: "li-workspace-floor", "aria-hidden": "true" });
    const { width, height } = layout;
    floor.append(
      svg("path", null, {
        d: `M20,90 L${width - 100},20 L${width - 20},${height - 90} L100,${height - 20} Z`,
        class: "li-floor-top",
      }),
      svg("path", null, {
        d: `M100,${height - 20} L${width - 20},${height - 90} v12 L100,${height - 8} L20,102 V90 Z`,
        class: "li-floor-side",
      }),
    );
    drawing.append(floor);
    if (machines.index.length > 0) {
      label([drawing, "其他機械索引", 30, layout.index[0][1] - 16, "li-small"]);
    }
    for (const [i, o] of machines.index.entries()) {
      const [x, y] = layout.index[i];
      const g = target("object", o.id, `物件 · ${o.label}`);
      g.classList.add("li-machine-index");
      g.append(
        svg("rect", null, { x, y, width: 250, height: 48, rx: 6, class: "li-card-outline" }),
      );
      art([g, o.mechanism?.template, x + 2, y + 2, 48, 44]);
      label([g, o.label, x + 56, y + 29, "li-index-name"]);
      drawing.append(g);
    }
    for (const [i, o] of machines.primary.entries()) {
      const [x, y] = layout.objects[i];
      machineCard([o, x, y, 250, 130]);
    }
    for (const [i, item] of items.entries()) {
      const [x, y] = layout.items[i];
      if (!state.run) {
        const r = groupView(item);
        const repo = target("repository", r.id, `Repo · ${r.label}`);
        repo.dataset.repoRegion = item.id;
        repo.append(
          svg("rect", null, {
            x,
            y,
            width: 248,
            height: 158,
            rx: 8,
            class: "li-card-outline",
          }),
        );
        label([repo, r.label, x + 10, y + 24]);
        label([
          repo,
          `${item.members.length} 來源快照 · ${r.tracked_files.length} 目錄索引`,
          x + 10,
          y + 56,
          "li-small",
        ]);
        label([repo, "選擇 Run 展開原資料流", x + 10, y + 92, "li-small"]);
        drawing.append(repo);
        continue;
      }
      const n = item;
      const node = target("node", n.id, `${n.kind} · ${n.label}`);
      node.dataset.repoRegion = groupFor.get(n.repository_id) || "unknown";
      node.append(
        svg("rect", null, { x, y, width: 248, height: 158, rx: 8, class: "li-card-outline" }),
      );
      art([node, n.mechanism, x + 24, y + 22, 200, 105]);
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
      placements.set(n.id, [x + 124, y]);
      drawing.append(node);
    }
    const expectedPoints = new Map();
    for (const step of overlay.steps) {
      if (step.matches.length === 1 && placements.has(step.matches[0])) {
        const point = placements.get(step.matches[0]);
        expectedPoints.set(step.id, [point[0] + 100, point[1] + 18]);
      }
    }
    for (const [i, step] of unmatched.entries()) {
      const [x, y] = layout.items[items.length + i];
      const card = target("expected", step.id, `預期步驟 · ${step.name}`);
      card.append(
        svg("rect", null, { x, y, width: 248, height: 158, rx: 7, class: "li-expected-card" }),
      );
      label([card, step.name.slice(0, 30), x + 8, y + 24, "li-small"]);
      label([
        card,
        `${step.owner?.label || "原歸屬未知"} · ${step.matches.length} 對應`,
        x + 8,
        y + 48,
        "li-small",
      ]);
      label([card, "契約預期 · 未唯一對應原節點", x + 8, y + 76, "li-small"]);
      drawing.append(card);
      expectedPoints.set(step.id, [x + 124, y + 150]);
    }
    const lanes = new Map();
    for (const route of overlay.routes) {
      const from = expectedPoints.get(route.from),
        to = expectedPoints.get(route.to);
      if (!from || !to) {
        continue;
      }
      const pair = JSON.stringify([route.from, route.to]);
      const lane = lanes.get(pair) || 0;
      lanes.set(pair, lane + 1);
      const a = [from[0], from[1] - lane * 22],
        b = [to[0], to[1] - lane * 22];
      const wire = target(
        "expected-route",
        `${route.layer}/${route.id}`,
        `${route.layer === "contract" ? "契約預期" : "可推翻假說"} · ${route.transformation}`,
      );
      wire.classList.add("li-expected-wire");
      wire.classList.toggle("li-hypothesis-wire", route.layer === "hypothesis");
      wire.dataset.comparison = route.result?.status || "unverified";
      const d = `M${a} C${a[0] + 28} ${a[1] - 24 - lane * 22},${b[0] + 28} ${b[1] - 24 - lane * 22},${b}`;
      wire.append(
        svg("path", null, { d, class: "li-route-hit" }),
        svg("path", null, { d, class: "li-route-line" }),
      );
      wire.append(
        svg("path", null, {
          d: `M${b[0] - 5} ${b[1] - 7} L${b} L${b[0] + 5} ${b[1] - 7}`,
          class: "li-arrow",
        }),
      );
      const gapLabel = { deviated: "✕", unknown: "?" };
      label([wire, gapLabel[route.result?.status] || "", b[0] + 6, b[1], "li-gap"]);
      drawing.append(wire);
    }
    drawing.setAttribute("viewBox", `0 0 ${layout.width} ${layout.height}`);
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
        `${targetEdge.getAttribute("class")} li-route li-${evidenceState(edge, graph)} ${observedFlow(edge, graph) ? "li-observed-flow" : ""}`,
      );
      const historical = observedFlow(edge, graph) && edge.current_validity !== "current";
      targetEdge.classList.toggle("li-historical", historical);
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
      if (edge.status === "failed" || historical) {
        label([
          targetEdge,
          [edge.status === "failed" ? "✕ 失敗" : "", historical ? "歷史" : ""]
            .filter(Boolean)
            .join(" · "),
          tip[0] + 8,
          tip[1] + 15,
          edge.status === "failed" ? "li-edge-failure" : "li-small",
        ]);
      }
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
    location.textContent = `${goal || "Goal / 全機總覽"} → ${state.run ? `Run ${state.run.slice(0, 12)}` : "全部執行"} · ${state.selection ? `${state.selection.kind} / ${(state.selection.kind === "object" && graph.objects.find((o) => o.id === state.selection.id)?.label) || state.selection.id}` : "本次路徑"}`;
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
        unified: true,
        onModel(model) {
          const key = JSON.stringify(model);
          if (key !== expectedKey) {
            const needsDrawing =
              model.status === "available" || expectedModel?.status === "available";
            expectedKey = key;
            expectedModel = model;
            if (needsDrawing) {
              queueMicrotask(() => {
                if (root.isConnected) {
                  render();
                }
              });
            }
          }
        },
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
      nav.append(
        html(
          "span",
          ` → ${(record?.label || record?.message || state.selection.kind).slice(0, 90)}`,
        ),
      );
    }
    nav.append(button("返回機械全景", returnToMachine));
    inspector.append(nav);
    if (!["object", "principle"].includes(state.selection?.kind)) {
      return;
    }
    const categories = html("div", null, { class: "li-principles", "aria-label": "契約分類入口" });
    for (const facet of contractFacets(facetGraph(), owner.id, state.run)) {
      const port = button(facet.name, () => selectItem("principle", `${owner.id}/${facet.id}`));
      port.dataset.isoPrinciple = `${owner.id}/${facet.id}`;
      port.setAttribute("aria-pressed", String(selected("principle", `${owner.id}/${facet.id}`)));
      const symbol = html("span", null, { "aria-hidden": "true" });
      symbol.innerHTML = principleFigure(facet.shape);
      port.prepend(symbol);
      categories.append(port);
    }
    inspector.append(categories);
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
    if (state.run) {
      const requestNode = graph.nodes.find(
        (n) =>
          n.run_id === state.run &&
          n.output_artifact_ids.some((id) =>
            graph.artifacts.some((a) => a.id === id && a.role === "loop_request"),
          ),
      );
      const requestArtifact = graph.artifacts.find(
        (a) => requestNode?.output_artifact_ids.includes(a.id) && a.role === "loop_request",
      );
      if (requestArtifact) {
        const original = html("details", null, { "aria-label": "本次原要求" });
        const content = html("p", "按需讀取本次 Run 綁定的原要求。");
        original.append(html("summary", "本次原要求"), content);
        let loading = false;
        original.addEventListener("toggle", async () => {
          if (!original.open || loading) {
            return;
          }
          loading = true;
          state.goalContents ??= {};
          try {
            const raw =
              requestArtifact.content ||
              state.goalContents[requestArtifact.id] ||
              (readArtifact && (await readArtifact(requestArtifact.id)));
            if (!raw) {
              throw new Error("原來源內容未提供");
            }
            state.goalContents[requestArtifact.id] = raw;
            const request = JSON.parse(raw);
            content.textContent =
              typeof request.goal === "string" ? request.goal : "原來源未提供 Goal";
          } catch (error) {
            content.textContent = `原要求讀回未確認：${error.message}。保留未知。`;
          } finally {
            loading = false;
          }
        });
        runHeader.append(original);
      }
    }
    inspector.append(runHeader);
    const toolNodes = graph.nodes.filter(
      (n) =>
        n.run_id === state.run &&
        n.output_artifact_ids.some((id) =>
          graph.artifacts.some((a) => a.id === id && a.role === "native_tool_call"),
        ),
    );
    if (toolNodes.length > 0) {
      const tools = html("details", null, { "aria-label": "原生工具事件" });
      tools.append(
        html("summary", `原生工具事件 · ${toolNodes.length} 次呼叫`),
        html("p", "原 Session／attempt 的呼叫與結果；不以時間順序推論因果或指引已被遵循。"),
        button(state.events ? "收合工具事件 DAG" : "展開工具事件 DAG", () => {
          state.events = !state.events;
          state.selection = null;
          render();
        }),
      );
      for (const node of toolNodes) {
        tools.append(
          button(`${node.label} · ${node.identity.invocation_id}`, () =>
            selectItem("node", node.id),
          ),
        );
      }
      inspector.append(tools);
    }
    const allExpected = expectedScene(graph, expectedModel);
    const hypotheses = allExpected.routes.filter((r) => r.layer === "hypothesis");
    const matched = allExpected.routes.filter(
      (r) => r.layer === "contract" && r.result?.status === "matched",
    );
    const folded = html("details", null, { "aria-label": "收合的預期與假說" });
    folded.append(
      html(
        "summary",
        `已符合預期 ${matched.length} · 假說 ${hypotheses.length + allExpected.unbound.length}`,
      ),
    );
    for (const route of [...matched, ...hypotheses]) {
      folded.append(
        button(
          `${route.layer === "hypothesis" ? "假說" : "已符合"} · ${route.transformation}`,
          () => selectItem("expected-route", `${route.layer}/${route.id}`),
        ),
      );
    }
    if (allExpected.unbound.length > 0) {
      folded.append(
        html(
          "p",
          `${allExpected.unbound.length} 項未綁定假說保留於契約與假說來源索引，不猜測端點。`,
        ),
      );
    }
    inspector.append(folded);
    machineInspector();
    const contextNodes = new Set(inspector.children);
    if (pick?.kind === "principle") {
      const [objectId, principleId] = pick.id.split("/");
      const facet = contractFacets(facetGraph(), objectId, state.run).find(
        (f) => f.id === principleId,
      );
      if (facet) {
        inspector.append(
          html("h4", facet.name),
          html("p", facet.rule?.meaning || "此快照尚無版本化分類來源。"),
          html(
            "p",
            facet.nodes.length > 0
              ? "已有相關來源引用；不證明此原則已執行或語義正確。"
              : "適用性／本次觀察未知；缺紀錄不等於不適用或未執行。",
          ),
        );
        for (const reference of facet.sources) {
          inspector.append(
            button(`原分類來源 · ${reference.role}`, () =>
              openOriginal({ kind: "artifact", id: reference.id }),
            ),
          );
        }
        for (const node of facet.nodes) {
          inspector.append(
            button(`${node.label} · ${node.status.execution}`, () => selectItem("node", node.id)),
          );
        }
      }
    }
    if (["expected", "expected-route"].includes(pick?.kind)) {
      const overlay = allExpected;
      const record =
        pick.kind === "expected"
          ? overlay.steps.find((s) => s.id === pick.id)
          : overlay.routes.find((r) => `${r.layer}/${r.id}` === pick.id);
      if (record) {
        inspector.append(
          html("h4", record.name || record.transformation),
          html(
            "p",
            record.layer === "contract"
              ? `契約預期 · ${record.condition || "依原契約的適用路徑；對應來源不代表已執行或語義通過。"}`
              : `可推翻假說 · ${record.condition || "尚未證實"}`,
          ),
        );
        const raw = html("details");
        raw.append(
          html("summary", "原對照紀錄"),
          html(
            "pre",
            JSON.stringify(
              record.result || {
                matches: record.matches,
                claim: record.claim,
                falsifier: record.falsifier,
              },
              null,
              2,
            ),
          ),
        );
        inspector.append(raw);
        const ids = record.matches || record.result?.edge_ids || [];
        for (const id of ids) {
          inspector.append(
            button(
              `定位 · ${(pick.kind === "expected" ? graph.nodes : graph.edges).find((v) => v.id === id)?.label || id} · ${(pick.kind === "expected" ? graph.nodes : graph.edges).find((v) => v.id === id)?.status?.execution || "原交接"}`,
              () => selectItem(pick.kind === "expected" ? "node" : "edge", id),
            ),
          );
        }
        inspector.append(
          button("讀取原預期與 Prompt", () =>
            openOriginal({ kind: "artifact", id: expectedModel.artifact.id }),
          ),
        );
      }
    }
    if (value && state.run && ["object", "node", "edge", "finding"].includes(pick.kind)) {
      const reviewStatus = html("p", "雲端／未支援交接：只讀取原資料。");
      if (onReviewSelection) {
        reviewStatus.textContent = "保存選取引用，供目前 Session 按需讀取；不啟動任務。";
        const reviewPick = { runId: state.run, kind: pick.kind, id: pick.id };
        const submit = button("交給 Session 檢視", async () => {
          submit.disabled = true;
          try {
            const receipt = await onReviewSelection(reviewPick);
            reviewStatus.textContent = `已保存 ${receipt.id} · ${receipt.status}；Session 可讀取原引用與續行。`;
          } catch (error) {
            reviewStatus.textContent = `選取未確認：${error.message}。先讀回；不重播任務。`;
          } finally {
            submit.disabled = false;
          }
        });
        inspector.append(submit);
      }
      reviewStatus.setAttribute("role", "status");
      inspector.append(reviewStatus);
    }
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
          inspector.append(button(`拆解 Run ${id.slice(0, 12)}`, () => expandRun(id)));
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
    } else if (!["principle", "expected", "expected-route"].includes(pick?.kind)) {
      inspector.append(
        html("h4", "選一個設備或資料流"),
        html(
          "p",
          "設備只出現一次。選 Run 展開 Session、Prompt 與 P-class；拆解只改變視角，沒有啟動或重新執行。",
        ),
      );
    }
    if (pick && !["object", "principle"].includes(pick.kind)) {
      const content = [...inspector.children].filter((element) => !contextNodes.has(element));
      inspector.prepend(...content);
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
      if (state.run !== id) {
        state.events = false;
      }
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
            events: state.events,
            apart: state.apart,
            angle: state.angle,
            zoom: state.zoom,
            fullOverlay: state.fullOverlay,
            fullArchitecture: state.fullArchitecture,
            scrollTop: viewport.scrollTop,
            scrollLeft: viewport.scrollLeft,
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
