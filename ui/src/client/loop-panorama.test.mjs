import { test } from "node:test";
import assert from "node:assert/strict";
import { projectPanorama, projectRunDetail, readStructuredField } from "./loop-panorama.mjs";
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
  g.nodes.forEach((n) => (n.identity.session_id = "same-name"));
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
