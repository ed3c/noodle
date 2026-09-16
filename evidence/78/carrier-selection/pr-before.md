## Current stdin / initialization receipt — 2026-09-16

The next discriminator is implemented on PR #79 candidate `e6d712a2512547cdf78a85c1ee218e1ba52e3b27`, tree `87c23adcc96b1bf10a1fd8983409cd80e3713310`. #78 remains **BLOCKED**; #79 stays draft/unmerged. This supersedes earlier candidate pointers, not their historical evidence.

The existing process owner now retains session-bound stdin expected/written bytes, write start/completion/error, close completion/error, first stdout/stderr read and first canonical init. Null means not observed. Observation persistence failure is visible. No retry/lifecycle authority, credentials, host permissions, timeout, or active external judge changed.

One real probe, session `launch-probe-20260916-093941-3c2f4a`, Codex 0.154.0-alpha.3 with existing ChatGPT login and gpt-5.6-sol:
- Expected=written **1085 bytes**. Write completed at 09:39:41.868470091Z; close completed at 09:39:41.868858520Z. Both errors empty.
- First stderr at 09:39:41.914105098Z, 29 bytes: `Reading prompt from stdin...\n`.
- First stdout=null; first init=null; Agent events=0; matching typed outcomes=0; raw output empty.
- The existing 90-second deadline cancelled the session at 90.101s. This establishes parent pipe write/close completion, not child consumption or the internal initialization cause. No unchanged rerun.

Validation: blocked/short/failed write and failed/pending close, real child stdin-to-EOF and consumer wiring, pre-init versus canonical init, cancellation with a blocked stdin, and visible persistence errors passed. Planted lost byte count, close error and init consumption failed behavioral assertions; restored dispatcher race suite passed. Canonical `pnpm check` ran once at this exact provider head: exit 0, 38.849s, clean source. Exact-head [Test](https://github.com/ed3c/noodle/actions/runs/35080572639) and [Actions worktree runtime](https://github.com/ed3c/noodle/actions/runs/35080572554) succeeded. The latter does not run a nested Agent.

Actual Noodle cleanup removed the disposable probe worktree/branch; Git readback was clean, session files archived and fixture root removed. Session completion followed the candidate's process-wait path. Independent OS process-absence readback is not claimed: a separate exec inspection did not share the probe PID namespace. The production #78 worktree remains clean and resumable.

[Complete raw receipts, observer, controls, acceptance, CI and cleanup](https://github.com/ed3c/noodle/blob/9b79c142c95916a1983834e9ce7c656f68e66a54/evidence/78/stdin-initialization.json) were published and contents read back exactly.

Next owner boundary: locate an observable Codex initialization failure after completed parent stdin delivery, or demonstrate a separately compatible carrier with changed evidence. Do not infer an API-key cause, weaken permissions, extend the deadline blindly, or repeat the unchanged probe. Original successful real Agent/typed-outcome acceptance remains outstanding; Soodles #18 receives no execution envelope or verification-skill capability upgrade from these green controls.

Refs #78.

Process dispatch previously forced a Codex approval/sandbox combination rejected by the managed carrier, and reduced its precise launch error to `no events emitted`. This candidate preserves host configuration, retains Codex's pre-event field/value diagnostic with supported help, and preserves the existing Sprite contract.

Current candidate: `1a663fc7eda778480ec858d30c90d2fac90796f2`.

The earlier API-key-only Actions gate was incorrect and has been removed. Its historical run 35055962345 checked secret presence without executing Noodle; that BLOCKED result must not be treated as a runtime failure or as a prerequisite for the ChatGPT-supervised Actions path.

The replacement workflow builds the exact candidate using `pnpm build` and executes the existing Soodles worktree control pinned to external commit `0256f2923e978b989e25df07c74db4370d343312`, with SHA-256 verification before execution. It checks real create/exec, missing-target refusal before side effects, and cleanup with Git readback. No model credential is supplied or required. Runtime and nested-Agent verdicts are separate; this receipt does not authorize landing or replace the fixed landing verifier.

Verification: exact-head canonical `pnpm check` passed (12.066s, source clean). [Noodle Actions runtime](https://github.com/ed3c/noodle/actions/runs/35056849442), attempt 1, passed all 13 transcript commands with zero residue. Receipt: `runtime=PASS`, `nested_agent=NOT_EVALUATED`, `authorizes_landing=false`. Artifact 10430243859 was published; actual receipt read from job log and metadata read back. Executed workflow bytes match the selected workflow at `ffb3466f1ef9f5d48163a39e2977d241f8ef2031`.

[Ordinary Test CI](https://github.com/ed3c/noodle/actions/runs/35056849401) failed: nested `TestProcessHandleSpawnAndDone` read empty stdout instead of `hello\n`. Separate ProcessHandle dependency [#80](https://github.com/ed3c/noodle/issues/80) preserves the evidence and required discriminator; no unchanged retry or acceptance weakening. This PR cannot land on the runtime result alone.

#78's original successful Noodle→Codex typed-outcome acceptance remains unproved. The earlier managed-carrier 90s timeout remains a separate observation; no credential cause was established. Keep this PR draft and #78 open until those original delivery requirements are met. Neither missing OPENAI_API_KEY nor the managed-carrier timeout blocks ordinary supervised Actions runtime checks.


## Dependency recovery checkpoint — 2026-09-16

Integrated delivered #80 by forward merge. Current head: 50cd7031adf1e96f8ca26a001a374af066e42f99; base: 1ba241abb68333b5c53d9d23955f538b2cb63877. Final-head canonical pnpm check, Actions Test 35077843608 and Actions runtime 35077843559 passed.

Real Codex process probe still had zero Agent events/typed outcomes before 90s cancellation. Cleanup completed. This PR remains draft/unmerged; successful nested Agent execution is still required by #78. No unchanged retry.

Raw evidence: https://github.com/ed3c/noodle/blob/7982327c073e5bdbd563c76e0e944105bc72087c/evidence/78/after-stream-recovery.json
