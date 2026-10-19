/** Source-bound classification and expectation layout; neither produces execution events. */
export const principleNames = [
  ["method", "純方法論", "compass"],
  ["case", "案例結合", "samples"],
  ["loop", "固定迴圈", "cam"],
  ["assertion", "P-class 斷言", "selector"],
  ["repair", "CLI 修正", "service"],
];
function content(artifact) {
  if (!artifact || !["available", "not_public"].includes(artifact.status)) {
    return null;
  }
  try {
    return JSON.parse(artifact.public_content ?? artifact.content);
  } catch {
    return null;
  }
}
export function contractFacets(graph, objectId, runId) {
  const owner = graph.objects.find((o) => o.id === objectId);
  const sources = graph.artifacts.filter((a) => owner?.source_artifact_ids?.includes(a.id));
  const catalogs = sources
    .filter((a) => a.role === "mechanism_contract")
    .map((a) => content(a))
    .filter(Boolean);
  const definitions =
    catalogs.length === 1 && Array.isArray(catalogs[0].principles) ? catalogs[0].principles : [];
  return principleNames.map(([id, name, shape]) => {
    const rule = definitions.find(
      (p) => p?.id === id && Array.isArray(p.kinds) && Array.isArray(p.roles),
    );
    const nodes = rule
      ? graph.nodes.filter(
          (n) =>
            n.object_id === objectId &&
            n.run_id === runId &&
            (rule.kinds.includes(n.kind) ||
              [...n.input_artifact_ids, ...n.output_artifact_ids, ...n.evidence_artifact_ids].some(
                (ref) => rule.roles.includes(graph.artifacts.find((a) => a.id === ref)?.role),
              )),
        )
      : [];
    return {
      id,
      name,
      shape,
      rule,
      nodes,
      sources,
      status: nodes.length > 0 ? "source_referenced" : "unknown",
    };
  });
}
export function expectedScene(graph, model) {
  if (model?.status !== "available") {
    return { steps: [], routes: [], unbound: [] };
  }
  const { expected, comparison } = model;
  const steps = expected.steps.map((step) => ({
    ...step,
    layer: "contract",
    owner: graph.objects.find(
      (o) => o.namespace === expected.components.find((c) => c.id === step.component)?.owner,
    ),
    matches: (comparison.step_matches[step.id] || []).filter((id) =>
      graph.nodes.some((n) => n.id === id),
    ),
  }));
  const ids = new Set(steps.map((s) => s.id));
  // A hypothesis with no original endpoints remains unconnected prose, never a guessed route.
  const hypotheses = expected.hypotheses.filter(
    (h) =>
      h && typeof h === "object" && typeof h.id === "string" && ids.has(h.from) && ids.has(h.to),
  );
  const routes = [];
  for (const route of expected.routes) {
    if (route.applicable) {
      routes.push({
        ...route,
        layer: "contract",
        result: comparison.rows.find((row) => row.route_id === route.id),
      });
    }
  }
  for (const hypothesis of hypotheses) {
    routes.push({
      ...hypothesis,
      layer: "hypothesis",
      transformation: hypothesis.claim || hypothesis.id,
    });
  }
  return {
    steps,
    routes,
    unbound: expected.hypotheses.filter((h) => !hypotheses.includes(h)),
  };
}
/** Tiny, shared physical symbols; no status-dependent geometry or fake instrument values. */
export function principleFigure(shape) {
  const body =
    '<path d="M4 33 28 45 54 31 30 19Z" fill="#55717c"/><path d="M4 33v6l24 12v-6Z" fill="#283e49"/><path d="m28 45 26-14v6L28 51Z" fill="#384f5a"/>';
  const ring =
    '<ellipse cx="29" cy="25" rx="17" ry="10" fill="#9ab3b9" stroke="#d5e3df"/><ellipse cx="29" cy="23" rx="14" ry="8" fill="#273f4b" stroke="#abc3ca"/>';
  const shapes = {
    compass: `${ring}<path d="m23 28 6-13 6 9-6 7Z" fill="#e5d9ac"/><circle cx="29" cy="23" r="2" fill="#fff"/>`,
    cam: `${ring}<path d="M18 24q-1-13 13-11l9 9-13 8Z" fill="#bdd1d1"/><circle cx="28" cy="23" r="3" fill="#243f49"/><path d="M39 19 49 14v-7" fill="none" stroke="#e5d9ac" stroke-width="4"/>`,
    selector: `${ring}<path d="m29 23 10-5" stroke="#e5d9ac" stroke-width="4"/><path d="M14 20v-5m12-1V9m14 8v-5" stroke="#c9d9db" stroke-width="3"/>`,
    samples:
      '<path d="m12 30 29-16v-6L12 24Z" fill="#b6cbcf"/><path d="m14 30 4 2V12l-4-2Zm10-6 4 2V6l-4-2Zm10-5 4 2V3l-4-2Z" fill="#e4d9ae" stroke="#6c8790"/>',
    service:
      '<path d="m10 27 18-10 17 8-18 11Z" fill="#b6cbcf"/><path d="M14 25V13l10-5v12m13 4V12l7 4v14" fill="none" stroke="#d8e4df" stroke-width="5"/><path d="m26 25 10-14m-3 0 5 4" stroke="#e5d9ac" stroke-width="3"/>',
  };
  return `<svg viewBox="0 0 60 54" aria-hidden="true">${body}${shapes[shape] || ""}</svg>`;
}
