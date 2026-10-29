# Owned worker exit and recovery evidence

The admitted process handle retains an exited Unix leader until its original group has no other members. Reaping it first can let the PID be reused before a later group signal. `dispatcher/process.go` serializes final reap and stop on that handle.

A native terminal event can precede process exit. A stale terminal `meta.json` cannot close an admitted worker or return its permit. The process session waits for the original handle. A steerable turn completion can retain a useful Session.

Recovery reads the original process record and observes both process and group. A missing launcher, a formatted JSON status, or a terminal claim alone cannot prove group exit. Discovery does not transfer child ownership. The recovered placeholder cannot acknowledge stop or exit.

The regression fixture initially removed its temporary directory before its descendant read the stop file. This left two test descendants alive. Restoring the exact fixture stop files allowed both to exit without PID signals. The fixture now waits for its cleanup acknowledgment before directory removal.

Checks: `TestOwnedWorkerReclaimsDescendantsAfterLauncherCompletes`, `TestAdmittedWorkerIgnoresStaleTerminalMetadata`, `TestProcessRecoveryDoesNotLoseGroupWhenLauncherExited`, and `TestRecoveredIdentityCannotAcknowledgeStopOrExit`. macOS live tests do not establish Windows or Linux live acceptance.
