# Guest progress preservation

This helper is an implementation input for controlled OpenShell interruption.
It does not yet replace the Runtime stop path. A helper-only test does not prove
Host/Loop new-attempt behavior or that a live model's results were retained.

## Selected boundary

| Option | Result |
| --- | --- |
| Stop the VM, then download | Rejected: native SSH cannot download from Stopped. |
| Signal a process group, wait for its leader, then archive | Rejected: detached descendants can keep writing. OpenShell also denies negative-PID signaling. |
| Original guest supervisor, separate stop-purpose Exec, descendant drain, archive, download, then VM Stop | Selected. No new OpenShell RPC or weakened seccomp is required. |

`noodle-progress run` becomes the original native Exec's parent of the existing
harness launcher. It installs Linux subreaper ancestry before spawning the
harness. A second, independently recorded native Exec invokes `stop`, using an
exact session, command digest, random token and one-shot stop UUID. The control
socket lives in a fresh 0700 directory under the admitted `/tmp`; it is 0600.
The client binds its peer PID, UID and Linux start time to the original private
process receipt. A different token/session/stop UUID is refused.

The sole waiter reads kernel-maintained direct-child lists under
`/proc/self/task/*/children`. It never scans the machine for similarly named
processes or signals caller-supplied PIDs. It sends TERM to positive child PIDs,
then KILL after the explicit grace interval. Signals and reaping are serialized,
so an unreaped child PID cannot be reused between selection and delivery.
Subreaper adoption catches descendants that create a new process group/session.
Only `wait4` reporting `ECHILD`, together with the observed original harness
exit, permits archival. The timer controls escalation; elapsed time never
certifies quiescence.

The helper archives the repository and dedicated harness session after that
barrier, excludes `codex/auth.json`, fsyncs both archives, then durably publishes
a result containing exact hashes and sizes. Only that completed supervisor
transport exits zero; the harness exit code stays in the receipt. A harness
terminated by TERM (143) can therefore have safely saved progress, whereas a
supervisor terminated by TERM cannot be mistaken for successful preservation. The control token is omitted from
receipts and removed from the harness environment. A completed same-ID stop
reads the existing receipt without signaling again. Natural completion may
produce an archive with no stop UUID; it is reported as natural completion, not
a newly admitted stop. Errors or unkillable descendants produce no successful
archive receipt. Post-launch metadata, stop-intent, signaling or waiter failures
keep the same subreaper and listener alive until its children are reaped; a
permanent inability to signal keeps that owner resident. The Host must retain
the VM/lease on any unresolved outcome.

The harness and supervisor currently share a Linux UID. Directory modes and
the token prevent accidental cross-session control; they do not make guest
JSON an independent attestation against malicious same-UID code. A consumer
must observe both original supervisor Exec and stop-purpose Exec outcomes,
validate the frozen command and downloaded archive bytes, and retain custody
on ambiguous exits. A control helper's exit code or `result.json` alone is
never proof of descendant drain. Protection from a harness deliberately
modifying or killing its supervisor needs a separate OS-enforced identity;
this slice does not claim that stronger isolation.

## Pinned native source checks

Source: NVIDIA/OpenShell `bdb956f2cd65a635f1294fccf1d138d263bf650f` (v0.1.5).
The existing refresh-drain patch does not change these syscall policies.

- `crates/openshell-sandbox/src/linux/child_seccomp.rs`: blocks direct
  `pidfd_open`, `pidfd_send_signal` and negative-PID signals. It does not block
  `prctl(PR_SET_CHILD_SUBREAPER)` or `wait4`.
- `crates/openshell-sandbox/src/sandbox/linux/seccomp.rs`: default-allow targeted
  blocking and AF_UNIX allowlist; no separate subreaper restriction.
- `crates/openshell-sandbox/src/linux/process_signal.rs`: positive-PID signals
  are brokered through retained pidfds; the trusted sandbox TGID is excluded.
  There is no requirement that the sender be the original main process.
- `crates/openshell-sandbox/src/managed_children.rs`: native code also uses
  `/proc/self/task/*/children` as direct-child ancestry.
- `crates/openshell-sandbox/src/network_broker.rs`: AF_UNIX remains kernel-driven,
  including the existing `unix_connect_remains_kernel_driven` test.

These source checks do not substitute for an actual OpenShell execution under
the intended filesystem/network policy. The control path needs read access to
`/proc` and write access to its private `/tmp` directory. Existing production
profiles and services are not modified by this slice.

## Verification scopes

On macOS, run the platform-neutral protocol/archive tests; Linux Run explicitly
refuses. The Linux fixture launches only disposable native test processes, no
LLM or network client. It includes a detached writer ignoring TERM, verifies
ECHILD before archives, preserves the writer's data, checks exact progress/transcript contents and archive hashes,
rejects a wrong token and checks same-ID stop readback. Additional Linux cases
inject process/stop persistence and signaling failures, proving the original
owner stays alive until the owned descendants are reaped. Run the same fixture
inside an isolated OpenShell sandbox to exercise the real syscall broker.

```sh
GOMAXPROCS=2 go test -race -p 1 ./internal/guestprogress ./cmd/noodle-progress
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c -o guestprogress.test ./internal/guestprogress
# Inside the isolated Linux/OpenShell environment:
./guestprogress.test -test.run='Test(StopReapsEscapedWriterBeforeArchiving|PostLaunchFailureRetainsOwnerUntilReaped|SupervisorSignalDoesNotReportSavedProgress)' -test.v -test.timeout=40s
```

Pending integration: separate original/stop-purpose custody receipts, frozen
profile helper selection, interrupted archive download/hash validation before
VM Stop, original-owner stopped readback, and the explicit new-attempt action.
Neither baseline-only restart nor native session resume completes these items.
