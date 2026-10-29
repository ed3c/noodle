package loop

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

func openShellLocalTaskFixture(t *testing.T, provider string) (*Loop, *cookHandle, ControlCommand, *readbackRuntime) {
	t.Helper()
	l, cook, cmd := localTaskReviewFixtureRoute(t, "candidate.txt", "candidate.txt", "openshell", provider)
	head, err := recoveryGit(cook.worktreePath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	rt := &readbackRuntime{mockRuntime: newMockRuntime()}
	rt.read = func(b loopruntime.CompletedSessionBinding) (loopruntime.CompletedSessionReadback, error) {
		if b.OrderID != cook.orderID || b.SessionID != cook.session.ID() || b.WorktreePath != cook.worktreePath || b.Provider != provider || b.Model != cook.stage.Model || b.StageIndex != 0 || b.RetryCount != 0 {
			t.Fatalf("original completion binding differs: %+v", b)
		}
		ref, err := loopruntime.ReadSessionReadbackFile(filepath.Join(l.runtimeDir, "sessions", b.SessionID, "openshell-dispatch.json"), nil)
		if err != nil {
			return loopruntime.CompletedSessionReadback{}, err
		}
		return loopruntime.CompletedSessionReadback{NativeAttemptID: "native-attempt", CandidateHead: head, NativeExecExitCode: 0, NativePhase: "Stopped", StopAcknowledged: true, MainProcessInstanceID: "original-main", Sources: map[string]loopruntime.SessionReadbackFileRef{"dispatch": ref}}, nil
	}
	l.deps.Runtimes["openshell"] = rt
	return l, cook, cmd, rt
}

func TestOpenShellLocalTaskReviewedPublishedCandidateMerges(t *testing.T) {
	for _, provider := range []string{"codex", "pi"} {
		t.Run(provider, func(t *testing.T) {
			l, cook, cmd, rt := openShellLocalTaskFixture(t, provider)
			localTaskRealWorktree(t, l)
			if err := l.controlMerge(cook.orderID); err == nil {
				t.Fatal("unreviewed local candidate merged")
			}
			cmd.Action = "merge-local-task"
			if err := l.dispatchControlCommand(cmd); err != nil {
				t.Fatal(err)
			}
			if rt.calls == 0 || len(rt.mockRuntime.calls) != 0 || l.canonical.Orders[cook.orderID].Status != state.OrderCompleted || string(recoveryRead(t, filepath.Join(l.projectDir, "candidate.txt"))) != "reviewed candidate\n" {
				t.Fatal("review failed to consume original proof and integrate exact candidate")
			}
			if _, err := os.Stat(filepath.Join(l.runtimeDir, "sessions", cook.session.ID(), "process.json")); !os.IsNotExist(err) {
				t.Fatal("OpenShell review invented host process evidence")
			}
		})
	}
}

func TestOpenShellLocalTaskRefusesMissingOrChangedOriginalProof(t *testing.T) {
	for _, kind := range []string{"missing-owner", "unknown-execution", "wrong-candidate", "running", "missing-ack", "missing-attempt", "missing-main", "failed-exec", "source-drift"} {
		t.Run(kind, func(t *testing.T) {
			l, cook, cmd, rt := openShellLocalTaskFixture(t, "pi")
			localTaskRealWorktree(t, l)
			original := rt.read
			rt.read = func(b loopruntime.CompletedSessionBinding) (loopruntime.CompletedSessionReadback, error) {
				result, err := original(b)
				switch kind {
				case "unknown-execution":
					return result, errors.New("original gateway unavailable")
				case "wrong-candidate":
					result.CandidateHead = "other"
				case "running":
					result.NativePhase = "Ready"
				case "missing-ack":
					result.StopAcknowledged = false
				case "missing-attempt":
					result.NativeAttemptID = ""
				case "missing-main":
					result.MainProcessInstanceID = ""
				case "failed-exec":
					result.NativeExecExitCode = 1
				case "source-drift":
					requestChangesWrite(t, result.Sources["dispatch"].Path, []byte("changed"))
				}
				return result, err
			}
			if kind == "missing-owner" {
				delete(l.deps.Runtimes, "openshell")
			}
			before, err := recoveryGit(l.projectDir, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			cmd.Action = "merge-local-task"
			if err := l.dispatchControlCommand(cmd); err == nil {
				t.Fatal("unproven original execution accepted")
			}
			after, err := recoveryGit(l.projectDir, "rev-parse", "HEAD")
			if err != nil || before != after || l.canonical.Orders[cook.orderID].Stages[0].Status != state.StageReview || len(rt.mockRuntime.calls) != 0 {
				t.Fatal("refusal changed candidate acceptance or dispatched")
			}
		})
	}
}

func TestOpenShellMergeRestorationRefusesBeforeEffects(t *testing.T) {
	l, cook, cmd, rt := openShellLocalTaskFixture(t, "codex")
	packet := localTaskMergeRecoveryPacket{Schema: 1, Candidate: localTaskFileRef{Path: cmd.Value, SHA256: cmd.Target}, Files: map[string]localTaskFileRef{}}
	source := filepath.Join(t.TempDir(), "restore.json")
	recoveryWriteJSON(t, source, packet)
	cmd.Action, cmd.Value, cmd.Target = "recover-local-task-merge", source, recoveryDigest(recoveryRead(t, source))
	before, err := recoveryGit(cook.worktreePath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	err = l.controlRecoverLocalTaskMerge(cmd)
	if err == nil || !strings.Contains(err.Error(), "OpenShell merge restoration is unavailable") {
		t.Fatalf("unsupported restoration was not explicit: %v", err)
	}
	after, err := recoveryGit(cook.worktreePath, "rev-parse", "HEAD")
	if err != nil || after != before || rt.calls != 0 || len(rt.mockRuntime.calls) != 0 {
		t.Fatal("unsupported restoration changed original execution or candidate")
	}
}

func TestOpenShellLocalTaskRequestChangesKeepsHistoryForExplicitNewAttempt(t *testing.T) {
	l, cook, _, rt := openShellLocalTaskFixture(t, "codex")
	if err := l.controlRequestChanges(cook.orderID, "correct the result"); err != nil {
		t.Fatal(err)
	}
	if err := l.controlRequeue(cook.orderID); err != nil {
		t.Fatal(err)
	}
	stage := l.canonical.Orders[cook.orderID].Stages[0]
	if stage.Status != state.StagePending || len(stage.Attempts) != 1 || stage.Attempts[0].Status != state.AttemptFailed || rt.calls == 0 || len(rt.mockRuntime.calls) != 0 {
		t.Fatal("explicit new attempt erased history, dispatched, or skipped original proof")
	}
	cand := dispatchCandidate{OrderID: cook.orderID, StageIndex: 0, Stage: cook.stage}
	if prior, err := l.resumeTaskSessionID(cand, len(stage.Attempts)); err != nil || prior != "" {
		t.Fatalf("new OpenShell attempt was converted into native session resume: %q %v", prior, err)
	}
}
