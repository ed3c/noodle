import { useEffect, useRef } from "react";
import { useQuery } from "@tanstack/react-query";
import { readStructuredField } from "~/client/loop-panorama.mjs";
import { fetchArtifact, fetchGraphFile } from "~/client/loop-graph";
import type { LoopGraph } from "~/client/loop-graph";
import type { GraphSelection } from "./LoopGraphCanvas";
import { findingSymbol } from "~/client/loop-graph-presentation";
import { LoopGraphComparison } from "./LoopGraphComparison";

function FileContent({ selection }: { selection: Extract<GraphSelection, { kind: "file" }> }) {
  const { entry } = selection;
  const query = useQuery({
    queryKey: ["loop-git-file", selection.repository, entry.id, entry.git_blob],
    queryFn: ({ signal }) => fetchGraphFile(selection.repository, entry, signal),
    enabled: entry.git_blob !== null,
    retry: false,
    staleTime: Infinity,
  });
  return (
    <>
      <dl className="lg-metadata">
        <dt>Git path</dt>
        <dd>{entry.path}</dd>
        <dt>Git blob</dt>
        <dd>{entry.git_blob ?? "unknown"}</dd>
        <dt>bytes / mode</dt>
        <dd>
          {entry.bytes ?? "unknown"} / {entry.git_mode ?? "unknown"}
        </dd>
      </dl>
      {query.isError && <p role="alert">{query.error.message}</p>}
      {query.data?.error && (
        <p role="alert">
          {query.data.status}: {query.data.error}
        </p>
      )}
      <pre tabIndex={0} aria-label="Git 來源內容">
        {query.data?.content ??
          (query.isFetching ? "讀取固定 Git blob…" : "unknown：沒有可讀的已登記 Git blob。")}
      </pre>
    </>
  );
}
function StructuredField({ graph, node }: { graph: LoopGraph; node: LoopGraph["nodes"][number] }) {
  const artifact = graph.artifacts.find(
    (a) =>
      a.source &&
      node.identity.locator?.startsWith(`${a.source.path}#/`) &&
      [
        ...node.input_artifact_ids,
        ...node.output_artifact_ids,
        ...node.evidence_artifact_ids,
      ].includes(a.id),
  );
  const query = useQuery({
    queryKey: ["loop-artifact", artifact?.id, artifact?.source?.sha256, artifact?.source?.bytes],
    queryFn: ({ signal }) => {
      if (!artifact) {
        throw new Error("Structured field source missing");
      }
      return fetchArtifact(artifact, signal);
    },
    enabled: artifact?.status === "available",
    retry: false,
    staleTime: Infinity,
  });
  if (!artifact) {
    return null;
  }
  const field =
    typeof query.data?.content === "string"
      ? readStructuredField(
          query.data.content,
          node.identity.locator,
          artifact.source?.path ?? null,
        )
      : null;
  return (
    <section aria-label="結構化欄位內容">
      <h3>原 Schema 欄位</h3>
      <p>欄位狀態：{field?.status ?? "unknown"}</p>
      <p>{field?.pointer ?? node.identity.locator}</p>
      <pre>
        {field?.text ??
          query.error?.message ??
          query.data?.error ??
          "UNKNOWN：原欄位內容尚未載入或不可讀。"}
      </pre>
      <p>這是原輸出的欄位投影；不代表 Session 判斷正確或 next 已執行。</p>
    </section>
  );
}

export function LoopGraphDetail({
  graph,
  selection,
  onSelect,
  onClose,
}: {
  graph: LoopGraph;
  selection: GraphSelection;
  onSelect: (value: GraphSelection) => void;
  onClose: () => void;
}) {
  const panel = useRef<HTMLElement>(null);
  useEffect(() => {
    panel.current?.scrollIntoView?.({ block: "start", behavior: "smooth" });
  }, [selection.kind, selection.id]);
  let title = selection.id;
  let inputs: string[] = [];
  let outputs: string[] = [];
  let evidence: string[] = [];
  let detail: unknown = null;
  if (selection.kind === "node") {
    const node = graph.nodes.find((n) => n.id === selection.id);
    if (node) {
      title = node.label;
      inputs = node.input_artifact_ids;
      outputs = node.output_artifact_ids;
      evidence = node.evidence_artifact_ids;
      detail = { identity: node.identity, status: node.status, stage_id: node.stage_id };
    }
  } else if (selection.kind === "edge") {
    const edge = graph.edges.find((e) => e.id === selection.id);
    if (edge) {
      title = edge.label;
      inputs = edge.input_artifact_ids;
      outputs = edge.output_artifact_ids;
      evidence = edge.evidence_artifact_ids;
      detail = edge;
    }
  } else if (selection.kind === "port") {
    const port = graph.ports.find((p) => p.id === selection.id);
    if (port) {
      title = port.name;
      evidence = port.source_artifact_ids;
      detail = port;
    }
  } else if (selection.kind === "object") {
    const owner = graph.objects.find((o) => o.id === selection.id);
    if (owner) {
      title = owner.label;
      evidence = owner.source_artifact_ids;
      detail = owner;
    }
  } else if (selection.kind === "artifact") {
    const artifact = graph.artifacts.find((a) => a.id === selection.id);
    if (artifact) {
      title = artifact.source?.path ?? artifact.id;
      inputs = [artifact.id];
      detail = artifact;
    }
  } else if (selection.kind === "finding") {
    const finding = graph.findings.find((f) => f.id === selection.id);
    if (finding) {
      title = `${finding.status}: ${finding.message}`;
      evidence = finding.artifact_ids;
      detail = finding;
    }
  } else if (selection.kind === "file") {
    title = selection.entry.path;
    inputs = selection.entry.artifact_ids;
    detail = selection.entry;
  }
  const connected = graph.edges.filter(
    (edge) =>
      (selection.kind === "node" && [edge.from_node_id, edge.to_node_id].includes(selection.id)) ||
      (selection.kind === "port" && [edge.from_port_id, edge.to_port_id].includes(selection.id)),
  );
  return (
    <aside ref={panel} className="lg-detail" aria-label="選取內容比較">
      <div className="lg-toolbar">
        <h2>{title}</h2>
        <button type="button" onClick={onClose}>
          關閉選取
        </button>
      </div>
      <p className="lg-small">
        {selection.kind} · {selection.id} · 固定選取時的來源版本；內容與原 Owner next 僅供讀回。
      </p>
      {detail !== null && (
        <details>
          <summary>原身份、狀態與來源 metadata</summary>
          <pre>{JSON.stringify(detail, null, 2)}</pre>
        </details>
      )}
      {selection.kind === "node" &&
        graph.nodes
          .filter((n) => n.id === selection.id)
          .map((node) => <StructuredField key={node.id} graph={graph} node={node} />)}
      {connected.length > 0 && (
        <div className="lg-related">
          <h3>明示關聯交接</h3>
          {connected.map((e) => (
            <button key={e.id} type="button" onClick={() => onSelect({ kind: "edge", id: e.id })}>
              {e.label} · {e.status}
            </button>
          ))}
        </div>
      )}
      {selection.kind === "object" && (
        <div className="lg-related">
          <h3>同一物件的 occurrences</h3>
          {graph.nodes
            .filter((n) => n.object_id === selection.id)
            .map((n) => (
              <button key={n.id} type="button" onClick={() => onSelect({ kind: "node", id: n.id })}>
                {n.label} · {n.identity.attempt_id ?? "attempt unknown"}
              </button>
            ))}
        </div>
      )}
      {selection.kind === "file" && inputs.length === 0 ? (
        <FileContent selection={selection} />
      ) : (
        <LoopGraphComparison
          key={`${selection.kind}:${selection.id}`}
          graph={graph}
          inputs={inputs}
          outputs={outputs}
          evidence={evidence}
        />
      )}
      <section aria-label="關聯斷點與需求">
        {graph.findings
          .filter(
            (f) =>
              f.id === selection.id ||
              (selection.kind === "node" && f.node_ids.includes(selection.id)) ||
              (selection.kind === "edge" && f.edge_ids.includes(selection.id)) ||
              f.artifact_ids.some((id) => [...inputs, ...outputs, ...evidence].includes(id)),
          )
          .map((f) => (
            <article key={f.id} className={`lg-finding is-${f.status}`}>
              <strong>
                {findingSymbol(f.status)} {f.kind} · {f.status}
              </strong>
              <p>{f.message}</p>
              <p>原需求：{f.requirement_refs.join(" · ") || "unknown"}</p>
              <pre>{JSON.stringify(f.original_next, null, 2)}</pre>
            </article>
          ))}
      </section>
    </aside>
  );
}
