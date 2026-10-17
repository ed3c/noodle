import { afterEach, describe, expect, it, vi } from "vitest";
import { fetchArtifact, fetchGraphFile, fetchLoopGraph, parseLoopGraph } from "./loop-graph";
import { graphFixture, required } from "./loop-graph-fixture";

afterEach(() => vi.unstubAllGlobals());
const root = () => ({ ...graphFixture(), schema_version: 2 });
describe("readonly v2 graph boundary", () => {
  it("preserves repeated occurrences, original unknown and all source metadata", () => {
    const parsed = parseLoopGraph(root());
    expect(parsed.graph.objects).toHaveLength(1);
    expect(parsed.graph.nodes).toHaveLength(4);
    expect(parsed.graph.nodes[0]?.status.acceptance).toBe("unknown");
    expect(parsed.graph.repositories[0]?.tracked_files[0]?.git_blob).toBe("blobA");
    expect(parsed.graph.artifacts[0]).not.toHaveProperty("content");
  });
  it("retains optional Git readback and refuses promotion to execution evidence", () => {
    const view = root();
    const repo = required(view.graph.repositories[0]);
    const obs = {
      status: "observed",
      scope: "display_readback",
      observed_at: "2026-10-17T06:00:00Z",
      repository_id: "git",
      worktree_id: "wt",
      snapshot_id: "snapshot",
      branch: "main",
      detached: false,
      dirty: true,
      revision: repo.revision,
      gap: null,
    };
    const input = {
      ...view,
      graph: {
        ...view.graph,
        repositories: [{ ...repo, observation: obs }, ...view.graph.repositories.slice(1)],
      },
    };
    expect(parseLoopGraph(input).graph.repositories[0]?.observation?.branch).toBe("main");
    obs.scope = "native_execution";
    expect(() => parseLoopGraph(input)).toThrow();
    expect(parseLoopGraph(view).graph.repositories[0]?.observation).toBeUndefined();
  });
  it("rejects malformed shape, dangling refs, duplicate IDs and causal cycles", () => {
    expect(() => parseLoopGraph({ schema_version: 3 })).toThrow();
    const duplicate = root();
    required(duplicate.graph.objects.push(duplicate.graph.objects[0]));
    expect(() => parseLoopGraph(duplicate)).toThrow("Duplicate");
    const missing = root();
    required(missing.graph.nodes[0]).object_id = "missing";
    expect(() => parseLoopGraph(missing)).toThrow("reference");
    const cycle = root();
    required(cycle.graph.edges[0]).to_node_id = "first";
    expect(() => parseLoopGraph(cycle)).toThrow("cycle");
  });
  it("retains ETag readback and reports unavailable Host", async () => {
    const previous = { view: graphFixture(), etag: '"revision1"' };
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(new Response(null, { status: 304 }))
      .mockResolvedValueOnce(new Response(null, { status: 503 }));
    vi.stubGlobal("fetch", fetch);
    expect(await fetchLoopGraph(previous)).toBe(previous);
    expect(fetch.mock.calls[0]?.[1].headers.get("If-None-Match")).toBe('"revision1"');
    await expect(fetchLoopGraph(previous)).rejects.toThrow("503");
  });
  it("only reads registered IDs, refuses changed digest and retains stale content status", async () => {
    const artifact = required(graphFixture().graph.artifacts[0]);
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(
        Response.json({ artifact, status: "available", content: "original", error: null }),
      )
      .mockResolvedValueOnce(
        Response.json(
          { artifact, status: "stale", content: null, error: "source drift" },
          { status: 409 },
        ),
      )
      .mockResolvedValueOnce(
        Response.json({
          artifact: { ...artifact, source: { ...artifact.source, sha256: "changed" } },
          status: "available",
          content: "wrong version",
          error: null,
        }),
      );
    vi.stubGlobal("fetch", fetch);
    const original = await fetchArtifact(artifact);
    expect(original.content).toBe("original");
    expect(fetch.mock.calls[0]?.[0]).toBe("/api/loops/artifacts/a");
    expect(await fetchArtifact(artifact)).toMatchObject({
      status: "stale",
      content: null,
      error: "source drift",
    });
    await expect(fetchArtifact(artifact)).rejects.toThrow("version changed");
  });
  it("pins source index content to its registered Git blob", async () => {
    const file = required(required(graphFixture().graph.repositories[1]).tracked_files[0]);
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        Response.json({
          file: { ...file, git_blob: "different" },
          status: "available",
          content: "wrong",
          error: null,
        }),
      ),
    );
    await expect(fetchGraphFile("repo2", file)).rejects.toThrow("Git blob changed");
  });
});
