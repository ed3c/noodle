import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { LoopArtifactPreview } from "../../../ui/src/components/LoopArtifactPreview";
import { LoopPeleGraph } from "../../../ui/src/components/LoopPeleGraph";
import { graphFixture, required } from "../../../ui/src/client/loop-graph-fixture";

const { graph } = graphFixture();
const node = required(graph.nodes[0]);
node.label = `original \"] --> injected[\" <img src=https://preview-blocked.invalid onerror=alert(1)>`;
const artifact = { ...required(graph.artifacts[0]), format: "text/html" };
createRoot(document.getElementById("root")!).render(
  <QueryClientProvider client={new QueryClient()}>
    <h1>唯讀圖解預覽驗收</h1>
    <LoopArtifactPreview artifact={artifact} version="proof" />
    <LoopPeleGraph graph={graph} runID={node.run_id} />
  </QueryClientProvider>,
);

// The browser harness returns the API document golden-checked by the Go test.
window.dispatchEvent(new CustomEvent("proof-artifact", { detail: artifact }));
