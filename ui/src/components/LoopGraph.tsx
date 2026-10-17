import { useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { fetchLoopView } from "~/client/loop-views";
import type { LoopViewResponse } from "~/client/loop-views";
import { fetchLoopGraph, fetchArtifact, submitReviewSelection } from "~/client/loop-graph";
import type { GraphResponse, GraphView, LoopGraph } from "~/client/loop-graph";
import { LoopViews, LoopViewsContent } from "./LoopViews";
import { LoopGraphSourceIndex, noHighlight } from "./LoopGraphSourceIndex";
import type { GraphHighlight } from "./LoopGraphSourceIndex";
import { LoopGraphCanvas } from "./LoopGraphCanvas";
import type { GraphSelection } from "./LoopGraphCanvas";
import { LoopGraphDetail } from "./LoopGraphDetail";
import { findingSymbol, knownFailureFrontier } from "~/client/loop-graph-presentation";
import "./loop-graph.css";
import { LoopPanorama } from "./LoopPanorama";
import { projectPanorama } from "~/client/loop-panorama.mjs";
import type { WorkspaceScope, PanoramaFocus } from "~/client/loop-panorama.mjs";

function selectionHighlight(graph: LoopGraph, selection: GraphSelection | null): GraphHighlight {
  if (!selection) {
    return noHighlight;
  }
  if (selection.kind === "file") {
    return { nodes: selection.entry.node_ids, artifacts: selection.entry.artifact_ids };
  }
  if (selection.kind === "node") {
    const node = graph.nodes.find((n) => n.id === selection.id);
    return {
      nodes: [selection.id],
      artifacts: node
        ? [...node.input_artifact_ids, ...node.output_artifact_ids, ...node.evidence_artifact_ids]
        : [],
    };
  }
  if (selection.kind === "edge") {
    const edge = graph.edges.find((e) => e.id === selection.id);
    return edge
      ? {
          nodes: [edge.from_node_id, edge.to_node_id],
          artifacts: [
            ...edge.input_artifact_ids,
            ...edge.output_artifact_ids,
            ...edge.evidence_artifact_ids,
          ],
        }
      : noHighlight;
  }
  if (selection.kind === "object") {
    return {
      nodes: graph.nodes.filter((n) => n.object_id === selection.id).map((n) => n.id),
      artifacts: graph.objects.find((o) => o.id === selection.id)?.source_artifact_ids ?? [],
    };
  }
  if (selection.kind === "artifact") {
    return { nodes: [], artifacts: [selection.id] };
  }
  if (selection.kind === "port") {
    return {
      nodes: [],
      artifacts: graph.ports.find((p) => p.id === selection.id)?.source_artifact_ids ?? [],
    };
  }
  const finding = graph.findings.find((f) => f.id === selection.id);
  return finding ? { nodes: finding.node_ids, artifacts: finding.artifact_ids } : noHighlight;
}
function Objects({
  graph,
  highlight,
  onSelect,
  onHighlight,
}: {
  graph: LoopGraph;
  highlight: GraphHighlight;
  onSelect: (selection: GraphSelection) => void;
  onHighlight: (value: GraphHighlight) => void;
}) {
  return (
    <section className="lg-objects" aria-label="唯一物件與 ports">
      <h2>物件與介面</h2>
      <p className="lg-small">
        同一 namespace 的物件只列一次。介面存在不代表已執行；下方 occurrences 保存每次原生身份。
      </p>
      <div className="lg-object-list">
        {graph.objects.map((owner) => {
          const instances = graph.nodes.filter((n) => n.object_id === owner.id);
          const active =
            instances.some((n) => highlight.nodes.includes(n.id)) ||
            owner.source_artifact_ids.some((id) => highlight.artifacts.includes(id));
          return (
            <article
              key={owner.id}
              className={active ? "is-linked" : ""}
              onMouseLeave={() => onHighlight(noHighlight)}
            >
              <button
                type="button"
                onClick={() => onSelect({ kind: "object", id: owner.id })}
                onMouseEnter={() =>
                  onHighlight({
                    nodes: instances.map((n) => n.id),
                    artifacts: owner.source_artifact_ids,
                  })
                }
                onFocus={() =>
                  onHighlight({
                    nodes: instances.map((n) => n.id),
                    artifacts: owner.source_artifact_ids,
                  })
                }
                onBlur={() => onHighlight(noHighlight)}
              >
                <strong>{owner.label}</strong>
                <small>
                  {owner.namespace} · {owner.scope} · {instances.length} occurrences
                </small>
              </button>
              <div className="lg-ports">
                {graph.ports
                  .filter((p) => p.object_id === owner.id)
                  .map((p) => (
                    <button
                      key={p.id}
                      type="button"
                      onClick={() => onSelect({ kind: "port", id: p.id })}
                    >
                      {p.direction === "input" ? "→" : "←"} {p.name}
                      <small>{p.schema}</small>
                    </button>
                  ))}
              </div>
            </article>
          );
        })}
      </div>
    </section>
  );
}
function Knowledge({
  view,
  kind,
  repository,
}: {
  view: GraphView;
  kind: "cards" | "notes";
  repository: string;
}) {
  const [search, setSearch] = useState(""),
    [type, setType] = useState(""),
    [version, setVersion] = useState(""),
    [consumed, setConsumed] = useState("all");
  const all = view.runs
    .filter(
      (run) =>
        !repository ||
        view.graph.repositories.find((r) => r.id === repository)?.root === run.repository ||
        view.graph.nodes.some((n) => n.run_id === run.id && n.repository_id === repository),
    )
    .flatMap((run) =>
      run[kind].map((item) => ({ item, run, type: "type" in item ? String(item.type) : "note" })),
    );
  const unique = [
    ...new Map(
      all.map((entry) => [
        `${entry.run.repository}:${entry.item.id}:${JSON.stringify(entry.item.source_refs)}:${entry.item.body}`,
        entry,
      ]),
    ).values(),
  ];
  const filtered = unique.filter(
    ({ item, type: itemType }) =>
      (!type || type === itemType) &&
      (!version || item.source_refs.includes(version)) &&
      `${item.title} ${item.body} ${item.id}`.toLowerCase().includes(search.toLowerCase()),
  );
  return (
    <section aria-label={kind === "cards" ? "全部卡片" : "全部筆記"}>
      <div className="lg-filters">
        <label>
          搜尋內容
          <input value={search} onChange={(e) => setSearch(e.target.value)} />
        </label>
        <label>
          類型
          <select value={type} onChange={(e) => setType(e.target.value)}>
            <option value="">全部</option>
            {[...new Set(unique.map((e) => e.type))].toSorted().map((t) => (
              <option key={t}>{t}</option>
            ))}
          </select>
        </label>
        <label>
          來源引用 / 版本
          <select value={version} onChange={(e) => setVersion(e.target.value)}>
            <option value="">全部</option>
            {[...new Set(unique.flatMap((e) => e.item.source_refs))].toSorted().map((ref) => (
              <option key={ref}>{ref}</option>
            ))}
          </select>
        </label>
        <label>
          消費證據
          <select value={consumed} onChange={(e) => setConsumed(e.target.value)}>
            <option value="all">全部</option>
            <option value="unknown">未觀察到明示綁定</option>
          </select>
        </label>
      </div>
      <p>
        {filtered.length} 個唯一 {kind === "cards" ? "卡片" : "筆記"}（repository + ID + 原來源 +
        內容版本）。重複 occurrence 不複製知識。
      </p>
      <p className="lg-small">
        v1 附件沒有 card → invocation 綁定；消費狀態保持 unknown，不由同名或文字相似推論。
      </p>
      <div className="lv-cards">
        {filtered.map(({ item, run, type: itemType }) => (
          <article
            className="lv-card"
            key={`${run.repository}:${item.id}:${JSON.stringify(item.source_refs)}:${item.body}`}
          >
            <small>
              {run.repository} · {itemType} · {item.id}
            </small>
            <h3>{item.title}</h3>
            <p className="lv-body">{item.body}</p>
            <p className="lg-unknown">? consumer binding unknown</p>
            <details>
              <summary>全部來源</summary>
              {[...new Set(item.source_refs)].map((ref) => (
                <pre key={ref}>{ref}</pre>
              ))}
            </details>
          </article>
        ))}
      </div>
    </section>
  );
}
function FullProjectionTab({
  kind,
  view,
  repository,
  runIds,
}: {
  kind: "timeline" | "cards" | "notes";
  view: GraphView;
  repository: string;
  runIds: string[] | null;
}) {
  const client = useQueryClient();
  const query = useQuery({
    queryKey: ["loop-views"],
    queryFn: ({ signal }) =>
      fetchLoopView(client.getQueryData<LoopViewResponse>(["loop-views"]), signal),
    refetchInterval: 3000,
    refetchIntervalInBackground: false,
    retry: false,
  });
  return (
    <>
      <p className="lg-small">此分頁另讀完整原投影。選取的圖比較仍固定於原版本。</p>
      {!query.data && !query.isError && <p role="status">讀取完整投影…</p>}
      {query.isError && (
        <p role="alert">
          {query.error.message}
          {query.data ? "。保留上次成功讀回；目前狀態未知。" : "。完整內容目前未知。"}
        </p>
      )}
      {query.data &&
        (kind === "timeline" ? (
          <LoopViewsContent
            view={{
              ...query.data.view,
              runs: query.data.view.runs.filter((r) => !runIds || runIds.includes(r.id)),
            }}
          />
        ) : (
          <Knowledge
            key={kind}
            view={{
              ...view,
              ...query.data.view,
              graph: view.graph,
              runs: query.data.view.runs.filter((r) => !runIds || runIds.includes(r.id)),
            }}
            kind={kind}
            repository={repository}
          />
        ))}
    </>
  );
}

export function LoopGraphContent({ view }: { view: GraphView }) {
  const [workspace, setWorkspace] = useState<WorkspaceScope>({ goal: "", runIds: null });
  const [focus, setFocus] = useState<PanoramaFocus | null>(null);
  const [pinned, setPinned] = useState<GraphView | null>(null),
    [selection, setSelection] = useState<GraphSelection | null>(null);
  const [tab, setTab] = useState("graph"),
    [run, setRun] = useState(""),
    [repository, setRepository] = useState("");
  const [attempt, setAttempt] = useState(""),
    [version, setVersion] = useState(""),
    [showUnknown, setShowUnknown] = useState(false);
  const [showObjects, setShowObjects] = useState(false),
    [hover, setHover] = useState<GraphHighlight>(noHighlight);
  const current = pinned ?? view,
    { graph } = current;
  const panorama = useMemo(
    () => projectPanorama(graph, workspace.runIds),
    [graph, workspace.runIds],
  );
  const newer = pinned !== null && JSON.stringify(pinned) !== JSON.stringify(view);
  const choose = (value: GraphSelection) => {
    setPinned(current);
    setSelection(value);
  };
  const selectedHighlight = useMemo(() => selectionHighlight(graph, selection), [graph, selection]);
  const highlight =
    hover.nodes.length > 0 || hover.artifacts.length > 0 ? hover : selectedHighlight;
  const nodes = useMemo(
    () =>
      panorama.nodes.filter(
        (n) =>
          (!focus ||
            focus.node_ids.includes(n.id) ||
            panorama.edges.some(
              (e) =>
                focus.edge_ids.includes(e.id) && (e.from_node_id === n.id || e.to_node_id === n.id),
            )) &&
          (!run || n.run_id === run) &&
          (!repository || n.repository_id === repository) &&
          (!attempt || n.identity.attempt_id === attempt) &&
          (!version ||
            [...n.input_artifact_ids, ...n.output_artifact_ids, ...n.evidence_artifact_ids].some(
              (id) => graph.artifacts.find((a) => a.id === id)?.source?.sha256 === version,
            )),
      ),
    [graph, panorama, focus, run, repository, attempt, version],
  );
  const nodeIDs = useMemo(() => new Set(nodes.map((n) => n.id)), [nodes]);
  const edges = useMemo(
    () =>
      panorama.edges.filter(
        (e) =>
          nodeIDs.has(e.from_node_id) &&
          nodeIDs.has(e.to_node_id) &&
          (showUnknown ||
            (e.kind === "handoff" && (e.status === "observed" || e.status === "failed"))),
      ),
    [panorama, nodeIDs, showUnknown],
  );
  const frontier = useMemo(() => knownFailureFrontier(graph), [graph]);
  const findings = graph.findings.filter(
    (f) => (!workspace.runIds || workspace.runIds.includes(f.run_id)) && (!run || f.run_id === run),
  );
  return (
    <>
      <LoopPanorama
        onReviewSelection={
          current.review_version
            ? ({ runId, kind, id }) => {
                if (!current.review_version) {
                  return Promise.reject(new Error("Graph version missing"));
                }
                return submitReviewSelection({
                  graphVersion: current.review_version,
                  runId,
                  kind,
                  targetId: id,
                });
              }
            : undefined
        }
        graph={graph}
        readArtifact={async (id) => {
          const artifact = graph.artifacts.find((a) => a.id === id);
          if (!artifact) {
            return null;
          }
          const result = await fetchArtifact(artifact);
          return result.content;
        }}
        selection={
          selection && selection.kind !== "file" && selection.kind !== "port"
            ? { kind: selection.kind, id: selection.id }
            : null
        }
        highlightedNodes={highlight.nodes}
        onReset={() => {
          setFocus(null);
          setSelection(null);
          setPinned(null);
          setHover(noHighlight);
        }}
        runs={current.runs}
        highlightedObjects={graph.objects
          .filter(
            (o) =>
              o.source_artifact_ids.some((id) => highlight.artifacts.includes(id)) ||
              graph.nodes.some((n) => n.object_id === o.id && highlight.nodes.includes(n.id)),
          )
          .map((o) => o.id)}
        goal={workspace.goal}
        runIds={workspace.runIds}
        onScope={(scope) => {
          setWorkspace(scope);
          setFocus(null);
          setRun("");
          setRepository("");
          setAttempt("");
          setVersion("");
          setSelection(null);
          setPinned(null);
          setHover(noHighlight);
        }}
        onSelect={choose}
        onFocus={(value) => {
          setSelection(null);
          setFocus(value);
          setRun("");
          setRepository("");
          setAttempt("");
          setVersion("");
          setTab("graph");
          setShowUnknown(true);
          setHover({ nodes: value.node_ids, artifacts: [] });
          if (value.reveal !== false) {
            requestAnimationFrame(() =>
              document
                .querySelector('[aria-label="執行 occurrences 因果圖"]')
                ?.scrollIntoView({ block: "start" }),
            );
          }
        }}
      />
      {focus && (
        <button
          type="button"
          onClick={() => {
            setFocus(null);
            setHover(noHighlight);
          }}
        >
          返回 Workspace 全部執行
        </button>
      )}
      <nav className="lg-tabs" aria-label="Loop 顯示方式">
        {[
          ["graph", "關係圖"],
          ["timeline", "v1 時間線"],
          ["cards", "全部卡片"],
          ["notes", "全部筆記"],
          ["requirements", "需求與斷點"],
          ["metrics", "量測"],
        ].map(([id, label]) => (
          <button
            key={id}
            type="button"
            aria-current={tab === id ? "page" : undefined}
            onClick={() => setTab(id)}
          >
            {label}
          </button>
        ))}
      </nav>
      {newer && (
        <div className="lg-update" role="status">
          有新版本。比較仍固定在選取時的來源；原始內容若漂移會拒絕讀取。
          <button
            type="button"
            onClick={() => {
              setSelection(null);
              setPinned(null);
              setHover(noHighlight);
              setFocus(null);
            }}
          >
            採用新版本
          </button>
        </div>
      )}
      {tab !== "timeline" && (
        <div className="lg-filters">
          <label>
            Run
            <select value={run} onChange={(e) => setRun(e.target.value)}>
              <option value="">全部 Runs</option>
              {current.runs
                .filter((r) => !workspace.runIds || workspace.runIds.includes(r.id))
                .map((r) => (
                  <option key={r.id} value={r.id}>
                    {r.repository} · {r.status} · {r.id}
                  </option>
                ))}
            </select>
          </label>
          <label>
            Repository
            <select value={repository} onChange={(e) => setRepository(e.target.value)}>
              <option value="">所有關聯區域</option>
              {graph.repositories.map((r) => (
                <option key={r.id} value={r.id}>
                  {r.label}
                </option>
              ))}
            </select>
          </label>
        </div>
      )}
      {(tab === "timeline" || tab === "cards" || tab === "notes") && (
        <FullProjectionTab
          kind={tab}
          view={current}
          repository={repository}
          runIds={workspace.runIds}
        />
      )}
      {tab === "requirements" && (
        <section aria-label="需求與驗收讀回">
          <p>需求忠實度、執行與 acceptance 各自保留原結果。歷史反例不由後續成功抹除。</p>
          {findings.map((f) => (
            <article key={f.id} className={`lg-finding is-${f.status}`}>
              <strong>
                {findingSymbol(f.status)} {f.kind} · {f.status}
              </strong>
              <p>{f.message}</p>
              <p>原需求：{f.requirement_refs.join(" · ") || "unknown"}</p>
              <button type="button" onClick={() => choose({ kind: "finding", id: f.id })}>
                讀原證據與 next
              </button>
            </article>
          ))}
          {findings.length === 0 && <p>尚無原生需求 / 驗收 finding，保持 unknown。</p>}
        </section>
      )}
      {tab === "metrics" && (
        <section aria-label="量測與分母">
          <p>原 Manager 值與直接量測分開；圖節點數不代表決策次數或成功率。</p>
          {graph.metrics
            .filter(
              (m) =>
                (!workspace.runIds || workspace.runIds.includes(m.run_id)) &&
                (!run || m.run_id === run),
            )
            .map((m) => (
              <article key={m.id} className="lv-card">
                <h3>{m.name}</h3>
                <p>
                  {m.status} · {m.value ?? "unknown"} {m.unit}
                </p>
                <p>
                  分子 {m.numerator ?? "unknown"} / 分母 {m.denominator ?? "unknown"}
                  {m.denominator === 0 ? " · not_applicable" : ""}
                </p>
                {m.observation_scope && (
                  <div className="lg-small">
                    <p>
                      歷史量測 · {m.observation_scope.host_version} · PID{" "}
                      {m.observation_scope.host_pid ?? "unknown"}
                    </p>
                    <p>
                      時間窗 {m.observation_scope.started_at ?? "unknown"} →{" "}
                      {m.observation_scope.finished_at ?? "unknown"}
                    </p>
                    <p>公式：{m.observation_scope.formula}</p>
                    <p>Run scope：{m.observation_scope.run_ids.join(" · ") || "unknown"}</p>
                    {m.observation_scope.limitations.map((limitation) => (
                      <p key={limitation}>限制：{limitation}</p>
                    ))}
                  </div>
                )}
                {m.gaps.map((gap) => (
                  <p key={gap}>? {gap}</p>
                ))}
                {m.source_artifact_ids.map((id) => (
                  <button key={id} type="button" onClick={() => choose({ kind: "artifact", id })}>
                    量測來源 {id}
                  </button>
                ))}
              </article>
            ))}
          {graph.metrics.length === 0 && <p>未提供可綁定的量測；unknown，不填零。</p>}
          <div className="lv-resources">
            {[...new Map(current.resources.map((r) => [`${r.label}:${r.value}`, r])).values()].map(
              (r) => (
                <div key={`${r.label}:${r.value}`}>
                  <strong>{r.label}</strong>
                  <p>{r.value}</p>
                </div>
              ),
            )}
          </div>
        </section>
      )}
      {tab === "graph" && (
        <div className="lg-workspace">
          <LoopGraphSourceIndex
            key={focus?.node_ids[0] ?? focus?.edge_ids[0] ?? "all"}
            initialRepository={
              graph.nodes.find((n) => focus?.node_ids.includes(n.id))?.repository_id
            }
            graph={graph}
            highlight={highlight}
            onHighlight={setHover}
            onSelect={(entry, repo) =>
              choose({ kind: "file", id: entry.id, entry, repository: repo })
            }
          />
          <div className="lg-main">
            <div className="lg-filters">
              <label>
                Attempt
                <select value={attempt} onChange={(e) => setAttempt(e.target.value)}>
                  <option value="">全部 / unknown</option>
                  {[
                    ...new Set(
                      graph.nodes.flatMap((n) =>
                        n.identity.attempt_id ? [n.identity.attempt_id] : [],
                      ),
                    ),
                  ].map((id) => (
                    <option key={id}>{id}</option>
                  ))}
                </select>
              </label>
              <label>
                Artifact 來源版本
                <select value={version} onChange={(e) => setVersion(e.target.value)}>
                  <option value="">全部</option>
                  {[
                    ...new Set(
                      graph.artifacts.flatMap((a) => (a.source?.sha256 ? [a.source.sha256] : [])),
                    ),
                  ].map((sha) => (
                    <option key={sha}>{sha}</option>
                  ))}
                </select>
              </label>
              <label className="lg-check">
                <input
                  type="checkbox"
                  checked={showUnknown}
                  onChange={(e) => setShowUnknown(e.target.checked)}
                />
                顯示 unknown / diagnostic edges
              </label>
              <label className="lg-check">
                <input
                  type="checkbox"
                  checked={showObjects}
                  onChange={(e) => setShowObjects(e.target.checked)}
                />
                顯示唯一物件關聯
              </label>
            </div>
            {showObjects && (
              <Objects
                graph={graph}
                highlight={highlight}
                onSelect={choose}
                onHighlight={setHover}
              />
            )}
            <LoopGraphCanvas
              graph={graph}
              nodes={nodes}
              edges={edges}
              highlight={highlight}
              onHighlight={setHover}
              onSelect={choose}
            />
            <section aria-label="已觀察斷點">
              <h2>斷點與缺口</h2>
              <p className="lg-small">
                已知因果前沿只沿 observed handoffs 判定。無順序證據的失敗並列；歷史失敗保留。
              </p>
              {findings.map((f) => (
                <button
                  type="button"
                  key={f.id}
                  className={`lg-finding is-${f.status}`}
                  onClick={() => choose({ kind: "finding", id: f.id })}
                >
                  {findingSymbol(f.status)} {f.status} · {f.message}
                  {frontier.has(f.id) && <small> · 已知因果前沿</small>}
                </button>
              ))}
              {findings.length === 0 && <p>未提供 findings；不推論整體 PASS。</p>}
            </section>
          </div>
        </div>
      )}
      {selection && (
        <LoopGraphDetail
          key={`${selection.kind}:${selection.id}`}
          graph={graph}
          selection={selection}
          onSelect={choose}
          onClose={() => {
            setSelection(null);
            setPinned(null);
            setHover(noHighlight);
          }}
        />
      )}
    </>
  );
}
const key = ["loop-graph"];
export function LoopGraphPanel() {
  const [legacy, setLegacy] = useState(false);
  const client = useQueryClient();
  const query = useQuery({
    queryKey: key,
    queryFn: ({ signal }) => fetchLoopGraph(client.getQueryData<GraphResponse>(key), signal),
    refetchInterval: 3000,
    refetchIntervalInBackground: false,
    retry: false,
  });
  let connectionLabel = "讀取中";
  if (query.data) {
    connectionLabel = "自動更新";
  }
  if (query.isError) {
    connectionLabel = "讀回失敗";
  }
  if (legacy) {
    return (
      <>
        <button type="button" onClick={() => setLegacy(false)}>
          返回關係圖
        </button>
        <LoopViews />
      </>
    );
  }
  return (
    <main className="lv-page lg-page">
      <header className="lv-header">
        <div>
          <p className="lv-eyebrow">NOODLE · 唯讀來源與交接</p>
          <h1>LoopManager</h1>
          <p>從原來源，讀取每一次具證據的交接。</p>
          <p className="lg-small">
            來源：Host 已登記的唯讀投影；每 3 秒讀回。Snapshot 與 live Owner 狀態、驗收分開。
          </p>
          {query.data && (
            <p className="lg-small">
              本頁上次讀回：{new Date(query.dataUpdatedAt).toLocaleString()} ·{" "}
              {query.data.etag ?? "版本標記 unknown"}
            </p>
          )}
        </div>
        <span className="lv-status">{connectionLabel}</span>
      </header>
      <p className="lv-notice">
        物件、執行身份、傳遞與驗收分開呈現。圖與比較不授權效果；缺證據保持 unknown。
      </p>
      {query.isError && (
        <p role="alert">
          {query.error.message}
          {query.data ? "。保留上次成功讀回；目前狀態未知。" : "。可使用原 v1 時間線。"}
        </p>
      )}
      {query.isError && (
        <button type="button" onClick={() => setLegacy(true)}>
          開啟 v1 時間線
        </button>
      )}
      {query.data && <LoopGraphContent view={query.data.view} />}
    </main>
  );
}
