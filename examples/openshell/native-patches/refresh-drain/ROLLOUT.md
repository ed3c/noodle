# Rollout boundary: existing unpatched gateway

Reviewed on 2026-10-28 against OpenShell v0.1.5 commit
`bdb956f2cd65a635f1294fccf1d138d263bf650f`. This is a read-only source finding,
not a record of a live migration. No existing service, grant or database was
operated on for this review.

## Native maintenance capability

The public refresh RPCs are `GetProviderRefreshStatus`,
`ConfigureProviderRefresh`, `RotateProviderCredential`, and
`DeleteProviderRefresh`. There is no maintenance RPC that closes all refresh
admission, waits for admitted mint results to persist, and preserves the grant.
See the [pinned protocol](https://github.com/NVIDIA/OpenShell/blob/bdb956f2cd65a635f1294fccf1d138d263bf650f/proto/openshell.proto#L375).

Status is a snapshot, not an admission barrier. A completed manual rotation only
settles that operation; it does not prevent a later ticker, manual request or
credential exchange from admitting another refresh. An absent next-refresh time
is not evidence that no refresh configuration or in-flight operation exists.
Changing refresh configuration is not a gateway-wide drain barrier either.

Deletion does not preserve the grant. The native deletion function marks the
state as deleting, changes its authorization epoch, persists that transition,
deletes pending secret handles and refresh-material handles, then deletes the
stored refresh state. It must not be used as a pause command. See
[the pinned deletion implementation](https://github.com/NVIDIA/OpenShell/blob/bdb956f2cd65a635f1294fccf1d138d263bf650f/crates/openshell-server/src/provider_refresh.rs#L224).

The old process does not acquire the new close/drain behavior when a patched
executable is placed on disk. The patch's internal shutdown boundary cannot be
invoked remotely in an already running unpatched process. Consequently, the
review found **no proven native path to preserve the current grant and safely
transfer this existing gateway**. An idle-status observation followed by TERM
cannot establish that property.

## Reviewable deployment conditions

1. Keep the existing owner, grant store and executable path intact while the
   migration remains unresolved. Do not start a second gateway on the same
   database, export secrets, modify or lock the database, inject behavior into
   the process, or reimport an older saved grant.
2. Keep the candidate at a separate versioned path, bound to the verified source,
   patch, binary and build-receipt hashes. The acceptance suite now verifies its
   built-in database credential store under original-process-group TERM as well
   as the independent external-driver persistence gate. These results apply to
   the candidate, not the old running process.
3. The original Noodle owner must retain custody until its original child and
   process group have actually exited. A stop request or VM-stopped observation
   is not proof of credential refresh drain. Do not terminate an admitted refresh
   merely because a local shutdown timeout expires.
4. If a new isolated deployment is chosen, prepare and verify its separate
   endpoint, state, credential store and patched executable before requesting a
   new dedicated login. Use a fresh authentication flow, not copied refresh
   material. Sharing an account does not prove that grants can be copied safely,
   nor that a fresh login leaves every older session valid.
5. Starting a new isolated gateway does not authorize retiring the old one.
   Retirement still needs a native barrier that the old version lacks, or a
   separate explicit decision accepting retirement/revocation of its grant.
   Neither action was performed by this work.
6. Do not promise lossless rollback by reimporting an old saved grant after
   rotation. A viable rollback may require retaining the original owner or a
   fresh login; it cannot be inferred from a saved credential file.

The built-in group-signal proof closes a candidate-validation gap. It does not
turn a successful patched test into a successful live migration. SIGKILL,
process crashes, machine failure, external credential-driver children sharing
the signalled group, and recovery of an already uncertain refresh remain outside
this acceptance scope.
