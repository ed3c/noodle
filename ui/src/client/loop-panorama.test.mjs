import { JSDOM } from "jsdom";
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  mountPanorama,
  projectPanorama,
  projectRunDetail,
  projectDetailLayer,
  readStructuredField,
} from "./loop-panorama.mjs";
const node = (id, object_id, [run_id, repository_id]) => ({
  id,
  object_id,
  run_id,
  repository_id,
  kind: "cli",
  identity: { status: "observed" },
  input_artifact_ids: [],
  output_artifact_ids: [],
  status: { execution: "completed", transfer: "input_recorded", acceptance: "unknown" },
  evidence_artifact_ids: ["proof"],
});
function fixture() {
  return {
    objects: [
      { id: "outer", namespace: "soodles.outer" },
      { id: "inner", namespace: "noodle.inner" },
      { id: "loop", namespace: "soodles.loop" },
    ],
    nodes: [
      node("a", "outer", ["a", "repoA"]),
      node("b", "inner", ["b", "repoB"]),
      node("c", "outer", ["b", "repoB"]),
      node("unrelated", "inner", ["c", "repoC"]),
    ],
    edges: [
      {
        id: "ab",
        run_id: "b",
        from_node_id: "a",
        to_node_id: "b",
        kind: "handoff",
        status: "observed",
        current_validity: "current",
        scope: "cross_repository",
        input_artifact_ids: ["input"],
        output_artifact_ids: ["output"],
        evidence_artifact_ids: ["proof"],
      },
      {
        id: "bc",
        run_id: "b",
        from_node_id: "b",
        to_node_id: "c",
        kind: "handoff",
        status: "observed",
        current_validity: "current",
        scope: "within_repository",
        input_artifact_ids: ["input"],
        output_artifact_ids: ["output"],
        evidence_artifact_ids: ["proof"],
      },
    ],
    artifacts: ["input", "output", "proof"].map((id) => ({ id, role: id, status: "available" })),
    findings: [],
  };
}
test("selected scope preserves cross-boundary endpoints without importing unrelated runs", () => {
  const p = projectPanorama(fixture(), ["b"]);
  assert.deepEqual(p.context_run_ids, ["a"]);
  assert.equal(p.nodes.length, 3);
  assert.equal(p.rows.length, 1);
  assert.equal(p.rows[0].run_id, "b");
});
test("an object cycle, completed worker and recorded input never prove consumption or closure", () => {
  const g = fixture(),
    p = projectPanorama(g, ["b"]);
  assert.equal(p.relations.length, 2);
  assert.equal(p.rows[0].consumption.status, "unknown");
  assert.equal(p.rows[0].feedback.status, "unknown");
  assert.equal(p.rows[0].goal.status, "unknown");
});
test("current scoped consumer review proves only its original boundary", () => {
  const g = fixture();
  g.nodes[2].status = {
    execution: "locally_integrated",
    transfer: "output_reviewed",
    acceptance: "original_scoped_review_approved",
  };
  const [row] = projectPanorama(g, ["b"]).rows;
  assert.equal(row.consumption.status, "observed");
  assert.deepEqual(row.consumption.edge_ids, ["bc"]);
  assert.equal(row.goal.status, "unknown");
  g.edges[1].current_validity = "stale";
  assert.equal(projectPanorama(g, ["b"]).rows[0].consumption.status, "unknown");
});
test("feedback needs the original continuation evidence set and current binding", () => {
  const g = fixture();
  for (const role of ["review_candidate", "review_feedback", "control_ack", "native_spawn"]) {
    g.artifacts.push({ id: role, role, status: "available" });
    g.edges[0].evidence_artifact_ids.push(role);
  }
  g.nodes[0].object_id = "loop";
  g.nodes[1].kind = "cli";
  assert.equal(projectPanorama(g, ["b"]).rows[0].feedback.status, "observed");
  g.edges[0].current_validity = "unknown";
  assert.equal(projectPanorama(g, ["b"]).rows[0].feedback.status, "observed");
  assert.equal(projectPanorama(g, ["b"]).rows[0].feedback.current, false);
  g.artifacts.find((a) => a.role === "review_feedback").status = "stale";
  assert.equal(projectPanorama(g, ["b"]).rows[0].feedback.status, "unknown");
});
test("failed findings retain their run boundary after a later success", () => {
  const g = fixture();
  g.findings = [
    { id: "old", run_id: "a", status: "failed" },
    { id: "current", run_id: "b", status: "failed" },
  ];
  assert.deepEqual(
    projectPanorama(g, ["b"]).rows[0].findings.map((f) => f.id),
    ["current"],
  );
});
test("no imaginary links are added for disconnected Manager objects", () => {
  const g = fixture();
  g.objects.push({ id: "test", namespace: "soodles.test" });
  const p = projectPanorama(g);
  assert.equal(p.edges.length, 2);
  assert.equal(
    p.relations.some((r) => r.from === "test" || r.to === "test"),
    false,
  );
});
test("stale payload cannot retain a current consumption claim", () => {
  const g = fixture();
  g.nodes[2].status.transfer = "output_reviewed";
  g.nodes[2].status.acceptance = "original_scoped_review_approved";
  g.artifacts[0].status = "stale";
  assert.equal(projectPanorama(g, ["b"]).rows[0].consumption.status, "unknown");
});

test("detail exposes every P-class occurrence and bound instruction without fabricating execution", () => {
  const g = fixture();
  g.nodes[1].kind = "pclass";
  g.nodes[1].identity.session_id = "native-session";
  g.nodes[2].identity.session_id = "native-session";
  g.nodes[2].input_artifact_ids = ["guide", "support"];
  g.artifacts.push(
    { id: "guide", role: "instruction", status: "not_public" },
    { id: "support", role: "instruction", status: "available" },
    { id: "unbound", role: "instruction", status: "available" },
  );
  const before = JSON.stringify(g);
  const p = projectRunDetail(g, "b");
  assert.deepEqual(
    p.pclass_nodes.map((n) => n.id),
    ["b"],
  );
  assert.deepEqual(
    p.instructions.map((i) => i.artifact.id),
    ["guide", "support"],
  );
  assert.deepEqual(p.instructions[0].node_ids, ["c"]);
  assert.equal(p.sessions.find((s) => s.session_id === "native-session").nodes.length, 2);
  assert.deepEqual(
    p.edges.map((e) => e.id),
    ["ab", "bc"],
  );
  assert.equal(JSON.stringify(g), before);
});
test("session names never merge across Run or repository and unknown identity stays unbound", () => {
  const g = fixture();
  for (const n of g.nodes) {
    n.identity.session_id = "same-name";
  }
  g.nodes[2].identity.status = "unknown";
  const p = projectRunDetail(g, "b");
  assert.equal(p.sessions.length, 3);
  assert.equal(p.sessions.filter((s) => s.session_id).length, 2);
  assert.deepEqual(
    p.sessions.find((s) => !s.session_id).nodes.map((n) => n.id),
    ["c"],
  );
  assert.equal(
    p.nodes.some((n) => n.id === "unrelated"),
    false,
  );
});

test("structured fields read only exact own source pointers, with missing and invalid data explicit", () => {
  assert.deepEqual(
    readStructuredField('{"a/b":{"~key":false}}', "/source.json#/a~1b/~0key", "/source.json"),
    { pointer: "/a~1b/~0key", status: "available", text: "false" },
  );
  assert.equal(readStructuredField("{}", "/other.json#/next", "/source.json"), null);
  assert.equal(
    readStructuredField("{}", "/source.json#/__proto__", "/source.json").status,
    "unknown",
  );
  assert.equal(readStructuredField("bad", "/source.json#/next", "/source.json").status, "unknown");
});

test("source-only guidance stays browseable without hiding real handoff endpoints", () => {
  const g = fixture();
  g.nodes.push({
    ...node("guide", "outer", ["b", "repoB"]),
    kind: "pclass",
    status: { execution: "available_reference" },
  });
  g.nodes[1].status.execution = "selected";
  const detail = projectRunDetail(g, "b");
  const main = projectDetailLayer(detail);
  assert.equal(
    main.nodes.some((n) => n.id === "guide"),
    false,
  );
  assert.equal(
    main.nodes.some((n) => n.id === "b"),
    true,
  );
  assert.equal(main.edges.length, 2);
  assert.equal(
    detail.pclass_nodes.some((n) => n.id === "guide"),
    true,
  );
  assert.equal(projectDetailLayer(detail, true).nodes.length, detail.nodes.length);
});

function navigationFixture(t, extra = {}, prepareGraph = null) {
  const dom = new JSDOM("<main></main>");
  const previous = globalThis.document;
  globalThis.document = dom.window.document;
  dom.window.HTMLElement.prototype.scrollIntoView = () => {
    /* jsdom has no layout scrolling. */
  };
  t.after(() => {
    dom.window.close();
    globalThis.document = previous;
  });
  const graph = fixture();
  graph.ports = [];
  graph.repositories = ["repoA", "repoB", "repoC"].map((id) => ({
    id,
    label: id,
    root: `/${id}`,
    tracked_files: [],
    selected_files: [],
    index_status: "unknown",
    revision: null,
  }));
  for (const object of graph.objects) {
    object.label = object.namespace;
    object.source_artifact_ids = [];
  }
  for (const occurrence of graph.nodes) {
    occurrence.label = occurrence.id;
  }
  for (const edge of graph.edges) {
    edge.label = edge.id;
  }
  prepareGraph?.(graph);
  const root = dom.window.document.querySelector("main");
  const calls = [];
  const resets = [];
  mountPanorama(root, {
    graph,
    runs: ["a", "b", "c"].map((id) => ({ id, repository: `repo-${id}`, mode: "inner-only" })),
    onScope() {
      assert.fail("Navigation must not change Workspace scope");
    },
    onSelect() {
      assert.fail("Navigation must not request original content");
    },
    onFocus(value) {
      calls.push(value);
    },
    onReset() {
      resets.push(true);
    },
    ...extra,
  });
  return { root, calls, resets, dom, graph };
}
test("repository inspection opens the same Run in console, Goal and detail", (t) => {
  const { root, calls, dom } = navigationFixture(t);
  root.querySelector("[data-iso-repository]").dispatchEvent(new dom.window.Event("click"));
  [...root.querySelectorAll("button")].find((b) => b.textContent === "拆解 Run a").click();
  assert.equal(root.querySelector('[aria-label="立體視圖執行範圍"]').value, "a");
  assert.equal(root.querySelector('[aria-label="細部 Run"]').value, "a");
  assert.match(root.querySelector('[aria-label="全景定位"]').textContent, /repo-a \/ a/);
  assert.deepEqual(
    [...root.querySelector(".lp-paths").children]
      .filter((n) => !n.hidden)
      .map((n) => n.dataset.panoramaRun),
    ["a"],
  );
  assert.deepEqual(calls.at(-1).node_ids, ["a"]);
});
test("Run changes update the external DAG focus and global reset clears it", (t) => {
  const { root, calls, resets, dom } = navigationFixture(t);
  const first = root.querySelector('[data-panorama-run="a"]');
  [...first.querySelectorAll("button")].find((b) => b.textContent === "展開本次執行 DAG").click();
  const selection = root.querySelector('[aria-label="立體視圖執行範圍"]');
  selection.value = "b";
  selection.dispatchEvent(new dom.window.Event("change"));
  assert.deepEqual(calls.at(-1).node_ids, ["b", "c"]);
  assert.deepEqual(calls.at(-1).edge_ids, ["ab", "bc"]);
  assert.equal(calls.at(-1).reveal, false);
  assert.equal(root.querySelector('[aria-label="細部 Run"]').value, "b");
  selection.value = "";
  selection.dispatchEvent(new dom.window.Event("change"));
  assert.equal(resets.length, 1);
  assert.match(root.querySelector('[aria-label="全景定位"]').textContent, /全部適用路徑/);
});
test("detail Run selector also updates the original external focus", (t) => {
  const { root, calls, dom } = navigationFixture(t);
  const selection = root.querySelector('[aria-label="細部 Run"]');
  selection.value = "c";
  selection.dispatchEvent(new dom.window.Event("change"));
  assert.deepEqual(calls.at(-1).node_ids, ["unrelated"]);
  assert.equal(root.querySelector('[aria-label="立體視圖執行範圍"]').value, "c");
});

test("Run switch after content inspection survives consumer remount", (t) => {
  const { root, dom, graph } = navigationFixture(t);
  const options = {
    graph,
    runs: ["a", "b", "c"].map((id) => ({ id, repository: `repo-${id}`, mode: "inner-only" })),
    onScope() {
      assert.fail("Run navigation must preserve Workspace scope");
    },
    onSelect() {
      assert.fail("Run navigation must not request old content");
    },
    onFocus() {
      mountPanorama(root, options);
    },
  };
  const controller = mountPanorama(root, options);
  controller.setSelection({ kind: "node", id: "a" });
  const selection = root.querySelector('[aria-label="立體視圖執行範圍"]');
  selection.value = "c";
  selection.dispatchEvent(new dom.window.Event("change"));
  assert.equal(root.querySelector('[aria-label="立體視圖執行範圍"]').value, "c");
  assert.equal(root.querySelector('[aria-label="細部 Run"]').value, "c");
  assert.match(root.querySelector('[aria-label="全景定位"]').textContent, /repo-c \/ c/);
});

test("explicit local review selection keeps the chosen Run; cloud has no submission control", async (t) => {
  const submitted = [];
  const { root, dom } = navigationFixture(t, {
    onReviewSelection: (pick) => {
      submitted.push(pick);
      return Promise.resolve({ id: "receipt", status: "current" });
    },
  });
  const run = root.querySelector('[aria-label="立體視圖執行範圍"]');
  run.value = "a";
  run.dispatchEvent(new dom.window.Event("change"));
  root.querySelector('[data-detail-node="a"]').dispatchEvent(new dom.window.Event("click"));
  const submit = [...root.querySelectorAll("button")].find(
    (b) => b.textContent === "交給 Session 檢視",
  );
  assert.ok(submit);
  assert.equal(submitted.length, 0);
  submit.click();
  await new Promise((resolve) => {
    setTimeout(resolve, 0);
  });
  assert.deepEqual(submitted, [{ runId: "a", kind: "node", id: "a" }]);
  assert.match(root.textContent, /已保存 receipt/);
  const cloud = navigationFixture(t);
  assert.equal(
    [...cloud.root.querySelectorAll("button")].some((b) => b.textContent === "交給 Session 檢視"),
    false,
  );
});

test("principle selection stays in the Run and does not submit or claim missing execution", (t) => {
  const { root, dom } = navigationFixture(t, {
    onReviewSelection() {
      assert.fail("selection is read-only");
    },
  });
  const run = root.querySelector('[aria-label="立體視圖執行範圍"]');
  run.value = "a";
  run.dispatchEvent(new dom.window.Event("change"));
  root.querySelector('[data-iso-object="outer"]').dispatchEvent(new dom.window.Event("click"));
  root
    .querySelector('[data-iso-principle="outer/method"]')
    .dispatchEvent(new dom.window.Event("click"));
  assert.equal(run.value, "a");
  assert.ok(root.querySelector('[data-iso-principle="outer/method"][aria-pressed="true"]'));
  assert.match(root.querySelector(".li-inspector").textContent, /缺紀錄不等於不適用或未執行/);
  assert.equal(root.querySelectorAll("[data-iso-principle]").length, 5);
});

test("one scene overlays bound expected routes without duplicating owner objects", async (t) => {
  const { root, dom, graph } = navigationFixture(t);
  const expected = {
    version: 1,
    request_sha256: "a",
    origin: "pre_execution",
    mode: "inner-only",
    goal: "Original goal",
    components: [{ id: "c", owner: "soodles.outer" }],
    steps: [
      { id: "s", component: "c", name: "Source" },
      { id: "t", component: "c", name: "Target" },
    ],
    routes: [{ id: "r", from: "s", to: "t", applicable: true, transformation: "intent → tool" }],
    hypotheses: [{ id: "h", from: "s", to: "t", claim: "falsifiable" }],
    prompt_delta: "Original delta",
  };
  const comparison = {
    version: 1,
    run_id: "a",
    expectation_artifact_id: "expect",
    source_status: "current",
    rows: [{ route_id: "r", status: "unknown", edge_ids: [] }],
    step_matches: { s: ["a"], t: [] },
    semantic_review: { node_ids: [], reason: "unknown" },
  };
  graph.artifacts.push(
    {
      id: "expect",
      role: "loop_expectation",
      status: "available",
      content: JSON.stringify(expected),
    },
    {
      id: "compare",
      role: "loop_comparison",
      status: "available",
      content: JSON.stringify(comparison),
    },
  );
  const run = root.querySelector('[aria-label="立體視圖執行範圍"]');
  run.value = "a";
  run.dispatchEvent(new dom.window.Event("change"));
  await new Promise((resolve) => {
    setTimeout(resolve, 0);
  });
  const scene = root.querySelector('[aria-label="內外迴圈立體資料流"]');
  assert.equal(scene.querySelectorAll('[data-iso-object="outer"]').length, 1);
  assert.ok(scene.querySelector('[data-iso-expected-route="contract/r"]'));
  assert.equal(scene.querySelector('[data-iso-expected-route="hypothesis/h"]'), null);
  [...root.querySelectorAll("button")].find((b) => b.textContent.startsWith("完整疊圖")).click();
  assert.ok(scene.querySelector('[data-iso-expected-route="hypothesis/h"].li-hypothesis-wire'));
  assert.notEqual(
    scene.querySelector('[data-iso-expected-route="contract/r"] .li-route-hit').getAttribute("d"),
    scene.querySelector('[data-iso-expected-route="hypothesis/h"] .li-route-hit').getAttribute("d"),
  );
  assert.equal(root.querySelectorAll('[aria-label="預期機構資料流 DAG"]').length, 0);
  scene
    .querySelector('[data-iso-expected-route="hypothesis/h"]')
    .dispatchEvent(new dom.window.Event("click"));
  assert.match(root.querySelector(".li-inspector").textContent, /可推翻假說/);

  scene
    .querySelector('[data-iso-expected-route="contract/r"]')
    .dispatchEvent(new dom.window.Event("click"));
  assert.match(root.querySelector(".li-inspector").textContent, /intent → tool/);
  const hidden = graph.nodes.find((n) => n.id === "unrelated");
  hidden.run_id = "a";
  hidden.kind = "pclass";
  hidden.status.execution = "available_reference";
  comparison.step_matches.s = ["a", "unrelated"];
  graph.artifacts.find((a) => a.id === "compare").content = JSON.stringify(comparison);
  run.value = "b";
  run.dispatchEvent(new dom.window.Event("change"));
  run.value = "a";
  run.dispatchEvent(new dom.window.Event("change"));
  await new Promise((resolve) => {
    setTimeout(resolve, 0);
  });
  assert.ok(root.querySelector('[data-iso-expected="s"]'), "hidden match cannot imply uniqueness");
  root.querySelector('[data-iso-expected="s"]').dispatchEvent(new dom.window.Event("click"));
  const readback = root.querySelector(".li-inspector");
  assert.match(readback.textContent, /契約預期/);
  assert.doesNotMatch(readback.textContent, /原假說，尚未證實/);
  assert.ok(
    [...readback.querySelectorAll("button")].some((b) =>
      b.textContent.includes("available_reference"),
    ),
  );
});

test("failed historical transport keeps green evidence, historical marker and separate failure", (t) => {
  const { root, dom, graph } = navigationFixture(t);
  graph.edges[0].status = "failed";
  graph.edges[0].current_validity = "stale";
  const run = root.querySelector('[aria-label="立體視圖執行範圍"]');
  run.value = "b";
  run.dispatchEvent(new dom.window.Event("change"));
  const edge = root.querySelector('[data-iso-edge="ab"]');
  assert.ok(edge.classList.contains("li-observed-flow"));
  assert.ok(edge.classList.contains("li-historical"));
  assert.match(edge.textContent, /✕.*失敗/);
  assert.match(edge.textContent, /歷史/);
});

test("one floor keeps all unique machines reachable and exposes only the selected owner facets", (t) => {
  const { root, dom, graph } = navigationFixture(t);
  const drawing = root.querySelector('[aria-label="內外迴圈立體資料流"]');
  assert.equal(drawing.querySelectorAll(".li-workspace-floor").length, 1);
  assert.equal(drawing.querySelectorAll(".li-region").length, 0);
  assert.equal(drawing.querySelectorAll(".li-station").length, graph.objects.length);
  assert.equal(root.querySelectorAll("[data-iso-principle]").length, 0);
  const run = root.querySelector('[aria-label="立體視圖執行範圍"]');
  run.value = "b";
  run.dispatchEvent(new dom.window.Event("change", { bubbles: true }));
  assert.equal(drawing.querySelectorAll(".li-station").length, 2);
  assert.equal(drawing.querySelectorAll(".li-machine-index").length, 1);
  assert.equal(drawing.querySelectorAll("[data-iso-object]").length, graph.objects.length);
  drawing.querySelector('[data-iso-object="loop"]').dispatchEvent(new dom.window.Event("click"));
  assert.equal(root.querySelectorAll("[data-iso-principle]").length, 5);
  assert.equal(drawing.querySelectorAll("[data-iso-edge]").length, 2);
  assert.equal(run.value, "b");
  [...root.querySelectorAll("button")].find((b) => b.textContent === "完整架構").click();
  assert.equal(drawing.querySelectorAll(".li-station").length, 3);
  assert.equal(drawing.querySelectorAll(".li-machine-index").length, 0);
  assert.equal(drawing.querySelectorAll(".li-region").length, 0);
  assert.ok(drawing.querySelector('[aria-label="cli · b"]'));
});

test("review enlargement and returning to the Run preserve selected source, camera and independent viewport scroll", (t) => {
  const { root, dom } = navigationFixture(t);
  const run = root.querySelector('[aria-label="立體視圖執行範圍"]');
  run.value = "b";
  run.dispatchEvent(new dom.window.Event("change"));
  const viewport = root.querySelector(".li-viewport");
  viewport.scrollTop = 70;
  root.querySelector('[data-iso-object="outer"]').dispatchEvent(new dom.window.Event("click"));
  const choose = (text) => [...root.querySelectorAll("button")].find((b) => b.textContent === text);
  choose("放大／返回審查").click();
  assert.ok(root.querySelector(".li-review-expanded"));
  assert.equal(run.value, "b");
  choose("放大／返回審查").click();
  assert.equal(root.querySelector(".li-review-expanded"), null);
  assert.equal(viewport.scrollTop, 70);
  choose("返回本次路徑").click();
  assert.equal(run.value, "b");
  assert.equal(root.querySelectorAll("[data-iso-principle]").length, 0);
  assert.equal(root.querySelectorAll("[data-iso-edge]").length, 2);
});

test("diagnostic back-links remain inspectable without becoming causal ranks", (t) => {
  const { root, dom, graph } = navigationFixture(t, {}, (sourceGraph) =>
    sourceGraph.edges.push({
      ...sourceGraph.edges[1],
      id: "diagnostic-cb",
      label: "diagnostic back-link",
      kind: "diagnostic",
      from_node_id: "c",
      to_node_id: "b",
      status: "unknown",
    }),
  );
  const errors = [];
  dom.window.addEventListener("error", (event) => {
    errors.push(event.error.message);
    event.preventDefault();
  });
  const run = root.querySelector('[aria-label="立體視圖執行範圍"]');
  run.value = "b";
  run.dispatchEvent(new dom.window.Event("change"));
  assert.deepEqual(errors, []);
  const scene = root.querySelector('[aria-label="內外迴圈立體資料流"]');
  assert.equal(scene.querySelectorAll("[data-iso-node]").length, 3);
  assert.equal(scene.querySelectorAll("[data-iso-edge]").length, 3);
  assert.ok(scene.querySelector('[data-iso-edge="diagnostic-cb"]'));
  const diagnostic = [...root.querySelectorAll(".li-inspector button")].find((b) =>
    b.textContent.includes("diagnostic back-link"),
  );
  assert.ok(diagnostic);
  diagnostic.click();
  assert.match(root.querySelector(".li-inspector").textContent, /diagnostic back-link/);
  assert.equal(graph.edges.length, 3);
});

test("location identifies the selected edge after inspecting its owner", (t) => {
  const { root, dom } = navigationFixture(t);
  const run = root.querySelector('[aria-label="立體視圖執行範圍"]');
  run.value = "b";
  run.dispatchEvent(new dom.window.Event("change"));
  root.querySelector('[data-iso-object="outer"]').dispatchEvent(new dom.window.Event("click"));
  root.querySelector('[data-iso-edge="ab"]').dispatchEvent(new dom.window.Event("click"));
  assert.match(root.querySelector(".li-position").textContent, /edge \/ ab$/);
});

test("local machine filtering does not relabel recorded participation as unobserved", (t) => {
  const { root, dom, graph } = navigationFixture(t, {}, (sourceGraph) => {
    sourceGraph.nodes.push({ ...node("separate", "loop", ["b", "repoB"]), label: "separate" });
  });
  const run = root.querySelector('[aria-label="立體視圖執行範圍"]');
  run.value = "b";
  run.dispatchEvent(new dom.window.Event("change"));
  assert.ok(root.querySelector('[data-iso-object="loop"].li-participating'));
  root.querySelector('[data-iso-object="outer"]').dispatchEvent(new dom.window.Event("click"));
  [...root.querySelectorAll("button")].find((b) => b.textContent === "只看此機械").click();
  assert.ok(root.querySelector('[data-iso-object="loop"].li-machine-index'));
  assert.doesNotMatch(root.querySelector(".li-viewport").textContent, /未觀察本次參與/);
  assert.equal(graph.nodes.find((n) => n.id === "separate").status.execution, "completed");
});

test("native event index opens a recorded tool and places its readback before secondary context", (t) => {
  const { root, dom } = navigationFixture(t, {}, (graph) => {
    graph.nodes[0].output_artifact_ids = ["call"];
    graph.nodes[0].identity.invocation_id = "thread/1/item_2";
    graph.artifacts.push({ id: "call", role: "native_tool_call", status: "available" });
  });
  const select = root.querySelector('[aria-label="立體視圖執行範圍"]');
  select.value = "a";
  select.dispatchEvent(new dom.window.Event("change"));
  const index = root.querySelector('[aria-label="原生工具事件"]');
  assert.match(index.textContent, /1 次呼叫/);
  [...index.querySelectorAll("button")]
    .find((b) => b.textContent.includes("thread/1/item_2"))
    .click();
  assert.ok(root.querySelector('[data-iso-node="a"]'));
  const inspector = root.querySelector(".li-inspector");
  const children = [...inspector.children];
  assert.ok(
    children.indexOf(inspector.querySelector("h4")) <
      children.indexOf(inspector.querySelector(".li-run-summary")),
  );
});

test("original Goal is read on demand when the native API omits inline content", async (t) => {
  const reads = [];
  const { root, dom } = navigationFixture(
    t,
    {
      readArtifact: (id) => {
        reads.push(id);
        return Promise.resolve(JSON.stringify({ goal: "Original human requirement" }));
      },
    },
    (graph) => {
      graph.nodes[0].output_artifact_ids = ["request"];
      graph.artifacts.push({ id: "request", role: "loop_request", status: "available" });
    },
  );
  const select = root.querySelector('[aria-label="立體視圖執行範圍"]');
  select.value = "a";
  select.dispatchEvent(new dom.window.Event("change"));
  assert.deepEqual(reads, []);
  const goal = root.querySelector('[aria-label="本次原要求"]');
  goal.open = true;
  goal.dispatchEvent(new dom.window.Event("toggle"));
  await Promise.resolve();
  assert.match(goal.textContent, /Original human requirement/);
  assert.deepEqual(reads, ["request"]);
  select.dispatchEvent(new dom.window.Event("change"));
  const refreshed = root.querySelector('[aria-label="本次原要求"]');
  assert.equal(refreshed.open, true);
  refreshed.dispatchEvent(new dom.window.Event("toggle"));
  await Promise.resolve();
  assert.match(refreshed.textContent, /Original human requirement/);
  assert.deepEqual(reads, ["request"]);
});
