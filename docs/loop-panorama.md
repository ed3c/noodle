# Loop panorama

The `/loops` page groups the original occurrence DAG by its unique responsibility
objects. A Workspace selects a human-named behavior goal and existing runs. This is
an in-page display selection, not admission, permission, a saved execution plan or
proof that the selected runs cover the whole goal.

The object overview preserves one object per original ID. Its relationships contain
only original handoff edges. A cycle between objects can combine different attempts
or runs; it does not turn the occurrence DAG into a cyclic execution trace. Selecting
a relationship expands its original edges and comparisons in the same page. Repo
regions retain source-index navigation. Source highlights also identify the related
overview objects. Runs without occurrences retain their unknown state and findings.

Each run has three independent evidence columns:

- Consumption requires a current handoff, observed endpoint identities, available
  evidence and payloads, and an original scoped consumer review. It covers only the
  listed edge, not every required handoff.
- Feedback continuation requires the original LoopManager-to-inner CLI edge with
  candidate, feedback, control ACK and native spawn evidence. Historical observation
  and current validity remain separate. Missing or merged artifact roles stay
  unknown; labels never substitute for role evidence. Stale evidence cannot pass.
- Whole-goal acceptance remains unknown because v2 does not supply a complete goal
  acceptance set. Worker completion and a scoped review cannot establish it.

An artifact marked `not_public` retains the original owner's observed status but
its contents cannot be reviewed in the public page. Unknown findings remain distinct
from failed findings. A later success does not erase an earlier failure.

The UI does not execute tools, schedule work or infer missing runtime edges. Cards,
notes and measurements remain original read-only projections. Measurement history
retains its scope and time window; display timing is not an automation success rate.
Adopting a new snapshot clears old occurrence focus. Comparison stays pinned until
that explicit update.

`ui/src/client/loop-panorama.mjs` is the shared, dependency-free projection and DOM
renderer. The React adapter adds current source highlights. The medium-compiler
static site vendors this module, its tests and `loop-panorama.css` unchanged. Changes
to these files must be copied together and checked for byte equality. The cloud
adapter still owns digest-checked public-content reads; the Noodle adapter still
owns native local reads. Neither adapter changes execution authority.

## Unified contract scene

Actual handoffs and expected routes share one scene and one Run selection.
Green identifies a bound, observed handoff, including an observed failed handoff.
It does not certify semantic consumption, freshness or Goal acceptance. Historical
validity and result badges remain separate. Yellow solid routes are applicable
contract expectations; yellow dashed routes are source-bound hypotheses. An
unconnected hypothesis stays in the source index. The renderer does not guess endpoints.

Each unique owner contains five principle selectors. The original Soodles
`mechanism_contract` artifact defines their source-role mapping; the separate
`state_machine_contract` artifact retains the governing prose. Geometry is shared
by classification. Related source references do not prove a principle was followed.
Missing classifications remain unknown, rather than becoming mandatory runtime steps.
The whole machine shows Goal closure and gaps. An owner shows interfaces, state and
neighbors. A principle selector exposes source references and matching occurrences.
All three keep the selected Run; clicks read data and never dispatch effects.
