import type { LoopGraph } from "./loop-graph";
export function findingSymbol(status: string) {
  if (status === "failed") {
    return "×";
  }
  if (status === "unknown") {
    return "?";
  }
  return "·";
}

export function knownFailureFrontier(graph: LoopGraph) {
  const failures = graph.findings.filter((finding) => finding.status === "failed");
  const endpoints = (finding: LoopGraph["findings"][number]) => [
    ...new Set([
      ...finding.node_ids,
      ...graph.edges
        .filter((edge) => finding.edge_ids.includes(edge.id))
        .map((edge) => edge.to_node_id),
    ]),
  ];
  const adjacency = new Map<string, string[]>();
  for (const edge of graph.edges) {
    if (edge.kind === "handoff" && edge.status === "observed") {
      adjacency.set(edge.from_node_id, [
        ...(adjacency.get(edge.from_node_id) ?? []),
        edge.to_node_id,
      ]);
    }
  }
  const reaches = (start: string, target: string) => {
    const seen = new Set<string>();
    const pending = [...(adjacency.get(start) ?? [])];
    while (pending.length > 0) {
      const current = pending.pop();
      if (current === undefined || seen.has(current)) {
        continue;
      }
      if (current === target) {
        return true;
      }
      seen.add(current);
      pending.push(...(adjacency.get(current) ?? []));
    }
    return false;
  };
  return new Set(
    failures
      .filter(
        (finding) =>
          endpoints(finding).length > 0 &&
          !failures.some(
            (earlier) =>
              earlier.id !== finding.id &&
              endpoints(earlier).some((from) =>
                endpoints(finding).every((to) => reaches(from, to)),
              ),
          ),
      )
      .map((finding) => finding.id),
  );
}
