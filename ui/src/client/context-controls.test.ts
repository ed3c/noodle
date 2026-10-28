import { expect, it } from "vitest";
import { parseContextObservation } from "./context-controls";

const hash = "a".repeat(64);
const choice = {
  id: "entry",
  label: "Pipeline",
  version: hash,
  composition_id: "pipeline",
  stage_id: "draw",
};
function packet() {
  const request = { path: "/owner/root.json", sha256: hash };
  const composition = {
    id: "pipeline",
    stage_id: "draw",
    definition: { path: "/owner/catalog.json", sha256: hash },
  };
  const unavailable = { available: false, reason: "unsupported" };
  return {
    schema: 1,
    owner: "soodles.loop-ui",
    status: "pending",
    version: hash,
    summary: "Review",
    request,
    composition,
    active_stage: {
      request: { path: "/owner/review.json", sha256: "b".repeat(64) },
      composition: {
        ...composition,
        stage_id: "review",
        definition: { ...composition.definition },
      },
      handoffs: [{ path: "/owner/handoff.json", sha256: "c".repeat(64) }],
    },
    identity: { order_id: "review-task", context_id: null, session_id: null },
    actions: {
      start: unavailable,
      stop: unavailable,
      new_attempt: unavailable,
      native_resume: unavailable,
    },
    candidate: null,
    acceptance: { status: "UNKNOWN", scope: "candidate", record: null, head: null, artifacts: [] },
    sources: {},
    effects: [],
  };
}
it("preserves root and current stage separately without interpreting handoff paths", () => {
  const raw = packet();
  const result = parseContextObservation(raw, choice);
  expect(result.request).toEqual(raw.request);
  expect(result.composition.stage_id).toBe("draw");
  expect(result.active_stage).toEqual(raw.active_stage);
  raw.active_stage.request = raw.request;
  raw.active_stage.composition = raw.composition;
  raw.active_stage.handoffs = [];
  expect(parseContextObservation(raw, choice).active_stage.handoffs).toEqual([]);
});
it.each([
  [
    "missing",
    (raw: Record<string, unknown>) => {
      delete raw.active_stage;
    },
  ],
  [
    "null",
    (raw: Record<string, unknown>) => {
      raw.active_stage = null;
    },
  ],
])("rejects %s active stage", (_name, mutate) => {
  const raw = packet();
  mutate(raw);
  expect(() => parseContextObservation(raw, choice)).toThrow();
});
it.each([
  [
    "changed entry",
    (p: ReturnType<typeof packet>) => {
      p.composition.stage_id = "review";
    },
  ],
  [
    "foreign catalog",
    (p: ReturnType<typeof packet>) => {
      p.active_stage.composition.definition.sha256 = "d".repeat(64);
    },
  ],
  [
    "foreign composition",
    (p: ReturnType<typeof packet>) => {
      p.active_stage.composition.id = "other";
    },
  ],
  [
    "invalid alias",
    (p: ReturnType<typeof packet>) => {
      p.active_stage.composition.stage_id = "../review";
    },
  ],
  [
    "no handoff",
    (p: ReturnType<typeof packet>) => {
      p.active_stage.handoffs = [];
    },
  ],
  [
    "relative request",
    (p: ReturnType<typeof packet>) => {
      p.active_stage.request.path = "request.json";
    },
  ],
  [
    "invalid request digest",
    (p: ReturnType<typeof packet>) => {
      p.active_stage.request.sha256 = "bad";
    },
  ],
  [
    "relative handoff",
    (p: ReturnType<typeof packet>) => {
      p.active_stage.handoffs[0].path = "handoff.json";
    },
  ],
  [
    "invalid handoff digest",
    (p: ReturnType<typeof packet>) => {
      p.active_stage.handoffs[0].sha256 = "bad";
    },
  ],
  [
    "duplicate handoff",
    (p: ReturnType<typeof packet>) => {
      p.active_stage.handoffs.push(p.active_stage.handoffs[0]);
    },
  ],
  [
    "entry request",
    (p: ReturnType<typeof packet>) => {
      p.active_stage.request = p.request;
    },
  ],
  [
    "entry stage",
    (p: ReturnType<typeof packet>) => {
      p.active_stage.composition.stage_id = p.composition.stage_id;
    },
  ],
])("rejects %s projection", (_name, mutate) => {
  const raw = packet();
  mutate(raw);
  expect(() => parseContextObservation(raw, choice)).toThrow();
});
it("keeps Unicode aliases and native absolute Windows references", () => {
  const raw = packet();
  raw.active_stage.composition.stage_id = "審查_②";
  raw.active_stage.request.path = String.raw`C:\owner\review.json`;
  raw.active_stage.handoffs[0].path = String.raw`\\server\owner\handoff.json`;
  expect(parseContextObservation(raw, choice).active_stage.composition.stage_id).toBe("審查_②");
});
