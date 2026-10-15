import { parseLoopGraph } from "./loop-graph";

const run = (id: string, repository: string) => ({
  id,
  repository,
  mode: "inner-only",
  status: "unknown",
  stages: [
    {
      id: "stage",
      kind: "native input",
      status: "recorded",
      prompt: "original prompt",
      input_refs: [],
      output_refs: [],
      consumer_status: "UNKNOWN",
    },
  ],
  cards: [
    {
      id: "SPEC_same",
      type: "SPEC",
      title: `Card ${repository}`,
      body: "original knowledge",
      source_refs: [`${repository}:revisionA`],
    },
  ],
  notes: [{ id: "MASTER", title: `Note ${repository}`, body: "original note", source_refs: [] }],
  breakpoints: [],
  next: "read original owner",
});
const node = (id: string, repository_id: string, run_id: string) => ({
  id,
  run_id,
  repository_id,
  object_id: "manager",
  kind: "cli",
  label: `Invocation ${id}`,
  stage_id: "stage",
  identity: {
    status: "observed",
    invocation_id: id,
    attempt_id: id,
    session_id: null,
    locator: `receipt:${id}`,
  },
  status: { execution: "observed", transfer: "unknown", acceptance: "unknown" },
  input_artifact_ids: ["a", "b"],
  output_artifact_ids: ["c"],
  evidence_artifact_ids: ["a"],
});
const edge = (id: string, from: string, to: string) => ({
  id,
  run_id: "run1",
  from_node_id: from,
  to_node_id: to,
  from_port_id: "out",
  to_port_id: "in",
  input_artifact_ids: ["a", "b"],
  output_artifact_ids: ["c"],
  evidence_artifact_ids: ["a"],
  kind: "handoff",
  status: "observed",
  label: `Handoff ${id}`,
  scope: "within_repository",
  current_validity: "unknown",
});
export function graphFixture() {
  const artifacts = ["a", "b", "c"].map((id) => ({
    id,
    repository_id: "repo1",
    role: id === "c" ? "native_input" : "prompt",
    format: "application/json",
    authority: "knowledge_data",
    knowledge_role: "SPEC",
    scope: "run1",
    status: "available",
    source: { path: `/source/${id}.json`, sha256: id.repeat(64), bytes: 11 },
  }));
  return parseLoopGraph({
    schema_version: 2,
    runs: [run("run1", "org/one"), run("run2", "org/two")],
    graph: {
      objects: [
        {
          id: "manager",
          namespace: "original.manager",
          label: "Original Manager",
          kind: "owner",
          scope: "shared",
          source_artifact_ids: ["a"],
        },
      ],
      ports: ["in", "out"].map((id) => ({
        id,
        object_id: "manager",
        name: id,
        direction: id === "in" ? "input" : "output",
        schema: "prompt JSON",
        effect_scope: "readonly",
        source_artifact_ids: [],
      })),
      repositories: ["repo1", "repo2"].map((id, index) => ({
        id,
        label: id,
        root: `org/${index ? "two" : "one"}`,
        revision: "revisionA",
        index_status: "complete",
        gaps: [],
        tracked_files: [
          {
            id: `file_${id}`,
            path: "src/prompt.json",
            git_blob: "blobA",
            git_mode: "100644",
            bytes: 11,
            artifact_ids: id === "repo1" ? ["a"] : [],
            node_ids: id === "repo1" ? ["first"] : [],
          },
        ],
        selected_files: [],
      })),
      artifacts,
      nodes: [
        node("first", "repo1", "run1"),
        node("second", "repo1", "run1"),
        node("third", "repo1", "run1"),
        node("other", "repo2", "run2"),
      ],
      edges: [
        edge("known", "first", "third"),
        edge("multiple", "second", "third"),
        { ...edge("unknown", "first", "second"), status: "unknown" },
      ],
      findings: [
        {
          id: "failed",
          run_id: "run1",
          kind: "mechanical",
          status: "failed",
          message: "Original command refused",
          node_ids: ["first"],
          edge_ids: ["known"],
          artifact_ids: ["a"],
          requirement_refs: ["REQ1"],
          original_next: { operation: "read_original" },
        },
        {
          id: "gap",
          run_id: "run1",
          kind: "evidence_gap",
          status: "unknown",
          message: "Consumer behavior not observed",
          node_ids: ["third"],
          edge_ids: [],
          artifact_ids: [],
          requirement_refs: [],
          original_next: null,
        },
      ],
      metrics: [
        {
          id: "coverage",
          run_id: "run1",
          name: "required_handoff_coverage",
          status: "unknown",
          value: null,
          unit: "ratio",
          numerator: 2,
          denominator: null,
          source_artifact_ids: ["a"],
          gaps: ["required denominator unavailable"],
        },
      ],
    },
  });
}

export function required<T>(value: T | undefined): T {
  if (value === undefined) {
    throw new Error("Fixture item missing");
  }
  return value;
}
