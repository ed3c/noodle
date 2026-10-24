# Session workspace discovery

A repository or workspace is enough to start `host status`. The reader uses
existing Git worktree metadata, Host receipts and canonical reducer snapshots.
There is no new registry, home traversal, automatic latest selection or runtime
startup. Each returned selection contains argv with that candidate's workspace,
context and Host instance, so discovery from a primary checkout can continue in
a linked checkout explicitly.

`loop.ReadWorkspaceInspection` is the shared pure source reader for Host status,
detached view enrichment and review inspection. It hashes the decoded snapshot's
exact bytes, lists existing known workspace/session files, and reports missing,
invalid and unsafe sources. A missing snapshot does not hide other retained
session sources. Context receipts and workspace orders are separate observations;
project equality alone never joins every order to each historical context.
All inventory is marked `persisted_only` with `live_verified=false`.

Historical request files remain optional explicit data selectors, decoded
without locks, writes or original-binary requirements. Exact native tuples can
instead select detach, recover-unstarted, resident and display association paths.
Mutations still acquire their original locks, reject ambiguous or changed data,
revalidate native context/effect/process facts and preserve request/lease dedup.
Read access and mutation admission are separate conditions.

Display source associations retain prior associations even when a previous
context has stopped or its receipt is missing. Missing facts are saved as gaps;
conflicting or duplicate recorded identities still fail. State flags do not
require either an active successor or a detached predecessor just to associate
retained display data. Such associations never transfer native process handles.

Session responsibility does not mean guessing missing facts, adopting a foreign
process, weakening a digest or interpreting an ACK as completed effects.

Selected `host status` starts with persisted facts and attempts an optional
read-only live observation without starting Host. Version mismatch, refusal or
Host absence preserves the selected facts and a named gap, with live_verified
false. A successful exact live reply preserves status ok and sets live_verified
true; effects can retain their existing live preflight. `--persisted-only` skips
contact completely. Upgrading a running Host is never a read prerequisite.
