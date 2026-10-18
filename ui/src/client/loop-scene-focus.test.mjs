import { test } from "node:test";
import assert from "node:assert/strict";
import { focusExpectations, partitionMachines, focusedLayout } from "./loop-scene-focus.mjs";
const overlay = {
  steps: [
    { id: "a", matches: ["n"], owner: { id: "active" } },
    { id: "b", matches: [], owner: { id: "missing" } },
  ],
  routes: [
    {
      id: "ok",
      layer: "contract",
      from: "a",
      to: "b",
      result: { status: "matched", edge_ids: ["e"] },
    },
    { id: "gap", layer: "contract", from: "a", to: "b", result: { status: "unknown" } },
    { id: "failure", layer: "contract", from: "a", to: "b", result: { status: "deviated" } },
    { id: "idea", layer: "hypothesis", from: "a", to: "b" },
  ],
  unbound: [{ claim: "unbound" }],
};
test("focus hides only matched expectations and hypotheses; unknown and failure stay distinct", () => {
  const before = JSON.stringify(overlay);
  const result = focusExpectations(overlay);
  assert.deepEqual(
    result.routes.map((r) => r.id),
    ["gap", "failure"],
  );
  assert.equal(result.hidden, 2);
  assert.equal(result.routes[0].result.status, "unknown");
  assert.equal(result.unbound, overlay.unbound);
  assert.equal(JSON.stringify(overlay), before);
});
test("edge inspection expands matching expectations; selected hypotheses and full overlay remain reachable", () => {
  assert.deepEqual(
    focusExpectations(overlay, { selection: { kind: "edge", id: "e" } }).routes.map((r) => r.id),
    ["ok", "gap", "failure"],
  );
  assert.deepEqual(
    focusExpectations(overlay, {
      selection: { kind: "expected-route", id: "hypothesis/idea" },
    }).routes.map((r) => r.id),
    ["gap", "failure", "idea"],
  );
  assert.deepEqual(focusExpectations(overlay, { full: true }).routes, overlay.routes);
  assert.equal(focusExpectations(overlay, { sourceStatus: "drift" }).routes.length, 3);
});
test("machine index preserves unique identities and promotes unobserved expected participants without fabricating events", () => {
  const objects = ["active", "missing", "unused"].map((id) => ({ id }));
  const scene = { nodes: [{ id: "n", object_id: "active" }] };
  const result = partitionMachines(objects, scene, {
    overlay: focusExpectations(overlay),
    run: "r",
  });
  assert.deepEqual(
    result.primary.map((o) => o.id),
    ["active", "missing"],
  );
  assert.deepEqual(
    result.index.map((o) => o.id),
    ["unused"],
  );
  assert.equal(scene.nodes.length, 1);
  assert.equal(
    partitionMachines(objects, scene, { overlay, run: "r", full: true }).index.length,
    0,
  );
  assert.equal(partitionMachines(objects, scene, { overlay }).primary.length, 3);
});
test("one floor separates compact index, machines and occurrence targets without overlap", () => {
  for (const count of [0, 1, 12, 40]) {
    const layout = focusedLayout(5, count, 14);
    assert.equal(layout.index.length, count);
    assert.equal(layout.objects.length, 5);
    const boxes = [
      ...layout.index.map(([x, y]) => [x, y, 250, 48]),
      ...layout.objects.map(([x, y]) => [x, y, 250, 130]),
      ...layout.items.map(([x, y]) => [x, y, 248, 158]),
    ];
    for (const [i, a] of boxes.entries()) {
      assert.ok(a[0] + a[2] <= layout.width && a[1] + a[3] <= layout.height);
      for (const b of boxes.slice(i + 1)) {
        assert.ok(
          a[0] + a[2] <= b[0] || b[0] + b[2] <= a[0] || a[1] + a[3] <= b[1] || b[1] + b[3] <= a[1],
        );
      }
    }
  }
});

test("source rail reserves separate source-only floor without dropping native occurrence slots", () => {
  const source = focusedLayout(2, 1, { count: 4, intentRail: true });
  const baseline = focusedLayout(2, 1, 4);
  assert.equal(source.items.length, 4);
  assert.equal(source.objects.length, 2);
  assert.equal(source.index.length, 1);
  assert.equal(source.items[0][1] - baseline.items[0][1], 146);
  assert.equal(source.height - baseline.height, 146);
});
