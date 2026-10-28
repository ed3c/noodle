import { register, render } from "pele/core";
import flowchart from "pele/diagrams/flowchart";
import type { LoopGraph } from "./loop-graph";

register(flowchart);

function label(text: string) {
  return text.replaceAll(/["<>&#\\`\r\n]/g, (char) => `#${char.codePointAt(0)};`);
}

export function peleProjection(graph: LoopGraph, runID: string) {
  const nodes = graph.nodes.filter((node) => node.run_id === runID);
  const edges = graph.edges.filter((edge) => edge.run_id === runID);
  if (!runID || nodes.length === 0) {
    throw new Error("請先選擇有原生投影的任務");
  }
  if (nodes.length > 80 || edges.length > 100) {
    throw new Error("此任務超過關係圖大小限制；請使用原圖檢視，資料未截斷");
  }
  const ids = new Map(nodes.map((node, i) => [node.id, `n${i}`]));
  const artifacts = new Set(
    nodes.flatMap((node) => [...node.input_artifact_ids, ...node.output_artifact_ids]),
  );
  if (artifacts.size > 100) {
    throw new Error("此任務成果數量超過關係圖大小限制");
  }
  const lines = ["flowchart TD"];
  for (const node of nodes) {
    const owner = graph.objects.find((object) => object.id === node.object_id);
    if (!owner) {
      throw new Error("原 owner 投影不存在");
    }
    lines.push(
      `${ids.get(node.id)}["${label(`${owner.label || owner.namespace} · ${node.label} · ${node.status.execution} / acceptance:${node.status.acceptance}`)}"]`,
    );
  }
  let count = 0;
  for (const edge of edges) {
    if (!ids.has(edge.from_node_id) || !ids.has(edge.to_node_id)) {
      throw new Error("交接端點不屬於選定任務");
    }
    lines.push(
      `${ids.get(edge.from_node_id)} -->|"${label(`${edge.kind} · ${edge.status} · ${edge.label}`)}"| ${ids.get(edge.to_node_id)}`,
    );
    count++;
  }
  let i = 0;
  for (const id of artifacts) {
    const artifact = graph.artifacts.find((item) => item.id === id);
    if (!artifact) {
      throw new Error("已登記成果投影不存在");
    }
    const key = `a${i++}`;
    lines.push(`${key}["${label(`${artifact.role} · ${artifact.id} · ${artifact.status}`)}"]`);
    for (const node of nodes) {
      if (node.input_artifact_ids.includes(id)) {
        lines.push(`${key} -. input .-> ${ids.get(node.id)}`);
        count++;
      }
      if (node.output_artifact_ids.includes(id)) {
        lines.push(`${ids.get(node.id)} -. output .-> ${key}`);
        count++;
      }
    }
  }
  if (count > 150) {
    throw new Error("此任務關係數量超過圖形限制");
  }
  const source = lines.join("\n");
  if (source.length > 16_000) {
    throw new Error("此任務標籤超過圖形文字限制");
  }
  return source;
}

export function renderPeleProjection(graph: LoopGraph, runID: string) {
  return render(peleProjection(graph, runID), {
    links: false,
    images: false,
    limit: 16_000,
    outputLimit: 1_000_000,
    maxEdges: 150,
    idPrefix: "noodle-pele",
    responsive: true,
  }).svg;
}
