# Real gateway SIGTERM acceptance

This command starts the **already built, patched OpenShell gateway executable**.
The issuer and external compute/credential drivers are synthetic protocol peers.
One scenario instead uses the native built-in database credential store. There is
no substitute gateway, VM, model, real grant, or connection to an existing Host.

This Unix-only fixture requires SIGTERM, process groups, and Unix sockets. Run
only when the operator has released the resource window:

```sh
GOMAXPROCS=2 go test -p 1 -race ./examples/openshell/native-patches/refresh-drain/acceptance
GOMAXPROCS=2 go run -p 1 ./examples/openshell/native-patches/refresh-drain/acceptance \
  -gateway /absolute/path/to/patched/target/debug/openshell-gateway \
  -build-receipt /absolute/path/to/successful/build-receipt.json
```

The executable must report `0.1.5+noodle.refresh-drain.1`. The command records its
SHA-256 before testing and verifies that the bytes have not changed afterward.
The successful build receipt must bind this exact binary path, hash, version,
unchanged source tree, upstream commit, and patch digest. Both binary and receipt
are checked again at completion. A version string alone does not establish how
an arbitrary binary was built. This command does not run
Cargo or alter/install the gateway binary.

## What is checked

Each scenario uses a fresh `/tmp/ndr-*` subdirectory, private PKI, loopback port,
SQLite database, HOME, and XDG roots. The gateway receives an explicit minimal
environment, not the caller's environment. Driver endpoints are private Unix
sockets. Native RPCs import one synthetic profile, create a synthetic provider,
configure refresh, then request rotation.

1. **Issuer pending:** the issuer has received the old synthetic refresh token,
   but has not returned the replacement. SIGTERM must close new admission while
   the admitted operation remains alive. Releasing the issuer completes refresh.
2. **Replacement store pending:** the issuer has returned the replacement and
   the real gateway has called native `StoreCredential` with it. The driver gates
   this write before persistence/acknowledgement. SIGTERM must not exit until the
   gate is released and persistence finishes.

3. **Built-in database, issuer pending, group TERM:** omit external credential
   driver configuration and use native `gateway.credential_storage` with an
   explicit private encryption-key path. No credential-driver fixture server is
   started. Verify the retained child is its original process-group leader, then
   send SIGTERM to the negative original PGID. After persistence and child Wait,
   require the original group to be absent. Restart the same isolated database
   and encryption-key path without reading or changing either, and rotate again.
   The second process also exits via original-group TERM.

In all three cases, manual rotation and configuration of the real fixture provider
must return `Unavailable: gateway refresh admission closed`. The process remains
alive for a measured interval with the gate held. After release, refresh must
acknowledge success and the process must exit successfully.

In the first two scenarios, the credential driver syncs its isolated file and
directory before acknowledging storage. After gateway exit, the fixture reloads that file, restarts the same
gateway with the same isolated database, checks committed refresh status, and
rotates again. The issuer privately checks that this new request uses the
persisted replacement token, rather than the old token. It accepts exactly two
requests per scenario. Compute creation/start and other unexpected operations
are refused and counted as acceptance failures.

An empty configuration request is used only to observe admission closure; before
closure it is invalid and cannot change a provider. It is not a retry of the
admitted refresh. No refresh is retried after an unknown result.

## Evidence and limitations

The printed evidence directory is retained on success and failure. It contains
`evidence.json` (source pin, binary/build-receipt hashes, backend, signal scope,
PIDs/PGIDs, observed exit codes and verified absent groups), per-scenario gateway
logs, fixture configuration, generated PKI,
synthetic credential storage, and the gateway databases. `passed=true` is written
only after all three complete scenarios. Neither logs nor the summary print token
values. All tokens on disk are hard-coded fake test values; no real auth source
is read. Errors retain evidence, and cleanup kills only the fixture process that
this command started.

The driver services run in the verifier process, outside the gateway process
group. This proves the gateway drain ordering while a driver remains available;
it does **not** prove that an independently launched credential-driver child will
survive a process-group SIGTERM. The third scenario separately verifies built-in
db-credstore with group TERM.
The fake compute peer remains outside that group and performs no VM operations.
Do not generalize this result to external drivers that receive the same group
signal, or to an active VM shutdown.

The held interval is a bounded observation, not a proof of an unbounded wait.
The later disk reload, second process, and issuer check establish the persistence
path. Filesystem crash/power-loss behavior and actual provider behavior are
outside this fixture.

## Protocol provenance

The public gateway client uses this repository's existing pinned OpenShell Go
SDK. The upstream Go SDK does not generate the private compute/credential
extension APIs. `contracts.pb` therefore contains official descriptors compiled
from the same upstream commit, with source hashes in `contracts-source.json`.
Go's existing gRPC/protobuf libraries serve those descriptors through
`dynamicpb`; there is no handwritten protobuf framing or new dependency.

To regenerate the descriptors from the verified upstream source:

```sh
protoc --proto_path=/absolute/path/to/verified/OpenShell/proto --include_imports \
  --descriptor_set_out=examples/openshell/native-patches/refresh-drain/acceptance/contracts.pb \
  credential_driver.proto compute_driver.proto
```

The focused unit tests verify the fixture's pre-persistence gate, environment
isolation, and refusal to signal a child whose recorded group identity changed.
They are not a substitute for running the real gateway acceptance.
