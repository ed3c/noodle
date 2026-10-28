# OpenShell v0.1.5 refresh shutdown repair

This source patch repairs graceful gateway shutdown without upgrading the native
protocol or replacing the installed CLI, VM driver, gateway or credentials.
It is a local Noodle integration artifact, not an official OpenShell release.
See [the validation record](VALIDATION.md) for observed results and limits.

OpenShell v0.1.5 starts provider refresh tasks without joining them during normal
TERM shutdown. An OAuth issuer can rotate its grant while the gateway is exiting,
before the replacement has finished persisting. Increasing the signal grace
period or observing an idle refresh status does not close that race. The
v0.1.6 refresh implementation has the same limitation.

The patch follows the existing supervisor-session admission/lifetime pattern:

- Each credential runtime owns a shared refresh lifetime. Credential runtime
  clones share that owner; independent gateways do not share shutdown state.
- Mint admission covers manual rotation, credential token exchange and the
  periodic worker. The existing detached mint task retains its lifetime through
  its persistence path, even if the requesting RPC disconnects.
- Configure-refresh, delete-refresh and delete-provider requests also retain a
  lifetime through their writes. Shutdown refuses new refresh obligations.
- Shutdown closes admission and signals only the refresh ticker, then joins its
  active tick and drains admitted operations before signalling global service
  watchers or starting compute/credential teardown. It never
  cancels an active tick or adds a timeout that discards an in-flight mint. The
  production cleanup function is also exercised directly against a paused
  credential write, including the global shutdown signal ordering.

No new configuration or RPC is introduced. Existing persistence failures retain
their original error/recovery semantics; this repair does not recover earlier
uncertain refresh states. SIGKILL, machine failure and external force-stop cannot
be made safe by graceful shutdown. Keep Noodle `force_after_grace: false`.

## Reproduce

This binary build recipe targets Apple Silicon macOS; the native source repair
does not introduce a platform-specific shutdown path. Requires Python 3.12+,
Git, Cargo and the pinned Rust 1.95.0 toolchain. Keep the
build directory outside any Git checkout; it is an isolated source/build input,
not an App-managed worktree. The source archive and reviewed patch are verified
against `source.json`. Existing directories are not overwritten.

```sh
python3 examples/openshell/native-patches/refresh-drain/build.py prepare --work-dir /absolute/new/build-directory
python3 examples/openshell/native-patches/refresh-drain/build.py fetch --work-dir /absolute/new/build-directory
python3 examples/openshell/native-patches/refresh-drain/build.py check --work-dir /absolute/new/build-directory
python3 examples/openshell/native-patches/refresh-drain/build.py test --work-dir /absolute/new/build-directory
python3 examples/openshell/native-patches/refresh-drain/build.py regression --work-dir /absolute/new/build-directory
python3 examples/openshell/native-patches/refresh-drain/build.py build --work-dir /absolute/new/build-directory
```

`prepare --archive /absolute/source.tar.gz` reuses a locally available archive
only after its digest matches the same upstream pin. Tests use a loopback fake
issuer, in-memory gateway store and test credential driver. No real grant,
model endpoint, VM or live gateway is used.

`fetch` also downloads the official Z3 5.1.0 archive pinned in `z3.json`, verifies
both archive and static-library hashes, and preserves its license. OpenShell's
native `Z3_LIBRARY_PATH_OVERRIDE` selects only this isolated static library;
system package discovery is disabled for Z3. This satisfies the gateway's native
link dependency without installing a system package or compiling Z3 locally.

Native test/build actions use one Cargo job, no debug information, a dedicated
target directory and offline locked dependencies. Every completed action writes
a separate result receipt and log, checks the source tree before and after, and
reports failure as failure. A killed build leaves its log without a successful
receipt. `test` covers shutdown barriers; `regression` runs existing provider
refresh tests as well. `check` only verifies compilation and must not be reported
as a passed execution test. The binary build enables the VM compute driver and
disables telemetry, but does not launch it.

The resulting binary identifies itself as `0.1.5+noodle.refresh-drain.1`.
Integration must bind its exact binary SHA256, native version and foreground
command in a new reviewed profile. This script does not install, switch,
restart, adopt or stop any running service. Native patch tests alone do not
prove a Host can safely release its gateway or that a model task was accepted.
