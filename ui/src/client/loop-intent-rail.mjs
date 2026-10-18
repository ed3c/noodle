/** Source-backed intent rail and exact Run-scoped selection highlights.
 * These are display-only source and selection projections: no synthetic
 * native invocation, transition, handoff edge, acceptance or reward.
 */
const LANES = [
  { id: "intent", title: "Human Intent", roles: ["loop_request"] },
  {
    id: "judgment",
    title: "Session Judgment",
    roles: ["intent_selection", "loop_owner_readback", "schema_feedback"],
  },
  {
    id: "prompt",
    title: "Prompt / P-class",
    roles: ["task_envelope", "instruction", "native_prompt", "native_input"],
  },
  { id: "tool", title: "CLI / Tool Call", roles: ["native_tool_call", "native_tool_result"] },
];

export function projectIntentRail(graph, runId) {
  const nodes = (graph.nodes || []).filter((n) => runId && n.run_id === runId);
  const refs = new Set(
    nodes.flatMap((n) => [
      ...(n.input_artifact_ids || []),
      ...(n.output_artifact_ids || []),
      ...(n.evidence_artifact_ids || []),
    ]),
  );
  const found = (graph.artifacts || []).filter((a) => refs.has(a.id));
  return LANES.map((lane) => {
    const artifacts = found.filter((a) => lane.roles.includes(a.role));
    const ids = new Set(artifacts.map((a) => a.id));
    const carriers = nodes.filter((n) =>
      [
        ...(n.input_artifact_ids || []),
        ...(n.output_artifact_ids || []),
        ...(n.evidence_artifact_ids || []),
      ].some((id) => ids.has(id)),
    );
    return {
      id: lane.id,
      title: lane.title,
      role: lane.roles,
      artifact_ids: artifacts.map((a) => a.id),
      node_ids: carriers.map((n) => n.id),
      source_status:
        artifacts.length > 0 && artifacts.every((a) => ["available", "not_public"].includes(a.status))
          ? "source_referenced"
          : "unknown",
      execution_status: "NOT_ESTABLISHED_BY_SOURCE_RAIL",
    };
  });
}

export function evidenceFocus({ scene, overlay, selection, facets = [] }) {
  if (!selection) {
    return null;
  }
  const edges = new Set();
  const expected = new Set();
  const nodes = new Set();
  const objects = new Set();
  const run = new Set(scene.nodes.map((n) => n.id));
  const known = new Map(scene.nodes.map((n) => [n.id, n]));
  const add = (id) => {
    if (run.has(id)) {
      nodes.add(id);
      const n = known.get(id);
      if (n) {
        objects.add(n.object_id);
      }
    }
  };
  if (selection.kind === "object" || selection.kind === "principle") {
    const id = selection.kind === "principle" ? selection.id.split("/")[0] : selection.id;
    objects.add(id);
    for (const node of selection.kind === "principle"
      ? facets
      : scene.nodes.filter((n) => n.object_id === id)) {
      add(node.id);
    }
  } else if (selection.kind === "node") {
    add(selection.id);
  } else if (selection.kind === "edge") {
    const edge = scene.edges.find((e) => e.id === selection.id);
    if (edge) {
      edges.add(edge.id);
      add(edge.from_node_id);
      add(edge.to_node_id);
    }
  } else if (selection.kind === "finding") {
    const f = scene.findings.find((x) => x.id === selection.id);
    for (const id of f?.node_ids || []) {
      add(id);
    }
    for (const id of f?.edge_ids || []) {
      edges.add(id);
    }
  } else if (selection.kind === "expected") {
    const step = overlay.steps.find((x) => x.id === selection.id);
    if (step) {
      expected.add(step.id);
      for (const id of step.matches) {
        add(id);
      }
    }
  } else if (selection.kind === "expected-route") {
    const route = overlay.routes.find((x) => selection.id === `${x.layer}/${x.id}`);
    if (route) {
      expected.add(route.from);
      expected.add(route.to);
      for (const id of route.result?.edge_ids || []) {
        const edge = scene.edges.find((e) => e.id === id);
        if (edge) {
          edges.add(id);
          add(edge.from_node_id);
          add(edge.to_node_id);
        }
      }
      for (const step of overlay.steps.filter((s) => expected.has(s.id))) {
        for (const id of step.matches) {
          add(id);
        }
      }
    }
  } else {
    return null;
  }
  // Only the original occurrence DAG supplies one-hop execution edges.
  if (["object", "principle", "node"].includes(selection.kind)) {
    for (const e of scene.edges) {
      if (nodes.has(e.from_node_id) || nodes.has(e.to_node_id)) {
        edges.add(e.id);
        add(e.from_node_id);
        add(e.to_node_id);
      }
    }
  }
  return { nodes, edges, objects, expected };
}
