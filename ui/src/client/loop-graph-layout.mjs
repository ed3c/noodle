/** Deterministic display geometry. Only supplied handoffs determine causal rank. */
export function layoutGraph({ nodes, edges }) {
  const sorted = [...nodes].toSorted((a, b) => a.id.localeCompare(b.id, "en"));
  const byID = new Map(sorted.map((node) => [node.id, node]));
  if (byID.size !== sorted.length) {
    throw new Error("Duplicate graph node");
  }
  const incoming = new Map(sorted.map((node) => [node.id, 0]));
  const outgoing = new Map(sorted.map((node) => [node.id, []]));
  const ranks = new Map(sorted.map((node) => [node.id, 0]));
  for (const edge of edges) {
    if (!byID.has(edge.from) || !byID.has(edge.to)) {
      throw new Error("Missing graph endpoint");
    }
    incoming.set(edge.to, incoming.get(edge.to) + 1);
    outgoing.get(edge.from).push(edge.to);
  }
  const ready = sorted.filter((node) => incoming.get(node.id) === 0).map((node) => node.id);
  let visited = 0;
  while (ready.length > 0) {
    const id = ready.shift();
    visited += 1;
    for (const target of outgoing.get(id)) {
      ranks.set(target, Math.max(ranks.get(target), ranks.get(id) + 1));
      incoming.set(target, incoming.get(target) - 1);
      if (incoming.get(target) === 0) {
        ready.push(target);
      }
    }
    ready.sort();
  }
  if (visited !== sorted.length) {
    throw new Error("Causal graph cycle");
  }
  const width = Math.max(700, (Math.max(0, ...ranks.values()) + 1) * 330 + 80);
  const positions = [];
  const bands = [];
  let top = 16;
  for (const repository_id of [...new Set(sorted.map((node) => node.repository_id))].toSorted()) {
    const rows = new Map();
    const members = sorted.filter((node) => node.repository_id === repository_id);
    for (const node of members) {
      const rank = ranks.get(node.id);
      const row = rows.get(rank) ?? 0;
      rows.set(rank, row + 1);
      positions.push({
        id: node.id,
        repository_id,
        rank,
        x: 40 + rank * 330,
        y: top + 48 + row * 126,
        width: 240,
        height: 92,
      });
    }
    const height = 76 + Math.max(1, ...rows.values()) * 126;
    bands.push({ repository_id, x: 16, y: top, width: width - 32, height });
    top += height + 24;
  }
  return { nodes: positions, bands, width, height: Math.max(240, top) };
}
