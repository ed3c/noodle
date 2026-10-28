import { parseArtifact } from "./loop-graph";
import type { Artifact } from "./loop-graph";

export async function fetchStaticPreview(
  artifact: Artifact,
  version: string,
  signal?: AbortSignal,
) {
  if (!artifact.source?.sha256 || artifact.format !== "text/html") {
    throw new Error("固定 HTML 來源版本不存在");
  }
  const query = new URLSearchParams({ ref: artifact.source.sha256, version });
  const response = await fetch(
    `/api/loops/artifacts/${encodeURIComponent(artifact.id)}/preview?${query}`,
    { signal },
  );
  if (!response.ok) {
    throw new Error(`無法預覽：${await response.text()}`);
  }
  const raw = await response.json();
  const actual = parseArtifact(raw.artifact);
  if (
    actual.id !== artifact.id ||
    actual.format !== artifact.format ||
    actual.source?.path !== artifact.source.path ||
    actual.source?.sha256 !== artifact.source.sha256 ||
    actual.source?.bytes !== artifact.source.bytes ||
    raw.graph_version !== version ||
    raw.acceptance !== "unknown" ||
    !Array.isArray(raw.effects) ||
    raw.effects.length > 0 ||
    typeof raw.document !== "string"
  ) {
    throw new Error("預覽來源或唯讀契約已改變");
  }
  return raw.document as string;
}
