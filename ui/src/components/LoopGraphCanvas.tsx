import { useEffect, useMemo, useRef } from "react";
import { select, zoom, zoomIdentity } from "d3";
import type { D3ZoomEvent, ZoomBehavior } from "d3";
import { layoutGraph } from "~/client/loop-graph-layout.mjs";
import type { LoopGraph } from "~/client/loop-graph";
import type { GraphHighlight } from "./LoopGraphSourceIndex";
import { noHighlight } from "./LoopGraphSourceIndex";

export type GraphSelection =
  | { kind: "node" | "edge" | "object" | "port" | "artifact" | "finding"; id: string }
  | {
      kind: "file";
      id: string;
      repository: string;
      entry: LoopGraph["repositories"][number]["tracked_files"][number];
    };
export function LoopGraphCanvas({
  graph,
  nodes,
  edges,
  highlight,
  onHighlight,
  onSelect,
}: {
  graph: LoopGraph;
  nodes: LoopGraph["nodes"];
  edges: LoopGraph["edges"];
  highlight: GraphHighlight;
  onHighlight: (value: GraphHighlight) => void;
  onSelect: (value: GraphSelection) => void;
}) {
  const svgRef = useRef<SVGSVGElement>(null);
  const contentRef = useRef<SVGGElement>(null);
  const zoomRef = useRef<ZoomBehavior<SVGSVGElement, unknown> | null>(null);
  const layout = useMemo(
    () =>
      layoutGraph({
        nodes,
        edges: edges
          .filter((e) => e.kind === "handoff")
          .map((e) => ({ from: e.from_node_id, to: e.to_node_id })),
      }),
    [nodes, edges],
  );
  const positions = useMemo(() => new Map(layout.nodes.map((node) => [node.id, node])), [layout]);
  useEffect(() => {
    if (!svgRef.current || !contentRef.current) {
      return;
    }
    const content = select(contentRef.current);
    const behavior = zoom<SVGSVGElement, unknown>()
      .extent([
        [0, 0],
        [layout.width, layout.height],
      ])
      .scaleExtent([0.25, 3])
      .filter(
        (event: MouseEvent) =>
          !event.target || !(event.target instanceof Element) || !event.target.closest("button"),
      )
      .on("zoom", (event: D3ZoomEvent<SVGSVGElement, unknown>) =>
        content.attr("transform", event.transform.toString()),
      );
    zoomRef.current = behavior;
    const svg = select(svgRef.current);
    svg.call(behavior);
    return () => {
      svg.on(".zoom", null);
    };
  }, [layout.width, layout.height]);
  const artifactLabel = (ids: string[]) =>
    ids.map((id) => graph.artifacts.find((a) => a.id === id)?.role ?? "unknown").join(" + ") ||
    "unknown";
  return (
    <section aria-label="執行 occurrences 因果圖" className="lg-canvas-section">
      <div className="lg-toolbar">
        <h2>
          執行紀錄{" "}
          <small>
            {nodes.length} occurrences · {edges.length} explicit edges
          </small>
        </h2>
        <button
          type="button"
          onClick={() => {
            if (svgRef.current && zoomRef.current) {
              select(svgRef.current).call(zoomRef.current.transform, zoomIdentity);
            }
          }}
        >
          重設縮放
        </button>
        <button
          type="button"
          onClick={() => {
            if (svgRef.current && zoomRef.current) {
              select(svgRef.current).call(zoomRef.current.scaleBy, 1.2);
            }
          }}
        >
          放大
        </button>
        <button
          type="button"
          onClick={() => {
            if (svgRef.current && zoomRef.current) {
              select(svgRef.current).call(zoomRef.current.scaleBy, 0.8);
            }
          }}
        >
          縮小
        </button>
      </div>
      <p className="lg-small">
        實線＝observed；虛線 ?＝unknown / diagnostic；紅 ×＝已確認 failed。位置由明示 handoff
        排列，無邊的觀察不表示先後。
      </p>
      <div className="lg-canvas-scroll">
        <svg
          ref={svgRef}
          width={layout.width}
          height={layout.height}
          role="group"
          aria-label="提示詞交接 DAG"
          onMouseLeave={() => onHighlight(noHighlight)}
        >
          <defs>
            <marker
              id="lg-arrow"
              viewBox="0 0 10 10"
              refX="9"
              refY="5"
              markerWidth="6"
              markerHeight="6"
              orient="auto-start-reverse"
            >
              <path d="M 0 0 L 10 5 L 0 10 z" fill="currentColor" />
            </marker>
          </defs>
          <g ref={contentRef}>
            {layout.bands.map((band) => (
              <g key={band.repository_id} className="lg-band">
                <rect x={band.x} y={band.y} width={band.width} height={band.height} rx="12" />
                <text x={band.x + 16} y={band.y + 28}>
                  {graph.repositories.find((r) => r.id === band.repository_id)?.label ??
                    band.repository_id}
                </text>
              </g>
            ))}
            {edges.map((edge, index) => {
              const from = positions.get(edge.from_node_id),
                to = positions.get(edge.to_node_id);
              if (!from || !to) {
                return null;
              }
              const sx = from.x + from.width,
                sy = from.y + from.height / 2,
                tx = to.x,
                ty = to.y + to.height / 2;
              const bend = Math.max(40, Math.abs(tx - sx) / 2),
                path = `M${sx},${sy} C${sx + bend},${sy} ${tx - bend},${ty} ${tx},${ty}`;
              const related = {
                nodes: [edge.from_node_id, edge.to_node_id],
                artifacts: [
                  ...edge.input_artifact_ids,
                  ...edge.output_artifact_ids,
                  ...edge.evidence_artifact_ids,
                ],
              };
              const active =
                related.nodes.some((id) => highlight.nodes.includes(id)) ||
                related.artifacts.some((id) => highlight.artifacts.includes(id));
              const label = `${artifactLabel(edge.input_artifact_ids)} → ${artifactLabel(edge.output_artifact_ids)}`;
              let edgeClass = "";
              let edgeSymbol = "";
              if (edge.status !== "observed" || edge.kind === "diagnostic") {
                edgeClass = "is-unknown";
                edgeSymbol = "? ";
              }
              if (edge.status === "failed") {
                edgeClass = "is-failed";
                edgeSymbol = "× ";
              }
              const edgeLabel = `交接 ${edge.label} · ${edge.status} · ${label}`;
              return (
                <g key={edge.id} className={`lg-edge ${edgeClass} ${active ? "is-linked" : ""}`}>
                  <title>
                    {edge.label} · {edge.kind} · {edge.scope} · {edge.current_validity}
                  </title>
                  <path className="lg-edge-hit" d={path} />
                  <path d={path} markerEnd="url(#lg-arrow)" />
                  <text
                    x={(sx + tx) / 2}
                    y={(sy + ty) / 2 - 9 - (index % 3) * 15}
                    textAnchor="middle"
                  >
                    {edgeSymbol}
                    {label.length > 52 ? `${label.slice(0, 49)}…` : label}
                  </text>
                  <foreignObject
                    x={(sx + tx) / 2 - 140}
                    y={(sy + ty) / 2 - 27 - (index % 3) * 15}
                    width={280}
                    height={28}
                  >
                    <button
                      type="button"
                      aria-label={edgeLabel}
                      className={`lg-svg-button ${active ? "is-linked" : ""}`}
                      onClick={() => onSelect({ kind: "edge", id: edge.id })}
                      onMouseEnter={() => onHighlight(related)}
                      onFocus={() => onHighlight(related)}
                      onBlur={() => onHighlight(noHighlight)}
                    />
                  </foreignObject>
                </g>
              );
            })}
            {nodes.map((node) => {
              const pos = positions.get(node.id);
              if (!pos) {
                return null;
              }
              const related = {
                nodes: [node.id],
                artifacts: [
                  ...node.input_artifact_ids,
                  ...node.output_artifact_ids,
                  ...node.evidence_artifact_ids,
                ],
              };
              const active =
                highlight.nodes.includes(node.id) ||
                related.artifacts.some((id) => highlight.artifacts.includes(id));
              const findings = graph.findings.filter((f) => f.node_ids.includes(node.id));
              const failed = findings.some((f) => f.status === "failed"),
                unknown =
                  node.identity.status === "unknown" ||
                  findings.some((f) => f.status === "unknown");
              let nodeClass = "",
                nodeSymbol = "";
              if (unknown) {
                nodeClass = "is-unknown";
                nodeSymbol = "? ";
              }
              if (failed) {
                nodeClass = "is-failed";
                nodeSymbol = "× ";
              }
              return (
                <g
                  key={node.id}
                  transform={`translate(${pos.x},${pos.y})`}
                  className={`lg-node ${active ? "is-linked" : ""} ${nodeClass}`}
                >
                  <title>
                    {node.label} · {node.id} · {JSON.stringify(node.status)}
                  </title>
                  <rect width={pos.width} height={pos.height} rx="8" />
                  <text x="14" y="23" className="lg-node-kind">
                    {nodeSymbol}
                    {node.kind} · {node.identity.status}
                  </text>
                  <text x="14" y="46">
                    {node.label.length > 26 ? `${node.label.slice(0, 25)}…` : node.label}
                  </text>
                  <text x="14" y="69" className="lg-node-state">
                    execution: {node.status.execution.slice(0, 23)}
                  </text>
                  <foreignObject x={0} y={0} width={pos.width} height={pos.height}>
                    <button
                      type="button"
                      aria-label={`紀錄 ${node.label} · identity ${node.identity.status}`}
                      className={`lg-svg-button ${active ? "is-linked" : ""}`}
                      onClick={() => onSelect({ kind: "node", id: node.id })}
                      onMouseEnter={() => onHighlight(related)}
                      onFocus={() => onHighlight(related)}
                      onBlur={() => onHighlight(noHighlight)}
                    />
                  </foreignObject>
                </g>
              );
            })}
          </g>
        </svg>
      </div>
      {nodes.length === 0 && <p>目前篩選沒有 occurrence。未推論缺少的執行。</p>}
    </section>
  );
}
