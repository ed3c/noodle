import { useMemo, useState } from "react";
import type { LoopGraph } from "~/client/loop-graph";
import { renderPeleProjection } from "~/client/pele-projection";

export function LoopPeleGraph({ graph, runID }: { graph: LoopGraph; runID: string }) {
  const [fit, setFit] = useState(false);
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
        <>
          <button type="button" aria-pressed={fit} onClick={() => setFit(!fit)}>
            {fit ? "原尺寸閱讀" : "顯示全圖"}
          </button>
          <p>{fit ? "全圖總覽；切回原尺寸可閱讀標籤。" : "原尺寸顯示，可左右及上下捲動閱讀。"}</p>
          <div
            className="lg-canvas-scroll lg-pele"
            data-fit={fit}
            role="img"
            aria-label="原任務與成果關係"
            tabIndex={0}
            dangerouslySetInnerHTML={{ __html: result.svg }}
          />
        </>
      )}
    </section>
  );
}
