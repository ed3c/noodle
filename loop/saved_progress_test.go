package loop

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/dispatcher"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/taskreg"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/skill"
)

type savedRuntime struct {
	*readbackRuntime
	saved func(loopruntime.CompletedSessionBinding) (loopruntime.SavedProgressReadback, error)
}

func (r *savedRuntime) ReadSavedProgress(_ context.Context, b loopruntime.CompletedSessionBinding) (loopruntime.SavedProgressReadback, error) {
	return r.saved(b)
}

func savedProgressFixture(t *testing.T) (*Loop, string, string, *savedRuntime) {
	t.Helper()
	l, cand, old := remoteInputFixture(t, "pi")
	l.registry = taskreg.NewFromSkills([]skill.SkillMeta{{Name: "local-task", Path: "/skills/local-task", Frontmatter: skill.Frontmatter{Schedule: "admitted task"}}})
	node := l.canonical.Orders[cand.OrderID]
	s := node.Stages[0]
	s.Attempts[0].Status = state.AttemptCancelled
	node.Stages[0] = s
	l.canonical.Orders[cand.OrderID] = node
	session := s.Attempts[0].SessionID
	dir := filepath.Join(l.runtimeDir, "sessions", session)
	bundle := &dispatcher.InstructionBundleRef{Path: filepath.Join(l.projectDir, "frozen-manifest.json"), SHA256: strings.Repeat("a", 64)}
	recoveryWriteJSON(t, filepath.Join(dir, "spawn.json"), map[string]any{"instruction_bundle": bundle, "reasoning_level": "high", "skill": "local-task"})
	requestChangesWrite(t, filepath.Join(dir, "prompt.txt"), []byte(buildCookPrompt(node.OrderID, cand.Stage, node.Plan, node.Title, node.Rationale, "")))
	rt := &savedRuntime{readbackRuntime: old}
	rt.saved = func(b loopruntime.CompletedSessionBinding) (loopruntime.SavedProgressReadback, error) {
		if b.OrderID != cand.OrderID || b.SessionID != session || b.RetryCount != 0 || b.Provider != "pi" || b.Model != cand.Stage.Model {
			t.Fatalf("wrong original binding: %+v", b)
		}
		spawn, err := loopruntime.ReadSessionReadbackFile(filepath.Join(dir, "spawn.json"), nil)
		if err != nil {
			return loopruntime.SavedProgressReadback{}, err
		}
		return loopruntime.SavedProgressReadback{Status: "saved", OutcomeStatus: loopruntime.StatusCancelled, HarnessExitCode: 143, ProgressHead: "saved-head", ProfileSHA256: strings.Repeat("b", 64), CommandSHA256: strings.Repeat("c", 64), NativeAttemptID: "original-native", NativePhase: "Stopped", StopAcknowledged: true, Instructions: &loopruntime.CompletedInstructionEvidence{Manifest: loopruntime.SessionReadbackFileRef{Path: bundle.Path, SHA256: bundle.SHA256}}, Sources: map[string]loopruntime.SessionReadbackFileRef{"spawn.json": spawn}}, nil
	}
	l.deps.Runtimes["openshell"] = rt
	l.setState(StatePaused)
	if err := l.persistCanonicalCheckpoint(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, session := range rt.sessions {
			session.complete("cancelled")
		}
		l.watcherWG.Wait()
	})
	return l, cand.OrderID, session, rt
}

func TestSavedProgressInspectionIsReadOnlyAndClockStable(t *testing.T) {
	l, order, session, rt := savedProgressFixture(t)
	before := recoveryRead(t, l.canonicalSnapshotPath())
	first, err := l.InspectSavedProgress(t.Context(), order, session)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "retryable" || first.NativeResumeSupported || first.AttemptStatus != state.AttemptCancelled || first.Execution.ProgressHead != "saved-head" || len(first.Effects) != 0 || len(rt.mockRuntime.calls) != 0 {
		t.Fatalf("inspection changed authority: %+v", first)
	}
	if string(before) != string(recoveryRead(t, l.canonicalSnapshotPath())) {
		t.Fatal("inspection changed checkpoint")
	}
	var snapshot reducer.DurableSnapshot
	if err = json.Unmarshal(before, &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.GeneratedAt = time.Now().Add(time.Hour)
	recoveryWriteJSON(t, l.canonicalSnapshotPath(), snapshot)
	second, err := l.InspectSavedProgress(t.Context(), order, session)
	if err != nil || first.Version != second.Version {
		t.Fatalf("clock invalidated consent: %v", err)
	}
}

func TestSavedProgressInspectionRefusesUnresolvedOrChangedBindings(t *testing.T) {
	for _, kind := range []string{"wrong-session", "not-latest", "completed", "unknown-native", "missing-stop", "changed-prompt", "changed-effort", "changed-materialization", "running-effect", "source-drift"} {
		t.Run(kind, func(t *testing.T) {
			l, order, session, rt := savedProgressFixture(t)
			original := rt.saved
			node := l.canonical.Orders[order]
			switch kind {
			case "wrong-session":
				session = "other"
			case "not-latest":
				node.Stages[0].Attempts = append(node.Stages[0].Attempts, state.AttemptNode{AttemptID: "next", SessionID: "next"})
			case "completed":
				node.Stages[0].Attempts[0].Status = state.AttemptCompleted
			case "changed-prompt":
				node.Stages[0].Prompt = strings.ReplaceAll(node.Stages[0].Prompt, "fix candidate", "different task")
			case "changed-effort":
				node.Stages[0].Prompt = strings.ReplaceAll(node.Stages[0].Prompt, "high", "low")
			case "changed-materialization":
				node.Stages[0].Prompt = strings.ReplaceAll(node.Stages[0].Prompt, strings.Repeat("a", 64), strings.Repeat("d", 64))
			case "running-effect":
				l.effectLedger.Record(reducer.Effect{EffectID: "running", Type: reducer.EffectDispatch})
				if err := l.effectLedger.MarkRunning("running"); err != nil {
					t.Fatal(err)
				}
			default:
				rt.saved = func(b loopruntime.CompletedSessionBinding) (loopruntime.SavedProgressReadback, error) {
					r, e := original(b)
					switch kind {
					case "unknown-native":
						return r, errors.New("native stop unknown")
					case "missing-stop":
						r.StopAcknowledged = false
					case "source-drift":
						requestChangesWrite(t, r.Sources["spawn.json"].Path, []byte("{}"))
					}
					return r, e
				}
			}
			l.canonical.Orders[order] = node
			if err := l.persistCanonicalCheckpoint(); err != nil {
				t.Fatal(err)
			}
			if _, err := l.InspectSavedProgress(t.Context(), order, session); err == nil {
				t.Fatal("invalid stopped progress accepted")
			}
			if len(rt.mockRuntime.calls) != 0 {
				t.Fatal("inspection dispatched")
			}
		})
	}
}

func TestSavedProgressNewAttemptUsesOriginalQueueAndPreservesHistory(t *testing.T) {
	l, order, session, rt := savedProgressFixture(t)
	observed, err := l.InspectSavedProgress(t.Context(), order, session)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := l.QueueSavedProgressAttempt(t.Context(), order, session, observed.Version)
	if err != nil {
		t.Fatal(err)
	}
	if queued.Status != "queued" || len(rt.mockRuntime.calls) != 0 {
		t.Fatal("queue acknowledged worker start")
	}
	if err = l.processControlCommands(); err != nil {
		t.Fatal(err)
	}
	if len(rt.mockRuntime.calls) != 1 {
		path, ack, _ := l.controlPaths()
		t.Fatalf("dispatch calls %d queue=%s ack=%s", len(rt.mockRuntime.calls), recoveryRead(t, path), recoveryRead(t, ack))
	}
	req := rt.mockRuntime.calls[0]
	actualWorktree, err := canonicalDirectory(req.WorktreePath)
	if err != nil {
		t.Fatal(err)
	}
	if req.RetryCount != 1 || req.ResumeSessionID != "" || req.Provider != "pi" || req.Model != observed.Model || actualWorktree != observed.WorktreePath || req.ReasoningLevel != "high" || !strings.Contains(req.Prompt, "saved-head") {
		t.Fatalf("new attempt lost predecessor binding: %+v", req)
	}
	node := l.canonical.Orders[order]
	if len(node.Stages[0].Attempts) != 2 || node.Stages[0].Attempts[0].Status != state.AttemptCancelled || node.Stages[0].Attempts[0].SessionID != session || node.Stages[0].Attempts[1].SessionID == session || l.state != StatePaused {
		t.Fatalf("history or dispatch hold changed: %+v", node)
	}
	if _, err = l.QueueSavedProgressAttempt(t.Context(), order, session, observed.Version); err == nil {
		t.Fatal("old consent replay accepted")
	}
	if string(recoveryRead(t, filepath.Join(observed.WorktreePath, "candidate.txt"))) != "reviewed candidate\n" {
		t.Fatal("saved progress reset")
	}
}

func TestSavedProgressQueueStaleAndDeferredHaveNoDispatch(t *testing.T) {
	l, order, session, rt := savedProgressFixture(t)
	observed, err := l.InspectSavedProgress(t.Context(), order, session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.QueueSavedProgressAttempt(t.Context(), order, session, "stale"); err == nil {
		t.Fatal("stale consent queued")
	}
	path, _, _ := l.controlPaths()
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("stale request changed queue")
	}
	b := resourceBudget(l, 1)
	b.Observe(admission.Sample{At: time.Now(), Pressure: admission.Warning}, nil)
	if _, err = l.QueueSavedProgressAttempt(t.Context(), order, session, observed.Version); err != nil {
		t.Fatal(err)
	}
	if err = l.processControlCommands(); err != nil {
		t.Fatal(err)
	}
	if len(rt.mockRuntime.calls) != 0 || len(l.canonical.Orders[order].Stages[0].Extra[savedProgressAttemptsKey]) != 0 {
		t.Fatal("resource deferral admitted new attempt")
	}
	b.Observe(admission.Sample{At: time.Now(), Unsupported: true}, nil)
	if err = l.processControlCommands(); err != nil {
		t.Fatal(err)
	}
	if len(rt.mockRuntime.calls) != 1 {
		t.Fatal("explicit queued consent did not dispatch after resources recovered")
	}
}

func TestSavedProgressQueueRevalidatesBeforeDispatchAndOfferIsOneShot(t *testing.T) {
	for _, kind := range []string{"changed-before-consume", "dispatch-refused"} {
		t.Run(kind, func(t *testing.T) {
			l, order, session, rt := savedProgressFixture(t)
			inspected, err := l.InspectSavedProgress(t.Context(), order, session)
			if err != nil {
				t.Fatal(err)
			}
			queued, err := l.QueueSavedProgressAttempt(t.Context(), order, session, inspected.Version)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "changed-before-consume" {
				node := l.canonical.Orders[order]
				node.Stages[0].Prompt = strings.ReplaceAll(node.Stages[0].Prompt, "fix candidate", "different task")
				l.canonical.Orders[order] = node
				if err = l.persistCanonicalCheckpoint(); err != nil {
					t.Fatal(err)
				}
			} else {
				rt.dispatchHook = func(loopruntime.DispatchRequest) (loopruntime.SessionHandle, error) {
					return nil, errors.New("local preflight refused")
				}
			}
			if err = l.processControlCommands(); err != nil {
				t.Fatal(err)
			}
			_, ack, _ := l.controlPaths()
			if !strings.Contains(string(recoveryRead(t, ack)), `"status":"error"`) {
				t.Fatal("refused new attempt reported success")
			}
			want := 0
			if kind == "dispatch-refused" {
				want = 1
			}
			if len(rt.mockRuntime.calls) != want {
				t.Fatalf("dispatch count=%d", len(rt.mockRuntime.calls))
			}
			if kind == "dispatch-refused" {
				node := l.canonical.Orders[order]
				if len(node.Stages[0].Extra[savedProgressAttemptsKey]) == 0 || node.Stages[0].Attempts[0].Status != state.AttemptCancelled {
					t.Fatal("offer or predecessor history lost after refusal")
				}
				if err = l.dispatchControlCommand(ControlCommand{ID: queued.CommandID, Action: "retry-saved-progress", OrderID: order, Name: session, Value: inspected.Version}); err == nil {
					t.Fatal("offer replay accepted")
				}
				if len(rt.mockRuntime.calls) != 1 {
					t.Fatal("offer dispatched twice")
				}
			}
		})
	}
}
