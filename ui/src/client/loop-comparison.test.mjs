import { test } from "node:test";
import assert from "node:assert/strict";
import { comparisonModel, loadComparison } from "./loop-comparison.mjs";
const artifact = (id, role, data) => ({
  id,
  role,
  status: "available",
  content: JSON.stringify(data),
});
function fixture() {
  const expected = {
    version: 1,
    request_sha256: "run",
    origin: "pre_execution",
    steps: [
      { id: "a", name: "Input", component: "owner/read" },
      { id: "b", name: "Output", component: "owner/read" },
    ],
    components: [{ id: "owner/read" }],
    routes: [{ id: "a-b", from: "a", to: "b", applicable: true }],
    hypotheses: [],
  };
  const comparison = {
    version: 1,
    run_id: "run",
    expectation_artifact_id: "expected",
    source_status: "current",
    rows: [{ route_id: "a-b", status: "unknown", edge_ids: [] }],
    step_matches: { a: [], b: [] },
    semantic_review: { node_ids: [] },
  };
  return {
    artifacts: [
      artifact("expected", "loop_expectation", expected),
      artifact("comparison", "loop_comparison", comparison),
    ],
  };
}
test("comparison reads only the bound producer snapshot and never derives expectations from success", () => {
  const graph = fixture();
  const before = JSON.stringify(graph);
  assert.equal(comparisonModel(graph, "run").status, "available");
  assert.equal(comparisonModel(graph, "other").status, "unknown");
  assert.equal(JSON.stringify(graph), before);
  graph.artifacts.push({ ...graph.artifacts[1], id: "ambiguous" });
  assert.equal(comparisonModel(graph, "run").status, "unknown");
});
test("missing, malformed and differently bound expectations remain unknown", () => {
  for (const content of [
    "not-json",
    JSON.stringify({ version: 1, request_sha256: "other" }),
    JSON.stringify({ version: 1, request_sha256: "run" }),
  ]) {
    const graph = fixture();
    graph.artifacts[0].content = content;
    assert.equal(comparisonModel(graph, "run").status, "unknown");
  }
  const graph = fixture();
  graph.artifacts[0].status = "missing";
  assert.equal(comparisonModel(graph, "run").status, "unknown");
});
test("reviewed public projection preserves source identity without exposing private bytes", () => {
  const graph = fixture();
  for (const a of graph.artifacts) {
    a.public_content = a.content;
    delete a.content;
    a.status = "not_public";
  }
  assert.equal(comparisonModel(graph, "run").status, "available");
});

test("lazy readback fetches only the selected Run comparison and its bound expectation", async () => {
  const graph = fixture();
  graph.nodes = [{ run_id: "run", evidence_artifact_ids: ["comparison"] }];
  graph.artifacts.push(artifact("other", "loop_comparison", { run_id: "other" }));
  const calls = [];
  const contents = await loadComparison(graph, "run", async (id) => {
    calls.push(id);
    return graph.artifacts.find((a) => a.id === id).content;
  });
  assert.deepEqual(calls, ["comparison", "expected"]);
  assert.equal(Object.keys(contents).length, 2);
  assert.deepEqual(
    await loadComparison(graph, "missing", async () => {
      throw Error("must not read");
    }),
    {},
  );
  await assert.rejects(
    loadComparison(graph, "run", async () => {
      throw Error("source unavailable");
    }),
    /source unavailable/,
  );
});
test("a malformed comparison row stays unknown", () => {
  const graph = fixture();
  const value = JSON.parse(graph.artifacts[1].content);
  value.rows = [null];
  graph.artifacts[1].content = JSON.stringify(value);
  assert.equal(comparisonModel(graph, "run").status, "unknown");
});
