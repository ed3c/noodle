import { useEffect, useRef } from "react";
import { mountPanorama } from "~/client/loop-panorama.mjs";
import "./loop-panorama.css";

export function LoopPanorama(
  props: Parameters<typeof mountPanorama>[1] & { highlightedObjects: string[] },
) {
  const root = useRef<HTMLDivElement>(null);
  const callbacks = useRef(props);
  useEffect(() => {
    callbacks.current = props;
  }, [props]);
  const { graph, runs, runIds, goal } = props;
  useEffect(() => {
    if (root.current)
      mountPanorama(root.current, {
        graph,
        runs,
        runIds,
        goal,
        onScope: (value) => callbacks.current.onScope(value),
        onSelect: (value) => callbacks.current.onSelect(value),
        onFocus: (value) => callbacks.current.onFocus(value),
      });
  }, [graph, runs, runIds, goal]);
  useEffect(() => {
    root.current?.querySelectorAll<SVGElement>("[data-panorama-object]").forEach((node) => {
      node.classList.toggle(
        "is-linked",
        props.highlightedObjects.includes(node.dataset.panoramaObject ?? ""),
      );
    });
  }, [props.highlightedObjects, graph, runs, runIds, goal]);
  return <div ref={root} aria-label="Workspace 全景圖" />;
}
