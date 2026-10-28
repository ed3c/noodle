# Local validation: 2026-10-28

The reviewed source patch is based on OpenShell commit
`bdb956f2cd65a635f1294fccf1d138d263bf650f` (v0.1.5). Its SHA256 is
`e1ad3c56497bb0de53f324fd8b3cf9e8ad609542ce6bfe28493a5d49cc635972`.

The successfully built Apple Silicon gateway reports
`openshell-gateway 0.1.5+noodle.refresh-drain.1`, with binary SHA256
`375d1330904e41edc980ce7ecde5a55758938d78eb70fe33f47cc5ce20455c49`.
The build receipt SHA256 is
`6689bc82fad1d9b89d69a02d8a531676e0bd9e455e1b76be9b8a4034e6c89ad4`.

## Passed

- The final patch applies to a fresh archive with the pinned upstream digest.
  Its resulting source tree matches the compiled source tree:
  `0ee81e9b5dfbbf44a490a9c5be07ab8ada38bf651f9852e1a1c564777b80c5bf`.
- `cargo check --locked --offline -p openshell-server --no-default-features
  --tests -j 1` compiled the final production source and test code successfully.
- The production gateway compiled and linked with the pinned Z3 static library.
  Its dynamic dependency list contains no external Z3 library. Compilation took
  46.779 seconds; the enclosing command reported maximum RSS 2,046,345,216 bytes.
- Real gateway processes passed both `issuer-pending` and
  `replacement-store-pending` scenarios. After TERM closed admission, manual
  rotation and configuration were rejected while the admitted refresh remained
  gated. The gateway remained alive. Releasing the gate allowed persistence and
  exit with code zero. A new gateway process using the same isolated database
  and reloaded credential-driver storage then used the saved replacement grant.
  All four original gateway process exits were observed with code zero.

The successful process evidence SHA256 is
`ddd0875796986aec98917e6ccfaf9b66063665da0e3456e84f01ccdd0848517a`.
It binds the source pin, build receipt, binary bytes, process IDs and exit
observations. The fake issuer and credential/compute drivers use the pinned
native protocols. They are separate fixture services, not gateway process-group
children. No live gateway, real grant, model request or VM was used.

## Built-in database and process-group extension

The subsequent real-process run on 2026-10-28 passed all three scenarios,
including `builtin-db-issuer-pending-group-term`, without rebuilding or changing
the gateway binary. The new scenario used the native database credential store
and a private encryption-key path, with no external credential-driver server.
Its original groups 60784 and 60798 received group SIGTERM; both children exited
zero and both original groups were absent afterward. Admission was closed while
the first issuer response remained gated for 1002 ms. After release, the admitted
refresh completed; a second process opening the same database/key used the
persisted replacement in a new rotation. No compute mutation was attempted.

All six gateway child exits in this run were observed with code zero. Evidence
SHA256: `5d6c6f33e8ec7342880d3eec1f3b1e0db01913e5e6ae6377b15fa32f83f32520`.
The retained fixture directory is
`/tmp/ndr-1662591414`; a durable copy is under
`work/native-refresh-drain-v015/builtin-group-acceptance-20261028/` in the task's
evidence workspace. Its `validation-receipt.json` also binds the fixture source
hashes and records the separately passing focused Go race tests. Those source
tests exercise fixture behavior; the native process evidence establishes the
actual gateway behavior. No Rust unit test execution is implied.

## Limits and preserved failures

The Rust library unit tests were **not executed**. Their initial executable build
was interrupted after sustained swapping; compilation checks are not reported as
execution tests. The tests remain enabled in the patch and reproduction recipe.
The first production link failed because system Z3 was missing; the reproducible
recipe now pins and verifies an isolated official static library. The first
process-verifier attempt was rejected during startup because the fixture omitted
the native configuration version; that fixture error was repaired before the
successful run. These failures are retained separately from successful receipts.

These results prove graceful shutdown of the patched gateway in the tested
topology. They do not prove that signalling an entire process group preserves an
external credential-driver child in that same group, nor do they cover SIGKILL,
machine failure or recovery of already uncertain refresh states.

The existing unpatched live gateway was not stopped or migrated. It has no native
refresh admission-close/drain API that can establish a race-free transition from
an observed idle status to TERM. A successful patched build and isolated test do
not authorize treating that old running process as patched or adopting its
ownership. Live migration remains a separate original-owner operation. See
[the rollout boundary report](ROLLOUT.md) for the native API evidence and
reviewable deployment conditions.
