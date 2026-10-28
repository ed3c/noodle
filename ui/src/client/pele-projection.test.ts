import { expect, it } from "vitest";
import { graphFixture, required } from "./loop-graph-fixture";
import { peleProjection, renderPeleProjection } from "./pele-projection";

it("runs pinned Pele on the original run, retains unknown and escapes syntax injection", () => {
  const { graph } = graphFixture();
  const n = required(graph.nodes[0]);
  n.label = `unsafe"] --> injected["<img src=https://evil.test onerror=alert(1)>`;
  const before = JSON.stringify(graph);
  const source = peleProjection(graph, n.run_id);
  expect(source).toContain("acceptance:unknown");
  const svg = renderPeleProjection(graph, n.run_id);
  expect(svg).toContain("<svg");
  const display = document.createElement("div");
  display.innerHTML = svg;
  expect(display.querySelector("img,script,foreignObject,[onerror]")).toBeNull();
  expect(display.querySelectorAll(".pele-node")).toHaveLength(6);
  expect(svg).toContain("acceptance:unknown");
  expect(JSON.stringify(graph)).toBe(before);
});

it("rejects an absent run and oversized scope without truncating", () => {
  const { graph } = graphFixture();
  expect(() => peleProjection(graph, "not-present")).toThrow();
  const n = required(graph.nodes[0]);
  graph.nodes = Array.from({ length: 81 }, (_, i) => ({ ...n, id: `n${i}` }));
  expect(() => peleProjection(graph, n.run_id)).toThrow("大小限制");
  expect(graph.nodes).toHaveLength(81);
});
