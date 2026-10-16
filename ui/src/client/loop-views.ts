function record(value: unknown): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error("Loop view object missing");
  }
  return value as Record<string, unknown>;
}

function text(value: unknown): string {
  if (typeof value !== "string") {
    throw new TypeError("Loop view text missing");
  }
  return value;
}

function array<T>(value: unknown, parse: (item: unknown) => T): T[] {
  if (!Array.isArray(value)) {
    throw new TypeError("Loop view array missing");
  }
  return value.map((item) => parse(item));
}

function resources(value: unknown) {
  if (value === undefined) {
    return [];
  }
  return array(value, (item) => {
    const r = record(item);
    return { label: text(r.label), value: text(r.value) };
  });
}

export function parseLoopView(value: unknown) {
  const root = record(value);
  if (root.schema_version !== 1 && root.schema_version !== 2) {
    throw new Error("Unsupported loop view schema");
  }
  return {
    schema_version: 1,
    resources: resources(root.resources),
    runs: array(root.runs, (rawRun) => {
      const run = record(rawRun);
      return {
        id: text(run.id),
        repository: text(run.repository),
        mode: text(run.mode),
        status: text(run.status),
        stages: array(run.stages, (item) => {
          const stage = record(item);
          return {
            id: text(stage.id),
            kind: text(stage.kind),
            status: text(stage.status),
            prompt: text(stage.prompt),
            input_refs: array(stage.input_refs, text),
            output_refs: array(stage.output_refs, text),
            consumer_status: text(stage.consumer_status),
          };
        }),
        cards: array(run.cards, (item) => {
          const card = record(item);
          return {
            id: text(card.id),
            type: text(card.type),
            title: text(card.title),
            body: text(card.body),
            source_refs: array(card.source_refs, text),
          };
        }),
        notes: array(run.notes, (item) => {
          const note = record(item);
          return {
            id: text(note.id),
            title: text(note.title),
            body: text(note.body),
            source_refs: array(note.source_refs, text),
          };
        }),
        breakpoints: array(run.breakpoints, (item) => {
          const point = record(item);
          return {
            kind: text(point.kind),
            status: text(point.status),
            stage_id: text(point.stage_id),
            message: text(point.message),
          };
        }),
        resources: resources(run.resources),
        next: run.next === null ? null : text(run.next),
      };
    }),
  };
}

export type LoopView = ReturnType<typeof parseLoopView>;
export type LoopRun = LoopView["runs"][number];
export interface LoopViewResponse {
  view: LoopView;
  etag: string | null;
}

export async function fetchLoopView(previous?: LoopViewResponse, signal?: AbortSignal) {
  const headers = new Headers();
  if (previous?.etag) {
    headers.set("If-None-Match", previous.etag);
  }
  const response = await fetch("/api/loops", { headers, signal });
  if (response.status === 304 && previous) {
    return previous;
  }
  if (!response.ok) {
    throw new Error(`Loop view unavailable (${response.status})`);
  }
  return {
    view: parseLoopView(await response.json()),
    etag: response.headers.get("ETag"),
  };
}
