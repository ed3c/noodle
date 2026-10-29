import { describe, expect, it } from "vitest";
import { mountReviewGraph, parseReviewGraph } from "./review-graph.mjs";

function fixture() {
  return {
    schema: 1,
    analyzer: "noodle-go-types-v1",
    head: "a".repeat(40),
    nodes: [
      {
        id: "file",
        kind: "file",
        name: "<img src=x onerror=alert(1)>",
        package: "sample",
        file: "main.go",
        line: 1,
        end_line: 8,
      },
      {
        id: "fn",
        kind: "function",
        name: "sample.main",
        package: "sample",
        file: "main.go",
        line: 3,
        end_line: 8,
      },
    ],
    edges: [{ from: "file", to: "fn", kind: "contains" }],
    sources: [{ path: "main.go", sha256: "b".repeat(64) }],
    unknowns: [{ from: "fn", file: "main.go", line: 5, reason: "interface dispatch" }],
    limitations: ["static source, not a runtime trace"],
  };
}

describe("source-bound review graph display", () => {
  it("preserves source/unknowns and renders untrusted names only as text", () => {
    const root = document.createElement("div");
    const cleanup = mountReviewGraph(root, JSON.stringify(fixture()));
    expect(root.querySelector("img")).toBeNull();
    const node = root.querySelector("button");
    if (!node) {
      throw new Error("source node missing");
    }
    node.click();
    expect(root.textContent).toContain("b".repeat(64));
    expect(root.textContent).toContain("品質狀態未知");
    expect(root.textContent).toContain("interface dispatch");
    cleanup();
    expect(root.children.length).toBe(0);
  });
  it("rejects missing targets and duplicate identities, without affecting normal artifacts", () => {
    expect(parseReviewGraph("ordinary native trace")).toBeNull();
    const broken = fixture();
    broken.edges[0].to = "absent";
    expect(() => parseReviewGraph(JSON.stringify(broken))).toThrow("無法讀回");
    const duplicate = fixture();
    duplicate.nodes.push(duplicate.nodes[0]);
    expect(() => parseReviewGraph(JSON.stringify(duplicate))).toThrow("重複");
    const unknown = fixture();
    unknown.unknowns[0].from = "absent";
    expect(() => parseReviewGraph(JSON.stringify(unknown))).toThrow("缺少來源");
  });
});
