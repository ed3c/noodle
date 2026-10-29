// Read-only projection shared with Medium. No task or execution state lives here.
export function parseReviewGraph(text) {
  let graph;
  try {
    graph = JSON.parse(text);
  } catch {
    return null;
  }
  if (graph?.schema !== 1 || graph?.analyzer !== "noodle-go-types-v1") {
    return null;
  }
  if (
    !Array.isArray(graph.nodes) ||
    !Array.isArray(graph.edges) ||
    !Array.isArray(graph.sources) ||
    !Array.isArray(graph.unknowns) ||
    !Array.isArray(graph.limitations) ||
    typeof graph.head !== "string"
  ) {
    throw new TypeError("Go 圖形缺少原分析資料。");
  }
  const ids = new Set();
  for (const node of graph.nodes) {
    if (
      !node ||
      !["id", "kind", "name", "package", "file"].every((k) => typeof node[k] === "string") ||
      !Number.isInteger(node.line) ||
      !Number.isInteger(node.end_line) ||
      ids.has(node.id)
    ) {
      throw new Error("Go 圖形節點無效或重複。");
    }
    ids.add(node.id);
  }
  for (const edge of graph.edges) {
    if (
      !edge ||
      !ids.has(edge.from) ||
      !ids.has(edge.to) ||
      !["contains", "static_call"].includes(edge.kind)
    ) {
      throw new Error("Go 圖形包含無法讀回的連線。");
    }
  }
  for (const source of graph.sources) {
    if (!source || typeof source.path !== "string" || !/^[a-f0-9]{64}$/.test(source.sha256)) {
      throw new Error("Go 圖形來源摘要無效。");
    }
  }
  for (const gap of graph.unknowns) {
    if (
      !gap ||
      !ids.has(gap.from) ||
      typeof gap.file !== "string" ||
      !Number.isInteger(gap.line) ||
      typeof gap.reason !== "string"
    ) {
      throw new Error("Go 圖形未知項目缺少來源。");
    }
  }
  if (!graph.limitations.every((value) => typeof value === "string")) {
    throw new Error("Go 圖形限制說明無效。");
  }
  return graph;
}

export function mountReviewGraph(root, text) {
  const graph = parseReviewGraph(text);
  if (!graph) {
    return () => root.replaceChildren();
  }
  const doc = root.ownerDocument;
  const make = (tag, value, parent = root) => {
    const element = doc.createElement(tag);
    if (value !== undefined) {
      element.textContent = value;
    }
    parent.append(element);
    return element;
  };
  root.classList.add("review-graph");
  make("h3", "Go 架構來源圖");
  make("p", "靜態結構與可解析呼叫。此圖不代表 runtime trace、品質通過或候選已接受。");
  make(
    "p",
    `來源 HEAD ${graph.head} · ${graph.nodes.length} 個節點 · ${graph.unknowns.length} 個未知項目`,
  );
  const controls = make("div");
  const label = make("label", "搜尋節點 ", controls);
  const search = make("input", undefined, label);
  search.type = "search";
  const kindLabel = make("label", " 顯示種類 ", controls);
  const kind = make("select", undefined, kindLabel);
  for (const value of ["all", ...new Set(graph.nodes.map((node) => node.kind))]) {
    const option = make("option", value, kind);
    option.value = value;
  }
  kind.value = "file";
  const count = make("p");
  const canvas = make("div");
  canvas.className = "review-graph-canvas";
  const list = make("div");
  list.className = "review-graph-nodes";
  const details = make("section");
  details.setAttribute("aria-label", "節點來源與呼叫");
  const lookup = new Map(graph.nodes.map((node) => [node.id, node]));
  const sources = new Map(graph.sources.map((source) => [source.path, source.sha256]));
  const parents = new Map(
    graph.edges.filter((edge) => edge.kind === "contains").map((edge) => [edge.to, edge.from]),
  );
  function select(node) {
    details.replaceChildren();
    make("h4", node.name, details);
    make("p", `${node.kind} · ${node.package}`, details);
    make(
      "p",
      node.file
        ? `${node.file}:${node.line}–${node.end_line}`
        : "外部目標或 package；此節點沒有本次檔案內容。",
      details,
    );
    make("p", `檔案 SHA-256：${sources.get(node.file) ?? "未附來源內容"}`, details);
    make("p", "CRAP／coverage／mutation：本圖未附原指標，品質狀態未知。", details);
    const calls = graph.edges.filter((edge) => edge.from === node.id || edge.to === node.id);
    for (const edge of calls) {
      const other = lookup.get(edge.from === node.id ? edge.to : edge.from);
      const button = make(
        "button",
        `${edge.kind} · ${edge.from === node.id ? "→" : "←"} ${other.name}`,
        details,
      );
      button.type = "button";
      button.addEventListener("click", () => select(other));
    }
    for (const gap of graph.unknowns.filter((item) => item.from === node.id)) {
      make("p", `未知：${gap.file}:${gap.line} · ${gap.reason}`, details);
    }
  }
  function render() {
    const matching = graph.nodes.filter(
      (node) =>
        (kind.value === "all" || node.kind === kind.value) &&
        `${node.name} ${node.file} ${node.package}`
          .toLowerCase()
          .includes(search.value.toLowerCase()),
    );
    const shown = matching.slice(0, 60);
    count.textContent = `符合 ${matching.length} 個；圖上顯示 ${shown.length} 個。可搜尋縮小範圍。`;
    list.replaceChildren();
    canvas.replaceChildren();
    const ns = "http://www.w3.org/2000/svg";
    const svg = doc.createElementNS(ns, "svg");
    const height = Math.max(180, Math.ceil(shown.length / 3) * 76);
    svg.setAttribute("viewBox", `0 0 1000 ${height}`);
    svg.setAttribute("role", "img");
    svg.setAttribute("aria-label", "Go 靜態關係圖；完整節點可用下方按鈕瀏覽");
    canvas.append(svg);
    const positions = new Map(
      shown.map((node, i) => [node.id, { x: (i % 3) * 330 + 10, y: Math.floor(i / 3) * 76 + 15 }]),
    );
    const projected =
      kind.value === "file"
        ? graph.edges
            .filter((edge) => edge.kind === "static_call")
            .map((edge) => ({
              kind: edge.kind,
              from: parents.get(edge.from),
              to: parents.get(edge.to),
            }))
        : graph.edges;
    const drawn = new Set();
    for (const edge of projected) {
      const a = positions.get(edge.from),
        b = positions.get(edge.to);
      const edgeKey = JSON.stringify([edge.from, edge.to, edge.kind]);
      if (!a || !b || edge.from === edge.to || drawn.has(edgeKey)) {
        continue;
      }
      drawn.add(edgeKey);
      const dx = b.x - a.x,
        dy = b.y - a.y;
      const inset = Math.min(150 / Math.abs(dx), 24 / Math.abs(dy));
      const x1 = a.x + 150 + dx * inset,
        y1 = a.y + 24 + dy * inset;
      const x2 = b.x + 150 - dx * inset,
        y2 = b.y + 24 - dy * inset;
      const line = doc.createElementNS(ns, "line");
      for (const [key, value] of Object.entries({
        x1,
        y1,
        x2,
        y2,
      })) {
        line.setAttribute(key, String(value));
      }
      line.setAttribute("stroke", edge.kind === "static_call" ? "#d6a45b" : "#71839a");
      const title = doc.createElementNS(ns, "title");
      title.textContent = `${edge.kind}: ${lookup.get(edge.from).name} → ${lookup.get(edge.to).name}`;
      line.append(title);
      svg.append(line);
      const distance = Math.hypot(dx, dy),
        ux = dx / distance,
        uy = dy / distance;
      const arrow = doc.createElementNS(ns, "polygon");
      arrow.setAttribute(
        "points",
        `${x2},${y2} ${x2 - 9 * ux + 4 * uy},${y2 - 9 * uy - 4 * ux} ${x2 - 9 * ux - 4 * uy},${y2 - 9 * uy + 4 * ux}`,
      );
      arrow.setAttribute("fill", edge.kind === "static_call" ? "#d6a45b" : "#71839a");
      svg.append(arrow);
    }
    for (const node of shown) {
      const position = positions.get(node.id);
      const group = doc.createElementNS(ns, "g");
      const rect = doc.createElementNS(ns, "rect");
      for (const [key, value] of Object.entries({
        x: position.x,
        y: position.y,
        width: 300,
        height: 48,
        rx: 8,
      })) {
        rect.setAttribute(key, String(value));
      }
      rect.setAttribute("fill", "#152438");
      rect.setAttribute("stroke", "#6eb7b4");
      const textNode = doc.createElementNS(ns, "text");
      textNode.setAttribute("x", String(position.x + 10));
      textNode.setAttribute("y", String(position.y + 29));
      textNode.setAttribute("fill", "#f0f4f8");
      textNode.setAttribute("font-size", "13");
      textNode.textContent = node.name.length > 36 ? `${node.name.slice(0, 33)}…` : node.name;
      group.append(rect, textNode);
      group.addEventListener("click", () => select(node));
      svg.append(group);
      const button = make("button", `${node.kind} · ${node.name}`, list);
      button.type = "button";
      button.addEventListener("click", () => select(node));
    }
  }
  search.addEventListener("input", render);
  kind.addEventListener("change", render);
  const limitations = make("details");
  make("summary", "分析限制與未知", limitations);
  make(
    "pre",
    JSON.stringify({ limitations: graph.limitations, unknowns: graph.unknowns }, null, 2),
    limitations,
  );
  render();
  return () => {
    root.replaceChildren();
    root.classList.remove("review-graph");
  };
}
