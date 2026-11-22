package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestArchiveRecoveryCleanupRequiresSameOwnerLanding(t *testing.T) {
	for _, test := range []struct {
		name, refusal string
	}{
		{name: "same candidate and resolved Landing"},
		{name: "owner never recorded proof", refusal: "not retained"},
		{name: "foreign candidate", refusal: "original request or candidate differs"},
		{name: "provider CI used another head", refusal: "Provider effects differ"},
		{name: "provider close not yet observed", refusal: "Provider effects differ"},
		{name: "pending checkpoint", refusal: "Cloud Landing is not resolved"},
		{name: "provider snapshot later replaced", refusal: "source changed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			write := func(name string, value any) loopruntime.SessionReadbackFileRef {
				t.Helper()
				path := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
				if err != nil {
					t.Fatal(err)
				}
				return ref
			}
			digest := func(hexDigit string) string { return strings.Repeat(hexDigit, 40) }
			head, tree, base, merge := digest("a"), digest("b"), digest("c"), digest("d")
			plan := archiveRecoveryCleanupPlan{OrderID: "order", AttemptID: "attempt", SessionID: "session",
				Head: head, Tree: tree, Request: write("host-request.json", map[string]any{"status": "accepted"}),
				Candidate: write("native/archive-recovery-candidate.json", map[string]any{"kind": "archive_recovery_candidate"})}
			request := write("request.json", map[string]any{"mode": "inner-only"})
			envelope := write("task-envelope.json", map[string]any{"order_id": "order"})
			soodlesCandidate := write("candidates/original.json", map[string]any{
				"kind": "archive_recovery", "original_return": "UNKNOWN", "task_envelope": envelope, "archive_receipt": plan.Candidate,
				"order_id": plan.OrderID, "attempt_id": plan.AttemptID, "session_id": plan.SessionID, "head": head, "tree": tree,
			})
			issue := write("issue.json", map[string]any{"number": 19, "html_url": "https://github.com/owner/repo/issues/19"})
			review := write("review_"+soodlesCandidate.SHA256+"-archive-review.json", map[string]any{
				"schema": 1, "kind": "archive_recovery", "original_issue": issue, "task_envelope": envelope,
				"review": map[string]any{"kind": "archive_recovery", "decision": "approve", "request": request, "candidate": soodlesCandidate,
					"reviewer":   map[string]any{"session_id": "original-main"},
					"acceptance": map[string]any{"scope": "candidate", "conditions": []map[string]any{{"status": "PASS"}}}},
			})
			publication := write("publication.json", map[string]any{"result": map[string]any{
				"owner": "soodles.candidate-publication", "status": "created", "continuation_state": "complete", "completion_scope": "publication",
				"repository": "owner/repo", "subject": "owner/repo#19", "head": head, "tree": tree, "pr": map[string]any{"number": 20},
			}})
			const closedAt = "2026-11-22T02:00:00Z"
			phase := "resolved"
			if test.name == "pending checkpoint" {
				phase = "close_pending"
			}
			landing := write("activation/checkpoint.json", map[string]any{
				"schema": 2, "phase": phase, "classification": "RESOLVED", "observations": []string{"observed"},
				"merge_sha": merge, "issue_closed_at": closedAt, "writes_offered": []string{"merge", "close"},
				"claim": map[string]any{"repository": "owner/repo", "issue": 19, "pr": 20, "head": head, "tree": tree, "base_head": base, "run_id": 81, "run_attempt": 2},
				"provider_reconciliation": map[string]any{
					"mode": "cloud", "provider": "GitHub", "main_head": merge, "merge_sha": merge, "local_reconciliation_required": false,
				},
			})
			ciHead, issueState := head, "closed"
			if test.name == "provider CI used another head" {
				ciHead = digest("e")
			}
			if test.name == "provider close not yet observed" {
				issueState = "open"
			}
			provider := write("provider-snapshot.json", map[string]any{
				"issue": map[string]any{"number": 19, "state": issueState, "state_reason": "completed", "closed_at": closedAt},
				"pr":    map[string]any{"number": 20, "state": "closed", "merged": true, "merge_commit_sha": merge, "head": map[string]string{"sha": head}},
				"merge_commit": map[string]any{"sha": merge, "tree": map[string]string{"sha": tree},
					"parents": []map[string]string{{"sha": base}, {"sha": head}}},
				"branch": map[string]any{"commit": map[string]string{"sha": merge}},
				"run":    map[string]any{"id": 81, "run_attempt": 2, "head_sha": ciHead, "status": "completed", "conclusion": "success"},
			})
			proof := archiveRecoveryLandingProof{Schema: 1, Kind: "archive_recovery_landed", Request: request,
				TaskEnvelope: envelope, NativeRequest: plan.Request, Candidate: plan.Candidate, Review: review,
				Publication: publication, LandingCheckpoint: landing, ProviderSnapshot: provider, OriginalReturn: "UNKNOWN"}
			if test.name == "foreign candidate" {
				proof.Candidate = write("native/foreign.json", map[string]any{"kind": "archive_recovery_candidate"})
			}
			plan.LandingProof = write("archive-landing-proof.json", proof)
			owner := map[string]any{"archive_recovery_adoption": map[string]any{
				"schema": 1, "request": request, "task_envelope": envelope, "candidate": soodlesCandidate,
				"main_session_id": "original-main", "original_return": "UNKNOWN", "order_id": plan.OrderID,
				"attempt_id": plan.AttemptID, "session_id": plan.SessionID, "head": head, "tree": tree,
			}}
			if test.name != "owner never recorded proof" {
				owner["archive_landing"] = plan.LandingProof
			}
			ownerRef := write("task-owner.json", owner)
			if test.name == "provider snapshot later replaced" {
				write("provider-snapshot.json", map[string]any{"fake": true})
			}
			err = verifyArchiveRecoveryLanding(plan)
			if test.refusal != "" && (err == nil || !strings.Contains(err.Error(), test.refusal)) {
				t.Fatalf("expected %q, got %v", test.refusal, err)
			}
			if test.refusal == "" && err != nil {
				t.Fatal(err)
			}
			if err := readArchiveCleanupRef(ownerRef, nil); err != nil {
				t.Fatalf("read-only plan changed original Owner: %v", err)
			}
		})
	}
}

func TestArchiveRecoveryCleanupMachineLeaseBindsOriginalSession(t *testing.T) {
	var owner hostOwnerBinding
	owner.HostRoot, owner.Current.HostInstanceID = "/original/host", "generation"
	expected := admission.MachineSessionBinding{ContextID: "context", ProjectDir: "/project", OrderID: "order",
		AttemptID: "attempt", SessionID: "session", Runtime: "openshell", WorktreePath: "/project/.worktrees/original"}
	lease := map[string]any{"owner": hostMachineGenerationOwner(owner.HostRoot, owner.Current.HostInstanceID),
		"source": "original-profile", "effects_possible": true, "session_binding": expected}
	path := filepath.Join(t.TempDir(), "capacity.json")
	write := func() {
		t.Helper()
		bytes, err := json.Marshal(map[string]any{"leases": map[string]any{"worker": lease}})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, bytes, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if _, err := readArchiveRecoveryCleanupLease(path, &owner, expected); err != nil {
		t.Fatal(err)
	}
	foreign := expected
	foreign.AttemptID = "foreign"
	lease["session_binding"] = foreign
	write()
	if _, err := readArchiveRecoveryCleanupLease(path, &owner, expected); err == nil {
		t.Fatal("foreign logical attempt inherited original lease")
	}
	lease["session_binding"], lease["settlement"] = expected, map[string]any{"outcome": "already_settled"}
	write()
	if _, err := readArchiveRecoveryCleanupLease(path, &owner, expected); err == nil {
		t.Fatal("an already settled lease was offered")
	}
}
