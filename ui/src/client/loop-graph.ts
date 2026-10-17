import { parseLoopView } from "./loop-views";
import { layoutGraph } from "./loop-graph-layout.mjs";

function object(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("Graph object missing");
  }
  return value as Record<string, unknown>;
}
function text(value: unknown): string {
  if (typeof value !== "string") {
    throw new TypeError("Graph text missing");
  }
  return value;
}
function id(value: unknown) {
  const result = text(value);
  if (!/^[A-Za-z0-9_-]+$/.test(result)) {
    throw new Error("Invalid graph ID");
  }
  return result;
}
function nullable<T>(value: unknown, parse: (value: unknown) => T): T | null {
  return value === null ? null : parse(value);
}
function number(value: unknown): number {
  if (typeof value !== "number" || !Number.isFinite(value)) {
    throw new TypeError("Invalid graph number");
  }
  return value;
}
function list<T>(value: unknown, parse: (value: unknown) => T): T[] {
  if (!Array.isArray(value)) {
    throw new TypeError("Graph array missing");
  }
  return value.map((item) => parse(item));
}
function choice<T extends string>(value: unknown, choices: readonly T[]): T {
  const found = choices.find((item) => item === value);
  if (found === undefined) {
    throw new Error(`Unknown graph value: ${String(value)}`);
  }
  return found;
}
const ids = (value: unknown) => list(value, id);
const strings = (value: unknown) => list(value, text);
function file(value: unknown) {
  const r = object(value);
  return {
    id: id(r.id),
    path: text(r.path),
    git_blob: nullable(r.git_blob, text),
    git_mode: nullable(r.git_mode, text),
    bytes: nullable(r.bytes, number),
    artifact_ids: ids(r.artifact_ids),
    node_ids: ids(r.node_ids),
  };
}
function bool(v: unknown) {
  if (typeof v !== "boolean") {
    throw new TypeError("observation boolean invalid");
  }
  return v;
}

function repositoryObservation(raw: unknown, revision: string | null) {
  if (raw === undefined) {
    return;
  }
  const o = object(raw);
  const result = {
    status: choice(o.status, ["observed", "unknown"]),
    scope: choice(o.scope, ["display_readback"]),
    observed_at: text(o.observed_at),
    repository_id: nullable(o.repository_id, id),
    worktree_id: nullable(o.worktree_id, id),
    snapshot_id: nullable(o.snapshot_id, id),
    branch: nullable(o.branch, text),
    detached: nullable(o.detached, bool),
    dirty: nullable(o.dirty, bool),
    revision: nullable(o.revision, text),
    gap: nullable(o.gap, text),
  };
  if (result.revision !== revision) {
    throw new Error("observation revision differs");
  }
  if (result.status === "observed") {
    if (
      !result.repository_id ||
      !result.worktree_id ||
      !result.snapshot_id ||
      result.dirty === null ||
      result.detached === null ||
      (result.branch === null) !== result.detached ||
      result.gap !== null
    ) {
      throw new Error("observed identity incomplete");
    }
  } else if (
    result.repository_id !== null ||
    result.worktree_id !== null ||
    result.snapshot_id !== null ||
    result.branch !== null ||
    result.detached !== null ||
    result.dirty !== null ||
    !result.gap
  ) {
    throw new Error("unknown observation claims state");
  }
  return result;
}

export function parseArtifact(value: unknown) {
  const r = object(value);
  const source = r.source === null ? null : object(r.source);
  return {
    id: id(r.id),
    repository_id: nullable(r.repository_id, id),
    role: text(r.role),
    format: text(r.format),
    authority: text(r.authority),
    knowledge_role: text(r.knowledge_role),
    scope: text(r.scope),
    status: choice(r.status, ["available", "unknown", "stale", "not_public"]),
    source: source && {
      path: text(source.path),
      sha256: nullable(source.sha256, text),
      bytes: nullable(source.bytes, number),
    },
  };
}
function mechanism(raw: unknown) {
  if (raw === undefined) {
    return;
  }
  const m = object(raw);
  if (m.version !== 1) {
    throw new Error("Unknown mechanism version");
  }
  const capabilities = list(m.capabilities, (item) => {
    const c = object(item);
    return {
      id: text(c.id),
      function: text(c.function),
      entry: text(c.entry),
      input: text(c.input),
      output: text(c.output),
      readback: text(c.readback),
      refusal: text(c.refusal),
      source: text(c.source),
    };
  });
  if (
    capabilities.length === 0 ||
    new Set(capabilities.map((c) => c.id)).size !== capabilities.length ||
    capabilities.some((c) => Object.values(c).some((v) => !v))
  ) {
    throw new Error("Invalid mechanism capability");
  }
  return { version: 1, template: id(m.template), capabilities };
}
export function parseLoopGraph(value: unknown) {
  const root = object(value);
  if (root.schema_version !== 2) {
    throw new Error("Loop graph requires schema 2");
  }
  const r = object(root.graph);
  const graph = {
    objects: list(r.objects, (raw) => {
      const o = object(raw);
      return {
        id: id(o.id),
        namespace: text(o.namespace),
        mechanism: mechanism(o.mechanism),
        label: text(o.label),
        kind: text(o.kind),
        scope: choice(o.scope, ["shared", "repository"]),
        source_artifact_ids: ids(o.source_artifact_ids),
      };
    }),
    ports: list(r.ports, (raw) => {
      const p = object(raw);
      return {
        id: id(p.id),
        object_id: id(p.object_id),
        name: text(p.name),
        direction: choice(p.direction, ["input", "output"]),
        schema: text(p.schema),
        effect_scope: text(p.effect_scope),
        source_artifact_ids: ids(p.source_artifact_ids),
      };
    }),
    repositories: list(r.repositories, (raw) => {
      const repo = object(raw);
      return {
        id: id(repo.id),
        label: text(repo.label),
        root: text(repo.root),
        observation: repositoryObservation(repo.observation, nullable(repo.revision, text)),
        revision: nullable(repo.revision, text),
        index_status: choice(repo.index_status, ["complete", "unknown", "not_public"]),
        tracked_files: list(repo.tracked_files, file),
        selected_files: list(repo.selected_files, file),
        gaps: strings(repo.gaps),
      };
    }),
    artifacts: list(r.artifacts, parseArtifact),
    nodes: list(r.nodes, (raw) => {
      const n = object(raw);
      const identity = object(n.identity);
      const status = object(n.status);
      return {
        id: id(n.id),
        run_id: id(n.run_id),
        repository_id: id(n.repository_id),
        object_id: id(n.object_id),
        mechanism: n.mechanism === undefined ? undefined : id(n.mechanism),
        kind: choice(n.kind, ["session", "pclass", "cli", "observation"]),
        label: text(n.label),
        identity: {
          status: choice(identity.status, ["observed", "unknown"]),
          invocation_id: nullable(identity.invocation_id, text),
          attempt_id: nullable(identity.attempt_id, text),
          session_id: nullable(identity.session_id, text),
          locator: nullable(identity.locator, text),
        },
        status: {
          execution: text(status.execution),
          transfer: text(status.transfer),
          acceptance: text(status.acceptance),
        },
        input_artifact_ids: ids(n.input_artifact_ids),
        output_artifact_ids: ids(n.output_artifact_ids),
        evidence_artifact_ids: ids(n.evidence_artifact_ids),
        stage_id: nullable(n.stage_id, text),
      };
    }),
    edges: list(r.edges, (raw) => {
      const e = object(raw);
      return {
        id: id(e.id),
        run_id: id(e.run_id),
        from_node_id: id(e.from_node_id),
        to_node_id: id(e.to_node_id),
        from_port_id: id(e.from_port_id),
        to_port_id: id(e.to_port_id),
        input_artifact_ids: ids(e.input_artifact_ids),
        output_artifact_ids: ids(e.output_artifact_ids),
        evidence_artifact_ids: ids(e.evidence_artifact_ids),
        kind: choice(e.kind, ["handoff", "diagnostic"]),
        status: choice(e.status, ["observed", "unknown", "failed", "not_applicable"]),
        label: text(e.label),
        scope: choice(e.scope, ["within_repository", "cross_repository"]),
        current_validity: choice(e.current_validity, ["current", "stale", "unknown"]),
      };
    }),
    findings: list(r.findings, (raw) => {
      const f = object(raw);
      return {
        id: id(f.id),
        run_id: id(f.run_id),
        kind: choice(f.kind, ["mechanical", "semantic", "acceptance", "evidence_gap"]),
        status: choice(f.status, ["failed", "unknown", "not_applicable", "resolved"]),
        message: text(f.message),
        node_ids: ids(f.node_ids),
        edge_ids: ids(f.edge_ids),
        artifact_ids: ids(f.artifact_ids),
        requirement_refs: strings(f.requirement_refs),
        original_next: f.original_next ?? null,
      };
    }),
    metrics: list(r.metrics, (raw) => {
      const m = object(raw);
      const scope =
        m.observation_scope === null || m.observation_scope === undefined
          ? null
          : object(m.observation_scope);
      return {
        id: id(m.id),
        run_id: id(m.run_id),
        name: text(m.name),
        status: choice(m.status, ["observed", "unknown", "not_applicable"]),
        value: nullable(m.value, number),
        unit: text(m.unit),
        numerator: nullable(m.numerator, number),
        denominator: nullable(m.denominator, number),
        source_artifact_ids: ids(m.source_artifact_ids),
        gaps: strings(m.gaps),
        observation_scope: scope && {
          kind: choice(scope.kind, ["historical_measurement"]),
          host_version: text(scope.host_version),
          host_pid: nullable(scope.host_pid, number),
          started_at: nullable(scope.started_at, text),
          finished_at: nullable(scope.finished_at, text),
          run_ids: ids(scope.run_ids),
          formula: text(scope.formula),
          limitations: strings(scope.limitations),
        },
      };
    }),
  };
  const sets = Object.fromEntries(
    Object.entries(graph).map(([key, items]) => {
      const unique = new Set(items.map((item) => item.id));
      if (unique.size !== items.length) {
        throw new Error(`Duplicate graph ${key} ID`);
      }
      return [key, unique];
    }),
  );
  const refs = (key: string, values: string[]) => {
    if (values.some((reference) => !sets[key]?.has(reference))) {
      throw new Error(`Missing graph ${key} reference`);
    }
  };
  for (const node of graph.nodes) {
    refs("objects", [node.object_id]);
    refs("repositories", [node.repository_id]);
    refs("artifacts", [
      ...node.input_artifact_ids,
      ...node.output_artifact_ids,
      ...node.evidence_artifact_ids,
    ]);
  }
  for (const edge of graph.edges) {
    refs("nodes", [edge.from_node_id, edge.to_node_id]);
    refs("ports", [edge.from_port_id, edge.to_port_id]);
    refs("artifacts", [
      ...edge.input_artifact_ids,
      ...edge.output_artifact_ids,
      ...edge.evidence_artifact_ids,
    ]);
  }
  for (const port of graph.ports) {
    refs("objects", [port.object_id]);
    refs("artifacts", port.source_artifact_ids);
  }
  for (const owner of graph.objects) {
    refs("artifacts", owner.source_artifact_ids);
  }
  for (const repo of graph.repositories) {
    for (const entry of [...repo.tracked_files, ...repo.selected_files]) {
      refs("artifacts", entry.artifact_ids);
      refs("nodes", entry.node_ids);
    }
  }
  for (const finding of graph.findings) {
    refs("nodes", finding.node_ids);
    refs("edges", finding.edge_ids);
    refs("artifacts", finding.artifact_ids);
  }
  for (const metric of graph.metrics) {
    refs("artifacts", metric.source_artifact_ids);
  }
  layoutGraph({
    nodes: graph.nodes,
    edges: graph.edges
      .filter((e) => e.kind === "handoff")
      .map((e) => ({ from: e.from_node_id, to: e.to_node_id })),
  });
  return { ...parseLoopView({ ...root, schema_version: 1 }), schema_version: 2, graph };
}
export type GraphView = ReturnType<typeof parseLoopGraph>;
export type LoopGraph = GraphView["graph"];
export type Artifact = LoopGraph["artifacts"][number];
export interface GraphResponse {
  view: GraphView;
  etag: string | null;
}

export async function fetchLoopGraph(
  previous?: GraphResponse,
  signal?: AbortSignal,
): Promise<GraphResponse> {
  const headers = new Headers();
  if (previous?.etag) {
    headers.set("If-None-Match", previous.etag);
  }
  const response = await fetch("/api/loops/graph", { headers, signal });
  if (response.status === 304 && previous) {
    return previous;
  }
  if (!response.ok) {
    throw new Error(`Graph readback unavailable (${response.status})`);
  }
  const view = parseLoopGraph(await response.json());
  if (previous && JSON.stringify(previous.view) === JSON.stringify(view)) {
    return previous;
  }
  return { view, etag: response.headers.get("ETag") };
}
export async function fetchArtifact(expected: Artifact, signal?: AbortSignal) {
  const response = await fetch(`/api/loops/artifacts/${encodeURIComponent(expected.id)}`, {
    signal,
  });
  const raw = object(await response.json());
  const artifact = parseArtifact(raw.artifact);
  const status = text(raw.status);
  const error = nullable(raw.error, text);
  if (
    artifact.id !== expected.id ||
    artifact.source?.sha256 !== expected.source?.sha256 ||
    artifact.source?.bytes !== expected.source?.bytes
  ) {
    throw new Error(
      "Selected artifact version changed; retain the selected snapshot and choose a new version explicitly.",
    );
  }
  if (!response.ok || status !== "available") {
    return {
      artifact,
      status,
      error: error ?? `Content unavailable (${response.status})`,
      content: null,
    };
  }
  return { artifact, status, error, content: nullable(raw.content, text) };
}

export async function fetchGraphFile(
  repository: string,
  expected: LoopGraph["repositories"][number]["tracked_files"][number],
  signal?: AbortSignal,
) {
  const response = await fetch(
    `/api/loops/repositories/${encodeURIComponent(repository)}/files/${encodeURIComponent(expected.id)}`,
    { signal },
  );
  const raw = object(await response.json());
  const actual = file(raw.file);
  if (
    actual.id !== expected.id ||
    actual.git_blob !== expected.git_blob ||
    actual.bytes !== expected.bytes
  ) {
    throw new Error("Selected Git blob changed; choose the new snapshot explicitly.");
  }
  const status = text(raw.status);
  const error = nullable(raw.error, text);
  return {
    status,
    error,
    content: response.ok && status === "available" ? nullable(raw.content, text) : null,
  };
}
