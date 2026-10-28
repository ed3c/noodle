# Fixed composition controls

The existing Host serves `/contexts` alongside the read-only `/loops` view. A Session prepares a task with Soodles `loop_prepare` and a fixed composition catalog, then registers that **exact** request with the already running original Host:

```text
noodle host contexts register --file /absolute/path/to/registration.json
```

Registration does not attach a context or dispatch a model. It does not start a Host. Its configuration binds an ID and label, the prepared request FileRef, original Host identity, and a fixed owner source bundle. Browser requests contain only the registered ID, registry version, original observation version and one named action. Paths, prompts, environment, provider/model settings, and command arguments cannot be supplied by the browser. An existing ID cannot be rebound to different sources; prepare a new registration when its inputs change.

## Trusted registration

`registration.json` has `schema: 1`, `id`, `label`, `request: {path, sha256}`, `original_host: {instance_id, binary_sha256, host_dir}`, and `launcher`:

- `python`: exact canonical interpreter FileRef; executable on this machine.
- `entry`: exact canonical `runtime/soodles.py` FileRef in the prepared owner source bundle.
- `manifest`: immutable FileRef using the existing supervisor runtime manifest shape: `schema: 1`, `runtime: [{path: "runtime/<relative-file>", sha256}]`. Every declared file is verified. The entry must be declared; symlinks, unlisted Python/native extension sources and cached Python bytecode are rejected. This is a source-bundle verification contract, not a fabricated supervisor or Issue admission.
- `environment`: explicit `HOME`, `PATH`, `NOODLE_HOST_DIR`; optional `LANG`, `LC_ALL`, `LC_CTYPE`. Other keys, including credentials and Python import overrides, are refused. `NOODLE_HOST_DIR` must identify the original Host.

Prepare the complete Soodles dependency closure from one reviewed revision, including `loop_ui.py`, rather than copying only `soodles.py`. Retain its original licenses and source pin alongside the bundle. Sources remain immutable while registered. The trusted Session is responsible for selecting the actual interpreter and complete owner bundle; registration does not turn arbitrary third-party code into a trusted owner.

The Host registry stores configuration only. It has no task execution checkpoint and does not replace Soodles intents, review decisions or Noodle lifecycle custody. A Host restart requires a new explicit registration for the new owner; old registrations are not adopted automatically.

## Actions and uncertain outcomes

`GET /api/contexts` returns fixed labels and composition identities. Explicit `POST /api/contexts/observe` asks `manager-request loop_ui_inspect` for a bounded original-owner projection; that inspection can save original evidence, so it is not represented as a read-only GET. Actions call `manager-request loop_ui_action` with the selected original request and expected observation version.

For a fixed composition, the projection keeps top-level `request` and `composition` bound to the registered entry. Required `active_stage` contains `{request: FileRef, composition: {definition: FileRef, id, stage_id}, handoffs: FileRef[]}`. Its catalog and composition ID match the entry; no handoffs means the same entry request/stage. A later stage has a distinct request/stage and ordered, nonrepeated handoff references. Go and the browser validate this shape, while the original Soodles owner verifies the complete accepted chain. Neither consumer reads these paths to choose another owner or derive an action. Identity, actions, candidate and acceptance describe the current stage. The page shows that stage and preserves entry/hand-off sources in its diagnostic details. Browser actions still contain only the registered choice ID, registry version, expected observation version and action.

The original owner decides whether `start`, `stop` or `new_attempt` is available. Stop uses the original task interruption contract, not Swap. A new attempt requires the original owner’s legal, saved review continuation and resolved previous effects. The page cannot issue generic requeue, scheduler resume, arbitrary control commands or review approval. Native session resume is explicitly unsupported.

First admission requires `host attach --require-existing-instance EXACT`. The client never starts a replacement when that socket is missing; the receiving Host checks its own instance before admission. The condition is saved in the original request binding and cannot be removed by omitting it on a retry. Pre/post liveness probes alone are not an atomic admission guard.

Owner operations run outside the Host actor queue so their existing status/attach calls cannot deadlock it. Browser disconnection does not interrupt the owner subprocess; it has a bounded two-minute execution window. Process failure, timeout, source drift or changed Host means the result is unconfirmed, with no automatic action retry. Private process diagnostics retain bounded stdout (256 KiB), stderr (64 KiB), truncation flags, exit/timeout and exact source refs in `context-control-diagnostics/` under the Host directory (directory 0700, files 0600, durably written). The browser receives only a diagnostic identifier. Failed diagnostic persistence is explicitly reported; these files are evidence, never a task journal. The UI clears prior enabled controls and requires explicit observation. The original owner remains responsible for effects and acknowledgement recovery.

Control POSTs require exact same-origin Host/Origin and JSON, reject extra fields/multiple documents, and have bounded request/output sizes. No credential environment or full owner command report is forwarded to the browser. Local trusted CLI configuration remains an authority boundary.

## Accepted output

The page displays the exact request, candidate and integrated acceptance record. An artifact list is available only from the original owner’s verified integrated PASS record bound to that candidate, task envelope and accepted HEAD. A node’s status text is insufficient.

Opening HTML calls the original owner again, requires the same observation version, selects the exact accepted artifact ID, verifies its content hash, and uses the existing static HTML/SVG sanitizer and an iframe with **no** sandbox privileges. Active content and external resources remain unavailable. Other artifacts retain their original source references. `/loops` general registered previews still report acceptance unknown; this page does not promote them by association.

## Verification scope

Focused Go tests cover cross-origin/non-JSON/extra-argument requests, source drift, stale registration, wrong original request, missing/foreign/contradictory active stages, accepted artifact drift and missing original Host without replacement. UI tests cover explicit selection/observation, no automatic retry after unknown outcome, new-selection isolation, current-stage display with root-bound controls, and exact accepted preview binding. These are isolated controls, not claims of model-generated or accepted diagrams; production acceptance remains with the original Soodles task.
