import { useEffect, useRef } from "react";
import { mountPanorama } from "~/client/loop-panorama.mjs";
import type { PanoramaSelection } from "~/client/loop-panorama.mjs";
import "./loop-panorama.css";

export function LoopPanorama(
  props: Parameters<typeof mountPanorama>[1] & {
    highlightedObjects: string[];
    highlightedNodes: string[];
    selection: PanoramaSelection | null;
  },
) {
  const root = useRef<HTMLDivElement>(null);
  const callbacks = useRef(props);
  const controller = useRef<ReturnType<typeof mountPanorama> | null>(null);
  useEffect(() => {
    callbacks.current = props;
  }, [props]);
  const { graph, runs, runIds, goal } = props;
  const canReview = Boolean(props.onReviewSelection);
  useEffect(() => {
    if (root.current) {
      controller.current = mountPanorama(root.current, {
        graph,
        runs,
        runIds,
        goal,
        onReviewSelection: canReview
          ? (value) => {
              const submit = callbacks.current.onReviewSelection;
              if (!submit) {
                return Promise.reject(new Error("Local review selection unavailable"));
              }
              return submit(value);
            }
          : undefined,
        readArtifact: (id) => callbacks.current.readArtifact?.(id) ?? Promise.resolve(null),
        onReset: () => callbacks.current.onReset?.(),
        onScope: (value) => callbacks.current.onScope(value),
        onSelect: (value) => callbacks.current.onSelect(value),
        onFocus: (value) => callbacks.current.onFocus(value),
      });
    }
  }, [graph, runs, runIds, goal, canReview]);
  useEffect(() => {
    controller.current?.setSelection(props.selection);
  }, [props.selection, graph, runs, runIds, goal]);
  useEffect(() => {
    for (const node of root.current?.querySelectorAll<SVGElement>("[data-panorama-object]") ?? []) {
      node.classList.toggle(
        "is-linked",
        props.highlightedObjects.includes(node.dataset.panoramaObject ?? ""),
      );
    }
    for (const node of root.current?.querySelectorAll<SVGElement>("[data-detail-node]") ?? []) {
      node.classList.toggle(
        "is-linked",
        props.highlightedNodes.includes(node.dataset.detailNode ?? ""),
      );
    }
  }, [props.highlightedObjects, props.highlightedNodes, graph, runs, runIds, goal]);
  return <div ref={root} aria-label="Workspace 全景圖" />;
}
