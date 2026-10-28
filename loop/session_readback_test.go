package loop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/statever"
	loopruntime "github.com/poteto/noodle/runtime"
)

type readbackRuntime struct {
	*mockRuntime
	calls int
	read  func(loopruntime.CompletedSessionBinding) (loopruntime.CompletedSessionReadback, error)
}

func (r *readbackRuntime) ReadCompletedSession(_ context.Context, b loopruntime.CompletedSessionBinding) (loopruntime.CompletedSessionReadback, error) {
	r.calls++
	return r.read(b)
}
func TestSessionReadbackKeepsCanonicalAndNativeAttemptsDistinct(t *testing.T) {
	for _, variant := range []string{"observed", "wrong-order", "wrong-session", "duplicate", "process", "stage-location", "source-drift"} {
		t.Run(variant, func(t *testing.T) {
			project := t.TempDir()
			runtimeDir := filepath.Join(project, ".noodle")
			for _, path := range []string{runtimeDir, filepath.Join(project, ".worktrees", "worker")} {
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			canonicalPath := filepath.Join(runtimeDir, "state.snapshot.json")
			attempt := state.AttemptNode{AttemptID: "canonical-attempt", SessionID: "session", Status: state.AttemptFailed, WorktreeName: "worker"}
			stage := state.StageNode{StageIndex: 0, Runtime: "openshell", Provider: "codex", Model: "model", Status: state.StageFailed, Attempts: []state.AttemptNode{attempt}}
			if variant == "duplicate" {
				stage.Attempts = append(stage.Attempts, attempt)
			}
			if variant == "stage-location" {
				stage.StageIndex = 1
			}
			if variant == "process" {
				stage.Runtime = "process"
			}
			snapshot := reducer.DurableSnapshot{State: state.State{SchemaVersion: statever.Current, Orders: map[string]state.OrderNode{"order": {OrderID: "order", Status: state.OrderFailed, Stages: []state.StageNode{stage}}}}}
			raw, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(canonicalPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			rt := &readbackRuntime{mockRuntime: newMockRuntime()}
			rt.read = func(b loopruntime.CompletedSessionBinding) (loopruntime.CompletedSessionReadback, error) {
				expected, _ := canonicalDirectory(filepath.Join(project, ".worktrees", "worker"))
				if b.OrderID != "order" || b.SessionID != "session" || b.WorktreePath != expected || b.Provider != "codex" || b.Model != "model" || b.StageIndex != 0 || b.RetryCount != 0 {
					t.Fatalf("runtime binding differs: %+v", b)
				}
				if variant == "source-drift" {
					if err = os.WriteFile(canonicalPath, append(raw, ' '), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return loopruntime.CompletedSessionReadback{NativeAttemptID: "native-attempt", CandidateHead: "candidate", Sources: map[string]loopruntime.SessionReadbackFileRef{}}, nil
			}
			l := &Loop{projectDir: project, runtimeDir: runtimeDir, deps: Dependencies{Runtimes: map[string]loopruntime.Runtime{"openshell": rt}}}
			order, session := "order", "session"
			if variant == "wrong-order" {
				order = "other"
			}
			if variant == "wrong-session" {
				session = "other"
			}
			observed, err := l.SessionReadback(t.Context(), order, session)
			if variant != "observed" {
				if err == nil {
					t.Fatal("invalid canonical binding accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if observed.Status != "observed" || observed.AttemptID != "canonical-attempt" || observed.Execution.NativeAttemptID != "native-attempt" || observed.AttemptStatus != "failed" || len(observed.Effects) != 0 || rt.calls != 1 || len(rt.mockRuntime.calls) != 0 {
				t.Fatalf("observation granted acceptance or replayed: %+v", observed)
			}
			current, _ := os.ReadFile(canonicalPath)
			if string(current) != string(raw) {
				t.Fatal("readback changed canonical status")
			}
		})
	}
}
