import { useMemo } from "react";
import type { LoopGraph } from "~/client/loop-graph";
import { renderPeleProjection } from "~/client/pele-projection";

export function LoopPeleGraph({ graph, runID }: { graph: LoopGraph; runID: string }) {
  const result = useMemo(() => {
    try {
      return { svg: renderPeleProjection(graph, runID), error: null };
    } catch (error) {
      return { svg: null, error: error instanceof Error ? error.message : String(error) };
    }
  }, [graph, runID]);
  return (
    <section aria-label="Pele 任務關係圖">
      <h3>Pele 任務關係圖</h3>
      <p>沿原 owner 的任務與成果關係顯示；unknown 保留，顯示圖形不代表驗收通過。</p>
      {result.error && <p role="status">{result.error}</p>}
      {result.svg && (
        <div
          role="img"
          aria-label="原任務與成果關係"
          dangerouslySetInnerHTML={{ __html: result.svg }}
        />
      )}
    </section>
  );
}
