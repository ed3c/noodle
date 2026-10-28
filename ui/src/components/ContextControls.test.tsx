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
