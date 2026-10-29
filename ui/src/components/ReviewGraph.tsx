import { useEffect, useRef } from "react";
import { mountReviewGraph } from "~/client/review-graph.mjs";
import "./review-graph.css";

export function ReviewGraph({ content }: { content: string }) {
  const root = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const element = root.current;
    if (!element) {
      return;
    }
    try {
      return mountReviewGraph(element, content);
    } catch (error) {
      element.textContent = error instanceof Error ? error.message : "圖形資料無法讀取。";
      return () => element.replaceChildren();
    }
  }, [content]);
  return <div ref={root} />;
}
