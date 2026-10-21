# TypeScript 7.0 stable — Noodle UI upgrade

**Source:** [Microsoft TypeScript 7.0 Stable, July 8 2026](https://devblogs.microsoft.com/typescript/announcing-typescript-7-0/). The official compiler is a native **Go** port; Cloudflare `tsrs` is a different Rust project. Soodles main is Python, so no fictitious TypeScript installation is introduced there.

## Exact migration

- `ui/package.json` has `@typescript/native: npm:typescript@7.0.2` — **TypeScript 7.0.2 stable `tsc` CLI** used by unchanged `ui/typecheck: tsc --noEmit`.
- `typescript: npm:@typescript/typescript6@6.0.2` — **TypeScript 6.0.x programmatic compiler API compatibility** for legacy JS-API consumers and tooling (Vite and `vite-tsconfig-paths`). This is Microsoft's documented dual-toolchain migration; it is not a claim that TypeScript 7 exposes a JS Compiler API.
- `ui/tsconfig.json` drops TS7-unsupported `baseUrl`. Existing `~/*` source-relative `paths` are preserved; `vite/client` types are explicit.
- `pnpm-lock.yaml` has been **generated mechanically by pnpm 10.30.3 on exact candidate CI** and copied using the complete checked unified Git diff, not model-computed integrity values. No manual package integrity edits or automatic dependency upgrades beyond those required by the package manager.
- `.github/workflows/test.yml` is restored to `pnpm install --frozen-lockfile` and checks the actual TS7 executable and TS6 programmatic API separately before UI, Browser/Graph, Vite and Go tests.

## CI and acceptance

On initial candidate `9699d2f5fa2b868725aec784e14a73cebd7e916d`, [Noodle Test run 39585009379](https://github.com/ed3c/noodle/actions/runs/39585009379) completed **SUCCESS** after a temporary `--no-frozen-lockfile` lock regeneration and produced: `tsc Version 7.0.2`, `Compiler JS API 6.0.3`, UI Verify/Build, original Go tests/Vet, source/graph tests. That run does **not** prove committed frozen-lockfile reproducibility; a **new exact-head** `--frozen-lockfile` run must complete separately.

Source/CI PASS is not original Soodles/Noodle Poteto Native Agent task acceptance. The original Noodle Host Go implementation, Snapshot Graph v2 Owner, original Soodles P-class and task/Issue bindings are unchanged. Soodles itself does not need or use the TypeScript compiler. No merge, Noodle release or Production effect has been authorized.

## Bun + TanStack + tsrs boundaries (MGC source selected)

Official [Bun check](https://bun.com/docs/runtime/check) is a secondary TypeScript 7 type checker whose **own TypeScript minor version** must be observed on CI; it does not replace Microsoft TypeScript 7.0.2 as the authoritative Noodle UI typecheck. Bun runs an additional Vite build without changing the frozen pnpm lockfile. No new Bun package manager/lockfile or global Noodle Go Host runtime change is assumed.

Noodle has [TanStack Router](https://tanstack.com/router/latest) in its current Vite+React UI, **not TanStack Start**. Migrating Router into a full-stack TanStack Start SSR server would change WebSocket/Host/API authority and is out of this compiler upgrade. The existing [medium-compiler TanStack Start workbench](https://github.com/ed3c/medium-compiler/pull/72) is the optional isolated Start consumer.

[Cloudflare Rust tsrs](https://github.com/oxc-project/tsrs) currently targets TypeScript **7.1-dev**, not this project's official 7.0.2 Stable. Keep it as optional outside the original Noodle typecheck; no side-by-side diagnostic-equivalence assertion without tsgo and tsrs at the **same pinned upstream revision**. The medium-compiler workbench owns the selected tsrs WASM diagnostic proof, with no Noodle Host/Runner authority.

This is a bounded toolchain compatibility test: Bun invocation and TypeScript compiler selection must have actual exact-head CI output. Source-only documentation is not native Agent/Poteto verification or release permission.
