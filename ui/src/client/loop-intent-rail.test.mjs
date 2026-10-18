import { test } from "node:test";
import assert from "node:assert/strict";
import { projectIntentRail, evidenceFocus } from "./loop-intent-rail.mjs";

function fixture() {
  const ids = ["req", "selection", "prompt", "readback", "call", "receipt"];
  const artifacts = ids.map((id) => ({
    id,
    role: {
      req: "loop_request",
      selection: "intent_selection",
      prompt: "native_prompt",
      readback: "loop_owner_readback",
      call: "native_tool_call",
      receipt: "native_tool_result",
    }[id],
    status: "available",
  }));
  const node = ({ id, run, object, ins, outs }) => ({
    id,
    run_id: run,
    object_id: object,
    input_artifact_ids: ins,
    output_artifact_ids: outs,
    evidence_artifact_ids: [],
  });
  const nodes = [
    node({ id: "intent-a", run: "a", object: "source", ins: [], outs: ["req"] }),
    node({ id: "prompt-a", run: "a", object: "inner", ins: ["selection"], outs: ["prompt"] }),
    node({ id: "tool-a", run: "a", object: "inner", ins: ["prompt"], outs: ["call", "receipt"] }),
    node({ id: "tool-b", run: "b", object: "outer", ins: ["prompt"], outs: ["receipt"] }),
  ];
  const edges = [
    {
      id: "native-a",
      run_id: "a",
      from_node_id: "prompt-a",
      to_node_id: "tool-a",
      kind: "handoff",
      status: "observed",
    },
    {
      id: "cross-b",
      run_id: "b",
      from_node_id: "tool-a",
      to_node_id: "tool-b",
      kind: "handoff",
      status: "observed",
    },
  ];
  return { objects: [], artifacts, nodes, edges, findings: [] };
}
test("source rail binds exact original Run carriers and never creates an invocation or causal edge", () => {
  const g = fixture(),
    original = JSON.stringify(g);
  const a = projectIntentRail(g, "a");
  assert.deepEqual(
    a.map((x) => x.id),
    ["intent", "judgment", "prompt", "tool"],
  );
  assert.deepEqual(a[0].artifact_ids, ["req"]);
  assert.deepEqual(a[1].artifact_ids, ["selection"]);
  assert.deepEqual(a[2].artifact_ids, ["prompt"]);
  assert.deepEqual(a[3].artifact_ids, ["call", "receipt"]);
  assert.ok(a.every((x) => x.execution_status === "NOT_ESTABLISHED_BY_SOURCE_RAIL"));
  assert.deepEqual(
    projectIntentRail(g, "missing").map((x) => x.source_status),
    ["unknown", "unknown", "unknown", "unknown"],
  );
  assert.equal(JSON.stringify(g), original);
});
test("private source remains represented but does not imply content permission", () => {
  const g = fixture();
  g.artifacts[0].status = "not_public";
  assert.equal(projectIntentRail(g, "a")[0].source_status, "source_referenced");
  g.artifacts[0].status = "stale";
  assert.equal(projectIntentRail(g, "a")[0].source_status, "unknown");
});
test("selection highlight keeps evidence inside selected Run, independent of spatial proximity", () => {
  const g = fixture();
  const scene = {
    nodes: g.nodes.filter((n) => n.run_id === "a"),
    edges: g.edges.filter((e) => e.run_id === "a"),
    findings: [],
  };
  const overlay = {
    steps: [
      { id: "s", matches: ["prompt-a"] },
      { id: "t", matches: ["tool-a"] },
    ],
    routes: [
      {
        id: "hyp",
        layer: "hypothesis",
        from: "s",
        to: "t",
        result: { edge_ids: ["native-a", "cross-b"] },
      },
    ],
  };
  const ref = evidenceFocus({
    scene,
    overlay,
    selection: { kind: "expected-route", id: "hypothesis/hyp" },
  });
  assert.deepEqual([...ref.edges], ["native-a"]);
  assert.ok(!ref.nodes.has("tool-b"));
  const facet = evidenceFocus({
    scene,
    overlay,
    selection: { kind: "principle", id: "inner/assertion" },
    facets: [scene.nodes[1]],
  });
  assert.ok(facet.objects.has("inner"));
  assert.ok(facet.edges.has("native-a"));
  assert.ok(!facet.nodes.has("tool-b"));
});
test("unsupported or unbound hypothesis selects no invented node or edge", () => {
  const g = fixture();
  const o = { steps: [], routes: [] };
  const s = { nodes: g.nodes, edges: [], findings: [] };
  const ref = evidenceFocus({
    scene: s,
    overlay: o,
    selection: { kind: "expected-route", id: "hypothesis/unknown" },
  });
  assert.equal(ref.edges.size, 0);
  assert.equal(ref.nodes.size, 0);
});
