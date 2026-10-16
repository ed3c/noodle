import { test } from "node:test";
import assert from "node:assert/strict";
import { evidenceState, projectConsole } from "./loop-isometric.mjs";
const n = (id, run, repo, execution = "completed") => ({
  id,
  run_id: run,
  repository_id: repo,
  identity: { status: "observed" },
  status: { execution },
});
function fixture() {
  const nodes = [
    n("a", "upstream", "repoA"),
    n("b", "selected", "repoB"),
    n("guide", "selected", "repoB", "available_reference"),
    n("other", "unrelated", "repoC"),
  ];
  const edges = [
    {
      id: "handoff",
      run_id: "selected",
      from_node_id: "a",
      to_node_id: "b",
      kind: "handoff",
      status: "observed",
      current_validity: "current",
      input_artifact_ids: ["input"],
      output_artifact_ids: ["output"],
      evidence_artifact_ids: ["receipt"],
    },
  ];
  const graph = {
    nodes,
    edges,
    artifacts: ["input", "output", "receipt"].map((id) => ({ id, status: "available" })),
    repositories: ["repoA", "repoB", "repoC"].map((id) => ({ id })),
    findings: [],
  };
  const scope = { nodes: nodes.slice(0, 3), edges, rows: [{ run_id: "selected" }] };
  return { graph, scope, edge: edges[0] };
}
test("current rendering requires original input, output, evidence and both native identities", () => {
  const { graph, edge } = fixture();
  assert.equal(evidenceState(edge, graph), "current");
  graph.artifacts[1].status = "missing";
  assert.equal(evidenceState(edge, graph), "unknown");
  graph.artifacts[1].status = "not_public";
  assert.equal(evidenceState(edge, graph), "current");
  graph.nodes[0].identity.status = "unknown";
  assert.equal(evidenceState(edge, graph), "unknown");
});
test("historical, failed and diagnostic edges never render as current success", () => {
  const { graph, edge } = fixture();
  edge.current_validity = "stale";
  assert.equal(evidenceState(edge, graph), "historical");
  edge.status = "failed";
  assert.equal(evidenceState(edge, graph), "failed");
  edge.status = "observed";
  edge.current_validity = "current";
  edge.kind = "diagnostic";
  assert.equal(evidenceState(edge, graph), "unknown");
});
test("run view preserves cross-repository original endpoints and excludes unrelated runs", () => {
  const { graph, scope } = fixture();
  const before = JSON.stringify(graph);
  const view = projectConsole(graph, scope, { runId: "selected" });
  assert.deepEqual(
    view.nodes.map((n) => n.id),
    ["a", "b"],
  );
  assert.deepEqual(
    view.repositories.map((r) => r.id),
    ["repoA", "repoB"],
  );
  assert.equal(view.hidden, 1);
  assert.equal(JSON.stringify(graph), before);
});
test("source expansion exposes original guide IDs without inventing invocation or dataflow", () => {
  const { graph, scope } = fixture();
  const view = projectConsole(graph, scope, { runId: "selected", sources: true });
  assert.deepEqual(
    view.nodes.map((n) => n.id),
    ["a", "b", "guide"],
  );
  assert.deepEqual(view.edges, scope.edges);
  assert.equal(view.nodes[2].status.execution, "available_reference");
});
test("empty run remains empty rather than creating a decorative runtime", () => {
  const { graph, scope } = fixture();
  const view = projectConsole(graph, scope, { runId: "missing" });
  assert.equal(view.nodes.length, 0);
  assert.equal(view.edges.length, 0);
});
