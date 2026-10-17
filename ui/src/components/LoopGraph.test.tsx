import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";
import { graphFixture, required } from "~/client/loop-graph-fixture";
import { LoopGraphContent } from "./LoopGraph";
import { knownFailureFrontier } from "~/client/loop-graph-presentation";
import { compareJSON, compareText } from "./LoopGraphComparison";

afterEach(() => vi.unstubAllGlobals());
function setup(customize?: (view: ReturnType<typeof graphFixture>) => void) {
  const view = graphFixture();
  customize?.(view);
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
  it("loads one selected anatomy, keeps unknown Git facts and makes view changes effect-free", async () => {
    const { fetch } = setup();
    const scene = within(screen.getByLabelText("立體工程全景"));
    expect(scene.queryByLabelText("節點 anatomy 詳圖")).not.toBeInTheDocument();
    await userEvent.click(scene.getByRole("button", { name: "物件 · Original Manager" }));
    const panel = within(await scene.findByLabelText("節點 anatomy 詳圖"));
    expect(await panel.findByRole("img", { name: "功能組件 · 接口配線台" })).toBeVisible();
    await userEvent.click(panel.getByRole("button", { name: "拆解／組裝機構" }));
    await userEvent.click(panel.getByRole("button", { name: "詳圖 1×／4×" }));
    await userEvent.selectOptions(scene.getByLabelText("來源Branch篩選"), "UNKNOWN");
    expect(fetch).not.toHaveBeenCalled();
    expect(scene.getAllByRole("button", { name: "物件 · Original Manager" })).toHaveLength(1);
    await userEvent.click(scene.getByRole("button", { name: "還原全景" }));
    expect(scene.getByLabelText("來源Branch篩選")).toHaveValue("");
  });
  it("keeps isometric inspection read-only and opens the original comparison on demand", async () => {
    const { fetch } = setup();
    const console = within(screen.getByLabelText("立體工程全景"));
    expect(console.getAllByRole("button", { name: "物件 · Original Manager" })).toHaveLength(1);
    await userEvent.selectOptions(console.getByLabelText("立體視圖執行範圍"), "run1");
    await userEvent.click(console.getByRole("button", { name: "拆解資料載體" }));
    await userEvent.click(console.getByRole("button", { name: "右轉" }));
    expect(fetch).not.toHaveBeenCalled();
    await userEvent.click(console.getByRole("button", { name: "資料流 · Handoff known · 1 段" }));
    expect(console.getByLabelText("立體選取讀回")).toHaveTextContent("歷史記錄／目前效力未證實");
    expect(fetch).not.toHaveBeenCalled();
    await userEvent.click(console.getByRole("button", { name: "比較原始輸入與輸出" }));
    expect(
      await within(screen.getByLabelText("選取內容比較")).findByText('{"value":2}'),
    ).toBeVisible();
    expect(fetch).toHaveBeenCalledTimes(2);
    await userEvent.click(console.getByRole("button", { name: "還原全景" }));
    expect(console.getByLabelText("立體視圖執行範圍")).toHaveValue("");
  });
  it("expands native occurrences and opens typed handoffs in the existing comparison", async () => {
    setup();
    await userEvent.click(screen.getByText("深入 Session、提示詞型態與全部 P-class 節點"));
    const detail = within(screen.getByLabelText("Session 與 P-class 細部全景"));
    expect(await detail.findByLabelText("Session 與提示詞細部 DAG")).toBeVisible();
    expect(detail.getByText(/未提供的 Session 工程判斷仍為 UNKNOWN/)).toBeVisible();
    await userEvent.click(detail.getByText("#1 Handoff known · observed/unknown"));
    expect(
      required(
        detail.getAllByText("prompt (application/json) → native_input (application/json)")[0],
      ),
    ).toBeVisible();
    await userEvent.click(required(detail.getAllByRole("button", { name: "同窗比較這次傳遞" })[0]));
    expect(
      await within(screen.getByLabelText("選取內容比較")).findByText('{"value":2}'),
    ).toBeVisible();
    await userEvent.selectOptions(detail.getByLabelText("細部 Run"), "run2");
    expect(detail.queryByText("#1 Handoff known · observed/unknown")).not.toBeInTheDocument();
    expect(detail.getByRole("button", { name: "細部 cli · Invocation other" })).toBeVisible();
  });
  it("keeps route, selected occurrence and return-to-global synchronized", async () => {
    // jsdom does not implement layout scrolling.
    // oxlint-disable-next-line no-extend-native
    Object.defineProperty(Element.prototype, "scrollIntoView", {
      configurable: true,
      value: vi.fn(),
    });
    setup();
    const panorama = within(screen.getByLabelText("Workspace 全景圖"));
    const route = panorama.getByRole("heading", { name: /run2/ }).closest("article");
    if (!route) {
      throw new Error("Missing route card");
    }
    await userEvent.click(within(route).getByRole("button", { name: "展開本次執行 DAG" }));
    expect(panorama.getByLabelText("細部 Run")).toHaveValue("run2");
    expect(panorama.getByRole("button", { name: "細部 cli · Invocation other" })).toBeVisible();
    await userEvent.click(panorama.getByRole("button", { name: "細部 cli · Invocation other" }));
    expect(panorama.getByLabelText("全景定位")).toHaveTextContent("Invocation other");
    await userEvent.click(panorama.getByRole("button", { name: "返回全局" }));
    expect(panorama.getByLabelText("全景定位")).toHaveTextContent("Goal / 全部適用路徑");
    expect(screen.getByRole("button", { name: /紀錄 Invocation first/ })).toBeVisible();
  });
  it("opens only the bound JSON field and keeps missing fields unknown", async () => {
    const { view, wrap, rerender } = setup((value) => {
      required(value.graph.nodes[0]).identity.locator = "/source/c.json#/value";
    });
    await userEvent.click(screen.getByRole("button", { name: /紀錄 Invocation first/ }));
    const field = within(screen.getByLabelText("結構化欄位內容"));
    expect(await field.findByText("2")).toBeVisible();
    const next = structuredClone(view);
    required(next.graph.nodes[0]).identity.locator = "/source/c.json#/missing";
    rerender(wrap(next));
    await userEvent.click(screen.getByRole("button", { name: "採用新版本" }));
    await userEvent.click(screen.getByRole("button", { name: /紀錄 Invocation first/ }));
    expect(
      await within(screen.getByLabelText("結構化欄位內容")).findByText("欄位狀態：unknown"),
    ).toBeVisible();
  });
  it("shows new occurrences after adopting a newer snapshot from an object focus", async () => {
    // jsdom has no layout scrolling; this mock is confined to the UI test environment.
    // oxlint-disable-next-line no-extend-native
    Object.defineProperty(Element.prototype, "scrollIntoView", {
      configurable: true,
      value: vi.fn(),
    });
    const { view, wrap, rerender } = setup();
    await userEvent.click(screen.getByRole("button", { name: "Original Manager" }));
    const updated = structuredClone(view);
    updated.graph.nodes.push({
      ...required(updated.graph.nodes[0]),
      id: "fresh",
      label: "Fresh attempt",
    });
    rerender(wrap(updated));
    expect(screen.queryByRole("button", { name: /紀錄 Fresh attempt/ })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "採用新版本" }));
    expect(screen.getByRole("button", { name: /紀錄 Fresh attempt/ })).toBeInTheDocument();
  });
  it("retains selected runs and their refusal when no invocation DAG exists", () => {
    const { view, wrap, rerender } = setup();
    const empty = structuredClone(view);
    empty.graph.nodes = [];
    empty.graph.edges = [];
    rerender(wrap(empty));
    const missing = within(screen.getByLabelText("尚無 DAG 的選定 Runs"));
    expect(missing.getByRole("button", { name: /Original command refused/ })).toBeVisible();
    expect(missing.getAllByText(/行為 Goal 驗收：UNKNOWN/)).toHaveLength(2);
  });
  it("opens original comparisons from the panorama and keeps relation controls after focus", async () => {
    // jsdom has no layout scrolling; this mock is confined to the UI test environment.
    // oxlint-disable-next-line no-extend-native
    Object.defineProperty(Element.prototype, "scrollIntoView", {
      configurable: true,
      value: vi.fn(),
    });
    setup();
    const panorama = within(screen.getByLabelText("Workspace 全景圖"));
    await userEvent.click(panorama.getByText("責任物件與跨 Repo 關係總覽"));
    await userEvent.click(panorama.getByText("Original Manager → Original Manager · 3 次交接"));
    await userEvent.click(
      required(panorama.getAllByRole("button", { name: "Handoff known · observed/unknown" })[0]),
    );
    expect(
      await within(screen.getByLabelText("選取內容比較")).findByText('{"value":2}'),
    ).toBeVisible();
    await userEvent.click(panorama.getByRole("button", { name: "展開這組關係 DAG" }));
    expect(
      required(panorama.getAllByRole("button", { name: "Handoff known · observed/unknown" })[0]),
    ).toBeVisible();
    expect(screen.queryByRole("button", { name: /紀錄 Invocation other/ })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "返回 Workspace 全部執行" }));
    expect(screen.getByRole("button", { name: /紀錄 Invocation other/ })).toBeInTheDocument();
  });
  it("scopes cards, notes and metrics to the selected goal without treating completion as closure", async () => {
    setup();
    const panorama = within(screen.getByLabelText("Workspace 全景圖"));
    await userEvent.click(panorama.getByText("工作範圍 · 全部 Runs"));
    await userEvent.type(panorama.getByLabelText("Workspace 行為目標"), "Goal two");
    await userEvent.click(panorama.getByRole("checkbox", { name: /org\/one/ }));
    await userEvent.click(panorama.getByRole("button", { name: "套用工作範圍" }));
    expect(panorama.getByText(/行為 Goal 驗收：UNKNOWN/)).toBeInTheDocument();
    expect(
      panorama.queryByRole("button", { name: /Original command refused/ }),
    ).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "全部卡片" }));
    expect(await screen.findByText("Card org/two")).toBeVisible();
    expect(screen.queryByText("Card org/one")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "全部筆記" }));
    expect(await screen.findByText("Note org/two")).toBeVisible();
    expect(screen.queryByText("Note org/one")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "量測" }));
    expect(screen.queryByText("分子 2 / 分母 unknown")).not.toBeInTheDocument();
  });
  it("shows unique objects, repeated occurrences, typed handoffs and distinct failure/unknown without preloading", async () => {
    const { fetch } = setup();
    expect(
      within(screen.getByLabelText("內外迴圈立體資料流")).getAllByText("Original Manager"),
    ).toHaveLength(1);
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
    expect(screen.getByRole("button", { name: "Original Manager" })).toHaveClass("is-linked");
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
