/** Display selection only. Original comparison results and runtime edges stay unchanged. */
export function focusExpectations(overlay, { full = false, selection = null, sourceStatus } = {}) {
  const related = (route) => {
    if (!selection) {
      return false;
    }
    if (selection.kind === "expected-route") {
      return selection.id === `${route.layer}/${route.id}`;
    }
    if (selection.kind === "edge") {
      return route.result?.edge_ids?.includes(selection.id);
    }
    return overlay.steps.some(
      (step) =>
        [route.from, route.to].includes(step.id) &&
        ((selection.kind === "node" && step.matches.includes(selection.id)) ||
          (selection.kind === "expected" && step.id === selection.id) ||
          (selection.kind === "object" && step.owner?.id === selection.id)),
    );
  };
  const routes = overlay.routes.filter(
    (route) =>
      full ||
      related(route) ||
      (route.layer === "contract" &&
        (sourceStatus === "drift" || route.result?.status !== "matched")),
  );
  const endpoints = new Set(routes.flatMap((route) => [route.from, route.to]));
  return {
    ...overlay,
    routes,
    steps: overlay.steps.filter((step) => endpoints.has(step.id)),
    hidden: overlay.routes.length - routes.length,
  };
}

export function partitionMachines(objects, scene, { overlay, run, full = false }) {
  const involved = new Set(scene.nodes.map((node) => node.object_id));
  for (const step of overlay.steps) {
    if (step.owner) {
      involved.add(step.owner.id);
    }
  }
  return {
    primary: objects.filter((object) => !run || full || involved.has(object.id)),
    index: objects.filter((object) => run && !full && !involved.has(object.id)),
  };
}

/** One floor: participating machines and occurrences, followed by the compact index. Position is not evidence. */
export function focusedLayout(primaryCount, indexCount, itemCount) {
  const columns = 3;
  const railHeight = typeof itemCount === "object" && itemCount.intentRail ? 146 : 0;
  const count = typeof itemCount === "number" ? itemCount : itemCount.count;
  const indexHeight = indexCount ? Math.ceil(indexCount / columns) * 58 + 36 : 0;
  const machineTop = 40 + railHeight;
  const itemTop = machineTop + Math.ceil(primaryCount / columns) * 145 + 35;
  const indexTop = itemTop + Math.ceil(count / columns) * 205 + 35;
  return {
    width: 880,
    height: indexTop + indexHeight + 50,
    index: Array.from({ length: indexCount }, (_, i) => [
      30 + (i % columns) * 280,
      indexTop + Math.floor(i / columns) * 58,
    ]),
    objects: Array.from({ length: primaryCount }, (_, i) => [
      30 + (i % columns) * 280,
      machineTop + Math.floor(i / columns) * 145,
    ]),
    items: Array.from({ length: itemCount }, (_, i) => [
      30 + (i % columns) * 280,
      itemTop + Math.floor(i / columns) * 205,
    ]),
  };
}
