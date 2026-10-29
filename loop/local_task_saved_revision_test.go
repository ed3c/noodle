package loop

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/taskreg"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/skill"
)

func TestLocalTaskSavedProgressAfterRequestChangesRevision(t *testing.T) {
	for _, outcome := range []string{"cancelled", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			l, cand, original := remoteInputFixture(t, "pi")
			l.registry = taskreg.NewFromSkills([]skill.SkillMeta{{Name: "local-task", Path: "/skills/local-task", Frontmatter: skill.Frontmatter{Schedule: "admitted task"}}})
			review := l.canonical.PendingReviews[cand.OrderID]
			review.Prompt = cand.Stage.Prompt
			l.canonical.PendingReviews[cand.OrderID] = review
			l.cooks.pendingReview[cand.OrderID].stage.Prompt = cand.Stage.Prompt
			if err := l.projectRecoveredOrder(cand.OrderID); err != nil {
				t.Fatal(err)
			}
			if err := l.controlRequestChanges(cand.OrderID, "correct the result"); err != nil {
				t.Fatal(err)
			}
			if err := l.controlRequeue(cand.OrderID); err != nil {
				t.Fatal(err)
			}
			history := append([]byte(nil), l.canonical.Orders[cand.OrderID].Stages[0].Extra[requestChangesRequeuedKey]...)
			rt := &savedRuntime{readbackRuntime: original}
			l.deps.Runtimes["openshell"] = rt
			var offered state.OrderNode
			var beforeOffer []byte
			rt.dispatchHook = func(req loopruntime.DispatchRequest) (loopruntime.SessionHandle, error) {
				if req.RetryCount == 2 {
					var checkpoint reducer.DurableSnapshot
					if err := json.Unmarshal(recoveryRead(t, l.canonicalSnapshotPath()), &checkpoint); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(beforeOffer, &offered); err != nil {
						t.Fatal(err)
					}
					// DispatchRequested has already marked the checkpoint dispatching.
					// Keep the actual stopped state and only add the newly persisted offer.
					offered.Stages[0].Extra[savedProgressAttemptsKey] = checkpoint.State.Orders[cand.OrderID].Stages[0].Extra[savedProgressAttemptsKey]
				}
				s := &mockSession{id: fmt.Sprintf("revision-session-%d", req.RetryCount), status: "running", done: make(chan struct{})}
				rt.sessions = append(rt.sessions, s)
				return s, nil
			}
			t.Cleanup(func() {
				for _, s := range rt.sessions {
					s.complete("cancelled")
				}
				l.watcherWG.Wait()
			})
			if err := l.spawnCookAdmitted(t.Context(), cand, Order{ID: cand.OrderID}, spawnOptions{attempt: 1}); err != nil {
				t.Fatal(err)
			}
			if len(rt.mockRuntime.calls) != 1 {
				t.Fatal("original revision did not dispatch")
			}
			req := rt.mockRuntime.calls[0]
			stopped := rt.sessions[0].ID()
			dir := filepath.Join(l.runtimeDir, "sessions", stopped)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			recoveryWriteJSON(t, filepath.Join(dir, "spawn.json"), map[string]any{"session_id": stopped, "instruction_bundle": req.InstructionBundle, "reasoning_level": req.ReasoningLevel, "skill": req.Skill})
			requestChangesWrite(t, filepath.Join(dir, "prompt.txt"), []byte(req.Prompt))
			requestChangesWrite(t, filepath.Join(req.WorktreePath, "candidate.txt"), []byte("saved revision progress\n"))
			runGitInRepo(t, req.WorktreePath, "add", "candidate.txt")
			runGitInRepo(t, req.WorktreePath, "commit", "-m", "Save stopped revision progress")
			head, err := recoveryGit(req.WorktreePath, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			rt.saved = func(b loopruntime.CompletedSessionBinding) (loopruntime.SavedProgressReadback, error) {
				if b.SessionID != stopped || b.RetryCount != 1 || b.OrderID != cand.OrderID || b.Provider != req.Provider || b.Model != req.Model {
					t.Fatalf("saved binding differs: %+v", b)
				}
				spawn, err := loopruntime.ReadSessionReadbackFile(filepath.Join(dir, "spawn.json"), nil)
				return loopruntime.SavedProgressReadback{Status: "saved", OutcomeStatus: loopruntime.SessionStatus(outcome), HarnessExitCode: 143, ProgressHead: head, ProfileSHA256: strings.Repeat("b", 64), CommandSHA256: strings.Repeat("c", 64), NativeAttemptID: "stopped-revision", NativePhase: "Stopped", StopAcknowledged: true, Instructions: &loopruntime.CompletedInstructionEvidence{Manifest: loopruntime.SessionReadbackFileRef{Path: req.InstructionBundle.Path, SHA256: req.InstructionBundle.SHA256}}, Sources: map[string]loopruntime.SessionReadbackFileRef{"spawn.json": spawn}}, err
			}
			rt.sessions[0].complete(outcome)
			l.watcherWG.Wait()
			l.drainCompletions(t.Context())
			l.setState(StatePaused)
			if err := l.persistCanonicalCheckpoint(); err != nil {
				t.Fatal(err)
			}
			beforeOffer, _ = json.Marshal(l.canonical.Orders[cand.OrderID])
			pending := l.canonical.PendingReviews[cand.OrderID]
			observed, err := l.InspectSavedProgress(t.Context(), cand.OrderID, stopped)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = l.QueueSavedProgressAttempt(t.Context(), cand.OrderID, stopped, observed.Version); err != nil {
				t.Fatal(err)
			}
			if err = l.processControlCommands(); err != nil {
				t.Fatal(err)
			}
			if len(rt.mockRuntime.calls) != 2 {
				_, ack, _ := l.controlPaths()
				t.Fatalf("saved successor did not dispatch: %s", recoveryRead(t, ack))
			}
			next := rt.mockRuntime.calls[1]
			if next.RetryCount != 2 || next.ResumeSessionID != "" || next.Provider != req.Provider || next.Model != req.Model || next.ReasoningLevel != req.ReasoningLevel || !recoveryJSONEqual(next.InstructionBundle, req.InstructionBundle) || next.WorktreePath != req.WorktreePath || !strings.Contains(next.Prompt, head) {
				t.Fatalf("successor lost original inputs: %+v", next)
			}
			node := l.canonical.Orders[cand.OrderID]
			attempts := node.Stages[0].Attempts
			if len(attempts) != 3 || !recoveryJSONEqual(attempts[:2], offered.Stages[0].Attempts) || attempts[1].Status != state.AttemptStatus(outcome) || attempts[1].SessionID != stopped || attempts[2].SessionID == stopped || string(node.Stages[0].Extra[requestChangesRequeuedKey]) != string(history) {
				t.Fatalf("original revision history changed: %+v", attempts)
			}
			if string(recoveryRead(t, filepath.Join(req.WorktreePath, "candidate.txt"))) != "saved revision progress\n" {
				t.Fatal("saved files were reset")
			}
			if _, err = l.QueueSavedProgressAttempt(t.Context(), cand.OrderID, stopped, observed.Version); err == nil {
				t.Fatal("consumed consent replay accepted")
			}
			// Recheck the exact pre-dispatch offer with each binding independently changed.
			var receipt requestChangesRequeued
			if err = json.Unmarshal(history, &receipt); err != nil {
				t.Fatal(err)
			}
			offeredRaw, _ := json.Marshal(offered)
			for _, change := range []string{"none", "session", "next-attempt", "version", "command", "head", "review", "history", "missing-grant", "stale-last-grant", "stage-running", "predecessor-completed"} {
				t.Run(change, func(t *testing.T) {
					var n state.OrderNode
					if err := json.Unmarshal(offeredRaw, &n); err != nil {
						t.Fatal(err)
					}
					l.canonical.PendingReviews[cand.OrderID] = pending
					var grants []savedProgressAttempt
					if err := json.Unmarshal(n.Stages[0].Extra[savedProgressAttemptsKey], &grants); err != nil {
						t.Fatal(err)
					}
					switch change {
					case "session":
						grants[0].SessionID = "other"
					case "next-attempt":
						grants[0].NextAttemptID = "other"
					case "version":
						grants[0].Version = "bad"
					case "command":
						grants[0].CommandID = "other"
					case "head":
						grants[0].ProgressHead = "other"
					case "review":
						r := pending
						r.SessionID = "other"
						l.canonical.PendingReviews[cand.OrderID] = r
					case "history":
						n.Stages[0].Attempts[0].SessionID = "other"
					case "missing-grant":
						grants = nil
					case "stale-last-grant":
						grants = append(grants, grants[0])
						grants[1].NextAttemptID = "stale"
					case "stage-running":
						n.Stages[0].Status = state.StageRunning
					case "predecessor-completed":
						n.Stages[0].Attempts[1].Status = state.AttemptCompleted
					}
					n.Stages[0].Extra[savedProgressAttemptsKey], _ = json.Marshal(grants)
					ok, err := l.localTaskSavedRevisionForDispatch(cand, 2, n, receipt)
					if change == "none" {
						if err != nil || !ok {
							t.Fatalf("original offer refused: %v", err)
						}
					} else if err == nil || ok {
						t.Fatalf("changed %s accepted", change)
					}
				})
			}
		})
	}
}
