## Current scope and physically verified execution boundaries — 2026-09-16

Supervisor clarification: **the remaining unproven capability in #78 is Noodle launching and completing a separate Agent through the local Codex CLI process-dispatch path in the tested Work Mode execution environment.** Here “local” means the session's managed execution environment running `/opt/codex/bin/codex`, not an experiment on the user's personal computer. This blocker must not be generalized to ChatGPT-supervised GitHub Actions, all Codex carriers, or all Noodle execution.

| Path | Physically observed result | Evidence ceiling |
|---|---|---|
| ChatGPT Supervisor → GitHub connector → repository/Actions → result readback | GitHub operations and Actions result retrieval work in this session | Does not transfer this conversation's model runtime, full context, or Codex login into the runner; no exact model-token accounting was measured |
| GitHub Actions → Noodle worktree runtime | PASS: candidate build, worktree create/exec, missing-target refusal before effects, cleanup and zero residue | Runtime verification only; no nested Codex Agent or autonomous Issue delivery was exercised |
| Local Noodle ProcessDispatcher → Codex CLI child | Process launch, complete parent stdin write/close and child internal config/plugin initialization observed; then 90-second cancellation | Successful Agent tool execution and matching session/order/stage-bound typed outcome remain unproven on this local path |

### Exact Actions evidence already obtained

Candidate `e6d712a2512547cdf78a85c1ee218e1ba52e3b27`; [run 35080572554](https://github.com/ed3c/noodle/actions/runs/35080572554), attempt 1, job `104743319240`, workflow SHA `ca2bd0d14468b2586ba096363e13fdd1896fcb92`. The workflow is triggered by `pull_request`; its filename is historical and does not imply a Codex launch.

[Exact candidate workflow](https://github.com/ed3c/noodle/blob/e6d712a2512547cdf78a85c1ee218e1ba52e3b27/.github/workflows/codex-actions-capability.yml) builds Noodle through `pnpm build`, invokes the fixed external Soodles `worktree_probe`, and publishes a receipt. The actual job log reports:
- `carrier=github-actions`
- `scope=Noodle worktree create/exec/refusal/cleanup`
- `runtime=PASS`
- `nested_agent=NOT_EVALUATED`
- `typed_outcome_observed=false`
- `authorizes_landing=false`
- `local_carrier_verdict=NOT_EVALUATED`

No model credential was supplied or required by this runtime control; it did not execute `codex exec` or a nested model Agent. ChatGPT supplies the supervising decisions outside the runner; Actions executes the checked-in workflow and returns runtime evidence. Model input/output tokens, GitHub connector authorization, the job's `GITHUB_TOKEN`, and local Codex login credentials are different things. This is an execution-boundary finding, not a measured token-usage claim.

### Exact remaining local scope

[Latest local child-initialization evidence](https://github.com/ed3c/noodle/blob/aa5de0cab604f58ae11905fed5ae65740e667c4e/evidence/78/child-initialization.json): Codex `0.154.0-alpha.3`, existing ChatGPT login, `gpt-5.6-sol`, actual Noodle ProcessDispatcher, session `launch-probe-20260916-113923-8a7cb2`. Parent stdin wrote 1085/1085 bytes and closed without error; these are bytes, not model tokens. Child emitted internal initialization logs, but no stdout/canonical init/Agent events/matching typed outcome before cancellation at 90.094847375s.

The featured-plugin HTTP 401 was emitted by this local Codex child. It does not establish GitHub connector failure, Actions authentication failure, or failure of the model endpoint. Background plugin warmup only warns on failure; its causal role in the stall remains unproven.

**Remaining #78 acceptance:** on this local Codex CLI process path, the Agent itself must execute the requested tool and produce the existing session/order/stage-bound typed outcome, which the observer reads back, followed by the original verification/delivery/reconciliation gates. Do not substitute direct CLI success, fixture output, canonical init alone, or Actions runtime PASS. No API-key gate, permission weakening, unchanged retry, or blind timeout extension.

#78 remains OPEN/BLOCKED for this local capability; PR #79 remains draft/unmerged. A nested Agent inside Actions is NOT_EVALUATED and is outside this clarified remaining #78 scope, not an additional required experiment. #78 is not a blanket prerequisite for the already demonstrated ChatGPT-supervised Actions runtime route. Soodles #18's existing combined automatic/supervised acceptance is not silently changed by this documentation correction; no fresh #18 admission or completion is claimed here.

The dated checkpoints below preserve historical inputs/results. Read their broad “Agent execution” or dependency wording through this current local-only scope; superseded cloud API-key gating remains withdrawn.

## Current child initialization discriminator — 2026-09-16

Supervisor requested child prompt-consumption or subsequent initialization evidence. Ran one actual Noodle ProcessDispatcher probe on unchanged candidate `e6d712a2512547cdf78a85c1ee218e1ba52e3b27`, with existing Codex 0.154.0-alpha.3, ChatGPT login, gpt-5.6-sol, identical 1085-byte composed prompt and 90-second deadline. Only the dispatched child's module-filtered native diagnostic level changed: `RUST_LOG=warn,codex_exec=info,codex_core::config=debug,codex_config=debug`. No production source, model, permission, credential, active judge or completion protocol changed.

A prior no-model `strace /bin/true` control failed with `PTRACE_TRACEME: Operation not permitted`; no syscall tracing of Codex was attempted and no restriction was bypassed.

Session `launch-probe-20260916-113923-8a7cb2`:
- Parent stdin write 1085/1085 and close completed without errors.
- Child emitted config-loader DEBUG, unknown-feature WARN, and plugin-cache initialization WARN after the initial stderr prompt.
- Featured-plugin request returned HTTP 401 with a token-parse diagnostic. This is a plugin endpoint result, not a model endpoint verdict.
- Same-version upstream source orders stdin read_to_end/decode before in-process app-server startup/plugin warmup, supporting prompt-read completion by source-path inference. Direct EOF syscall observation and Agent comprehension are NOT claimed. Installed binary version/hash are recorded; reproducible-build source identity was not independently verified.
- Featured-plugin warmup is a background spawned task that logs failure; its 401 is NOT proven to block startup. Do not infer API-key need or change credentials from this warning.
- No stdout/canonical init/Agent events/matching typed outcome. Deadline cancelled at 90.094847375s. Observer exit 0 is not Agent success.

Actual Noodle removed the disposable worktree and branch, raw session evidence was archived before fixture removal, and candidate source remained clean. No unchanged canonical acceptance or CI rerun was needed for this source-unchanged diagnostic experiment.

[Immutable raw receipt, observer, native logs, same-version source references and cleanup](https://github.com/ed3c/noodle/blob/aa5de0cab604f58ae11905fed5ae65740e667c4e/evidence/78/child-initialization.json) was published and read back exactly.

Result remains **BLOCKED**; PR #79 remains draft/unmerged and Soodles #18 unadmitted. Observation has advanced from parent transport into child internal initialization. Next diagnostic boundary is the in-process app-server initialize/thread-start path; current logs do not distinguish its exact waiting operation. Preserve actual Agent tool execution and session/order/stage-bound typed outcome, followed by original delivery acceptance, as the success gate. No unchanged retry.

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

## Fresh stdin / initialization discriminator envelope — 2026-09-16

Supervisor requested observing stdin write/close results and first initialization progress. Same #78 process-launch owner, existing worktree/PR #79 and rollback atom. Resume candidate 50cd7031adf1e96f8ca26a001a374af066e42f99 (tree 6530b5abd9390f53fa6e4de663273473b8e01c52), delivered base 1ba241abb68333b5c53d9d23955f538b2cb63877.

Amend the existing seven-file write boundary to also admit dispatcher/codex_startup.go and dispatcher/codex_startup_test.go. Implement only bounded, session-bound diagnostics in the existing producer and consumers: stdin expected/written bytes, write and close completion/error, first stdout/stderr observation and first canonical initialization. No prompt or credential contents in the new receipt. Diagnostic persistence failure must be visible; observations never authorize execution or reclassify completion.

Controls must distinguish incomplete/failed writes, failed closes, pre-init stderr, actual init, and preserve byte forwarding and cancellation. Run the nearest focused positive/planted-negative controls, then canonical pnpm check once on the final provider candidate. One instrumented real Codex process probe is admitted at the SAME 90-second deadline, existing ChatGPT authentication/model/host policy. Additional observability is the material input change; no unchanged retries, API-key gate, broader permissions or timeout extension. Raw results and cleanup must be preserved even on failure.

Prior acceptance does not apply to the upcoming candidate. #78 remains BLOCKED, #79 draft and Soodles #18 unadmitted until the original real typed-outcome requirement is met. No skill capability upgrade is authorized by diagnostic green tests.

## Current checkpoint after delivered #80 — 2026-09-16

#80 is **RESOLVED** through PR #81, merge `1ba241abb68333b5c53d9d23955f538b2cb63877`, including provider merge/closure and local Noodle cleanup. Its old blocker statements below are historical.

This Issue remains **BLOCKED**. Existing PR #79 now has candidate `50cd7031adf1e96f8ca26a001a374af066e42f99`, tree `6530b5abd9390f53fa6e4de663273473b8e01c52`, with #80 integrated by a forward merge. Both corrections remain present; no force-push or new launch-policy choice.

- Final-head canonical `pnpm check`: exit 0, 37.996s, clean source; one invocation on this head.
- Exact-head [Test run 35077843608](https://github.com/ed3c/noodle/actions/runs/35077843608): success.
- Exact-head [Actions runtime run 35077843559](https://github.com/ed3c/noodle/actions/runs/35077843559): success. This is the existing no-model-credential runtime control, not nested Agent execution.
- One materially changed real process-dispatch probe used the integrated stream implementation and asked the actual Codex Agent to emit the existing session/order/stage-bound typed event. Codex 0.154.0-alpha.3 reports ChatGPT login. No credentials or host policy were changed.
- Actual session `launch-probe-20260916-091052-d754f2`: no Agent events, raw output empty, matching typed outcomes=0; stderr only `Reading prompt from stdin...`. Deadline cancellation at 90.019s; observer elapsed 90.452s. Observer exit 0 reports a cancelled session and is not success.
- Process absence read back; actual Noodle removed the fixture worktree/branch. Session files were archived before the disposable fixture root was removed. Production #78 worktree remains clean and resumable at the exact PR head.

[Raw observer, session files, acceptance, CI readbacks and cleanup](https://github.com/ed3c/noodle/blob/7982327c073e5bdbd563c76e0e944105bc72087c/evidence/78/after-stream-recovery.json).

The stream fix did not establish successful startup. Do not infer that missing API keys caused this result or rerun unchanged input. Next owner work must locate and demonstrate a material change in the pre-event Codex carrier path, or supply a separately verified compatible carrier. No host restrictions may be weakened. #79 stays draft/unmerged; Soodles #18 remains unadmitted until its dependency acceptance is actually met.

## Corrected Actions runtime receipt — 2026-09-16
Candidate `1a663fc7eda778480ec858d30c90d2fac90796f2`, tree `1eb54a5e896a6b134d1d92fb1a504ed300b1f6a8`; base remains `bad0f1efe3525e8a75e9acb6cdea2bf1e404f1ae`.

[Runtime run 35056849442](https://github.com/ed3c/noodle/actions/runs/35056849442), attempt 1, job 104668718077: **PASS**. Exact-head checkout and repository-owned `pnpm build` succeeded. The fixed external Soodles worktree control produced 13 transcript commands: real create/exec, missing-target refusal before side effects, cleanup, no residual branch/worktree/source change. Candidate binary SHA-256 `92d52df9600429d3843d99b8685d1831f94af4979e693c50c354d18edf6b99e3`. No model secret was supplied or required; child commands received the oracle's credential-stripped environment.

Executed workflow SHA `ffb3466f1ef9f5d48163a39e2977d241f8ef2031`; provider bytes read back equal the supervisor-selected workflow. External oracle remained Soodles `0256f2923e978b989e25df07c74db4370d343312`, file SHA-256 `d65b8ba15f2cdbfdd43c4fc0bf267c78ccb2b38208171623b1291704b144c6d9`. This is a pinned, non-authorizing runtime observer, not a replacement or promotion of the landing judge.

Receipt classifications: `runtime=PASS`, `nested_agent=NOT_EVALUATED`, `typed_outcome_observed=false`, `authorizes_landing=false`, `local_carrier_verdict=NOT_EVALUATED`.
Artifact [noodle-actions-runtime-1a663fc7eda778480ec858d30c90d2fac90796f2-1](https://github.com/ed3c/noodle/actions/runs/35056849442/artifacts/10430243859); provider digest `sha256:e38ba746d267cb37c040f00dfd65e72063784c094454a58b036991fe853cc98b`. The actual JSON was read from the completed job log and artifact metadata was read back; archive bytes were not independently downloaded.

Exact-head local canonical `pnpm check` passed in 12.066s, source clean. However, [ordinary Test CI 35056849401](https://github.com/ed3c/noodle/actions/runs/35056849401) failed in the nested fixture suite: `TestProcessHandleSpawnAndDone` observed empty stdout instead of `hello\n`. This is preserved as separate dependency [#80](https://github.com/ed3c/noodle/issues/80); unchanged ProcessHandle source was not modified and no unchanged CI rerun was issued.

Current next owner actions: admit and resolve/dispose of #80 through its existing ProcessHandle owner; independently obtain the real successful typed-outcome evidence still required by #78. Missing OPENAI_API_KEY is not a runtime or supervised-Actions blocker. #78 stays open, PR #79 draft, and #18 has no new production admission. No merge/close or full Agent-cycle success is claimed.

## Current correction — API-key-only gate withdrawn
The owner correctly identified that API keys are not a prerequisite for Noodle runtime or the ChatGPT-supervised GitHub Actions path. The secret-presence gate introduced in the cloud envelope below was an incorrect admission rule. Its historical BLOCKED receipt describes that rule, not a Noodle runtime failure. It is superseded; do not request a model credential to enable ordinary Actions runtime verification.

Fresh execution envelope: same owner, branch/worktree, seven-file write boundary and rollback atom; resume from 7c8620a470d36981dd67b42dda31e0134aeb4bb3, base bad0f1efe3525e8a75e9acb6cdea2bf1e404f1ae. Replace the workflow's availability check with real candidate Noodle worktree creation, execution, missing-target refusal before side effects, cleanup and Git readback. Reuse worktree_probe from fixed external ed3c/soodles@0256f2923e978b989e25df07c74db4370d343312/soodles.py, SHA-256 d65b8ba15f2cdbfdd43c4fc0bf267c78ccb2b38208171623b1291704b144c6d9. Do not load the default branch verifier or promote a candidate verifier. Update the existing brain note with this correction.

Record runtime and nested Agent claims separately. The probe is non-authorizing and does not replace #78's original successful real Codex typed-outcome acceptance. No secret reads, model requests, host-policy changes, new scheduler or unaltered launch retry. Prior acceptance is invalid for the upcoming new head; run canonical acceptance once at the final provider candidate head and read back Actions results. Soodles #18's actual admission remains a separate owner decision; absent OPENAI_API_KEY is not its blocker.

## Observed blocker for Soodles target integration
Owner: Noodle's Codex process dispatch boundary. This is one launch-contract correction, not permission to alter host requirements or build another scheduler.

Physical runtime: ed3c/noodle v0.1.19, commit 391f3154c680fc6724b8cdabf6bcec831b61065d, binary SHA-256 9f1354bea227a5e71fa36260645fa6102f1474f9fba170f9a76ad9f6dd678472.
Latest source readback: bad0f1efe3525e8a75e9acb6cdea2bf1e404f1ae; dispatcher/command.go still unconditionally emits --dangerously-bypass-approvals-and-sandbox.

## Physical trigger
In the current managed carrier, /opt/codex/bin/codex --version returns codex-cli 0.154.0-alpha.3 and login status returns Logged in using ChatGPT. It is outside PATH; the probe added only /opt/codex/bin and the pinned Noodle directory to PATH. No authentication material or requirements were modified.

An isolated temporary Git fixture used routing.defaults provider=codex/model=gpt-5.6-sol, agents.codex args=[--ignore-user-config], require_typed_outcome=true, concurrency.max_concurrency=1, server.enabled=false and existing process runtime. Backlog fixture scripts were true/false; a capability-only probe skill/order was seeded. No schedule skill was present, so the real startup owner took its bootstrap route.

Invocation: /tmp/soodles-runtime/noodle start
A supervisor stopped its process group after 40 seconds, allowing graceful shutdown. Observed elapsed 40.028 seconds; exit 0 reflects shutdown, not successful work.

All three bootstrap session stderr.log records contain:
```
Error: `approval_policy = "never"` cannot be used because requirements do not allow `sandbox_mode = "danger-full-access"`; Codex would fall back to read-only permissions with approvals disabled. Choose an `approval_policy` based on what you need, such as `on-request`, or choose an allowed sandbox mode.
```
The three meta.json states are failed. No successful Agent execution or Issue admission was observed. Receipt SHA-256: 5f5400e96f37d699b98e8c35ff7df498719d796490a77663b94e6e266b158146.

## Cause / nearest source
dispatcher/command.go codexBaseArgs hardcodes exec + --dangerously-bypass-approvals-and-sandbox + --skip-git-repo-check + --json before dispatch.
The host requirements reject that policy combination. This is distinct from missing credentials and from the separate missing-schedule bootstrap behavior.

## Bounded correction
Use the existing host/repository-owned permitted launch contract, preserving actual host restrictions. Do not modify requirements.toml, permission controls or credentials to make the command pass; do not introduce an Agent-facing authority-policy selector, guessed fallback or unchanged retries.
Include the argument producer, relevant dispatch consumer/capability diagnostic and nearest positive/planted-negative tests necessary for this one correction. Precise file boundary and acceptance must be admitted by this repository's owner before execution.

## Acceptance — local Codex CLI process path
Re-read latest source and reproduce on the affected local/managed Codex CLI carrier described above. A permitted host configuration must launch a real Codex session through Noodle's local ProcessDispatcher and obtain the existing typed outcome emitted by that Agent; a forbidden configuration must produce an actionable blocked/refused result naming the invalid field/value and supported next entry before task side effects. Preserve existing worktree/dispatch ownership and canonical pnpm check. Prove with runtime receipts; fixtures alone cannot establish authenticated Agent execution.

## Non-claims / dependency handling
This is a reported exact dependency, not a source-authorized ready task or an execution envelope. Soodles has not modified this repository's lifecycle or code. Its integration remains blocked pending a verified runtime capability or a separately admitted compatible release.
The same fixture also emitted 9,217 bootstrap-exhausted warnings after the three failures; that is separate loop-owner evidence and is not included in this launch-policy patch. No broad retry/loop redesign belongs here.

## Execution checkpoint — 2026-09-16
The repository owner explicitly requested completing #78 and then resuming Soodles #18 in the supervising session. This authorizes isolated implementation of this atom, not authority-policy changes or a production scheduler claim.

Source base: bad0f1efe3525e8a75e9acb6cdea2bf1e404f1ae. Noodle-created worktree: .worktrees/issue-78-codex-launch; branch issue-78-codex-launch.

Exact initial write boundary: dispatcher/command.go, dispatcher/process_dispatcher.go, dispatcher/process_session.go, dispatcher/sprites_dispatcher.go, dispatcher/codex_launch_test.go, brain/codebase/codex-launch-carrier-contract.md. Include only changes needed for the shared argv producer, process consumer/diagnostic, existing Sprite caller compatibility and discriminating controls. The current external acceptance source and host requirements stay fixed. No candidate acceptance exists yet; any widened boundary requires a new checkpoint and invalidation.

Compatibility scope: ordinary ChatGPT GitHub plugin transport plus Actions runtime tests does not require a nested Codex Agent. #78's remaining execution acceptance is limited to the tested local Noodle-to-Codex CLI process path, using its permitted host configuration and existing ChatGPT login. Nested Agent execution inside Actions has not been evaluated and is not required by this clarified #78 scope; GitHub connector access does not establish it. Do not treat the local carrier rejection or timeout as failure of Soodles' already demonstrated supervised delivery route.

Pre-edit probe removed the forced bypass and retained host configuration: codex exec --json --ephemeral --model gpt-5.6-sol with a no-tool capability prompt. It timed out after 55.012 seconds with no Agent events (stderr only Reading additional input from stdin...). This is not a successful launch receipt. Do not retry that unchanged input.

This checkpoint supersedes only the earlier report-only implementation status. Production scheduling remains unadmitted, and all original real-runtime and canonical acceptance requirements remain outstanding.

## Verified implementation checkpoint — Draft PR #79
Candidate: 2a9d7a9c1687563abd8c511b98bf223945cd2932; tree: 1c0c64c78484a2838fcad67e49ff38bd62010beb; base remains bad0f1efe3525e8a75e9acb6cdea2bf1e404f1ae.
PR: https://github.com/ed3c/noodle/pull/79 (draft; not merged).

Implemented inside the admitted boundary: process launches no longer receive an implicit permission override; Sprite preserves its existing carrier argv; pre-event Codex errors retain the exact field/value and `codex exec --help` in the terminal outcome after stderr persistence. No lifecycle classification, retry owner, credentials, host requirements or active verifier was changed.

Evidence:
- Focused RED→GREEN; dispatcher suite and focused race controls passed.
- Planted forced-permission injection, diagnostic loss and Sprite policy drift were each rejected.
- Real installed Codex through the candidate process dispatcher rejected `invalid-mode` for `--sandbox` before Agent events; outcome now names the owning configuration action and `codex exec --help`. Noodle cleanup succeeded.
- Canonical `pnpm check` passed at the exact provider head in 12.195s, with a clean worktree. An earlier pre-transport head with the identical tree passed in 55.571s; the second acceptance was required because connector transport changed commit identity. These times are not a performance comparison.
- GitHub Actions Test run 35054935096 / attempt 1, job 104662992744, succeeded for head 2a9d7a9c1687563abd8c511b98bf223945cd2932: https://github.com/ed3c/noodle/actions/runs/35054935096 . Test, Vet, architecture lint and skill snapshot passed.

Remaining blocker: the real permitted-config process probe emitted no Agent events in 90s and was cancelled; Noodle cleanup then succeeded. Direct probes on installed Codex 0.154.0-alpha.3 (55s) and 0.153.3 (120s) also timed out. Successful Agent executions=0; typed outcomes=0; production Issue admissions=0. Do not infer a credential cause from these observations. Ordinary plugin transport + Actions checks works, but does not prove nested Agent execution.

Checkpoint classification: BLOCKED, not RESOLVED. Keep #78 open, #79 draft and Soodles #18 unadmitted. Preserve the committed implementation worktree for resumption. No unchanged launch retry. Next admissible step is a materially changed and authorized carrier capability, or a demonstrated fix to the pre-event startup failure, followed by real typed completion and the existing delivery gates.

Evidence file: noodle-78-compatibility-checkpoint-20260916.json; SHA-256 7a542a3650b1413212808adb63ae37fcb341c5aea17e26c3af3f402c93dcda87. It includes raw probe/session receipts, cleanup, planted-negative outputs, both local acceptance logs and the provider CI log. No landing authority is granted by this checkpoint.

## Historical cloud-first envelope — superseded by correction above
The owner explicitly separated GitHub Actions evidence from local/managed Codex CLI evidence and requested proving Actions first. Cloud success will establish only the exact Actions carrier; the earlier managed-carrier timeout remains a separate unresolved observation and is not a prerequisite for running the cloud experiment.

Amend the same #78 atom's write boundary to add `.github/workflows/codex-actions-capability.yml`; retain the six previously admitted files. Resume the same Noodle worktree and PR #79 from candidate 2a9d7a9c1687563abd8c511b98bf223945cd2932 and source base bad0f1efe3525e8a75e9acb6cdea2bf1e404f1ae. Prior acceptance applies only to the old head and cannot authorize this new envelope.

First cloud discriminator: a same-repository PR workflow, with no candidate checkout or execution, checks only the boolean availability of the documented `secrets.OPENAI_API_KEY` input. Never expose or copy the secret value. Bind the receipt to Actions run/attempt, workflow SHA and candidate head. Missing input is BLOCKED before Agent dispatch; available input is AUTH_INPUT_AVAILABLE, not authenticated execution or success. No alternate credential identity is guessed and no host or repository permission is changed.

If the input is available, admit the exact additional probe/observer files before the next mutation, then run the real Noodle process path on Actions with model auth isolated from candidate code. Preserve a fixed external observer, actual session/order/stage typed outcome and Noodle cleanup. If unavailable, preserve this exact cloud blocker and do not run a known unauthenticated retry. Do not infer that all possible authentication methods or all repository secrets are absent.

This experiment is non-authorizing. #78 remains open and #79 draft until actual Agent execution and the unchanged delivery acceptance are satisfied. Soodles #18 remains contingent on those receipts.

## Historical receipt — incorrect API-key-only admission rule
Head: 7c8620a470d36981dd67b42dda31e0134aeb4bb3. Cloud run: https://github.com/ed3c/noodle/actions/runs/35055962345 (attempt 1), job 104666104364. The selected workflow bytes match the executed workflow SHA 2f7bdea22c9c0d07f5e39230b9cf9ec677b24e4c (workflow SHA-256 5d6b85c49a95e31bd036b316184be3f7de5e3c9acd82d8f113f336143b973f06).

Actual cloud result: `BLOCKED`, field `secrets.OPENAI_API_KEY.available`, value `false`, owner `Actions Codex authentication`. Preflight and artifact publication succeeded; the final refusal step deliberately exited 1 before Agent dispatch. No checkout, secret value, model request or local CLI probe was used. Agent execution attempted=false; typed outcome observed=false; landing authority=false. This establishes only that the documented input was unavailable in this workflow context, not that all possible repository authentication methods are absent.

Artifact 10430990790, digest sha256:ffcf328fdad42ea191a3e7c7970bbda2fdb877e7acbdc600b584f34fa3cef655, was read back from GitHub metadata. The receipt itself was read from the job log. Direct retrieval of the connector-provided artifact file reference returned HTTP 403; archive-byte verification is not claimed.

Exact-head canonical `pnpm check` passed in 12.004s with clean source. Ordinary Test CI passed separately: https://github.com/ed3c/noodle/actions/runs/35055962213 . Neither result supplies model authentication or proves Agent execution.

Next owner action: provide an authorized model-authentication input to this cloud workflow context, then admit the exact real Noodle→Codex probe and obtain session/order/stage-bound completion plus cleanup. Do not retry unchanged input. The local/managed 90s timeout remains separately unverified and does not block a properly authenticated cloud experiment. #78 stays open, PR #79 draft, Soodles #18 unadmitted.

Saved evidence: noodle-78-actions-capability-20260916.json, SHA-256 de6e7e2d5e492a96eb88833fb384856585dd8425e2cddf48e25f28e58069fea3.

## Fresh dependency-resolved execution envelope — 2026-09-16

Supervisor explicitly requested finishing this dependency then Soodles #18. ProcessHandle #80 is now merged through PR #81: merge 1ba241abb68333b5c53d9d23955f538b2cb63877, candidate 1a12c6c7f08c569d843f3997d7e111c46e510acc. Exact-head Test run 35077395709 and canonical pnpm check succeeded; provider merge/closure and Noodle cleanup were read back. It fixed delayed stdout/stderr loss; it does not establish the cause of this Issue's no-event timeout.

Resume the existing issue-78-codex-launch worktree/PR #79. Integrate that delivered base by a forward merge, preserving both histories; resolve any overlap in this Issue's already-admitted process_dispatcher.go/process_session.go without dropping either correction. The seven-file #78 implementation boundary remains; #80 changes enter only as delivered ancestry. Prior acceptance for the old head is invalid for the new candidate.

One bounded real Noodle process-dispatch probe is now eligible because stream lifetime materially changed. Use the existing configured Codex/ChatGPT carrier and the same model, without changing credentials or host permission policy. The new observer may instruct the actual Agent to emit one existing typed stage_message bound to supplied fixture order/stage and actual session identity. Capture actual events, stderr, event-store readback and cleanup; never have the observer fabricate Agent completion. A timeout remains BLOCKED and receives no unchanged retry. Ordinary Actions runtime without model credentials remains a separate already-proven scope.
