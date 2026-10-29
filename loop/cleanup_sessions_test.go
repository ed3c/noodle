package loop

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

type cleanupReadbackRuntime struct {
	*mockRuntime
	read func(loopruntime.CompletedSessionBinding, string, loopruntime.SessionStatus) (loopruntime.CleanupSessionReadback, error)
}

func (r *cleanupReadbackRuntime) ReadCleanupSession(_ context.Context, b loopruntime.CompletedSessionBinding, project string, status loopruntime.SessionStatus) (loopruntime.CleanupSessionReadback, error) {
	return r.read(b, project, status)
}

func TestCleanupObservesEveryOriginalOpenShellAttempt(t *testing.T) {
	for _, variant := range []string{"all-settled", "missing-session", "orphan-session", "duplicate-session", "unknown-runtime", "missing-owner", "prestart-failure", "native-unknown", "source-drift", "wrong-outcome", "unstopped", "wrong-stage", "unsafe-worktree", "active-attempt"} {
		t.Run(variant, func(t *testing.T) {
			l := completedCleanupFixture(t)
			l.projectDir = t.TempDir()
			order := l.canonical.Orders["done"]
			order.Stages[0].Runtime = "openshell"
			order.Stages[0].Provider = "codex"
			order.Stages[0].Model = "original-model"
			statuses := []state.AttemptStatus{state.AttemptCancelled, state.AttemptFailed, state.AttemptCompleted}
			sessions := []string{"a-cancelled", "b-failed", "c-completed"}
			refs := map[string]loopruntime.SessionReadbackFileRef{}
			for i, id := range sessions {
				order.Stages[0].Attempts = append(order.Stages[0].Attempts, state.AttemptNode{AttemptID: "canonical-" + id, SessionID: id, Status: statuses[i], WorktreeName: "removed-" + id})
				path := filepath.Join(l.runtimeDir, "sessions", id)
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				recoveryJSON(t, filepath.Join(path, "meta.json"), map[string]any{"session_id": id, "runtime": "openshell", "status": "exited", "alive": false})
				evidence := filepath.Join(path, "bound-evidence.json")
				recoveryJSON(t, evidence, map[string]any{"session_id": id})
				ref, err := loopruntime.ReadSessionReadbackFile(evidence, nil)
				if err != nil {
					t.Fatal(err)
				}
				refs[id] = ref
			}
			l.canonical.Orders["done"] = order
			original, _ := json.Marshal(l.canonical)
			calls := []string{}
			rt := &cleanupReadbackRuntime{mockRuntime: newMockRuntime()}
			rt.read = func(b loopruntime.CompletedSessionBinding, project string, status loopruntime.SessionStatus) (loopruntime.CleanupSessionReadback, error) {
				calls = append(calls, b.SessionID)
				index := -1
				for i, id := range sessions {
					if id == b.SessionID {
						index = i
					}
				}
				expectedProject, err := canonicalDirectory(l.projectDir)
				if err != nil {
					t.Fatal(err)
				}
				if index < 0 || b.OrderID != "done" || b.StageIndex != 0 || b.RetryCount != index || b.Provider != "codex" || b.Model != "original-model" || project != expectedProject || b.WorktreePath != filepath.Join(project, ".worktrees", "removed-"+b.SessionID) || string(status) != string(statuses[index]) {
					t.Fatalf("original binding differs: %+v %s %s", b, project, status)
				}
				if variant == "prestart-failure" && index == 1 {
					return loopruntime.CleanupSessionReadback{}, errors.New("saved progress unavailable")
				}
				if variant == "native-unknown" {
					return loopruntime.CleanupSessionReadback{}, errors.New("fresh native state unknown")
				}
				if variant == "source-drift" && index == 2 {
					recoveryJSON(t, refs[sessions[0]].Path, map[string]any{"changed": true})
				}
				got := loopruntime.CleanupSessionReadback{OutcomeStatus: status, PublishedHead: "published-" + b.SessionID, Execution: loopruntime.CompletedSessionReadback{NativeExecExitCode: 0, NativePhase: "Stopped", StopAcknowledged: true, Sources: map[string]loopruntime.SessionReadbackFileRef{"bound": refs[b.SessionID]}}}
				if variant == "wrong-outcome" {
					got.OutcomeStatus = loopruntime.StatusCompleted
				}
				if variant == "unstopped" {
					got.Execution.StopAcknowledged = false
				}
				return got, nil
			}
			l.deps.Runtimes = map[string]loopruntime.Runtime{"openshell": rt}
			switch variant {
			case "missing-session":
				if err := os.RemoveAll(filepath.Join(l.runtimeDir, "sessions", sessions[0])); err != nil {
					t.Fatal(err)
				}
			case "orphan-session":
				if err := os.Rename(filepath.Join(l.runtimeDir, "sessions", sessions[0]), filepath.Join(l.runtimeDir, "sessions", "orphan")); err != nil {
					t.Fatal(err)
				}
			case "duplicate-session":
				order.Stages[0].Attempts[1].SessionID = sessions[0]
			case "unknown-runtime":
				order.Stages[0].Runtime = "foreign"
			case "missing-owner":
				delete(l.deps.Runtimes, "openshell")
			case "wrong-stage":
				order.Stages[0].StageIndex = 2
			case "unsafe-worktree":
				order.Stages[0].Attempts[0].WorktreeName = "../foreign"
			case "active-attempt":
				order.Stages[0].Attempts[0].Status = state.AttemptRunning
			}
			l.canonical.Orders["done"] = order
			err := l.cleanupCompleted(false)
			if variant != "all-settled" {
				if err == nil {
					t.Fatal("unsafe cleanup accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(calls, sessions) || len(rt.calls) != 0 {
				t.Fatalf("coverage or dispatch changed: %v", calls)
			}
			after, _ := json.Marshal(l.canonical)
			if string(after) != string(original) {
				t.Fatal("cleanup changed original attempt history")
			}
			if err = observeAdmissionSessions(l.runtimeDir, l.canonical); err == nil {
				t.Fatal("offline admission adopted OpenShell ownership")
			}
		})
	}
}
