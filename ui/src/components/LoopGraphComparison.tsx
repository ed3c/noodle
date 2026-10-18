import { useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { fetchArtifact } from "~/client/loop-graph";
import type { Artifact, LoopGraph } from "~/client/loop-graph";

function useContent(artifact: Artifact | undefined) {
  return useQuery({
    queryKey: ["loop-artifact", artifact?.id, artifact?.source?.sha256, artifact?.source?.bytes],
    queryFn: ({ signal }) => {
      if (!artifact) {
        throw new Error("No selected artifact");
      }
      return fetchArtifact(artifact, signal);
    },
    enabled: artifact?.status === "available",
    retry: false,
    staleTime: Infinity,
  });
}
function jsonFields(value: unknown, path = "$", result = new Map<string, string>()) {
  if (value !== null && typeof value === "object") {
    const entries = Object.entries(value);
    if (entries.length === 0) {
      result.set(path, JSON.stringify(value));
    }
    for (const [key, child] of entries) {
      jsonFields(child, `${path}/${key.replaceAll("~", "~0").replaceAll("/", "~1")}`, result);
    }
  } else {
    result.set(path, JSON.stringify(value));
  }
  return result;
}
export function compareText(before: string, after: string) {
  const left = [...before],
    right = [...after];
  let prefix = 0,
    suffix = 0;
  while (prefix < left.length && prefix < right.length && left[prefix] === right[prefix]) {
    prefix++;
  }
  while (
    suffix < left.length - prefix &&
    suffix < right.length - prefix &&
    left[left.length - 1 - suffix] === right[right.length - 1 - suffix]
  ) {
    suffix++;
  }
  return {
    same: before === after,
    before: left.length,
    after: right.length,
    removed: left.slice(prefix, left.length - suffix).join(""),
    added: right.slice(prefix, right.length - suffix).join(""),
  };
}
export function compareJSON(
  before: string,
  after: string,
): { path: string; before: string; after: string }[] | null {
  try {
    const left = jsonFields(JSON.parse(before)),
      right = jsonFields(JSON.parse(after));
    return [...new Set([...left.keys(), ...right.keys()])]
      .toSorted()
      .filter((key) => left.get(key) !== right.get(key))
      .map((path) => ({
        path,
        before: left.get(path) ?? "（不存在）",
        after: right.get(path) ?? "（不存在）",
      }));
  } catch {
    return null;
  }
}
export function comparisonPurpose(left?: Artifact, right?: Artifact) {
  if (!left || !right) {
    return "先選擇原輸入與輸出；缺來源保持未知。";
  }
  if (
    left.id === right.id ||
    (left.source?.sha256 && left.source.sha256 === right.source?.sha256)
  ) {
    return "載體一致性：比較同一份內容的交接，不代表 Prompt 型態轉換。";
  }
  if (left.role === "native_tool_call" && right.role === "native_tool_result") {
    return "原生呼叫 → 結果：核對呼叫參數與原結果；成功退出不等於需求驗收通過。";
  }
  return `表示轉換：${left.role} → ${right.role}。文字差異不證明語義改變或需求已被保留。`;
}

function Metadata({ artifact }: { artifact: Artifact }) {
  return (
    <dl className="lg-metadata">
      <dt>原來源</dt>
      <dd>{artifact.source?.path ?? "unknown"}</dd>
      <dt>原 SHA-256 / bytes</dt>
      <dd>
        {artifact.source?.sha256 ?? "unknown"} / {artifact.source?.bytes ?? "unknown"}
      </dd>
      <dt>形態 / role</dt>
      <dd>
        {artifact.format} / {artifact.role}
      </dd>
      <dt>authority / knowledge role</dt>
      <dd>
        {artifact.authority} / {artifact.knowledge_role}
      </dd>
      <dt>scope / status</dt>
      <dd>
        {artifact.scope} / {artifact.status}
      </dd>
    </dl>
  );
}
export function LoopGraphComparison({
  graph,
  inputs,
  outputs,
  evidence,
}: {
  graph: LoopGraph;
  inputs: string[];
  outputs: string[];
  evidence: string[];
}) {
  const [leftID, setLeftID] = useState(inputs[0] ?? evidence[0]);
  const [rightID, setRightID] = useState(outputs[0]);
  const [sync, setSync] = useState(true);
  const leftRef = useRef<HTMLPreElement>(null),
    rightRef = useRef<HTMLPreElement>(null);
  const availableInputs = [...new Set([...inputs, ...evidence])];
  const left = graph.artifacts.find((a) => a.id === leftID),
    right = graph.artifacts.find((a) => a.id === rightID);
  const l = useContent(left),
    r = useContent(right);
  const lt = l.data?.content,
    rt = r.data?.content;
  const difference = useMemo(
    () => (typeof lt === "string" && typeof rt === "string" ? compareText(lt, rt) : null),
    [lt, rt],
  );
  const fields = useMemo(
    () =>
      typeof lt === "string" &&
      typeof rt === "string" &&
      left?.format.includes("json") &&
      right?.format.includes("json")
        ? compareJSON(lt, rt)
        : null,
    [lt, rt, left?.format, right?.format],
  );
  return (
    <section aria-label="原始輸入輸出比較">
      <p aria-label="比較用途">{comparisonPurpose(left, right)}</p>
      <p className="lg-small">
        多輸入依原 artifact 分開選取；只讀取目前兩個選項。文字不同不代表語義改變、遵循或驗收通過。
      </p>
      <label className="lg-check">
        <input type="checkbox" checked={sync} onChange={(e) => setSync(e.target.checked)} />
        同步捲動
      </label>
      <div className="lg-pair">
        {[
          {
            label: "輸入 / 證據",
            choices: availableInputs,
            selected: leftID,
            choose: setLeftID,
            artifact: left,
            query: l,
            own: leftRef,
            other: rightRef,
          },
          {
            label: "輸出",
            choices: outputs,
            selected: rightID,
            choose: setRightID,
            artifact: right,
            query: r,
            own: rightRef,
            other: leftRef,
          },
        ].map((side) => (
          <div key={side.label}>
            <label>
              {side.label}
              <select value={side.selected ?? ""} onChange={(e) => side.choose(e.target.value)}>
                {side.choices.length === 0 && <option value="">未觀察到</option>}
                {side.choices.map((id) => {
                  const a = graph.artifacts.find((item) => item.id === id);
                  return (
                    <option key={id} value={id}>
                      {a?.role} · {a?.source?.path ?? id}
                    </option>
                  );
                })}
              </select>
            </label>
            {side.artifact && <Metadata artifact={side.artifact} />}
            {side.query.isError && <p role="alert">{side.query.error.message}</p>}
            {side.query.data?.error && (
              <p role="alert">
                {side.query.data.status}: {side.query.data.error}
              </p>
            )}
            <pre
              ref={side.own}
              tabIndex={0}
              aria-label={`${side.label}原文`}
              onScroll={(e) => {
                if (!sync || !side.other.current) {
                  return;
                }
                const from = e.currentTarget,
                  to = side.other.current;
                const ratio = from.scrollTop / Math.max(1, from.scrollHeight - from.clientHeight);
                const target = ratio * (to.scrollHeight - to.clientHeight);
                if (Math.abs(to.scrollTop - target) > 1) {
                  to.scrollTop = target;
                }
              }}
            >
              {side.query.data?.content ??
                (side.query.isFetching
                  ? "讀取原內容中…"
                  : "內容 unknown / 不可讀；未推論消費或成功。")}
            </pre>
          </div>
        ))}
      </div>
      {difference && (
        <>
          <p>
            {difference.same ? "完全相同" : "文字不同"} · {difference.before} → {difference.after}{" "}
            字元（{difference.after - difference.before >= 0 ? "+" : ""}
            {difference.after - difference.before}）
          </p>
          {!difference.same && (
            <details>
              <summary>文字變化範圍（移除共同前後綴，非最小 diff）</summary>
              <div className="lg-pair">
                <pre className="lg-removed">− {difference.removed || "（空）"}</pre>
                <pre className="lg-added">+ {difference.added || "（空）"}</pre>
              </div>
            </details>
          )}
        </>
      )}
      {fields && (
        <details>
          <summary>JSON 欄位變化 · {fields.length}</summary>
          {fields.map((f) => (
            <div key={f.path}>
              <code>{f.path}</code>
              <div className="lg-pair">
                <pre>− {f.before}</pre>
                <pre>+ {f.after}</pre>
              </div>
            </div>
          ))}
        </details>
      )}
    </section>
  );
}
