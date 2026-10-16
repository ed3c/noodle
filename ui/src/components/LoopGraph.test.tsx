import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";
import { graphFixture, required } from "~/client/loop-graph-fixture";
import { LoopGraphContent } from "./LoopGraph";
import { knownFailureFrontier } from "~/client/loop-graph-presentation";
import { compareJSON, compareText } from "./LoopGraphComparison";

afterEach(() => vi.unstubAllGlobals());
function setup() {
  const view = graphFixture();
  const fullView = structuredClone(view);
  for (const run of view.runs) {
    for (const stage of run.stages) {
      stage.prompt = "";
    }
    for (const card of run.cards) {
      card.body = "";
    }
    for (const note of run.notes) {
      note.body = "";
    }
  }
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const fetch = vi.fn().mockImplementation((url: string) => {
    if (url === "/api/loops") {
      return Promise.resolve(Response.json(fullView));
    }
    const artifact = view.graph.artifacts.find((a) => url.endsWith(`/${a.id}`));
    if (artifact) {
      return Promise.resolve(
        Response.json({
          artifact,
          status: "available",
          content: artifact.id === "c" ? '{"value":2}' : '{"value":1}',
          error: null,
        }),
      );
    }
    const file = required(required(view.graph.repositories[1]).tracked_files[0]);
    return Promise.resolve(
      Response.json({ file, status: "available", content: "fixed Git content", error: null }),
    );
  });
  vi.stubGlobal("fetch", fetch);
  const wrap = (value = view) => (
    <QueryClientProvider client={client}>
      <LoopGraphContent view={value} />
    </QueryClientProvider>
  );
  return { view, fetch, wrap, ...render(wrap()) };
}
describe("source-backed DAG interactions", () => {
  it("shows unique objects, repeated occurrences, typed handoffs and distinct failure/unknown without preloading", async () => {
    const { fetch } = setup();
    expect(screen.getAllByText("Original Manager")).toHaveLength(1);
    expect(
      screen.getByRole("button", { name: "紀錄 Invocation first · identity observed" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", {
        name: /交接 Handoff known · observed · prompt \+ prompt → native_input/,
      }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /× failed · Original command refused/ }),
    ).toBeVisible();
    expect(
      screen.getByRole("button", { name: "? unknown · Consumer behavior not observed" }),
    ).toBeVisible();
    expect(screen.queryByRole("button", { name: /交接 Handoff unknown/ })).not.toBeInTheDocument();
    await userEvent.click(screen.getByLabelText("顯示 unknown / diagnostic edges"));
    expect(screen.getByRole("button", { name: /交接 Handoff unknown/ })).toBeInTheDocument();
    expect(fetch).not.toHaveBeenCalled();
  });
  it("supports keyboard edge selection, multi-input lazy comparison and explicit snapshot updates", async () => {
    const { view, fetch, rerender, wrap } = setup();
    screen.getByRole("button", { name: /交接 Handoff known/ }).focus();
    await userEvent.keyboard("{Enter}");
    const detail = within(screen.getByLabelText("選取內容比較"));
    expect(await detail.findByText('{"value":2}')).toBeInTheDocument();
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(detail.getByText(/^文字不同 ·/)).toBeInTheDocument();
    await userEvent.selectOptions(detail.getByLabelText("輸入 / 證據"), "b");
    expect(fetch).toHaveBeenCalledTimes(3);
    const changed = structuredClone(view);
    required(changed.graph.nodes[0]).label = "New version";
    rerender(wrap(changed));
    expect(screen.getByRole("status")).toHaveTextContent("有新版本");
    expect(screen.queryByRole("button", { name: /紀錄 New version/ })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "採用新版本" }));
    expect(screen.getByRole("button", { name: /紀錄 New version/ })).toBeInTheDocument();
    expect(screen.queryByLabelText("選取內容比較")).not.toBeInTheDocument();
  });
  it("highlights source associations in both directions and reads unregistered-content files through pinned Git metadata", async () => {
    const { fetch } = setup();
    const source = within(screen.getByLabelText("完整來源索引"));
    const file = source.getByRole("button", { name: "檔案 src/prompt.json" });
    await userEvent.hover(file);
    expect(screen.getByRole("button", { name: /紀錄 Invocation first/ })).toHaveClass("is-linked");
    await userEvent.unhover(file);
    await userEvent.hover(screen.getByRole("button", { name: /紀錄 Invocation first/ }));
    expect(file).toHaveClass("is-linked");
    await userEvent.selectOptions(source.getByLabelText("Repository"), "repo2");
    await userEvent.click(source.getByRole("button", { name: "檔案 src/prompt.json" }));
    expect(await screen.findByText("fixed Git content")).toBeVisible();
    expect(fetch).toHaveBeenCalledWith(
      "/api/loops/repositories/repo2/files/file_repo2",
      expect.anything(),
    );
  });
  it("retains all namespaced cards/notes, v1 timeline and missing metric denominator", async () => {
    const { fetch } = setup();
    expect(fetch).not.toHaveBeenCalled();
    expect(screen.queryByText("original knowledge")).not.toBeInTheDocument();
    expect(screen.queryByText("original prompt")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "全部卡片" }));
    expect(await screen.findByText("Card org/one")).toBeVisible();
    expect(screen.getAllByText("original knowledge")).toHaveLength(2);
    expect(fetch).toHaveBeenCalledWith("/api/loops", expect.anything());
    expect(screen.getByText("Card org/two")).toBeVisible();
    await userEvent.click(screen.getByRole("button", { name: "全部筆記" }));
    expect(screen.getByText("Note org/one")).toBeVisible();
    expect(screen.getByText("Note org/two")).toBeVisible();
    await userEvent.click(screen.getByRole("button", { name: "量測" }));
    expect(screen.getByText("分子 2 / 分母 unknown")).toBeVisible();
    expect(screen.getByText("unknown · unknown ratio")).toBeVisible();
    await userEvent.click(screen.getByRole("button", { name: "v1 時間線" }));
    expect(await screen.findByText("stage · consumer: UNKNOWN")).toBeVisible();
    expect(screen.getByText("original prompt")).toBeInTheDocument();
  });
  it("keeps full-content failure unknown without changing the graph or reading artifacts", async () => {
    const { fetch } = setup();
    fetch.mockResolvedValueOnce(new Response(null, { status: 503 }));
    await userEvent.click(screen.getByRole("button", { name: "全部筆記" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("完整內容目前未知");
    expect(screen.queryByText("original note")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "關係圖" }));
    expect(screen.getByRole("button", { name: /紀錄 Invocation first/ })).toBeInTheDocument();
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(fetch).toHaveBeenCalledWith("/api/loops", expect.anything());
  });
  it("finds only the evidenced failure frontier and keeps later failures", () => {
    const { graph } = graphFixture();
    const original = required(graph.findings[0]);
    graph.findings.push({ ...original, id: "later", node_ids: ["third"], edge_ids: [] });
    expect(knownFailureFrontier(graph).has("failed")).toBe(true);
    expect(knownFailureFrontier(graph).has("later")).toBe(false);
    expect(graph.findings.some((finding) => finding.id === "later")).toBe(true);
    graph.edges = [];
    expect(knownFailureFrontier(graph).has("later")).toBe(true);
  });
  it("compares text and JSON structurally without semantic judgments", () => {
    expect(compareText("甲😀", "甲😀乙")).toMatchObject({
      before: 2,
      after: 3,
      removed: "",
      added: "乙",
    });
    expect(compareJSON('{"value":1,"unchanged":true}', '{"value":2,"unchanged":true}')).toEqual([
      { path: "$/value", before: "1", after: "2" },
    ]);
    expect(compareJSON("not JSON", "{}")).toBeNull();
  });
});
