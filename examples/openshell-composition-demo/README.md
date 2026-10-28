# Fixed two-stage OpenShell demonstration

These are task materials, not a new scheduler or gateway launcher. They make one existing cross-harness workflow exercise multi-skill composition, distinct per-task network policies, and explicit retry from saved guest progress. Preparation and the included offline tests do not establish that any live model, VM, policy or acceptance check passed.

## Frozen inputs and task definitions

Register these two complete skill directories from an exact repository commit, including their `scripts/` files. Keep the existing pinned dimensional-drawing source and its license unchanged. Use only the previously published training case, supplied as an exact materialized `case.json` reference. No held-out cases are part of this demo or these scripts.

| Stage | Selected skill | Supporting skill | Fixed harness | Deliverables |
| --- | --- | --- | --- | --- |
| A: component | `component-author` | `network-proof` | Codex subscription | `component.html`, `component.partial.html`, `progress-marker.json`, `network-proof-component.intent.json`, `network-proof-component.json` |
| B: drawing | Existing pinned `dimensional-drawing` | `network-proof` | Pi + OpenRouter, fixed DeepSeek model from the original task | `drawing.html`, `network-proof-drawing.intent.json`, `network-proof-drawing.json` |

The original task write scopes must include those exact stage-specific outputs. Do not widen them dynamically after stopping. A and B use distinct, immutable route specs sharing the same gateway identity. The fixed probe executable is `/sandbox/payload/node/bin/node`; A allows `example.com:443` GET and denies `example.org`, while B reverses the hosts. Existing inference routes remain unchanged. The policies are created by the original owner; the skills cannot change them.

## A: planned interruption and saved-progress retry

Before A's first model dispatch, fix the full task definition to include the checkpoint sequence in `component-author/SKILL.md`. The model writes a real partial component, labeled incomplete, then invokes the bundled checkpoint with the exact public case path. The helper durably records the partial/case hashes, announces `checkpoint_wait_started`, and waits at most 180 seconds. This is the observation window for the original Session stop operation. Do not kill the VM directly.

The original Runtime must drain the owned model, save both repository and session archives, verify return/publication, then record native Stop ACK and a fresh Stopped read. The partial and marker must be present in the saved `ProgressHead`. Inspect that exact original session, then explicitly request a version-bound **new attempt** through the original Host. Do not rewrite the goal, fabricate acceptance or resume the scheduler to simulate a new attempt.

`NOODLE_SESSION_DIR` is set by `OpenShellRuntime` from `RemoteHarness.GuestSessionDir`, which is `/sandbox/noodle-sessions/<generated-session-id>`. The helper checks that the new attempt has a different value and observes the unchanged case/partial hashes. This is only a demo invariant, not an independent identity attestation or authority to restart. Original Host/custody receipts remain authoritative. A same-session rerun is refused. An elapsed wait returns 75 and explicitly says native stop was not proved; it cannot be treated as a successful interruption test.

After explicit retry, complete `component.html` from the unchanged case and run the network proof once. Preserve the partial/marker. Accept A only after the final component passes the original complete browser/geometry/content checks and the candidate is accepted by its original owner. An incomplete partial component never satisfies A.

## B: exact accepted handoff and multiple skills

Only the original accepted A commit and exact declared artifact refs may drive the predeclared B handoff. Read that accepted component and the same public case through the existing materialization mapping. Apply the complete pinned dimensional-drawing skill, deliver a self-contained static SVG/CSS `drawing.html`, then invoke the supporting network skill with stage `drawing` once. Generation may use JavaScript as a tool; delivered HTML must contain no script, fetch, external assets, forms or navigation. Do not embed a network probe in the visual output.

The original acceptance process must independently check the drawing's values, unknown labels, region correspondence, static-preview compatibility and actual rendered output. The original workflow graph must show the fixed selection and the real accepted stage handoff. These materials do not authorize `/loops` writes or mark a graph node accepted.

## Network observations and independent evidence

The fixed Node script issues at most one GET `/` per host: no query/body/credentials, redirect following or retries, a 10-second absolute deadline per request, and 64 KiB maximum response body. It writes an exclusive intent before the first request. An unfinished intent makes a later invocation refuse, retaining ambiguity instead of reissuing an unknown operation. A completed report can be reread without traffic.

`UNKNOWN` is preserved as an observation and does not prevent report generation. Reset, timeout, DNS/TLS failure, or HTTP status alone cannot prove policy denial. The original supervisor must correlate the report with each context's native Create policy and native network-denial evidence. A JSON artifact by itself does not establish R09.

## Offline verification

Run `node --test examples/openshell-composition-demo/demo.test.mjs` using the repository's supported Node runtime. Tests inject transport and the wait function: no internet, gateway, credentials, real model or 180-second pause is used. They cover fixed GET shape, no redirect follow, response bounds, UNKNOWN classification, non-replay after an interrupted intent, and preservation across distinct simulated session IDs. Native end-to-end verification is separate.

The component specification wording derives from the already-public `diagram-workflow-20261028/component-author/SKILL.md` preparation. Host choices and limits match the existing `openshell-progress-deployment/network-policy-demo-plan.json`. New scripts and text use this repository's MIT license. Neither upstream dimensional-drawing nor Pele source is copied or repinned here.
