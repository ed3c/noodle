import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { Artifact } from "~/client/loop-graph";
import { fetchStaticPreview } from "~/client/loop-preview";

export function LoopArtifactPreview({
  artifact,
  version,
}: {
  artifact: Artifact;
  version: string;
}) {
  const [open, setOpen] = useState(false);
  const query = useQuery({
    queryKey: ["loop-static-preview", artifact.id, artifact.source?.sha256, version],
    queryFn: ({ signal }) => fetchStaticPreview(artifact, version, signal),
    enabled: open && artifact.status === "available",
    retry: false,
    staleTime: 0,
  });
  return (
    <section aria-label="固定來源靜態預覽">
      <h3>HTML 靜態預覽</h3>
      <p>顯示已登記的固定版本；驗收狀態未知，預覽不授權後續操作。腳本與外部資源不會執行。</p>
      <button type="button" onClick={() => setOpen(!open)}>
        {open ? "關閉預覽" : "開啟靜態預覽"}
      </button>
      {open && query.isFetching && <p role="status">核對來源版本…</p>}
      {open && query.isError && <p role="alert">{query.error.message}</p>}
      {open && !query.isFetching && !query.isError && query.data && (
        <iframe
          title="HTML 成果靜態預覽"
          sandbox=""
          referrerPolicy="no-referrer"
          srcDoc={query.data}
          style={{
            width: "100%",
            height: 640,
            border: "1px solid currentColor",
            background: "white",
          }}
        />
      )}
    </section>
  );
}
