import { afterEach, describe, expect, it, vi } from "vitest";
import { fetchLoopView, parseLoopView, fetchLoopRun } from "./loop-views";

afterEach(() => vi.unstubAllGlobals());

describe("loop display readback", () => {
  it("rejects malformed remote data", () => {
    expect(() => parseLoopView({ schema_version: 3, runs: [] })).toThrow();
    expect(() => parseLoopView({ schema_version: 1, runs: [{ id: "a" }] })).toThrow();
  });
  it("keeps the same observation on 304 and requests its ETag", async () => {
    const previous = { view: parseLoopView({ schema_version: 1, runs: [] }), etag: '"v1"' };
    const fetch = vi.fn().mockResolvedValue(new Response(null, { status: 304 }));
    vi.stubGlobal("fetch", fetch);
    expect(await fetchLoopView(previous)).toBe(previous);
    expect(fetch).toHaveBeenCalledWith(
      "/api/loops",
      expect.objectContaining({ headers: expect.any(Headers) }),
    );
    const options: unknown = fetch.mock.calls[0]?.[1];
    if (
      typeof options !== "object" ||
      options === null ||
      !("headers" in options) ||
      !(options.headers instanceof Headers)
    ) {
      throw new Error("headers missing");
    }
    expect(options.headers.get("If-None-Match")).toBe('"v1"');
  });
  it("replaces changed data and reports failed readback", async () => {
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(
        Response.json(
          { schema_version: 1, runs: [] },
          {
            headers: { ETag: '"v2"' },
          },
        ),
      )
      .mockResolvedValueOnce(new Response("unreadable", { status: 503 }));
    vi.stubGlobal("fetch", fetch);
    const result = await fetchLoopView();
    expect(result.etag).toBe('"v2"');
    await expect(fetchLoopView()).rejects.toThrow("503");
  });
});

it("rejects a response for a different selected run digest", async () => {
  const detail = { sha256: "a".repeat(64), bytes: 100, tracked: 0, selected: 0, artifact_ids: [] };
  const view = parseLoopView({
    schema_version: 1,
    runs: [
      {
        id: "run",
        repository: "repo",
        mode: "manual",
        status: "observed",
        stages: [],
        cards: [],
        notes: [],
        breakpoints: [],
        next: null,
        detail,
      },
    ],
  });
  const [run] = view.runs;
  if (!run) {
    throw new Error("fixture missing");
  }
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValue(
        Response.json({ ...view, projection_source: { ...detail, sha256: "b".repeat(64) } }),
      ),
  );
  await expect(fetchLoopRun(run)).rejects.toThrow("Selected run version changed");
});
