package loop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/workspaceartifacts"
	loopruntime "github.com/poteto/noodle/runtime"
)

func artifactOwnerFixture(t *testing.T) (*Loop, completedTaskAcceptanceRequest, string) {
	t.Helper()
	l, request := completedAcceptanceFixture(t)
	if err := l.acceptCompletedTask(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if err := l.persistCanonicalCheckpoint(); err != nil {
		t.Fatal(err)
	}
	project, err := filepath.EvalSymlinks(l.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	l.projectDir, l.runtimeDir = project, filepath.Join(project, ".noodle")
	dir := filepath.Join(l.runtimeDir, "sessions", request.SessionID)
	order := l.canonical.Orders[request.OrderID]
	stage := order.Stages[0]
	a := stage.Attempts[len(stage.Attempts)-1]
	target := filepath.Join(project, ".worktrees", a.WorktreeName)
	var acceptance loopruntime.TaskDeliveryAcceptance
	if _, err := loopruntime.ReadSessionReadbackFile(request.Acceptance.Path, &acceptance); err != nil {
		t.Fatal(err)
	}
	baseline := &workspaceartifacts.Baseline{Directory: filepath.Join(dir, "repo"), Head: acceptance.CandidateHead}
	delivery := &workspaceartifacts.Result{Applied: true, State: "published", PatchPath: filepath.Join(dir, "delivery", "changes.patch"), ReturnedDirectory: filepath.Join(dir, "returned")}
	recoveryWriteJSON(t, filepath.Join(dir, "openshell-dispatch.json"), map[string]any{
		"Version": 1, "ContextID": "original-context", "OrderID": request.OrderID, "SessionID": request.SessionID,
		"AttemptID": "native-attempt", "Target": target, "Phase": "settled", "ProfileSHA256": recoveryDigest([]byte("profile")),
		"CandidateHead": acceptance.CandidateHead, "Baseline": baseline, "Delivery": delivery,
		"Outcome": loopruntime.SessionOutcome{Status: loopruntime.StatusCompleted, ExitCode: 0},
		"Worker":  loopruntime.OpenShellWorkerRecord{Identity: loopruntime.OpenShellIdentity{OrderID: request.OrderID, SessionID: request.SessionID, AttemptID: "native-attempt"}, Boundary: loopruntime.OpenShellBoundaryStopped},
	})
	recoveryWriteJSON(t, filepath.Join(dir, "spawn.json"), map[string]any{"session_id": request.SessionID, "order_id": request.OrderID, "stage_index": "0", "retry_count": len(stage.Attempts) - 1, "runtime": "openshell", "provider": stage.Provider, "model": stage.Model, "worktree_path": target})
	recoveryWriteJSON(t, filepath.Join(dir, "meta.json"), map[string]any{"session_id": request.SessionID, "runtime": "openshell", "provider": stage.Provider, "model": stage.Model, "alive": false, "status": "exited"})
	recoveryWriteJSON(t, filepath.Join(l.runtimeDir, "status.json"), map[string]any{"loop_state": "paused", "active": []string{}})
	recoveryWriteJSON(t, filepath.Join(l.runtimeDir, "control-ack.ndjson"), ControlAck{ID: "pause-original", Action: "pause", Status: "ok", DispatchState: StatePaused})
	return l, request, dir
}

func TestArtifactCompactionOwnerRequiresOriginalClosedPausedAdmission(t *testing.T) {
	for _, variant := range []string{"valid", "running", "duplicate-ack", "pending-control", "foreign-context", "canonical-drift", "explicit-delete", "acceptance-drift", "active-meta", "foreign-spawn"} {
		t.Run(variant, func(t *testing.T) {
			l, request, dir := artifactOwnerFixture(t)
			contextID := "original-context"
			switch variant {
			case "running":
				recoveryWriteJSON(t, filepath.Join(l.runtimeDir, "status.json"), map[string]any{"loop_state": "running"})
			case "duplicate-ack":
				p := filepath.Join(l.runtimeDir, "control-ack.ndjson")
				raw, _ := os.ReadFile(p)
				if err := os.WriteFile(p, append(raw, append([]byte("\n"), raw...)...), 0600); err != nil {
					t.Fatal(err)
				}
			case "pending-control":
				if err := os.WriteFile(filepath.Join(l.runtimeDir, "control.ndjson"), []byte("{}\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "foreign-context":
				contextID = "foreign"
			case "canonical-drift", "explicit-delete":
				p := filepath.Join(l.runtimeDir, "state.snapshot.json")
				var s reducer.DurableSnapshot
				if _, err := loopruntime.ReadSessionReadbackFile(p, &s); err != nil {
					t.Fatal(err)
				}
				if variant == "canonical-drift" {
					o := s.State.Orders[request.OrderID]
					o.Status = state.OrderFailed
					s.State.Orders[request.OrderID] = o
				} else {
					for i := range s.EffectLedger {
						if s.EffectLedger[i].Effect.Type == reducer.EffectInitialAdmission {
							var v map[string]any
							_ = json.Unmarshal(s.EffectLedger[i].Effect.Payload, &v)
							v["task_data_retention"] = "explicit_delete"
							s.EffectLedger[i].Effect.Payload, _ = json.Marshal(v)
						}
					}
				}
				recoveryWriteJSON(t, p, s)
			case "acceptance-drift":
				recoveryWriteJSON(t, request.Acceptance.Path, map[string]any{"status": "UNKNOWN"})
			case "active-meta":
				recoveryWriteJSON(t, filepath.Join(dir, "meta.json"), map[string]any{"alive": true})
			case "foreign-spawn":
				recoveryWriteJSON(t, filepath.Join(dir, "spawn.json"), map[string]any{"session_id": "foreign"})
			}
			got, err := ReadArtifactCompactionOwner(l.projectDir, l.config, request.OrderID, request.SessionID, contextID, "pause-original")
			if variant != "valid" {
				if err == nil {
					t.Fatal("unsafe continuation owner accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Binding.SessionID != request.SessionID || got.Head == "" || got.CanonicalSHA256 == "" || len(got.Sources) < 4 {
				t.Fatalf("incomplete owner: %+v", got.Binding)
			}
		})
	}
}
