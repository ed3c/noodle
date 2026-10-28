import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, expect, it, vi } from "vitest";
import { graphFixture, required } from "~/client/loop-graph-fixture";
import { LoopArtifactPreview } from "./LoopArtifactPreview";

afterEach(() => vi.unstubAllGlobals());
it("loads only an explicitly opened, version-bound static preview with no sandbox capabilities", async () => {
  const artifact = { ...required(graphFixture().graph.artifacts[0]), format: "text/html" };
  const fetch = vi.fn().mockResolvedValue(
    Response.json({
      artifact,
      graph_version: "version",
      acceptance: "unknown",
      effects: [],
      document: "<p>drawing</p>",
    }),
  );
  vi.stubGlobal("fetch", fetch);
  render(
    <QueryClientProvider client={new QueryClient()}>
      <LoopArtifactPreview artifact={artifact} version="version" />
    </QueryClientProvider>,
  );
  expect(fetch).not.toHaveBeenCalled();
  await userEvent.click(screen.getByRole("button", { name: "開啟靜態預覽" }));
  const frame = await screen.findByTitle("HTML 成果靜態預覽");
  expect(frame).toHaveAttribute("sandbox", "");
  expect(frame).toHaveAttribute("referrerpolicy", "no-referrer");
  expect(fetch).toHaveBeenCalledWith(expect.stringContaining("version=version"), expect.anything());
  expect(screen.getByText(/驗收狀態未知/)).toBeVisible();
});
