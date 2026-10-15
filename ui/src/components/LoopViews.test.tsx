import { render, screen } from "@testing-library/react";
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
});
