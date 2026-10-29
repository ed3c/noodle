import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, expect, it, vi } from "vitest";
import { graphFixture, required } from "~/client/loop-graph-fixture";
import { LoopGraphSourceIndex, noHighlight } from "./LoopGraphSourceIndex";

afterEach(() => vi.unstubAllGlobals());
it("loads a bounded page from the pinned index and makes later files reachable", async () => {
  const { graph } = graphFixture();
  const repo = required(graph.repositories[0]);
  repo.detail = {
    sha256: "a".repeat(64),
    bytes: 1000,
    tracked: 101,
    selected: 0,
    artifact_ids: [],
  };
  repo.tracked_files = [];
  repo.selected_files = [];
  const fetch = vi.fn().mockImplementation((url: string) => {
    const query = new URL(url, "http://localhost").searchParams;
    const offset = Number(query.get("offset"));
    const files = Array.from({ length: offset === 0 ? 100 : 1 }, (_, i) => ({
      id: `file${offset + i}`,
      path: `file${offset + i}.txt`,
      git_blob: null,
      git_mode: null,
      bytes: null,
      node_ids: [],
      artifact_ids: [],
    }));
    return Promise.resolve(
      Response.json({
        repository_id: repo.id,
        revision: repo.revision,
        ref: repo.detail,
        total: 101,
        offset,
        files,
      }),
    );
  });
  vi.stubGlobal("fetch", fetch);
  const select = vi.fn();
  render(
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
    >
      <LoopGraphSourceIndex
        graph={graph}
        highlight={noHighlight}
        onSelect={select}
        onHighlight={vi.fn()}
      />
    </QueryClientProvider>,
  );
  expect(await screen.findByRole("button", { name: "檔案 file0.txt" })).toBeVisible();
  expect(fetch).toHaveBeenCalledTimes(1);
  await userEvent.click(screen.getByRole("button", { name: "下一頁" }));
  await userEvent.click(await screen.findByRole("button", { name: "檔案 file100.txt" }));
  expect(select).toHaveBeenCalledWith(expect.objectContaining({ id: "file100" }), repo.id);
  expect(fetch).toHaveBeenLastCalledWith(
    expect.stringContaining(`ref=${"a".repeat(64)}&section=tracked&search=&offset=100&limit=100`),
    expect.anything(),
  );
  await userEvent.click(screen.getByRole("button", { name: "上一頁" }));
  expect(await screen.findByRole("button", { name: "檔案 file0.txt" })).toBeVisible();
  expect(fetch).toHaveBeenCalledTimes(2);
});
