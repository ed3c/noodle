package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

func acceptanceFixtureGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Host Fixture", "GIT_AUTHOR_EMAIL=host@example.invalid", "GIT_COMMITTER_NAME=Host Fixture", "GIT_COMMITTER_EMAIL=host@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture git %v: %s: %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}

func prepareTaskReclamationFixture(t *testing.T, h *noodleHost, req *hostRequest) {
	t.Helper()
	acceptanceFixtureGit(t, req.ProjectDir, "init")
	acceptanceFixtureGit(t, req.ProjectDir, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "original candidate")
	head := acceptanceFixtureGit(t, req.ProjectDir, "rev-parse", "HEAD")
	rewriteHostAcceptance(t, req, func(value map[string]any) { value["candidate_head"] = head })
	e, err := execution.New(req.ProjectDir, req.Environment)
	if err != nil {
		t.Fatal(err)
	}
	h.retainTaskAcceptanceReadbackOwner(req.ContextID, e, h.contexts[req.ContextID].environmentSHA)
}

func acceptanceFixtureJSON(t *testing.T, path string, value any) loopruntime.SessionReadbackFileRef {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func writeTaskReclamationFixture(t *testing.T, req hostRequest) loopruntime.ExecutionReclamationReceipt {
	t.Helper()
	authority, err := readHostTaskAcceptanceReceipt(req)
	if err != nil {
		t.Fatal(err)
	}
	var closure struct {
		Order state.OrderNode `json:"order"`
	}
	if _, err := loopruntime.ReadSessionReadbackFile(authority.Path, &closure); err != nil {
		t.Fatal(err)
	}
	acceptance, err := loopruntime.ReadTaskDeliveryAcceptance(req.TaskAcceptance)
	if err != nil {
		t.Fatal(err)
	}
	id := loopruntime.OpenShellIdentity{OrderID: req.ReadbackOrderID, SessionID: req.ReadbackSessionID, AttemptID: "native-attempt", SandboxName: "original-worker", ConfigSource: "original-profile", Sandbox: loopruntime.OpenShellSandboxRef{Gateway: "https://original.invalid", Workspace: "original-workspace", SandboxID: "original-native-id"}}
	nativeClosure := acceptanceFixtureJSON(t, filepath.Join(filepath.Dir(authority.Path), "native-closure.json"), loopruntime.OpenShellClosure{Identity: id, Outcome: loopruntime.StatusCompleted, Reason: "original owner accepted delivery", Evidence: map[string]loopruntime.SessionReadbackFileRef{"task_closure": authority}})
	command := loopruntime.OpenShellCommand{RequestID: "d593da59-10b3-477d-8760-c01d52139a47", Argv: []string{"original-command"}}
	commandHash, err := loopruntime.OpenShellCommandSHA256(command)
	if err != nil {
		t.Fatal(err)
	}
	task := loopruntime.OpenShellTask{OrderID: id.OrderID, SessionID: id.SessionID, AttemptID: id.AttemptID}
	observation := loopruntime.OpenShellCompletedObservation{Identity: id, Task: task, ExecAttemptID: "original-exec", ExecRequestID: command.RequestID, MainProcessInstanceID: "original-main", Phase: "Stopped", CreateAcknowledged: true, StopAcknowledged: true, CommandSHA256: commandHash}
	record := loopruntime.OpenShellCustodyRecord{Task: task, Name: id.SandboxName, CreateRequestID: "original-create", CreateAcknowledged: true, MainProcessInstanceID: observation.MainProcessInstanceID, Worker: loopruntime.OpenShellWorkerRecord{Identity: id}, Exec: &loopruntime.OpenShellCustodyExec{AttemptID: observation.ExecAttemptID, Command: command, Result: loopruntime.OpenShellExecResult{RequestID: command.RequestID, StartAttempted: true, ExitObserved: true}}, StopRequestID: "original-stop", StopAcknowledged: true, Stopped: &loopruntime.OpenShellStoppedObservation{}, Retirement: &loopruntime.OpenShellRetirement{Closure: nativeClosure, Observation: observation, RequestID: "original-delete", Acknowledged: true, Completed: true}}
	custody := acceptanceFixtureJSON(t, filepath.Join(filepath.Dir(authority.Path), "immutable-custody.json"), loopruntime.OpenShellCustodyState{Version: 4, Records: []loopruntime.OpenShellCustodyRecord{record}})
	readback := loopruntime.CleanupSessionReadback{OutcomeStatus: loopruntime.StatusCompleted, PublishedHead: acceptance.CandidateHead, Execution: loopruntime.CompletedSessionReadback{NativeAttemptID: id.AttemptID, MainProcessInstanceID: observation.MainProcessInstanceID, NativePhase: "Stopped", StopAcknowledged: true, Sources: map[string]loopruntime.SessionReadbackFileRef{"native_custody": custody}}, Retirement: &loopruntime.CleanupRetirementReadback{ObservationBasis: "historical_stopped_with_fresh_native_absence", Closure: nativeClosure, DeleteRequestID: record.Retirement.RequestID, DeleteAcknowledged: true, AbsenceObserved: true}}
	value := loopruntime.ExecutionReclamationReceipt{Schema: 1, OrderID: id.OrderID, Authority: authority, Sessions: []loopruntime.ExecutionReclaimedSession{{OrderID: id.OrderID, SessionID: id.SessionID, Readback: readback}}, Worktrees: map[string]string{"worker": acceptance.CandidateHead}}
	acceptanceFixtureJSON(t, filepath.Join(filepath.Dir(authority.Path), hostDigest([]byte(id.OrderID))+".reclaimed.json"), value)
	if err := reducer.WriteSnapshotAtomic(filepath.Join(req.ProjectDir, ".noodle", "state.snapshot.json"), reducer.DurableSnapshot{State: state.State{Orders: map[string]state.OrderNode{id.OrderID: closure.Order}}}); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestHostTaskAcceptanceReclamationRequiresFullOriginalEvidence(t *testing.T) {
	for _, variant := range []string{"reclaimed", "unrelated-order", "missing-reclamation", "missing-canonical", "canonical-drift", "missing-session", "extra-worktree", "candidate-drift", "unknown-delete", "custody-source-drift", "worktree-reappeared", "branch-reappeared", "missing-original-owner", "environment-drift"} {
		t.Run(variant, func(t *testing.T) {
			h, req, rt, _ := automaticTaskAcceptanceFixture(t)
			prepareTaskReclamationFixture(t, h, &req)
			accepted := h.taskAcceptance(t.Context(), req)
			if accepted.Status != "accepted" {
				t.Fatalf("prepare acceptance: %+v", accepted)
			}
			value := writeTaskReclamationFixture(t, req)
			if err := detachAcceptedTaskFixture(h, req); err != nil {
				t.Fatal(err)
			}
			req.Operation = "task_delivery_acceptance_readback"
			reclaimPath := filepath.Join(filepath.Dir(accepted.ClosureReceipt.Path), hostDigest([]byte(req.ReadbackOrderID))+".reclaimed.json")
			snapshotPath := filepath.Join(req.ProjectDir, ".noodle", "state.snapshot.json")
			switch variant {
			case "unrelated-order", "canonical-drift":
				snapshot, err := reducer.ReadSnapshot(snapshotPath)
				if err != nil {
					t.Fatal(err)
				}
				if variant == "unrelated-order" {
					snapshot.State.Orders["another-order"] = state.OrderNode{OrderID: "another-order"}
				} else {
					order := snapshot.State.Orders[req.ReadbackOrderID]
					order.Stages[0].Attempts[0].Error = "replaced canonical attempt"
					snapshot.State.Orders[req.ReadbackOrderID] = order
				}
				if err := reducer.WriteSnapshotAtomic(snapshotPath, snapshot); err != nil {
					t.Fatal(err)
				}
			case "missing-canonical":
				if err := os.Remove(snapshotPath); err != nil {
					t.Fatal(err)
				}
			case "missing-reclamation":
				if err := os.Remove(reclaimPath); err != nil {
					t.Fatal(err)
				}
			case "missing-session":
				value.Sessions = nil
				acceptanceFixtureJSON(t, reclaimPath, value)
			case "extra-worktree":
				value.Worktrees["foreign"] = value.Worktrees["worker"]
				acceptanceFixtureJSON(t, reclaimPath, value)
			case "candidate-drift":
				value.Sessions[0].Readback.PublishedHead = strings.Repeat("b", 40)
				value.Worktrees["worker"] = strings.Repeat("b", 40)
				acceptanceFixtureJSON(t, reclaimPath, value)
			case "unknown-delete":
				value.Sessions[0].Readback.Retirement.DeleteAcknowledged = false
				acceptanceFixtureJSON(t, reclaimPath, value)
			case "custody-source-drift":
				closureTestDrift(t, value.Sessions[0].Readback.Execution.Sources["native_custody"].Path)
			case "worktree-reappeared":
				if err := os.MkdirAll(filepath.Join(req.ProjectDir, ".worktrees", "worker"), 0700); err != nil {
					t.Fatal(err)
				}
			case "branch-reappeared":
				acceptanceFixtureGit(t, req.ProjectDir, "update-ref", "refs/heads/noodle/worker", value.Worktrees["worker"])
			case "missing-original-owner":
				delete(h.taskReadbacks, req.ContextID)
			case "environment-drift":
				original := h.taskReadbacks[req.ContextID]
				original.environmentSHA = "replacement"
				h.taskReadbacks[req.ContextID] = original
			}
			got := h.taskAcceptanceReadback(req)
			if variant == "reclaimed" || variant == "unrelated-order" {
				if got.Status != "reclaimed" || got.Error != "" || got.Reclamation == nil || got.ReclamationReceipt == nil || got.ObservationBasis != "original_owner_reclamation_readback" || got.Sources["reclamation_receipt"] != *got.ReclamationReceipt {
					t.Fatalf("readback proof: %+v", got)
				}
			} else if got.Status == "reclaimed" || got.Error == "" || got.Reclamation != nil {
				t.Fatalf("incomplete readback claimed reclaimed: %+v", got)
			}
			if len(got.Effects) != 0 || rt.calls.Load() != 1 || rt.shutdowns != 0 {
				t.Fatal("readback caused a mutation")
			}
		})
	}
}

func TestHostTaskAcceptanceReclamationReplyIsBound(t *testing.T) {
	for _, variant := range []string{"lost-response", "wrong-basis", "wrong-receipt", "wrong-readback", "wrong-source", "effects", "canonical-drift-after-read"} {
		t.Run(variant, func(t *testing.T) {
			h, req, rt, path := automaticTaskAcceptanceFixture(t)
			prepareTaskReclamationFixture(t, h, &req)
			if got := h.taskAcceptance(t.Context(), req); got.Status != "accepted" {
				t.Fatalf("prepare acceptance: %+v", got)
			}
			writeTaskReclamationFixture(t, req)
			if err := detachAcceptedTaskFixture(h, req); err != nil {
				t.Fatal(err)
			}
			serveTaskAcceptanceOnce(t, h, func(result *hostTaskAcceptance) bool {
				if result.Status != "reclaimed" {
					t.Errorf("prepare readback: %+v", result)
					return false
				}
				switch variant {
				case "lost-response":
					return false
				case "wrong-basis":
					result.ObservationBasis = "fresh_native_absence"
				case "wrong-receipt":
					result.ReclamationReceipt.SHA256 = strings.Repeat("0", 64)
				case "wrong-readback":
					result.Reclamation.Sessions[0].Readback.PublishedHead = strings.Repeat("0", 40)
				case "wrong-source":
					source := result.Sources["reclamation_receipt"]
					source.SHA256 = strings.Repeat("0", 64)
					result.Sources["reclamation_receipt"] = source
				case "effects":
					result.Effects = []string{"unexpected_mutation"}
				case "canonical-drift-after-read":
					snapshotPath := filepath.Join(req.ProjectDir, ".noodle", "state.snapshot.json")
					snapshot, err := reducer.ReadSnapshot(snapshotPath)
					if err != nil {
						t.Error(err)
						return false
					}
					delete(snapshot.State.Orders, req.ReadbackOrderID)
					if err := reducer.WriteSnapshotAtomic(snapshotPath, snapshot); err != nil {
						t.Error(err)
						return false
					}
				}
				return true
			})
			got, err := callHostTaskAcceptanceReadback(t.Context(), req.ProjectDir, path, req.ReadbackOrderID, req.ReadbackSessionID, req.TaskAcceptance.Path, req.TaskAcceptance.SHA256)
			if err == nil || got.Status != "unknown" || len(got.Effects) != 0 || got.Reclamation != nil || got.ClosureReceipt != nil || rt.calls.Load() != 1 {
				t.Fatalf("unresolved readback claimed success or replayed: %+v %v", got, err)
			}
		})
	}
}
