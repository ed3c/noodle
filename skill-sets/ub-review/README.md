# Optional ub-mode product review set

This set is selected only for final candidate review. It does not change
Noodle's default `.noodle.toml`, global Codex discovery, Poteto, or outer-loop
instructions. `skills/ub-review` is the selected entry; the upstream ub-mode
files inside it remain unchanged. There is no `schedule:` trigger.

The original review admission selects this set's absolute `skills` directory
through Noodle's existing skill resolver and selects `ub-review` by name.
Soodles local tasks bind it through their existing selected Skill/source-input
contract. Do not edit a running worker's configuration or frozen input.
This is a provisioned optional set, not a new global runtime mode or scheduler.

`sources.lock.json` pins all five upstream sources. `tools/` holds their actual
checkouts as siblings, including the Crapper sibling needed by Mutator.
`runtime/` holds local dependency installation. Both directories are ignored;
the lock and skill entry are versioned. They must be provisioned on the selected
Host/guest before execution. Host macOS executables are not Linux guest tools.

The three Python tools support Python >=3.11 and use their own `.venv` as their
upstream launchers expect. Provision their declared dependencies before review,
then inspect their help without running project analysis. Java >=21 and Clojure
CLI support UML Viewer. Use the original `:ir` generator for source artifacts;
the desktop GUI needs its own display and Quil dependencies. Neither a source
checkout nor `--help` proves quality analysis, GUI execution or OpenShell use.

Preserve the original CLI/parser outputs. Test Manager supplies the selected
test/coverage commands and concurrency budget. Do not invoke default analysis
while provisioning. The Neo integration retains the original desktop and local
Grok CLI companion. Use `scripts/uml-companion.py` as the Viewer's `GROK_BIN`
in a dedicated diagram workspace. It replaces the upstream source-scanning and
test-running prompt with `skills/ub-review/companion-rules.md`. It does not edit
the vendored Viewer. Product work remains with the original Noodle OpenShell
role. Do not run the upstream project installer inside a product repository.

The local companion uses the pinned executable at `runtime/grok/grok-1.0.70`
and a separate `GROK_HOME`. The macOS launcher applies kernel read/write limits
before Grok starts and has no unrestricted fallback. Its `--probe` checks
denied source access, allowed proposal writes and executable startup. These
installation checks do not establish candidate review or mailbox recovery.
The selected directory must have `inputs/review-context.json`; inputs are
read-only, while `.uml-viewer/`, `proposals/` and the private Grok home are writable.
The upstream CLI installer also modifies shell/global paths, so this set uses
the official versioned binary directly and leaves normal skill discovery alone.

`pnpm build:review-graph` builds the optional Go analyzer. Invoke it with an
explicit repository and selected package patterns. It reads compiler inputs
and exports source hashes, structure, direct calls and unresolved targets as
JSON. `scripts/review-snapshot.py` projects that JSON to the existing desktop
EDN format. It aggregates calls by file for display; the JSON retains individual
functions and call sites. It adds no acceptance or metrics, and does not claim
an upstream Go scanner. Guest execution and consumer display remain separate
acceptance obligations.

Use the lock to obtain the same upstream revisions; refuse mismatched or dirty
tracked source rather than updating it silently. Dependency package versions
and local capability observations belong in `install-readback.json`. That local
receipt is installation evidence, not a product PASS or a shared admission.

For missing review-role, guest transfer, dependency or recovery capability,
retain the original refusal/NOT_RUN. NeoWeaver must not fall back to process.
The original Soodles candidate owner continues to own acceptance and correction;
this package alone does not prove automatic native review handoff.

The prompt-only bridge accepts `uml-companion.py --turn-file CONTEXT.json`.
Soodles' original candidate owner supplies that context and its source bindings.
`--read-mailbox` reads the unchanged Viewer's EDN queue through its original
Clojure library. Grok receives the queue data in its prompt and cannot invent IDs.
`companion_turn.py` journals one native CLI request before launch. An interrupted
intent remains unknown; the same context never silently starts a second model.
The structured response contains requests, not executable commands. Soodles
binds its Session assessment and calls the original candidate review or role
handoff. This adds no background worker or scheduler. A real Noodle Host and
candidate are still required for product correction, metrics and re-review.
