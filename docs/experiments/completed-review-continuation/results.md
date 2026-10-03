# Completed review correction custody

Issue: https://github.com/ed3c/noodle/issues/106
Base: e86995ea96ea01d9f56e0598dab6de3d7f0f85e8

A completed candidate can need another correction before publication. The prior
request-changes owner only retained typed-blocked custody. A completed review
therefore lost its recovery binding on restart. A consumed interruption marker
also rejected every ordinary attempt after its single successor.

The existing review owner now accepts a nonblocking completed terminal outcome.
It checks the exact latest attempt, session hashes, absent process group,
registered worktree, clean candidate HEAD and review definition. It records the
reason in the existing custody binding before reducing the request-changes event.
The reducer retains the bound pending review. Prompt editing retains the other
stage fields. Requeue validates custody again and records its readback receipt in
the same checkpoint as the pending transition and review release.

If an active interruption marker exists, request-changes verifies its durable
dispatch result and exact completed successor. It moves that marker into stage
history in the custody checkpoint. Original receipts and session events remain
unchanged. Historical interruption inspect still identifies the original
successor after another ordinary attempt. A later attempt receives no dirty
worktree exception. Normal reset preserves the committed candidate HEAD.

Restart restores custody projections before legacy state can overwrite the
checkpoint. It respects terminal order removal. Reentry cannot recapture a
changed candidate, session or reason. A missing typed outcome cannot bypass an
existing intent. Unknown dispatch results and live processes remain refusals.

No new CLI, scheduler or retry engine was added. Existing request-changes,
edit-item, requeue and mode controls own the continuation. Noodle does not select
Soodles authorization, instructions, judge or projected base. Publication still
requires its selected remote base to be an ancestor of the final candidate.

## Verification

The first completed-review restart control failed with request-changes recovery
binding missing. The implementation repairs that cause. Independent review found
intent recapture, stale requeue receipt and terminal projection risks. Controls
cover each boundary after correction. The broader scoped run also exposed stale
legacy projection writeback; startup and in-memory mirrors now retain custody.

Controls cover interrupted request intent, request acknowledgement and requeue
acknowledgement. They preserve attempts and avoid another dispatch. They also
cover later typed-blocked requeue, historical inspect after the next completed
attempt, and terminal completed/cancelled restart. Planted negatives cover changed
HEAD, prompt and events, a changed reason, dirty candidate, live successor,
foreign successor, changed original session and unknown dispatch result.

Fixtures use real temporary Git repositories and native state/control files.
The existing mock runtime supplies distinct session identities. No model session
or production owner is launched. This is not evidence of real Codex execution.
The parent supervisor owns independent carrier build and acceptance. No live
Soodles #229 files or state were changed.

Actual commands and results are retained in the external task evidence directory:
autopilot-recovery/noodle-review-continuation. The scoped command selects only
completed-review, request-changes, requeue, interruption and publication-claim
controls. It does not run the full test suite. A separate terminal receipt test
checks the final removal correction. Normal go build verifies the executable.

Observed results on 2026-10-03:

- Scoped race controls passed: loop 102.180 s. The reducer package compiled;
  the selected expression matched no package-local reducer tests. The loop
  reducer control exercised the changed reduction through the native owner.
- Final legacy-ack and terminal-removal race controls passed: loop 3.380 s.
- Normal go build passed. git diff --check passed.
- Earlier failed outputs remain in the external evidence. One mock fixture
  reused session IDs, and one assertion compared in-memory time metadata.
  Those fixture assumptions were corrected before reporting the owner result.

Old bindings omit reason. The requeue owner first validates their original
failure events, then copies the exact attempt disposition into the new requeue
receipt. This preserves legacy readback without accepting an arbitrary error.

The final compatibility selection also ran TestControlRequestChanges*,
TestControlReject*, TestReconcileFailedRequestChangesLegalTerminalArchive and
TestCompletedReviewExplicitRejectionRevokesCustody. It passed under race checking
in 4.443 s. The unbound review control confirms ordinary review removal.

The legacy archive fixture now explicitly omits reason. Legacy diagnostic-event
validation remains unchanged. A new persisted intent retains invalid session
evidence for diagnosis and refuses further edits or requeue. An explicit reject
revokes that custody through the existing reject owner. A diagnostic log alone
does not revoke a durable intent. This distinction closes the checkpoint-to-log
interruption window without turning missing evidence into continuation authority.
