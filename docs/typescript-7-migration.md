# TypeScript 7.0 stable — Noodle UI upgrade

**Source:** Microsoft [TypeScript 7.0 stable announcement (2026-07-08)](https://devblogs.microsoft.com/typescript/announcing-typescript-7-0/) and official 6/7 side-by-side guidance. This migration is selected for **Noodle UI**; Soodles is Python-based and has no TS compiler package to upgrade.

- TS7 native Go `tsc` is installed via `@typescript/native: npm:typescript@7.0.2`. It is the default CLI used by `ui/package.json`'s existing `typecheck: tsc --noEmit`.
- Retain JS compiler API for Vite/tsconfck/plugin consumers via `typescript: npm:@typescript/typescript6@6.0.2`. TS7 does **not** provide the previous JS Compiler API, so replacing `typescript` with TS7 directly would break consumers using `require('typescript')`.
- Remove TS7-unsupported `compilerOptions.baseUrl` from `ui/tsconfig.json`. The existing `~/*: ['./src/*']` project-root paths already satisfy TS7's relative-path requirement.
- Set `types: ['vite/client']` explicitly because TS7 defaults type inclusions to an empty list; add only observed other global declarations if selected UI tests require them.
- Go Backend, Noodle graph Owner, generated snapshot types, API authority and runtime Task/Stage identity are **unchanged**. This is a UI toolchain upgrade, not an Agent/Host/Runner upgrade.
- Exact library versions are pinned for repeatable adoption. No `@typescript/native-preview`, Rust tsrs port, broad rewrite, or global CLI install.

## Verification and provider readback

The original CI workflow must install with **frozen lockfile** after mechanical `pnpm-lock.yaml` regeneration and validate:
`tsc --version` equals `7.0.2`; `require('typescript').version` is `6.0.x`; targeted UI typecheck, lint, formatting, React/Vite build, LoopGraph and browser/Go native host tests, and original owner readback. A source-only patch cannot claim the runtime was upgraded until a real package-manager install and independent exact-head CI report succeeds. Do not guess or hand-author pnpm integrity hashes.

Initial PR may temporarily run `pnpm install --no-frozen-lockfile` **only to collect a generated lockfile patch**. Before merging, commit the mechanically generated lock and restore `--frozen-lockfile`. If TypeScript 7 emits differences, inspect actual diagnostics and fix only affected symbols; do not silently weaken `strict` or skip tests. Preserve Soodles original Test Manager and Poteto scopes; source CI is not Native Agent acceptance. No merge or Production deployment is authorized.
