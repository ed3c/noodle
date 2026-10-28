import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { ContextControls } from "./ContextControls";
import type { ContextObservation } from "~/client/context-controls";

const hash = "a".repeat(64);
const choice = {
  id: "fixed-task",
  label: "Codex diagram",
  version: hash,
  composition_id: "codex-drawing",
  stage_id: "draw",
};
const ref = { path: "/private/fixture/accepted.json", sha256: hash };
function observation(): ContextObservation {
  return {
    schema: 1,
    owner: "soodles.loop-ui",
    status: "prepared",
    version: "b".repeat(64),
    summary: "Original task prepared",
    request: ref,
    composition: { id: choice.composition_id, stage_id: choice.stage_id, definition: ref },
    active_stage: {
      request: ref,
      composition: { id: choice.composition_id, stage_id: choice.stage_id, definition: ref },
      handoffs: [],
    },
    actions: {
      start: { available: true, reason: "prepared" },
      stop: { available: false, reason: "not_started" },
      new_attempt: { available: false, reason: "review_required" },
      native_resume: { available: false, reason: "unsupported" },
    },
    identity: { order_id: null, context_id: null, session_id: null },
    candidate: null,
    acceptance: { status: "UNKNOWN", scope: "candidate", record: null, head: null, artifacts: [] },
    sources: {},
    effects: [],
  };
}
afterEach(() => vi.unstubAllGlobals());
it("starts a saved-progress attempt only explicitly and does not present a queued acknowledgement as running", async () => {
  const saved = observation();
  saved.status = "stopped";
  saved.actions.start = { available: false, reason: "not_started" };
  saved.actions.new_attempt = { available: true, reason: "original_saved_progress_retryable" };
  const queued = {
    ...saved,
    status: "queued",
    actions: {
      ...saved.actions,
      new_attempt: { available: false, reason: "original_owner_readback_required" },
    },
    effects: ["original_control_queued"],
  };
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ schema: 1, choices: [choice] }))
    .mockResolvedValueOnce(Response.json(saved))
    .mockResolvedValueOnce(Response.json(queued));
  vi.stubGlobal("fetch", fetch);
  render(<ContextControls />);
  await screen.findByRole("option", { name: choice.label });
  await userEvent.selectOptions(screen.getByLabelText("選擇組合與任務"), choice.id);
  await userEvent.click(screen.getByRole("button", { name: "查明原任務狀態" }));
  const retry = await screen.findByRole("button", { name: "以已保存成果開始新嘗試" });
  expect(fetch).toHaveBeenCalledTimes(2);
  await userEvent.click(retry);
  await screen.findByText("原任務狀態：已收到操作，等待原任務確認");
  expect(screen.queryByText("原任務狀態：執行中")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "依原審查開始新嘗試" })).toBeDisabled();
  expect(JSON.parse(fetch.mock.calls[2]?.[1].body)).toEqual({
    id: choice.id,
    registry_version: choice.version,
    expected_version: saved.version,
    action: "new_attempt",
  });
  expect(fetch).toHaveBeenCalledTimes(3);
});
it("requires explicit selection and observation before sending only bound IDs to the owner", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ schema: 1, choices: [choice] }))
    .mockResolvedValueOnce(Response.json(observation()))
    .mockResolvedValueOnce(
      Response.json({ ...observation(), status: "running", effects: ["dispatch_requested"] }),
    );
  vi.stubGlobal("fetch", fetch);
  render(<ContextControls />);
  await screen.findByRole("option", { name: /Codex diagram/ });
  expect(fetch).toHaveBeenCalledTimes(1);
  await userEvent.selectOptions(screen.getByLabelText("選擇組合與任務"), "fixed-task");
  expect(fetch).toHaveBeenCalledTimes(1);
  await userEvent.click(screen.getByRole("button", { name: "查明原任務狀態" }));
  await screen.findByText("原任務狀態：已準備");
  expect(screen.getByRole("button", { name: "依原審查開始新嘗試" })).toBeDisabled();
  expect(screen.getByText(/原模型對話續接：不支援/)).toBeVisible();
  await userEvent.click(screen.getByRole("button", { name: "執行已準備任務" }));
  await screen.findByText("原任務狀態：執行中");
  expect(JSON.parse(fetch.mock.calls[2]?.[1].body)).toEqual({
    id: choice.id,
    registry_version: choice.version,
    expected_version: "b".repeat(64),
    action: "start",
  });
  expect(fetch.mock.calls[2]?.[1].body).not.toContain("path");
});
it("clears stale action availability after an unknown result without automatically retrying", async () => {
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ schema: 1, choices: [choice] }))
    .mockResolvedValueOnce(Response.json(observation()))
    .mockResolvedValueOnce(new Response("unknown", { status: 502 }));
  vi.stubGlobal("fetch", fetch);
  render(<ContextControls />);
  await screen.findByRole("option", { name: /Codex diagram/ });
  await userEvent.selectOptions(screen.getByLabelText("選擇組合與任務"), choice.id);
  await userEvent.click(screen.getByRole("button", { name: "查明原任務狀態" }));
  await screen.findByText("原任務狀態：已準備");
  await userEvent.click(screen.getByRole("button", { name: "執行已準備任務" }));
  await screen.findByRole("alert");
  expect(screen.queryByRole("button", { name: "執行已準備任務" })).not.toBeInTheDocument();
  expect(fetch).toHaveBeenCalledTimes(3);
});
it("shows an accepted static artifact only after a second exact owner observation and keeps the iframe unprivileged", async () => {
  const observed = observation();
  const artifact = { id: "diagram", label: "diagram.html", format: "html", source: ref };
  observed.candidate = ref;
  observed.acceptance = {
    status: "PASS",
    scope: "integrated",
    record: ref,
    head: "fixed-head",
    artifacts: [artifact],
  };
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ schema: 1, choices: [choice] }))
    .mockResolvedValueOnce(Response.json(observed))
    .mockResolvedValueOnce(
      Response.json({
        schema: 1,
        version: observed.version,
        artifact,
        candidate: ref,
        acceptance: observed.acceptance,
        document: "<p>accepted drawing</p>",
      }),
    );
  vi.stubGlobal("fetch", fetch);
  render(<ContextControls />);
  await screen.findByRole("option", { name: /Codex diagram/ });
  await userEvent.selectOptions(screen.getByLabelText("選擇組合與任務"), choice.id);
  await userEvent.click(screen.getByRole("button", { name: "查明原任務狀態" }));
  await userEvent.click(await screen.findByRole("button", { name: "預覽已接受圖解" }));
  const iframe = await screen.findByTitle("已接受圖解：diagram.html");
  expect(iframe).toHaveAttribute("sandbox", "");
  expect(iframe).toHaveAttribute("referrerpolicy", "no-referrer");
  expect(JSON.parse(fetch.mock.calls[2]?.[1].body)).toEqual({
    id: choice.id,
    registry_version: choice.version,
    expected_version: observed.version,
    artifact_id: "diagram",
  });
});
it("does not treat a new selection as a continuation of the previous task", async () => {
  const other = { ...choice, id: "pi", label: "Pi diagram", composition_id: "pi-drawing" };
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ schema: 1, choices: [choice, other] }))
    .mockResolvedValueOnce(Response.json(observation()));
  vi.stubGlobal("fetch", fetch);
  render(<ContextControls />);
  await screen.findByRole("option", { name: /Codex diagram/ });
  await userEvent.selectOptions(screen.getByLabelText("選擇組合與任務"), choice.id);
  await userEvent.click(screen.getByRole("button", { name: "查明原任務狀態" }));
  await screen.findByText("原任務狀態：已準備");
  await userEvent.selectOptions(screen.getByLabelText("選擇組合與任務"), other.id);
  expect(screen.queryByText("原任務狀態：已準備")).not.toBeInTheDocument();
  expect(fetch).toHaveBeenCalledTimes(2);
});

it("does not let an optimistic reason override an unavailable original action", async () => {
  const observed = observation();
  observed.actions.start = { available: false, reason: "prepared_original_owner" };
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ schema: 1, choices: [choice] }))
    .mockResolvedValueOnce(Response.json(observed));
  vi.stubGlobal("fetch", fetch);
  render(<ContextControls />);
  await screen.findByRole("option", { name: /Codex diagram/ });
  await userEvent.selectOptions(screen.getByLabelText("選擇組合與任務"), choice.id);
  await userEvent.click(screen.getByRole("button", { name: "查明原任務狀態" }));
  expect(await screen.findByRole("button", { name: "執行已準備任務" })).toBeDisabled();
  expect(screen.getByText("目前無法執行，請查明狀態。")).toBeVisible();
  expect(screen.queryByText("任務已準備，可以執行。")).not.toBeInTheDocument();
});

it("shows the current stage and handoff sources while actions retain the registered entry", async () => {
  const observed = observation();
  observed.active_stage = {
    request: { path: "/private/fixture/review-request.json", sha256: "c".repeat(64) },
    composition: { ...observed.composition, stage_id: "review" },
    handoffs: [{ path: "/private/fixture/handoff.json", sha256: "d".repeat(64) }],
  };
  observed.actions.stop = { available: true, reason: "original_task_active" };
  const fetch = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ schema: 1, choices: [choice] }))
    .mockResolvedValueOnce(Response.json(observed))
    .mockResolvedValueOnce(Response.json(observed));
  vi.stubGlobal("fetch", fetch);
  render(<ContextControls />);
  await screen.findByRole("option", { name: /Codex diagram/ });
  await userEvent.selectOptions(screen.getByLabelText("選擇組合與任務"), choice.id);
  await userEvent.click(screen.getByRole("button", { name: "查明原任務狀態" }));
  expect(await screen.findByText("目前階段：review")).toBeVisible();
  expect(screen.getByText("已依原驗收完成 1 次階段交接。")).toBeVisible();
  await userEvent.click(screen.getByText("診斷與固定來源"));
  expect(screen.getByText("/private/fixture/handoff.json")).toBeVisible();
  expect(screen.getByText(/入口階段：draw/)).toBeVisible();
  await userEvent.click(screen.getByRole("button", { name: "停止原任務" }));
  expect(JSON.parse(fetch.mock.calls[2]?.[1].body)).toEqual({
    id: choice.id,
    registry_version: choice.version,
    expected_version: observed.version,
    action: "stop",
  });
});
