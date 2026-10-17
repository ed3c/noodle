import { useEffect, useMemo, useRef, useState } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import type { LoopGraph } from "~/client/loop-graph";

export interface GraphHighlight {
  nodes: string[];
  artifacts: string[];
}
export const noHighlight: GraphHighlight = { nodes: [], artifacts: [] };
type FileEntry = LoopGraph["repositories"][number]["tracked_files"][number];
export function LoopGraphSourceIndex({
  graph,
  initialRepository,
  highlight,
  onHighlight,
  onSelect,
}: {
  graph: LoopGraph;
  initialRepository?: string;
  highlight: GraphHighlight;
  onHighlight: (value: GraphHighlight) => void;
  onSelect: (entry: FileEntry, repository: string) => void;
}) {
  const [repository, setRepository] = useState(
    initialRepository ?? graph.repositories[0]?.id ?? "",
  );
  const [search, setSearch] = useState("");
  const [section, setSection] = useState("tracked");
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  const parent = useRef<HTMLDivElement>(null);
  const repo = graph.repositories.find((item) => item.id === repository) ?? graph.repositories[0];
  const files = useMemo(() => {
    if (!repo) {
      return [];
    }
    if (section === "runtime") {
      const registered = new Set(repo.selected_files.flatMap((entry) => entry.artifact_ids));
      for (const node of graph.nodes) {
        if (node.repository_id !== repo.id) {
          continue;
        }
        for (const id of [
          ...node.input_artifact_ids,
          ...node.output_artifact_ids,
          ...node.evidence_artifact_ids,
        ]) {
          registered.add(id);
        }
      }
      return graph.artifacts
        .filter((a) => a.repository_id === repo.id || registered.has(a.id))
        .map(
          (a): FileEntry => ({
            id: a.id,
            path: a.source?.path ?? a.id,
            git_blob: null,
            git_mode: "runtime",
            bytes: a.source?.bytes ?? null,
            artifact_ids: [a.id],
            node_ids: graph.nodes
              .filter((n) =>
                [
                  ...n.input_artifact_ids,
                  ...n.output_artifact_ids,
                  ...n.evidence_artifact_ids,
                ].includes(a.id),
              )
              .map((n) => n.id),
          }),
        );
    }
    return section === "selected" ? repo.selected_files : repo.tracked_files;
  }, [graph, repo, section]);
  const rows = useMemo(() => {
    const entries: (
      | { kind: "file"; entry: FileEntry; path: string }
      | { kind: "directory"; path: string }
    )[] = [];
    const directories = new Set<string>();
    for (const entry of [...files].toSorted((a, b) => a.path.localeCompare(b.path))) {
      if (!entry.path.toLowerCase().includes(search.toLowerCase())) {
        continue;
      }
      const parts = entry.path.split("/").filter(Boolean);
      let hidden = false;
      for (let i = 1; i < parts.length; i++) {
        const path = `${parts.slice(0, i).join("/")}/`;
        if (!directories.has(path)) {
          entries.push({ kind: "directory", path });
          directories.add(path);
        }
        if (collapsed.has(path) && !search) {
          hidden = true;
          break;
        }
      }
      if (!hidden) {
        entries.push({ kind: "file", entry, path: entry.path });
      }
    }
    return entries;
  }, [files, search, collapsed]);
  const virtual = useVirtualizer({
    count: rows.length,
    getScrollElement: () => parent.current,
    estimateSize: () => 38,
    overscan: 12,
    getItemKey: (index) => `${rows[index]?.kind}:${rows[index]?.path}`,
  });
  const linked = (entry: FileEntry) =>
    entry.node_ids.some((id) => highlight.nodes.includes(id)) ||
    entry.artifact_ids.some((id) => highlight.artifacts.includes(id));
  const firstLinked = rows.findIndex((row) => row.kind === "file" && linked(row.entry));
  useEffect(() => {
    if (firstLinked !== -1) {
      virtual.scrollToIndex(firstLinked, { align: "auto" });
    }
  }, [firstLinked, virtual]);
  const repoWithMatch = graph.repositories.find((candidate) =>
    [...candidate.tracked_files, ...candidate.selected_files].some((entry) => linked(entry)),
  );
  const items = virtual.getVirtualItems();
  const rendered =
    items.length > 0
      ? items
      : rows.slice(0, 30).map((_, index) => ({ index, key: index, start: index * 38, size: 38 }));
  return (
    <aside className="lg-source" aria-label="完整來源索引">
      <h2>來源索引</h2>
      <label>
        Repository
        <select value={repo?.id ?? ""} onChange={(e) => setRepository(e.target.value)}>
          {graph.repositories.map((r) => (
            <option key={r.id} value={r.id}>
              {r.label}
            </option>
          ))}
        </select>
      </label>
      {repo && (
        <>
          <p className="lg-small">
            版本 <code>{repo.revision ?? "unknown"}</code>
            <br />
            索引 {repo.index_status} · {repo.tracked_files.length} tracked files
          </p>
          {repo.gaps.map((gap) => (
            <p key={gap} className="lg-unknown">
              ? {gap}
            </p>
          ))}
        </>
      )}
      {repoWithMatch && repoWithMatch.id !== repo?.id && (
        <button type="button" onClick={() => setRepository(repoWithMatch.id)}>
          定位關聯來源：{repoWithMatch.label}
        </button>
      )}
      <label>
        來源範圍
        <select value={section} onChange={(e) => setSection(e.target.value)}>
          <option value="tracked">完整 Git 索引</option>
          <option value="selected">選定工作檔</option>
          <option value="runtime">已註冊 runtime artifacts</option>
        </select>
      </label>
      <label>
        搜尋路徑
        <input value={search} onChange={(e) => setSearch(e.target.value)} placeholder="路徑包含…" />
      </label>
      <p className="lg-small">{files.length} 個 metadata 項目；索引不授權讀取未註冊內容。</p>
      <div
        ref={parent}
        className="lg-file-scroll"
        role="group"
        aria-label="來源檔案"
        onMouseLeave={() => onHighlight(noHighlight)}
      >
        <div style={{ height: virtual.getTotalSize() || rows.length * 38, position: "relative" }}>
          {rendered.map((item) => {
            const row = rows[item.index];
            if (!row) {
              return null;
            }
            const descendants =
              row.kind === "directory"
                ? files.filter((f) => f.path.replace(/^\//, "").startsWith(row.path))
                : [row.entry];
            const related = {
              nodes: [...new Set(descendants.flatMap((f) => f.node_ids))],
              artifacts: [...new Set(descendants.flatMap((f) => f.artifact_ids))],
            };
            const active = descendants.some((entry) => linked(entry));
            return (
              <button
                type="button"
                className={`lg-file ${active ? "is-linked" : ""}`}
                key={item.key}
                style={{
                  position: "absolute",
                  top: 0,
                  transform: `translateY(${item.start}px)`,
                  height: 38,
                  width: "100%",
                }}
                title={row.path}
                aria-label={`${row.kind === "directory" ? "資料夾" : "檔案"} ${row.path}`}
                onMouseEnter={() => onHighlight(related)}
                onFocus={() => onHighlight(related)}
                onBlur={() => onHighlight(noHighlight)}
                onClick={() => {
                  if (row.kind === "file") {
                    onSelect(row.entry, repo?.id ?? "");
                  } else {
                    setCollapsed((old) => {
                      const next = new Set(old);
                      if (next.has(row.path)) {
                        next.delete(row.path);
                      } else {
                        next.add(row.path);
                      }
                      return next;
                    });
                  }
                }}
              >
                {row.kind === "file" && "· "}
                {row.kind === "directory" && (collapsed.has(row.path) ? "▸ " : "▾ ")}
                {row.path}
              </button>
            );
          })}
        </div>
      </div>
    </aside>
  );
}
