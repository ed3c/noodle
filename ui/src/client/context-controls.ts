export type ContextAction = "start" | "stop" | "new_attempt";
export interface ContextChoice {
  id: string;
  label: string;
  version: string;
  composition_id: string;
  stage_id: string;
}
export interface ContextFileRef {
  path: string;
  sha256: string;
}
export interface ContextAcceptedArtifact {
  id: string;
  label: string;
  format: string;
  source: ContextFileRef;
}
export interface ContextAvailability {
  available: boolean;
  reason: string;
}
export interface ContextObservation {
  schema: 1;
  owner: "soodles.loop-ui";
  status: string;
  version: string;
  summary: string;
  request: ContextFileRef;
  composition: { id: string; stage_id: string; definition: ContextFileRef };
  actions: Record<ContextAction | "native_resume", ContextAvailability>;
  identity: { order_id: string | null; context_id: string | null; session_id: string | null };
  candidate: ContextFileRef | null;
  acceptance: {
    status: string;
    scope: string;
    record: ContextFileRef | null;
    head: string | null;
    artifacts: ContextAcceptedArtifact[];
  };
  sources: Record<string, ContextFileRef>;
  effects: string[];
}
const hash = /^[a-f0-9]{64}$/;
function object(raw: unknown): Record<string, unknown> {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    throw new Error("控制回應格式不符");
  }
  return raw as Record<string, unknown>;
}
function text(raw: unknown): string {
  if (typeof raw !== "string") {
    throw new TypeError("控制回應缺少文字欄位");
  }
  return raw;
}
function digest(raw: unknown): string {
  const value = text(raw);
  if (!hash.test(value)) {
    throw new Error("控制來源版本不符");
  }
  return value;
}
function nullableText(raw: unknown) {
  return raw === null ? null : text(raw);
}
function file(raw: unknown): ContextFileRef {
  const value = object(raw);
  return { path: text(value.path), sha256: digest(value.sha256) };
}
function artifact(raw: unknown): ContextAcceptedArtifact {
  const value = object(raw);
  return {
    id: text(value.id),
    label: text(value.label),
    format: text(value.format),
    source: file(value.source),
  };
}
function nullableFile(raw: unknown) {
  return raw === null ? null : file(raw);
}
function availability(raw: unknown): ContextAvailability {
  const value = object(raw);
  if (typeof value.available !== "boolean") {
    throw new TypeError("原任務操作判定缺失");
  }
  return { available: value.available, reason: text(value.reason) };
}
function read(response: Response): Promise<unknown> {
  if (!response.ok) {
    throw new Error(`操作未確認（${response.status}）；請重新查明原任務狀態。`);
  }
  return response.json();
}
export async function fetchContextChoices(): Promise<ContextChoice[]> {
  const raw = object(await read(await fetch("/api/contexts", { cache: "no-store" })));
  if (raw.schema !== 1 || !Array.isArray(raw.choices)) {
    throw new Error("固定組合登記格式不符");
  }
  return raw.choices.map((item: unknown) => {
    const value = object(item);
    return {
      id: text(value.id),
      label: text(value.label),
      version: digest(value.version),
      composition_id: text(value.composition_id),
      stage_id: text(value.stage_id),
    };
  });
}
export function parseContextObservation(raw: unknown, choice: ContextChoice): ContextObservation {
  const value = object(raw);
  if (value.schema !== 1 || value.owner !== "soodles.loop-ui") {
    throw new Error("原任務管理者不符");
  }
  const composition = object(value.composition);
  if (composition.id !== choice.composition_id || composition.stage_id !== choice.stage_id) {
    throw new Error("固定組合版本已改變");
  }
  const actions = object(value.actions);
  const nativeResume = availability(actions.native_resume);
  if (nativeResume.available) {
    throw new Error("不支援原模型對話續接");
  }
  const identity = object(value.identity);
  const acceptance = object(value.acceptance);
  const sources = object(value.sources);
  if (!Array.isArray(acceptance.artifacts)) {
    throw new TypeError("驗收成果清單缺失");
  }
  if (!Array.isArray(value.effects) || !value.effects.every((item) => typeof item === "string")) {
    throw new Error("原任務效果回應不符");
  }
  return {
    schema: 1,
    owner: "soodles.loop-ui",
    status: text(value.status),
    version: digest(value.version),
    summary: text(value.summary),
    request: file(value.request),
    composition: {
      id: text(composition.id),
      stage_id: text(composition.stage_id),
      definition: file(composition.definition),
    },
    actions: {
      start: availability(actions.start),
      stop: availability(actions.stop),
      new_attempt: availability(actions.new_attempt),
      native_resume: nativeResume,
    },
    identity: {
      order_id: nullableText(identity.order_id),
      context_id: nullableText(identity.context_id),
      session_id: nullableText(identity.session_id),
    },
    candidate: nullableFile(value.candidate),
    acceptance: {
      status: text(acceptance.status),
      scope: text(acceptance.scope),
      record: nullableFile(acceptance.record),
      head: nullableText(acceptance.head),
      artifacts: acceptance.artifacts.map(artifact),
    },
    sources: Object.fromEntries(Object.entries(sources).map(([key, entry]) => [key, file(entry)])),
    effects: value.effects,
  };
}
export async function observeContext(choice: ContextChoice): Promise<ContextObservation> {
  return parseContextObservation(
    await read(
      await fetch("/api/contexts/observe", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          id: choice.id,
          registry_version: choice.version,
          action: "",
          expected_version: "",
        }),
      }),
    ),
    choice,
  );
}
export async function actOnContext(
  choice: ContextChoice,
  observation: ContextObservation,
  action: ContextAction,
): Promise<ContextObservation> {
  if (!observation.actions[action].available) {
    throw new Error("原任務管理者尚未允許此操作");
  }
  return parseContextObservation(
    await read(
      await fetch("/api/contexts/action", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          id: choice.id,
          registry_version: choice.version,
          expected_version: observation.version,
          action,
        }),
      }),
    ),
    choice,
  );
}

function sameFile(a: ContextFileRef | null, b: ContextFileRef | null) {
  return a?.path === b?.path && a?.sha256 === b?.sha256;
}
export async function previewContextArtifact(
  choice: ContextChoice,
  observation: ContextObservation,
  item: ContextAcceptedArtifact,
): Promise<string> {
  const raw = object(
    await read(
      await fetch("/api/contexts/preview", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          id: choice.id,
          registry_version: choice.version,
          expected_version: observation.version,
          artifact_id: item.id,
        }),
      }),
    ),
  );
  const actual = artifact(raw.artifact);
  const acceptance = object(raw.acceptance);
  if (
    raw.schema !== 1 ||
    raw.version !== observation.version ||
    actual.id !== item.id ||
    actual.format !== "html" ||
    !sameFile(actual.source, item.source) ||
    acceptance.status !== "PASS" ||
    acceptance.scope !== "integrated" ||
    acceptance.head !== observation.acceptance.head ||
    !sameFile(nullableFile(acceptance.record), observation.acceptance.record) ||
    !sameFile(nullableFile(raw.candidate), observation.candidate)
  ) {
    throw new Error("已接受成果來源或驗收版本改變");
  }
  return text(raw.document);
}
