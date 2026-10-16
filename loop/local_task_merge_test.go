package loop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/worktree"
)

func localTaskRealWorktree(t *testing.T, l *Loop) {
	t.Helper()
	app, err := worktree.NewAppWithExecution(execution.Capture(l.projectDir))
	if err != nil {
		t.Fatal(err)
	}
	app.Quiet = true
	if err := bindContextIntegration(app, l.runtimeDir); err != nil {
		t.Fatal(err)
	}
	l.deps.Worktree = app
}

func TestLocalTaskMergeCommitsOnlyReviewedBytesIntoControlBranch(t *testing.T) {
	l, cook, cmd := localTaskReviewFixture(t)
	localTaskRealWorktree(t, l)
	cmd.Action = "merge-local-task"
	if err := l.dispatchControlCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if string(recoveryRead(t, filepath.Join(l.projectDir, "candidate.txt"))) != "reviewed candidate\n" {
		t.Fatal("candidate not integrated")
	}
	if branch, err := recoveryGit(l.projectDir, "branch", "--show-current"); err != nil || branch != "control" {
		t.Fatalf("branch %s: %v", branch, err)
	}
	if l.canonical.Orders[cook.orderID].Status != state.OrderCompleted {
		t.Fatalf("not completed: %+v", l.canonical.Orders[cook.orderID])
	}
	if _, err := os.Stat(cook.worktreePath); !os.IsNotExist(err) {
		t.Fatal("successful candidate not cleaned up")
	}
}

func TestLocalTaskMergeDoesNotSkipReviewedCandidateForShadowBranch(t *testing.T) {
	l, cook, cmd := localTaskReviewFixture(t)
	localTaskRealWorktree(t, l)
	// Native cooks use noodle/<name>; a bare branch may independently name the base.
	runGitInRepo(t, cook.worktreePath, "branch", "-m", "noodle/"+cook.worktreeName)
	base, err := recoveryGit(l.projectDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	runGitInRepo(t, l.projectDir, "branch", cook.worktreeName, base)
	cmd.Action = "merge-local-task"
	if err := l.dispatchControlCommand(cmd); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(l.projectDir, "candidate.txt"))
	if err != nil || string(actual) != "reviewed candidate\n" {
		t.Fatalf("reviewed candidate was not integrated: bytes=%q error=%v order=%s", actual, err, l.canonical.Orders[cook.orderID].Status)
	}
	var binding localTaskMergeBinding
	if err := json.Unmarshal(l.canonical.Orders[cook.orderID].Stages[0].Extra[localTaskMergeKey], &binding); err != nil {
		t.Fatal(err)
	}
	if head, err := recoveryGit(l.projectDir, "rev-parse", "HEAD"); err != nil || head != binding.Head {
		t.Fatalf("control HEAD does not match reviewed commit: %s error=%v", head, err)
	}
	if shadow, err := recoveryGit(l.projectDir, "rev-parse", cook.worktreeName); err != nil || shadow != base {
		t.Fatalf("unrelated branch changed: %s error=%v", shadow, err)
	}
	if l.canonical.Orders[cook.orderID].Status != state.OrderCompleted {
		t.Fatal("reviewed merge did not complete")
	}
}

func TestLocalTaskMergeRecoveryUsesReviewedCommitInsteadOfBranch(t *testing.T) {
	for _, scenario := range []string{"unmerged-shadow", "merged-cleanup", "advanced-integration"} {
		t.Run(scenario, func(t *testing.T) {
			l, cook, cmd := localTaskReviewFixture(t)
			localTaskRealWorktree(t, l)
			base, err := recoveryGit(l.projectDir, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "merged-cleanup" {
				runGitInRepo(t, cook.worktreePath, "branch", "-m", "noodle/"+cook.worktreeName)
				runGitInRepo(t, l.projectDir, "branch", cook.worktreeName, base)
			}
			// Stop the original merge after its durable StageMerging transition.
			// A dirty control checkout makes the real merge owner refuse before landing.
			l.mergeQueue.Close()
			l.mergeQueue = nil
			ignorePath := filepath.Join(l.projectDir, ".gitignore")
			ignore := recoveryRead(t, ignorePath)
			requestChangesWrite(t, ignorePath, append(append([]byte{}, ignore...), []byte("# merge barrier\n")...))
			cmd.Action = "merge-local-task"
			if err := l.dispatchControlCommand(cmd); err == nil {
				t.Fatal("dirty integration root did not prevent original merge")
			}
			stage := l.canonical.Orders[cook.orderID].Stages[0]
			if stage.Status != state.StageMerging {
				t.Fatal("original control did not persist a pending merge")
			}
			requestChangesWrite(t, ignorePath, ignore)
			var binding localTaskMergeBinding
			if err := json.Unmarshal(stage.Extra[localTaskMergeKey], &binding); err != nil {
				t.Fatal(err)
			}
			if scenario == "merged-cleanup" {
				if err := l.deps.Worktree.Merge(cook.worktreeName, worktree.MergeOpts{ExpectedHead: binding.Head, ExpectedTree: binding.Tree}); err != nil {
					t.Fatal(err)
				}
			} else if scenario == "advanced-integration" {
				requestChangesWrite(t, filepath.Join(l.projectDir, "other.txt"), []byte("independent change\n"))
				runGitInRepo(t, l.projectDir, "add", "other.txt")
				runGitInRepo(t, l.projectDir, "commit", "-m", "advance integration")
			}
			l = New(l.projectDir, "noodle", l.config, l.deps)
			if loaded, err := l.loadCanonicalSnapshot(); err != nil || !loaded {
				t.Fatalf("reload original merge: loaded=%v error=%v", loaded, err)
			}
			if err := l.reconcileMergingStages(); err != nil {
				t.Fatal(err)
			}
			if err := l.drainMergeResults(context.Background()); err != nil {
				t.Fatal(err)
			}
			if scenario == "advanced-integration" {
				if l.canonical.Orders[cook.orderID].Stages[0].Status != state.StageReview {
					t.Fatal("integration drift did not retain candidate for review")
				}
				if _, err := os.Stat(filepath.Join(l.projectDir, "candidate.txt")); !os.IsNotExist(err) {
					t.Fatal("integration drift landed candidate")
				}
				if _, err := os.Stat(cook.worktreePath); err != nil {
					t.Fatal("integration drift lost candidate worktree")
				}
				return
			}
			actual, err := os.ReadFile(filepath.Join(l.projectDir, "candidate.txt"))
			if err != nil || string(actual) != "reviewed candidate\n" {
				t.Fatalf("recovery did not integrate candidate: bytes=%q error=%v order=%s", actual, err, l.canonical.Orders[cook.orderID].Status)
			}
			if head, err := recoveryGit(l.projectDir, "rev-parse", "HEAD"); err != nil || head != binding.Head {
				t.Fatalf("recovery HEAD differs from reviewed commit: %s error=%v", head, err)
			}
			if l.canonical.Orders[cook.orderID].Status != state.OrderCompleted {
				t.Fatal("landed candidate did not complete after restart")
			}
		})
	}
}

func TestLocalTaskMergeRejectsDriftBeforeCommit(t *testing.T) {
	for _, change := range []string{"bytes", "index", "foreign", "branch", "mode", "unbound"} {
		t.Run(change, func(t *testing.T) {
			l, cook, cmd := localTaskReviewFixture(t)
			localTaskRealWorktree(t, l)
			cmd.Action = "merge-local-task"
			original, _ := recoveryGit(cook.worktreePath, "rev-parse", "HEAD")
			switch change {
			case "bytes":
				requestChangesWrite(t, filepath.Join(cook.worktreePath, "candidate.txt"), []byte("changed"))
			case "index":
				runGitInRepo(t, cook.worktreePath, "add", "candidate.txt")
			case "foreign":
				requestChangesWrite(t, filepath.Join(cook.worktreePath, "foreign.txt"), []byte("foreign"))
			case "branch":
				runGitInRepo(t, l.projectDir, "checkout", "-b", "foreign")
			case "mode":
				if err := os.Chmod(filepath.Join(cook.worktreePath, "candidate.txt"), 0755); err != nil {
					t.Fatal(err)
				}
			case "unbound":
				cmd.Action = "merge"
			}
			if err := l.dispatchControlCommand(cmd); err == nil {
				t.Fatal("accepted candidate drift")
			}
			after, _ := recoveryGit(cook.worktreePath, "rev-parse", "HEAD")
			if after != original {
				t.Fatal("refusal committed")
			}
			if l.canonical.Orders[cook.orderID].Stages[0].Status != state.StageReview {
				t.Fatal("refusal lost review")
			}
		})
	}
}

func localTaskLostMergeFixture(t *testing.T) (*Loop, *cookHandle, ControlCommand) {
	t.Helper()
	l, cook, candidate := localTaskReviewFixture(t)
	backup := t.TempDir()
	requestChangesWrite(t, filepath.Join(backup, "candidate.txt"), recoveryRead(t, filepath.Join(cook.worktreePath, "candidate.txt")))
	packet := filepath.Join(t.TempDir(), "recovery.json")
	if _, err := PrepareLocalTaskMergeRecovery(candidate.Value, candidate.Target, backup, packet); err != nil {
		t.Fatal(err)
	}
	if err := l.failStage(context.Background(), cook, "merge failed: integration branch differs"); err != nil {
		t.Fatal(err)
	}
	recoveryWriteJSON(t, filepath.Join(l.runtimeDir, "control-ack.ndjson"), ControlAck{ID: "original-merge", Action: "merge", Status: "ok"})
	payload, _ := json.Marshal(map[string]any{"order_id": cook.orderID, "stage_index": 0, "worktree_name": cook.worktreeName})
	l.effectLedger.Record(reducer.Effect{EffectID: "old-merge", Type: reducer.EffectMerge, Payload: payload})
	if err := l.persistCanonicalCheckpoint(); err != nil {
		t.Fatal(err)
	}
	runGitInRepo(t, l.projectDir, "worktree", "remove", "--force", cook.worktreePath)
	runGitInRepo(t, l.projectDir, "branch", "-D", cook.worktreeName)
	return l, cook, ControlCommand{ID: "recover-merge", Action: "recover-local-task-merge", OrderID: cook.orderID, Value: packet, Target: recoveryDigest(recoveryRead(t, packet)), Name: "original-merge"}
}

func TestLocalTaskMergeRecoveryRestoresOriginalBytesWithoutNewAttempt(t *testing.T) {
	l, cook, cmd := localTaskLostMergeFixture(t)
	before := l.canonical.Orders[cook.orderID].Stages[0].Attempts[0]
	if err := l.dispatchControlCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if err := l.dispatchControlCommand(cmd); err != nil {
		t.Fatalf("readback: %v", err)
	}
	stage := l.canonical.Orders[cook.orderID].Stages[0]
	if stage.Status != state.StageReview || !recoveryJSONEqual(before, stage.Attempts[0]) {
		t.Fatal("original failed history changed")
	}
	if string(recoveryRead(t, filepath.Join(cook.worktreePath, "candidate.txt"))) != "reviewed candidate\n" {
		t.Fatal("candidate not restored")
	}
	if len(l.cooks.activeCooksByOrder) != 0 {
		t.Fatal("recovery dispatched")
	}
	for _, r := range l.effectLedger.All() {
		if r.EffectID == "old-merge" && (r.Status != reducer.EffectLedgerDone || r.Result.Status != reducer.EffectResultCancelled || r.Attempts != 0) {
			t.Fatal("old merge was replayed")
		}
	}
	l = New(l.projectDir, "noodle", l.config, l.deps)
	if err := l.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, r := range l.effectLedger.All() {
		if r.EffectID == "old-merge" && r.Attempts != 0 {
			t.Fatal("restart invented cancelled execution")
		}
	}
	localTaskRealWorktree(t, l)
	var packet localTaskMergeRecoveryPacket
	if err := json.Unmarshal(recoveryRead(t, cmd.Value), &packet); err != nil {
		t.Fatal(err)
	}
	if err := l.dispatchControlCommand(ControlCommand{Action: "merge-local-task", OrderID: cook.orderID, Value: packet.Candidate.Path, Target: packet.Candidate.SHA256}); err != nil {
		t.Fatalf("new reviewed merge: %v", err)
	}
	if !recoveryJSONEqual(before, l.canonical.Orders[cook.orderID].Stages[0].Attempts[0]) {
		t.Fatal("merge erased failed history")
	}
}

func TestLocalTaskMergeRecoveryRejectsChangedSourcesBeforeRestoration(t *testing.T) {
	for _, change := range []string{"ack", "backup", "source", "scope", "active-effect"} {
		t.Run(change, func(t *testing.T) {
			l, cook, cmd := localTaskLostMergeFixture(t)
			switch change {
			case "ack":
				cmd.Name = "foreign"
			case "source":
				cmd.Target = "changed"
			case "scope":
				l.config.Runtime.Process.AdmittedOrders = nil
			case "active-effect":
				if err := l.effectLedger.MarkRunning("old-merge"); err != nil {
					t.Fatal(err)
				}
			case "backup":
				var p localTaskMergeRecoveryPacket
				if err := json.Unmarshal(recoveryRead(t, cmd.Value), &p); err != nil {
					t.Fatal(err)
				}
				requestChangesWrite(t, p.Files["candidate.txt"].Path, []byte("changed"))
			}
			before, _ := json.Marshal(l.canonical)
			if err := l.dispatchControlCommand(cmd); err == nil {
				t.Fatal("accepted changed recovery source")
			}
			after, _ := json.Marshal(l.canonical)
			if string(before) != string(after) {
				t.Fatal("refusal changed canonical")
			}
			if _, err := os.Stat(cook.worktreePath); !os.IsNotExist(err) {
				t.Fatal("refusal recreated worktree")
			}
		})
	}
}

func TestLocalTaskMergeCASRejectsLateDirtyOrIntegrationDrift(t *testing.T) {
	for _, change := range []string{"head", "tree", "dirty", "integration"} {
		t.Run(change, func(t *testing.T) {
			l, cook, _ := localTaskReviewFixture(t)
			localTaskRealWorktree(t, l)
			runGitInRepo(t, cook.worktreePath, "add", "candidate.txt")
			runGitInRepo(t, cook.worktreePath, "commit", "-m", "reviewed candidate")
			head, _ := recoveryGit(cook.worktreePath, "rev-parse", "HEAD")
			tree, _ := recoveryGit(cook.worktreePath, "rev-parse", "HEAD^{tree}")
			switch change {
			case "head":
				head = "foreign"
			case "tree":
				tree = "foreign"
			case "dirty":
				requestChangesWrite(t, filepath.Join(cook.worktreePath, "late.txt"), []byte("late"))
			case "integration":
				requestChangesWrite(t, filepath.Join(l.projectDir, "new-control.txt"), []byte("control"))
				runGitInRepo(t, l.projectDir, "add", "new-control.txt")
				runGitInRepo(t, l.projectDir, "commit", "-m", "control advanced")
			}
			if err := l.deps.Worktree.Merge(cook.worktreeName, worktree.MergeOpts{ExpectedHead: head, ExpectedTree: tree}); err == nil {
				t.Fatal("accepted drift")
			}
			if _, err := os.Stat(cook.worktreePath); err != nil {
				t.Fatal("refusal lost candidate")
			}
			if _, err := os.Stat(filepath.Join(l.projectDir, "candidate.txt")); !os.IsNotExist(err) {
				t.Fatal("refusal integrated candidate")
			}
		})
	}
}

func TestLocalTaskMergeRecoveryRefusesSymlinkBeforeRestoringBytes(t *testing.T) {
	l, cook, cmd := localTaskLostMergeFixture(t)
	runGitInRepo(t, l.projectDir, "worktree", "add", "-b", cook.worktreeName, cook.worktreePath)
	external := filepath.Join(t.TempDir(), "source")
	requestChangesWrite(t, external, []byte("external bytes"))
	if err := os.Symlink(external, filepath.Join(cook.worktreePath, "candidate.txt")); err != nil {
		t.Fatal(err)
	}
	if err := l.dispatchControlCommand(cmd); err == nil {
		t.Fatal("restored through symlink")
	}
	if string(recoveryRead(t, external)) != "external bytes" {
		t.Fatal("external bytes changed")
	}
	if l.canonical.Orders[cook.orderID].Status != state.OrderFailed {
		t.Fatal("refusal changed custody")
	}
}
