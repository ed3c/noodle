// Isolated CUA proof server: no Host, gateway, credentials or model calls.
import { createRequire } from "node:module";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import path from "node:path";
const repo = fileURLToPath(new URL("../../", import.meta.url));
const ui = path.join(repo, "ui");
const requireUI = createRequire(path.join(ui, "package.json"));
const { createServer } = await import(requireUI.resolve("vite"));
const document = await readFile(path.join(repo, "server/testdata/static-preview.html"), "utf8");
const artifact = {id:"a", repository_id:"repo1", role:"prompt", format:"text/html", authority:"knowledge_data", knowledge_role:"SPEC", scope:"run1", status:"available", source:{path:"/source/a.json", sha256:"a".repeat(64),bytes:11}};
const server = await createServer({
  configFile: false, root: ui, logLevel: "warn", esbuild: { jsx: "automatic" },
  resolve: { alias: { "~": path.join(ui, "src") }, dedupe: ["react", "react-dom"] },
  server: { host: "127.0.0.1", port: 0, fs: { allow: [repo] } },
  plugins: [{ name: "isolated-preview-proof", configureServer(server) {
    server.middlewares.use("/proof", (_req, res) => {
      res.setHeader("Content-Type", "text/html");
      res.end(`<html><body><div id="root"></div><script type="module" src="/@fs/${repo}/e2e/ui/fixtures/loop-preview-entry.tsx"></script></body></html>`);
    });
    server.middlewares.use("/api/loops/artifacts/a/preview", (req, res) => {
      const query = new URL(req.url,"http://fixture").searchParams;
      if(req.method!=="GET" || query.get("ref")!==artifact.source.sha256 || query.get("version")!=="proof") {res.statusCode=409;res.end("fixture version mismatch");return;}
      res.setHeader("Content-Type","application/json");
      res.end(JSON.stringify({artifact,graph_version:"proof",acceptance:"unknown",effects:[],document}));
    });
  } }],
});
await server.listen();
console.log(JSON.stringify({url:`http://127.0.0.1:${server.httpServer.address().port}/proof`,api:"fixture transport; exact document golden-checked by real Go API test",modelCalls:0}));
for(const signal of ["SIGINT","SIGTERM"]) process.once(signal,async()=>{await server.close();process.exit(0);});
