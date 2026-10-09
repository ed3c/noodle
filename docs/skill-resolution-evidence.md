# Noodle Skill resolution evidence — read-only configured snapshot

The existing `noodle skills list` uses `skill.Resolver` with ordered configured Search Paths and first-match-wins precedence. It does **not** prove a launched Agent's complete effective Skill catalog. A worker may also discover other global/system/user Skills outside this resolver.

Run from the **correct original Noodle project/configuration** with an externally pinned Noodle binary:

```sh
noodle skills list --json
```

The JSON result records the configured Resolver's ordered search paths, winning Skill name, winning source path, absolute selected path, raw `SKILL.md` SHA-256, and content-bound `tree_sha256` for each selected directory. The tree digest hashes compact JSON of sorted `[relative-posix-path, raw-file-sha256]` pairs, with the same supported byte conventions as FactoryWeaver's candidate `skill_tree_sha256`.

An empty discovered set gives `NO_SKILLS_DISCOVERED` and a nonzero exit status. A duplicate Skill name from multiple configured provider paths emits `SHADOWED_SKILL_SOURCES`, lists shadowed paths, and returns nonzero. Symlinks, invalid/oversized Skill contents, and missing selected resources are refused rather than producing a trusted hash.

Old `noodle skills list` without `--json` is unchanged: TSV with the winning source and first-match-wins behavior. A machine consumer should explicitly request `--json` and check its status and exit code, not parse human output.

## Provenance ceiling

This proves only the paths that **that CLI invocation** resolves, at that time, under its current working directory/configuration. It is not an atomic source snapshot, an original-owner attestation, a configured Profile admission, a native Worker Session, or proof that global Skills are excluded. The result intentionally reports `actual_worker_session_observed=false`, `effective_agent_catalog_verified=false`, `global_skill_inheritance_excluded=false` and `effect_authority=false`.

For Soodles [#305](https://github.com/ed3c/soodles/issues/305), the original Supervisor must separately pin current executable/config and select a valid target Work Order, Factory Profile and Carrier. Only an independently captured **actual launched Worker** Skill view, session/worktree identity and Owner readback can raise the evidence above `CONFIGURED_RESOLVER_SNAPSHOT`. This CLI does not install Skills, switch Workflow Roots, create Worktrees, resume Workers, merge PRs or authorize effects. The older deleted auxiliary control repository is not involved.
