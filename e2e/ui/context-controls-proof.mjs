// Isolated browser transport fixture, never a production owner or admission.
import { createRequire } from "node:module";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import path from "node:path";
const repo = fileURLToPath(new URL("../../", import.meta.url));
const ui = path.join(repo, "ui");
const requireUI = createRequire(path.join(ui, "package.json"));
const { createServer } = await import(requireUI.resolve("vite"));
const { default: tailwindcss } = await import(requireUI.resolve("@tailwindcss/vite"));
const document = await readFile(path.join(repo, "server/testdata/static-preview.html"), "utf8");
const ref = { path: "/fixture/accepted.json", sha256: "a".repeat(64) };
const choice = { id: "fixture-drawing", label: "Codex 圖解（隔離驗收）", version: "a".repeat(64), composition_id: "codex-drawing", stage_id: "draw" };
const artifact = { id: "drawing", label: "drawing.html", format: "html", source: ref };
const calls = [];
let status = "prepared", sequence = 1;
function observation() {
 return { schema: 1, owner: "soodles.loop-ui", status, summary: "原任務狀態投影（隔離 fixture）", version: sequence.toString(16).padStart(64,"0"), request: ref,
 composition: { id: choice.composition_id, stage_id: choice.stage_id, definition: ref },
 identity: { order_id: "fixture-order", context_id: "fixture-context", session_id: status === "prepared" ? null : "fixture-session" },
 actions: { start: { available: status === "prepared", reason: status === "prepared" ? "prepared_original_owner" : "original_dispatch_next_required" }, stop: { available: status === "running", reason: status === "running" ? "original_task_active" : "no_active_worker" }, new_attempt: { available: false, reason: "original_review_required" }, native_resume: { available: false, reason: "native_session_resume_unsupported" } },
 candidate: ref, acceptance: { status:"PASS", scope:"integrated", record:ref, head:"fixture-accepted-head", artifacts:[artifact] }, sources:{task_envelope:ref}, effects:[] };
}
const server = await createServer({
 configFile:false, root:ui, logLevel:"warn", esbuild:{jsx:"automatic"},
 resolve:{alias:{"~":path.join(ui,"src")},dedupe:["react","react-dom"]},
 server:{host:"127.0.0.1",port:0,fs:{allow:[repo]}},
 plugins:[tailwindcss(),{name:"context-controls-proof",configureServer(server){
  server.middlewares.use("/proof",(_req,res)=>{res.setHeader("Content-Type","text/html");res.end(`<html><body><div id="root"></div><script type="module" src="/@fs/${repo}/e2e/ui/fixtures/context-controls-entry.tsx"></script></body></html>`);});
  server.middlewares.use("/api/contexts",async(req,res)=>{
   res.setHeader("Content-Type","application/json");
   if(req.method==="GET" && (req.url==="/"||req.url==="")){res.end(JSON.stringify({schema:1,choices:[choice]}));return;}
   let body="";for await(const chunk of req)body+=chunk;
   const input=JSON.parse(body);calls.push({path:req.url,input});
   if(input.id!==choice.id||input.registry_version!==choice.version){res.statusCode=409;res.end("{}");return;}
   if(req.url==="/observe"){res.end(JSON.stringify(observation()));return;}
   if(input.expected_version!==observation().version){res.statusCode=409;res.end("{}");return;}
   if(req.url==="/action"){
    if(!observation().actions[input.action]?.available){res.statusCode=409;res.end("{}");return;}
    status=input.action==="start"?"running":"stopped";sequence++;res.end(JSON.stringify(observation()));return;
   }
   if(req.url==="/preview"&&input.artifact_id===artifact.id){const o=observation();res.end(JSON.stringify({schema:1,version:o.version,artifact,candidate:o.candidate,acceptance:o.acceptance,document}));return;}
   res.statusCode=404;res.end("{}");
  });
 }}],
});
await server.listen();
console.log(JSON.stringify({url:`http://127.0.0.1:${server.httpServer.address().port}/proof`,scope:"real UI with fixture transport; Go authority controls tested separately",modelCalls:0}));
for(const signal of ["SIGINT","SIGTERM"])process.once(signal,async()=>{console.log(JSON.stringify({calls}));await server.close();process.exit(0);});
