import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { parseLoopView } from "~/client/loop-views";
import { LoopViewsContent } from "./LoopViews";

const run = {
  id: "source-run",
  repository: "org/repo",
  mode: "outer-only",
  status: "unknown",
  stages: [
    {
      id: "stage-1",
      kind: "dispatch",
      status: "transmitted",
      prompt: "<script>unsafe()</script>",
      input_refs: ["file:///private/source"],
      output_refs: [],
      consumer_status: "NOT_OBSERVED",
    },
  ],
  cards: [
    {
      id: "card-1",
      type: "SPEC",
      title: "Original specification",
      body: "Preserve the source",
      source_refs: ["source-card"],
    },
  ],
  notes: [
    {
      id: "note-1",
      title: "Working note",
      body: "Hypothesis remains unknown",
      source_refs: ["source-note"],
    },
  ],
  breakpoints: [
    {
      kind: "semantic_candidate",
      status: "unknown",
      stage_id: "stage-1",
      message: "Consumer evidence missing",
    },
  ],
  next: "read original owner",
};

describe("readonly loop display", () => {
  it("shows prompt, evidence, cards and notes without promoting consumption or executing refs", () => {
    const { container } = render(
      <LoopViewsContent view={parseLoopView({ schema_version: 1, runs: [run] })} />,
    );
    expect(screen.getByText("stage-1 · consumer: NOT_OBSERVED")).toBeInTheDocument();
    expect(screen.getByText("Original specification")).toBeInTheDocument();
    expect(screen.getByText("Working note")).toBeInTheDocument();
    expect(screen.getByText("Consumer evidence missing")).toBeInTheDocument();
    expect(screen.getByText("read original owner")).toBeInTheDocument();
    expect(screen.getByText("<script>unsafe()</script>")).toBeInTheDocument();
    expect(container.querySelector("script")).toBeNull();
    expect(container.querySelector("a")).toBeNull();
    expect(screen.getAllByRole("button")).toHaveLength(1);
  });
  it("switches runs and renders a refreshed source projection", async () => {
    const other = {
      ...run,
      id: "second",
      repository: "other/repo",
      stages: [],
      cards: [],
      notes: [],
      breakpoints: [],
      next: null,
    };
    const view = parseLoopView({ schema_version: 1, runs: [run, other] });
    const { rerender } = render(<LoopViewsContent view={view} />);
    await userEvent.click(screen.getByRole("button", { name: /other\/repo/ }));
    expect(screen.queryByText("Original specification")).not.toBeInTheDocument();
    rerender(
      <LoopViewsContent
        view={parseLoopView({
          schema_version: 1,
          runs: [run, { ...other, next: "new owner observation" }],
        })}
      />,
    );
    expect(screen.getByText("new owner observation")).toBeInTheDocument();
  });
  it("compares adjacent prompts and refreshes differences without changing UNKNOWN", async () => {
    const first = { ...run.stages[0], prompt: "甲😀", consumer_status: "UNKNOWN" };
    const second = { ...first, id: "stage-2" };
    const third = { ...first, id: "stage-3", prompt: "甲😀乙" };
    const project = (prompt: string) =>
      parseLoopView({
        schema_version: 1,
        runs: [{ ...run, stages: [first, second, { ...third, prompt }] }],
      });
    const { rerender } = render(<LoopViewsContent view={project(third.prompt)} />);
    expect(screen.getByText("第一階段 · 2 字元 · 無前一階段可比較。")).toBeVisible();
    const same = within(screen.getByLabelText("stage-1 → stage-2 文字比較"));
    expect(same.getByText("完全相同")).toBeVisible();
    expect(same.getByText(/2 → 2 字元（0）/)).toBeVisible();
    const changed = within(screen.getByLabelText("stage-2 → stage-3 文字比較"));
    expect(changed.getByText("文字不同")).toBeVisible();
    expect(changed.getByText(/2 → 3 字元（\+1）/)).toBeVisible();
    await userEvent.click(changed.getByText("前一階段 → 本階段：並列比較"));
    expect(changed.getByText("甲😀")).toBeVisible();
    expect(changed.getByText("甲😀乙")).toBeVisible();
    expect(changed.getByText(/僅比較文字；不代表語義改變或 consumer 已遵循/)).toBeVisible();
    rerender(<LoopViewsContent view={project("甲")} />);
    expect(changed.getByText(/2 → 1 字元（-1）/)).toBeVisible();
    expect(changed.getByText("甲")).toBeVisible();
    expect(screen.getByText("stage-3 · consumer: UNKNOWN")).toBeVisible();
    expect(screen.getByText("read original owner")).toBeVisible();
  });
});
