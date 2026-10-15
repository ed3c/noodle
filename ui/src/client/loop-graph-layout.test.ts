import { describe, expect, it } from "vitest";
import { layoutGraph } from "./loop-graph-layout.mjs";

describe("canonical graph geometry", () => {
  it("places a multi-input handoff after both parents without inventing adjacency", () => {
    const nodes = ["a", "b", "c", "unrelated"].map((id) => ({
      id,
      repository_id: id === "c" ? "repo2" : "repo1",
    }));
    const edges = [
      { from: "a", to: "c" },
      { from: "b", to: "c" },
    ];
    const result = layoutGraph({ nodes, edges });
    expect(result.nodes.find((node) => node.id === "c")?.rank).toBe(1);
    expect(result.nodes.find((node) => node.id === "unrelated")?.rank).toBe(0);
    expect(result.bands).toHaveLength(2);
    expect(layoutGraph({ nodes: nodes.toReversed(), edges: edges.toReversed() })).toEqual(result);
  });
  it("refuses cycles, duplicate nodes and missing endpoints", () => {
    const nodes = [{ id: "a", repository_id: "repo" }];
    expect(() => layoutGraph({ nodes, edges: [{ from: "a", to: "a" }] })).toThrow("cycle");
    expect(() => layoutGraph({ nodes: [...nodes, ...nodes], edges: [] })).toThrow("Duplicate");
    expect(() => layoutGraph({ nodes, edges: [{ from: "a", to: "b" }] })).toThrow("endpoint");
  });
});
