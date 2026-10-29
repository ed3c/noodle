import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { fetchLoopView, fetchLoopRun } from "~/client/loop-views";
import type { LoopRun, LoopView, LoopViewResponse } from "~/client/loop-views";
import "./loop-views.css";

function occurrences<T>(values: T[], identify: (value: T) => string) {
  const counts = new Map<string, number>();
  return values.map((value) => {
    const identity = identify(value);
    const count = (counts.get(identity) ?? 0) + 1;
    counts.set(identity, count);
    return { value, key: `${identity}:${count}` };
  });
}

function Refs({ label, refs }: { label: string; refs: string[] }) {
  if (refs.length === 0) {
    return null;
  }
  return (
    <details className="lv-refs">
      <summary>
        {label} · {refs.length}
      </summary>
      {occurrences(refs, (ref) => ref).map(({ value, key }) => (
        <code key={key}>{value}</code>
      ))}
    </details>
  );
}

function Resources({ items }: { items: LoopView["resources"] }) {
  if (items.length === 0) {
    return null;
  }
  return (
    <dl className="lv-resources">
      {occurrences(items, (item) => `${item.label}:${item.value}`).map(({ value: item, key }) => (
        <div key={key}>
          <dt>{item.label}</dt>
          <dd>{item.value}</dd>
        </div>
      ))}
    </dl>
  );
}

function PromptComparison({
  stage,
  previous,
}: {
  stage: LoopRun["stages"][number];
  previous: LoopRun["stages"][number] | undefined;
}) {
  const { length } = [...stage.prompt];
  if (!previous) {
    return <p className="lv-muted">第一階段 · {length} 字元 · 無前一階段可比較。</p>;
  }
  const { length: previousLength } = [...previous.prompt];
  const delta = length - previousLength;
  const change = delta > 0 ? `+${delta}` : String(delta);
  return (
    <div className="lv-comparison" aria-label={`${previous.id} → ${stage.id} 文字比較`}>
      <p className="lv-muted">
        <strong>{previous.prompt === stage.prompt ? "完全相同" : "文字不同"}</strong>
        {` · ${previousLength} → ${length} 字元（${change}）`}
      </p>
      <details>
        <summary>前一階段 → 本階段：並列比較</summary>
        <div className="lv-prompt-pair">
          <div>
            <p className="lv-muted">
              前一階段 · {previous.kind} · {previous.id}
            </p>
            <pre>{previous.prompt || "（空字串）"}</pre>
          </div>
          <div>
            <p className="lv-muted">
              本階段 · {stage.kind} · {stage.id}
            </p>
            <pre>{stage.prompt || "（空字串）"}</pre>
          </div>
        </div>
      </details>
      <p className="lv-muted">
        僅比較文字；不代表語義改變或 consumer 已遵循。字元數按 Unicode code point 計算。
      </p>
    </div>
  );
}

function RunDetail({ run }: { run: LoopRun }) {
  return (
    <article className="lv-detail">
      <header>
        <p className="lv-eyebrow">{run.mode}</p>
        <h2>{run.repository}</h2>
        <p className="lv-muted">
          {run.id} · {run.status}
        </p>
      </header>
      <Resources items={run.resources} />
      <section aria-label="提示詞交接">
        <h3>
          提示詞交接 <span>{run.stages.length}</span>
        </h3>
        {run.stages.length === 0 && <p className="lv-muted">尚無交接觀察。</p>}
        <ol className="lv-timeline">
          {run.stages.map((stage, index) => (
            <li key={stage.id}>
              <span className="lv-step">{index + 1}</span>
              <div className="lv-stage">
                <div className="lv-stage-title">
                  <h4>{stage.kind}</h4>
                  <span className="lv-status">{stage.status}</span>
                </div>
                <p className="lv-muted">
                  {stage.id} · consumer: {stage.consumer_status}
                </p>
                <details>
                  <summary>提示詞</summary>
                  <pre>{stage.prompt || "尚無提示詞內容。"}</pre>
                </details>
                <PromptComparison stage={stage} previous={run.stages[index - 1]} />
                <Refs label="輸入來源" refs={stage.input_refs} />
                <Refs label="輸出來源" refs={stage.output_refs} />
              </div>
            </li>
          ))}
        </ol>
      </section>
      {run.breakpoints.length > 0 && (
        <section aria-label="斷點">
          <h3>斷點</h3>
          {occurrences(run.breakpoints, (point) => JSON.stringify(point)).map(
            ({ value: point, key }) => (
              <div className="lv-breakpoint" key={key}>
                <strong>
                  {point.kind} · {point.status}
                </strong>
                <p>{point.message}</p>
                <small>{point.stage_id}</small>
              </div>
            ),
          )}
        </section>
      )}
      <section aria-label="規格卡片">
        <h3>
          規格卡片 <span>{run.cards.length}</span>
        </h3>
        {run.cards.length === 0 && <p className="lv-muted">此投影沒有規格卡片。</p>}
        <div className="lv-cards">
          {run.cards.map((card) => (
            <article className="lv-card" key={card.id}>
              <p className="lv-eyebrow">
                {card.type} · {card.id}
              </p>
              <h4>{card.title}</h4>
              <p className="lv-body">{card.body}</p>
              <Refs label="來源" refs={card.source_refs} />
            </article>
          ))}
        </div>
      </section>
      <section aria-label="卡盒筆記">
        <h3>
          卡盒筆記 <span>{run.notes.length}</span>
        </h3>
        {run.notes.length === 0 && <p className="lv-muted">此投影沒有筆記。</p>}
        <div className="lv-cards">
          {run.notes.map((note) => (
            <article className="lv-card" key={note.id}>
              <p className="lv-eyebrow">{note.id}</p>
              <h4>{note.title}</h4>
              <p className="lv-body">{note.body}</p>
              <Refs label="來源" refs={note.source_refs} />
            </article>
          ))}
        </div>
      </section>
      <section className="lv-next" aria-label="原 Owner 下一步">
        <h3>原 Owner 下一步</h3>
        <p>{run.next ?? "未提供下一步。"}</p>
        <small>此處僅供讀回，不會執行。</small>
      </section>
    </article>
  );
}

export function useLoopRun(summary: LoopRun | undefined) {
  return useQuery({
    queryKey: ["loop-run", summary?.id, summary?.detail?.sha256],
    queryFn: ({ signal }) => {
      if (!summary) {
        throw new Error("Run missing");
      }
      return fetchLoopRun(summary, signal);
    },
    enabled: Boolean(summary?.detail),
    retry: false,
    refetchInterval: 3000,
    refetchIntervalInBackground: false,
  });
}
function SelectedRun({ summary }: { summary: LoopRun }) {
  const query = useLoopRun(summary);
  const run = summary.detail ? query.data : summary;
  return (
    <>
      {query.isError && <p role="alert">{query.error.message}。目前明細未知；保留上次成功讀回。</p>}
      {!run && !query.isError && <p role="status">讀取選定任務明細…</p>}
      {run && <RunDetail run={run} />}
    </>
  );
}

export function LoopViewsContent({ view }: { view: LoopView }) {
  const [selected, setSelected] = useState<string>();
  const run = view.runs.find((item) => item.id === selected) ?? view.runs[0];
  return (
    <>
      <Resources items={view.resources} />
      {view.runs.length === 0 ? (
        <p className="lv-empty">尚無已匯入的迴圈投影。</p>
      ) : (
        <div className="lv-grid">
          <nav aria-label="迴圈執行" className="lv-nav">
            {view.runs.map((item) => (
              <button
                type="button"
                key={item.id}
                aria-current={item.id === run?.id ? "true" : undefined}
                onClick={() => setSelected(item.id)}
              >
                <strong>{item.repository}</strong>
                <span>
                  {item.mode} · {item.status}
                </span>
                <small>{item.id}</small>
              </button>
            ))}
          </nav>
          {run &&
            (run.detail ? <SelectedRun key={run.id} summary={run} /> : <RunDetail run={run} />)}
        </div>
      )}
    </>
  );
}

const key = ["loop-views"];

export function LoopViews() {
  const client = useQueryClient();
  const query = useQuery({
    queryKey: key,
    queryFn: ({ signal }) => fetchLoopView(client.getQueryData<LoopViewResponse>(key), signal),
    refetchInterval: 2000,
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
  return (
    <main className="lv-page">
      <header className="lv-header">
        <div>
          <p className="lv-eyebrow">NOODLE · 唯讀觀察</p>
          <h1>LoopManager</h1>
          <p>提示詞交接、規格卡片與卡盒筆記。</p>
        </div>
        <span className="lv-status">{connectionLabel}</span>
      </header>
      <p className="lv-notice">
        觀察與原 Owner 的判定分開呈現。已準備或已傳送的提示詞，不代表 consumer 已遵守。
      </p>
      {query.isError && (
        <p role="alert" className="lv-breakpoint">
          {query.error.message}
          {query.data ? "。保留上次成功讀回；目前狀態未知。" : ""}
        </p>
      )}
      {query.data && <LoopViewsContent view={query.data.view} />}
    </main>
  );
}
