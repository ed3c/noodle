# Noodle Skill resolution evidence — read-only configured snapshot

The existing `noodle skills list` uses `skill.Resolver` with ordered configured Search Paths and first-match-wins precedence. It does **not** prove a launched Agent's complete effective Skill catalog. A worker may also discover other global/system/user Skills outside this resolver.

Run from the **correct original Noodle project/configuration** with an externally pinned Noodle binary:

```sh
noodle skills list --json
```

The JSON result records the configured Resolver's ordered search paths, winning Skill name, winning source path, absolute selected path, raw `SKILL.md` SHA-256, and content-bound `tree_sha256` for each selected directory. The tree digest hashes compact JSON of sorted `[relative-posix-path, raw-file-sha256]` pairs, with the same supported byte conventions as FactoryWeaver's candidate `skill_tree_sha256`.

An empty discovered set gives `NO_SKILLS_DISCOVERED` and a nonzero exit status. A duplicate Skill name from multiple configured provider paths emits `SHADOWED_SKILL_SOURCES`, lists shadowed paths, and returns nonzero. Symlinks, invalid/oversized Skill contents, and missing selected resources are refused rather than producing a trusted hash.

Old `noodle skills list` without `--json` is unchanged: TSV with the winning source and first-match-wins behavior. A machine consumer should explicitly request `--json` and check its status and exit code, not parse human output.

## Optional required Factory Skill policy at original Noodle dispatch

The existing `dispatcher.DispatchRequest` now accepts **two jointly required original-dispatch-input pins**, `RequiredSkillSHA256` (raw `SKILL.md`) and `RequiredSkillTreeSHA256` (entire normalized Skill folder). When **both are absent**, the original Noodle behavior, including its warning-only missing-method fallback and explicit `SystemPrompt` override, remains unchanged for general tasks.

When either pin is supplied, the original `resolveSkillBundle` refuses before OS process launch unless **both** are valid lowercase 64-hex SHA256 values, the named Skill is present, its `SKILL.md` and entire tree match the pins, no second search-path provider shadows it, no `SystemPrompt` override is present, and the selected method had no incomplete/truncated reference warnings. The checked raw and tree digests are recorded as required inputs in the same original `skill-input.json` session receipt.

**Authority boundary:** This is an opt-in Noodle dispatcher *capability*, not currently activated for Soodles #305. A Writer/Agent-supplied pin is not original Owner authority, and the public FactoryWeaver Profile is still a Draft. The original Soodles Supervisor/Carrier owner must independently select the exact pins and ensure its native Noodle launch actually passes them. This does not prove global/user/system Agent Skill exclusion, nor grant any PR effect.

## Current Noodle Order → Worker pin propagation

The original Noodle `Stage.extra` JSON can optionally carry **both** `required_skill_sha256` and `required_skill_tree_sha256` as lowercase 64-digit strings. The original `loop.spawnCook` now parses and validates that exact pair *before* interruption offering or Worktree creation, and passes it without modification into the existing `dispatcher.DispatchRequest.RequiredSkillSHA256` / `RequiredSkillTreeSHA256` fields. The ProcessDispatcher applies the physical source/raw/tree checks before OS launch; an incomplete or invalid pair is refused before a Worktree or process is created. Existing stages with neither key keep the original legacy behavior.

This path only pins the **Noodle Stage's directly selected `Skill`**. For Soodles, that is usually `execute`, while pstack `poteto-mode` is currently selected **inside the Agent's engineering method discovery**. Pinning `execute` does not verify that the Agent subsequently loaded `poteto-mode` or that it could not discover Builder/HumanLayer/global Skills. That requires separate Supervisor-selected effective Agent Skill catalog evidence. A Stage extra field by itself is not authorization; the original Supervisor must ensure this Stage and both pins originate in its exact admitted order and are preserved through resume/revisions.

The focused Noodle test executes a real original `orders.json → Loop.Cycle → spawnCook → Runtime.Dispatch` using the existing mock runtime, and verifies the original Stage pair is preserved byte-for-byte in the emitted request and retained Order. A malformed half-pair refuses before any Worktree or runtime interaction. These are bounded ordering/serialization controls, not a full live-agent delivery.

## Actual Noodle OS process input receipt (new bounded stage)

Noodle's **process dispatcher** additionally stores `.noodle/sessions/<session>/skill-input.json` with:

- `selected_skill`, `selection_mode` (selected Skill, missing-method warning, no Skill or System Prompt override), the resolved selected file path/source path and raw `SKILL.md` SHA-256;
- `methodology_prompt_sha256` and `composed_input_sha256` calculated from the **actual assembled in-memory prompt** that the dispatcher prepares for the child process;
- `session_id`, selected `worktree_path` and `process_pid` after the OS process has started.

The lifecycle states are deliberately narrow. `PREPARED_BEFORE_OS_LAUNCH` is written before dispatch. Only after the Noodle OS child starts and its original process/spawn metadata is written does the receipt advance to `OS_PROCESS_LAUNCHED_NOT_AGENT_ATTESTED`. If that final receipt cannot be persisted, the dispatcher terminates its own just-started child and refuses completion. It does not create a second Worker, retry a failed session or replace any Soodles owner.

A warning that a selected Skill was not found is made visible as `SELECTED_SKILL_MISSING_WARNING`, not silently promoted to methodology success. A supplied `SystemPrompt` that bypasses the Skill Resolver is explicitly classified `SYSTEM_PROMPT_OVERRIDE`. The older Noodle fallback behavior remains unchanged for compatibility; an original Soodles supervisor still needs a separate policy gate to reject a missing mandatory method.

**Proof ceiling:** PID and exact assembled prompt digest demonstrate *Noodle's OS process launch and prompt preparation*, not whether Codex/Claude consumed stdin, interpreted any Skill, discovered other global/user/system Skills, or accepted a Factory Profile. These facts also lack the independent Soodles Owner Admission and Provider landing receipt. Real worker use must compare this receipt and current binary/config with the original Supervisor-selected Work Order/Skill Profile and a separately captured Agent-effective Skill Catalog.

## Fixed private Soodles oracle authorization

The separate `Noodle Actions runtime` workflow uses a fixed private Soodles source at commit `0256f2923e978b989e25df07c74db4370d343312` and expected raw SHA-256 `d65b8ba15f2cdbfdd43c4fc0bf267c78ccb2b38208171623b1291704b144c6d9`. Anonymous `raw.githubusercontent.com` may return HTTP 404 for a private repository, which is **not evidence that the fixed source does not exist**. The workflow now classifies 401/403/404 as `SOODLES_ORACLE_OWNER_READBACK_REQUIRED` and preserves its failed runtime result.

There is a separate source-locked `scripts/fixed_soodles_oracle.py` reader with an exact Git commit/blob/content hash and no unpinned fallback. It is only appropriate inside an independently trusted original-owner environment after review and pinning. **Do not expose the private Soodles repository's read credential as a GitHub Actions secret inside a `pull_request` job that checks out and runs candidate code.** An untrusted candidate could execute code that reads or transmits that secret. The PR-run test deliberately verifies no `SOODLES_ORACLE_READ_TOKEN` secret is injected.

The original Soodles Oracle Owner needs to provide a trusted, separately authenticated **oracle execution/readback** for the fixed source. Until that capability is available, Noodle runtime verification remains `FAILED / NOT_EVALUATED` and is not replaceable with a candidate-produced fixture or a stale public copy. Go Test success and any Noodle Skill file receipt are separate evidence dimensions.

## Provenance ceiling

This proves only the paths that **that CLI invocation** resolves, at that time, under its current working directory/configuration. It is not an atomic source snapshot, an original-owner attestation, a configured Profile admission, a native Worker Session, or proof that global Skills are excluded. The result intentionally reports `actual_worker_session_observed=false`, `effective_agent_catalog_verified=false`, `global_skill_inheritance_excluded=false` and `effect_authority=false`.

For Soodles [#305](https://github.com/ed3c/soodles/issues/305), the original Supervisor must separately pin current executable/config and select a valid target Work Order, Factory Profile and Carrier. Only an independently captured **actual launched Worker** Skill view, session/worktree identity and Owner readback can raise the evidence above `CONFIGURED_RESOLVER_SNAPSHOT`. This CLI does not install Skills, switch Workflow Roots, create Worktrees, resume Workers, merge PRs or authorize effects. The older deleted auxiliary control repository is not involved.
