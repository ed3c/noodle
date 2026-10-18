import { test } from "node:test";
import assert from "node:assert/strict";
import { contractFacets, expectedScene, principleFigure } from "./loop-contract.mjs";
import { observedFlow } from "./loop-isometric.mjs";

test("principles bind to owner, run and exact producer roles without claiming execution", () => {
  const node = {
    id: "n",
    object_id: "o",
    run_id: "r",
    kind: "pclass",
    input_artifact_ids: [],
    output_artifact_ids: [],
    evidence_artifact_ids: [],
  };
  const graph = {
    objects: [{ id: "o", source_artifact_ids: ["catalog"] }],
    nodes: [node, { ...node, id: "other", run_id: "other" }],
    artifacts: [
      {
        id: "catalog",
        role: "mechanism_contract",
        status: "available",
        content: JSON.stringify({
          principles: [{ id: "assertion", kinds: ["pclass"], roles: [] }],
        }),
      },
    ],
  };
  const before = JSON.stringify(graph);
  const facets = contractFacets(graph, "o", "r");
  assert.equal(facets.length, 5);
  assert.deepEqual(
    facets.find((f) => f.id === "assertion").nodes.map((n) => n.id),
    ["n"],
  );
  assert.equal(facets.find((f) => f.id === "method").status, "unknown");
  assert.equal(JSON.stringify(graph), before);
  graph.artifacts[0].content = JSON.stringify({ principles: { assertion: true } });
  assert.ok(contractFacets(graph, "o", "r").every((f) => f.status === "unknown"));
  graph.artifacts[0].content = JSON.stringify({ principles: [null] });
  assert.ok(contractFacets(graph, "o", "r").every((f) => f.status === "unknown"));
  graph.artifacts[0].status = "missing";
  assert.ok(contractFacets(graph, "o", "r").every((f) => f.status === "unknown"));
});
test("expected overlay preserves unique objects, multiple matches and unbound hypotheses", () => {
  const graph = { objects: [{ id: "o", namespace: "owner" }], nodes: [{ id: "n" }, { id: "n2" }] };
  const model = {
    status: "available",
    expected: {
      components: [{ id: "c", owner: "owner" }],
      steps: [
        { id: "a", component: "c" },
        { id: "b", component: "c" },
      ],
      routes: [
        { id: "r", from: "a", to: "b", applicable: true },
        { id: "skip", from: "b", to: "a", applicable: false },
      ],
      hypotheses: [
        "unbound",
        { id: "h", from: "a", to: "b", claim: "testable" },
        { id: "bad", from: "missing", to: "b" },
      ],
    },
    comparison: {
      step_matches: { a: ["n", "n2", "missing"] },
      rows: [{ route_id: "r", status: "unknown" }],
    },
  };
  const view = expectedScene(graph, model);
  assert.equal(view.steps[0].owner, graph.objects[0]);
  assert.deepEqual(view.steps[0].matches, ["n", "n2"]);
  assert.deepEqual(
    view.routes.map((r) => r.layer),
    ["contract", "hypothesis"],
  );
  assert.equal(view.unbound.length, 2);
  assert.equal(expectedScene(graph, { status: "unknown" }).routes.length, 0);
});
test("observed failed transport stays evidenced; missing proof cannot turn green", () => {
  const edge = {
    kind: "handoff",
    status: "failed",
    current_validity: "current",
    from_node_id: "a",
    to_node_id: "b",
    input_artifact_ids: ["i"],
    output_artifact_ids: ["o"],
    evidence_artifact_ids: ["e"],
  };
  const graph = {
    nodes: ["a", "b"].map((id) => ({ id, identity: { status: "observed" } })),
    artifacts: ["i", "o", "e"].map((id) => ({ id, status: "available" })),
  };
  assert.equal(observedFlow(edge, graph), true);
  graph.artifacts.pop();
  assert.equal(observedFlow(edge, graph), false);
});
test("five classification silhouettes are distinct and independent of status", () => {
  const shapes = ["compass", "samples", "cam", "selector", "service"].map((shape) =>
    principleFigure(shape),
  );
  assert.equal(new Set(shapes).size, 5);
  assert.ok(shapes.every((s) => s.includes('viewBox="0 0 60 54"')));
});
