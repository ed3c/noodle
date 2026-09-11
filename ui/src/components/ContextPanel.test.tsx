import { render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ContextPanel } from "./ContextPanel";
import { buildOrder, buildSession, buildSnapshot, buildStage } from "../test-utils";

let mockSnapshot = buildSnapshot();
let mockActiveChannel: { type: "scheduler" } | { type: "agent"; sessionId: string } = {
  type: "scheduler",
};

vi.mock("~/client", async () => {
  const actual = await vi.importActual("~/client");
  return {
    ...(actual as Record<string, unknown>),
    useSuspenseSnapshot: () => ({ data: mockSnapshot }),
    useActiveChannel: () => ({
      activeChannel: mockActiveChannel,
      setActiveChannel: vi.fn(),
    }),
    useReviewDiff: () => ({ data: undefined, isLoading: false, error: undefined }),
  };
});

describe("ContextPanel", () => {
  beforeEach(() => {
    const session = buildSession({ id: "actor-1", worktree_name: "wt-actor-1" });
    mockSnapshot = buildSnapshot({ sessions: [session] });
    mockActiveChannel = { type: "agent", sessionId: session.id };
  });

  it("renders the session worktree in the agent stats area", () => {
    render(<ContextPanel />);

    expect(screen.getByText("Worktree")).toBeInTheDocument();
    expect(screen.getByText("wt-actor-1")).toBeInTheDocument();
  });

  it("renders the matched stage invocation with a collapsed prompt", () => {
    const session = buildSession({ id: "actor-1" });
    const matchingStage = buildStage({
      session_id: session.id,
      task_key: "execute",
      skill: "execute",
      provider: "codex",
      model: "gpt-5.4",
      prompt: "subject: ed3c/noodle#15",
    });
    mockSnapshot = buildSnapshot({
      sessions: [session],
      orders: [buildOrder({ id: "ed3c/noodle#15", stages: [matchingStage] })],
    });
    mockActiveChannel = { type: "agent", sessionId: session.id };

    render(<ContextPanel />);

    expect(screen.getByText("Invocation")).toBeInTheDocument();
    expect(screen.getByText("Task").parentElement).toHaveTextContent("execute");
    expect(screen.getByText("Skill").parentElement).toHaveTextContent("execute");
    expect(screen.getByText("Provider").parentElement).toHaveTextContent("codex");
    expect(screen.getAllByText("Model")[1]?.parentElement).toHaveTextContent("gpt-5.4");
    const promptDisclosure = screen.getByText("Prompt").closest("details");
    expect(promptDisclosure).not.toHaveAttribute("open");
    expect(screen.getByText("subject: ed3c/noodle#15")).toBeInTheDocument();
  });

  it("does not render invocation fields from another order", () => {
    const session = buildSession({ id: "actor-1" });
    mockSnapshot = buildSnapshot({
      sessions: [session],
      orders: [
        buildOrder({
          id: "unrelated-order",
          stages: [
            buildStage({
              session_id: "actor-2",
              task_key: "unrelated-task",
              skill: "unrelated-skill",
              provider: "unrelated-provider",
              model: "unrelated-model",
              prompt: "unrelated-prompt",
            }),
          ],
        }),
      ],
    });
    mockActiveChannel = { type: "agent", sessionId: session.id };

    render(<ContextPanel />);

    expect(screen.queryByText("Invocation")).not.toBeInTheDocument();
    for (const value of [
      "unrelated-task",
      "unrelated-skill",
      "unrelated-provider",
      "unrelated-model",
      "unrelated-prompt",
    ]) {
      expect(screen.queryByText(value)).not.toBeInTheDocument();
    }
  });

  it("preserves the session context when no stage matches", () => {
    const session = buildSession({ id: "actor-1", worktree_name: "wt-unmatched" });
    mockSnapshot = buildSnapshot({ sessions: [session], orders: [] });
    mockActiveChannel = { type: "agent", sessionId: session.id };

    render(<ContextPanel />);

    expect(screen.getByText("wt-unmatched")).toBeInTheDocument();
    expect(screen.queryByText("Invocation")).not.toBeInTheDocument();
  });
});
