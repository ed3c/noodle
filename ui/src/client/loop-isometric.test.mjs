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

import { repositoryGroups, filterRepositories, branchName } from "./loop-repository.mjs";
import { mechanismFigure, anatomyModel } from "./loop-anatomy.mjs";
test("same-name repositories stay separate; native worktrees group without rewriting evidence", () => {
  const repos = [
    {
      id: "a",
      label: "same",
      root: "/a",
      observation: { status: "observed", repository_id: "git1", worktree_id: "wa", branch: "main" },
    },
    {
      id: "b",
      label: "same",
      root: "/b",
      observation: {
        status: "observed",
        repository_id: "git1",
        worktree_id: "wb",
        branch: "feature",
      },
    },
    { id: "c", label: "same", root: "/c" },
  ];
  const before = JSON.stringify(repos);
  assert.equal(repositoryGroups(repos).length, 2);
  assert.deepEqual(
    filterRepositories(repos, { repo: "git1", branch: "feature" }).map((r) => r.id),
    ["b"],
  );
  assert.equal(branchName(repos[2]), "UNKNOWN");
  assert.equal(JSON.stringify(repos), before);
});
test("repository filtering retains original handoff endpoint but excludes unrelated work", () => {
  const { graph, scope } = fixture();
  const view = projectConsole(graph, scope, { repositoryIds: ["repoB"] });
  assert.deepEqual(
    view.nodes.map((n) => n.id),
    ["a", "b"],
  );
  assert.equal(view.edges[0].id, "handoff");
});
test("each mechanism has independently renderable static detail without executing a timeline", () => {
  for (const kind of ["loop", "manager", "session", "prompt", "pclass", "cli", "observation"]) {
    const drawing = mechanismFigure(kind);
    assert.match(drawing, new RegExp(`data-anatomy-template="${kind}"`));
    assert.ok(Number(drawing.match(/data-parts="(\d+)"/)[1]) >= 40);
    assert.doesNotMatch(drawing, /NaN|undefined|<script|<animate/);
  }
});
test("shared manager anatomy preserves original objects and unobserved conditions", () => {
  const { graph } = fixture();
  graph.objects = [{ id: "manager", namespace: "soodles.loop" }];
  graph.ports = [];
  for (const node of graph.nodes)
    Object.assign(node, {
      object_id: "manager",
      kind: "cli",
      label: "Original call",
      input_artifact_ids: [],
      output_artifact_ids: [],
      evidence_artifact_ids: [],
    });
  const before = JSON.stringify(graph);
  const model = anatomyModel(graph, { kind: "object", id: "manager" });
  assert.equal(model.template, "manager");
  assert.equal(model.peers.length, 0);
  assert.equal(model.inputs.length, 0);
  assert.equal(JSON.stringify(graph), before);
});
