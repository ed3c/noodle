import { afterEach, expect, it, vi } from "vitest";
import { graphFixture, required } from "./loop-graph-fixture";
import { fetchStaticPreview } from "./loop-preview";

afterEach(() => vi.unstubAllGlobals());
const artifact = { ...required(graphFixture().graph.artifacts[0]), format: "text/html" };
it.each([
  { artifact: { ...artifact, source: { ...artifact.source, sha256: "f".repeat(64) } } },
  { graph_version: "other" },
  { acceptance: "accepted" },
  { effects: ["dispatch"] },
])("refuses changed preview contract %j", async (change) => {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(
      Response.json({
        artifact,
        graph_version: "v",
        document: "html",
        acceptance: "unknown",
        effects: [],
        ...change,
      }),
    ),
  );
  await expect(fetchStaticPreview(artifact, "v")).rejects.toThrow("唯讀契約");
});
