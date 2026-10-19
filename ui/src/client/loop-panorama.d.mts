import type { LoopGraph } from "./loop-graph";
export interface WorkspaceScope {
  goal: string;
  runIds: string[] | null;
}
export interface PanoramaSelection {
  kind: "object" | "edge" | "node" | "finding" | "artifact";
  id: string;
}
export interface PanoramaFocus {
  reveal?: boolean;
  node_ids: string[];
  edge_ids: string[];
}
export function projectPanorama(
  graph: LoopGraph,
  runIds?: string[] | null,
): {
  nodes: LoopGraph["nodes"];
  edges: LoopGraph["edges"];
  relations: { id: string; from: string; to: string; edge_ids: string[]; current: number }[];
  rows: {
    run_id: string;
    node_ids: string[];
    edge_ids: string[];
    repository_ids: string[];
    kinds: string[];
    cross_repository: boolean;
    consumption: { status: string; edge_ids: string[] };
    feedback: { status: string; edge_ids: string[]; current: boolean };
    goal: { status: string; node_ids: string[] };
    findings: LoopGraph["findings"];
  }[];
  context_run_ids: string[];
};
export function mountPanorama(
  root: HTMLElement,
  options: WorkspaceScope & {
    graph: LoopGraph;
    runs: { id: string; repository: string; mode: string }[];
    onScope: (scope: WorkspaceScope) => void;
    onSelect: (selection: PanoramaSelection) => void;
    onReset?: () => void;
    onReviewSelection?: (selection: {
      runId: string;
      kind: string;
      id: string;
    }) => Promise<{ id: string; status: string }>;
    readArtifact?: (id: string) => Promise<string | null>;
    onFocus: (focus: PanoramaFocus) => void;
  },
): { setSelection: (selection: PanoramaSelection | null) => void };

export function readStructuredField(
  text: string,
  locator: string | null,
  sourcePath: string | null,
): { pointer: string; status: string; text: string } | null;
