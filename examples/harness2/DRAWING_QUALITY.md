# Executable drawing quality checks

This is fixed evaluation infrastructure, outside the two editable instruction files.
It does not optimize a harness, call a model, repair a drawing, or adopt a candidate.
Use the repository-declared pnpm version and existing `e2e/ui` Chromium installation.

```sh
pnpm check:drawing-quality
pnpm quality:drawing /absolute/drawing.html /absolute/new-evidence-directory /absolute/source-facts.json
```

The output directory must not already exist. Exit 0 means PASS, 1 means a known
FAIL, and 2 means UNKNOWN. Neither FAIL nor UNKNOWN may pass the delivery gate;
manual overrides are excluded. The last stdout line identifies the immutable
report and SHA-256. Screenshots, rendered HTML, source facts, engine file hashes,
environment, finding locations and glyph pixel counts accompany the report.
An interrupted invocation retains its partial directory; do not overwrite it.

## Accepted scope (2026-10-29)

- Inspect the entire static HTML and inline SVG: header, drawing, tables and footer.
- Reconcile dimension uses against table definitions in both directions, including
  duplicates, missing definitions, values, units, independently sourced values,
  and calculable formula equalities. Summary mentions are uses, not definitions.
- Measure text collisions using actual glyph ink after rectangle broad-phase.
  Detect drawing ink obscuring text, including partially covering lines; normal
  table borders, background fills and effective white knockouts must not fail.
- Detect clipped glyphs and drawing ink. Accessible horizontal scrolling is valid;
  `overflow:hidden` is clipping, not a scrolling exemption.
- Check 1800×1200 desktop and 390×844 narrow viewports, device scale 1, fixed Arial
  font stack, and completed font loading. These exact conditions are reported.
- Preserve author-generated SVG, serialize the rendered DOM, and perform measurement
  in a separate context with author JavaScript disabled. A changed snapshot blocks.
  External dependencies block; unsupported paint or ambiguous structure stays UNKNOWN.
- Validate the checker against positive and negative controls and retained disclosed
  failures before using it for optimization feedback. Engineered controls are not
  new unseen benchmark successes or population-level evidence.
- Do not repair or retry submitted formal test artifacts. Training feedback may
  inform instruction changes; test reports must never flow to the optimizer.
- Keep disclosed cases as regressions. Re-optimize Pi and Codex, freeze their exact
  candidates and the common checker, then evaluate two new paired cases per group.
  New cases require new layouts and dimension relationships, not number substitution.
- Candidate success requires all quality gates to pass and no dimension regression.
  Repairing a baseline failure counts as quality improvement. Compare efficiency
  only where both outputs satisfy quality. Old experiment decisions remain immutable.
- Use independent new execution records and isolation; retain the old faulted Host
  and evidence. Shared resource admission still applies. Total authorized Pi spending
  remains US$5 including prior spending; obtain fresh original billing before calls.
  Subscription USD is unknown when unattributable. Four new pairs are a small study.

## Source facts

The independent evaluator supplies a JSON object, never worker-authored metadata:

```json
{"values":{"S1":"14 px","H3":"36 px","V1":null},"defaultUnit":"px"}
```

Codes must correspond to independently established source facts for that case.
Literal numeric definitions without source evidence block with UNKNOWN even if
the page is internally consistent. A derived value can be checked through
source-backed leaf values. A legitimate `[TBD]` is allowed when the source has
no known value; it cannot replace a known source value. The checker verifies
explicit arithmetic, not the semantic adequacy of an arbitrary design formula.
Source preparation must establish those design relationships independently.

The facts parser never evaluates JavaScript. It supports restricted arithmetic,
explicit units and ranges. Ambiguous/correlated ranges, contextual symbols,
unsupported tables or mixed relative-unit context remain UNKNOWN. SVG table
extraction follows actual ruled Code/Variable/Value cells and transforms.
The detailed supported grammar and limitations live with `drawing-facts.mjs`.

## Visual measurement and its limits

Text rectangles select possible collisions; solid glyph pixels decide them.
Isolated clipped and unclipped glyph renders measure clipping. Comparing the
original render against the same render with one text fill removed measures
its actual contribution, retaining its real white stroke knockout. Loss of
contribution identifies occlusion rather than treating all background shapes
as obstacles. Risky SVG shapes receive independent clipped/unclipped ink checks.

The current measurement profile uses black-ink coverage ≥128 for solid glyph pixels, RGB
delta ≥8 for measurable contribution, and a minimum contribution ratio of 0.8.
Drawing clipping compares alpha ≥8 on transparent renders, so pale/white ink is
covered as well. Risky shapes without measurable ink remain UNKNOWN.
These are explicit detector parameters exercised by controls, not calibrated
claims about all human readability. Negative capture coordinates, unsupported
composition, mixed parent text, unreadable embedded content and layout-changing
measurement are UNKNOWN. No bitmap/OCR or PDF/print coverage is claimed.

Keep checker controls, disclosed regression cases, optimization training data,
and sealed final tests separate. A known failure takes precedence over unknown
checks. Full original owner/input/geometry/portability/independence evidence is
still required; this checker does not turn a receipt or process completion into
complete benchmark acceptance.

## Explicit training feedback

Each experiment group may opt into `training_feedback` with the exact fields:

```text
scope: training
node: FileRef of the executable
engine: FileRefs of drawing-quality.mjs, drawing-visual.mjs, drawing-facts.mjs (in order)
timeout_s: finite positive timeout
allowlist:
  - task_id: the explicit training task
    source: its frozen TreeRef
    source_files: independent input FileRefs within that tree
    source_facts: independent evaluator FileRef outside editable instructions
```

Both groups must opt in together and use the same Node, checker and timeout.
Only settled original runs with exactly matching `drawing.html` bytes receive
feedback. The adapter appends bounded findings, text locations and a report
reference to the proposer trajectory, without changing owner receipts, native
scores or original artifacts. Oracle expectations are not projected.
Formal unseen tasks are not enrolled. A completed quality UNKNOWN blocks quality
acceptance; an interrupted or invalid checker execution additionally blocks
further dispatch and replay until that execution is resolved.
