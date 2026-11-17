package loop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/statever"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
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
	for _, variant := range []string{"observed", "durable", "durable-unbound", "wrong-order", "wrong-session", "duplicate", "process", "stage-location", "source-drift"} {
		t.Run(variant, func(t *testing.T) {
			project, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			runtimeDir := filepath.Join(project, ".noodle")
			var owner *execution.Context
			if variant == "durable" || variant == "durable-unbound" {
				var app *worktree.App
				var storage string
				owner, app, storage = durableReadbackFixture(t, project)
				if err = app.Create("worker"); err != nil {
					t.Fatal(err)
				}
				if variant == "durable-unbound" {
					if err = os.RemoveAll(filepath.Join(storage, "worktree-bindings")); err != nil {
						t.Fatal(err)
					}
				}
			}
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
				expected := filepath.Join(project, ".worktrees", "worker")
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
			l := &Loop{projectDir: project, runtimeDir: runtimeDir, deps: Dependencies{Execution: owner, Runtimes: map[string]loopruntime.Runtime{"openshell": rt}}}
			order, session := "order", "session"
			if variant == "wrong-order" {
				order = "other"
			}
			if variant == "wrong-session" {
				session = "other"
			}
			observed, err := l.SessionReadback(t.Context(), order, session)
			if variant != "observed" && variant != "durable" {
				if err == nil {
					t.Fatal("invalid canonical binding accepted")
				}
				if variant == "durable-unbound" && rt.calls != 0 {
					t.Fatal("unbound worker reached native reader")
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

func durableReadbackFixture(t *testing.T, project string) (*execution.Context, *worktree.App, string) {
	t.Helper()
	gitOutputInRepo(t, project, "init", "-q")
	gitOutputInRepo(t, project, "-c", "user.name=Test", "-c", "user.email=test@invalid", "commit", "--allow-empty", "-m", "fixture")
	owner, err := execution.New(project, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	app, appErr := worktree.NewAppWithExecution(owner)
	if appErr != nil {
		t.Fatal(appErr)
	}
	app.Quiet = true
	storage, _ := filepath.EvalSymlinks(t.TempDir())
	for _, name := range []string{".noodle", ".worktrees"} {
		if err = os.Mkdir(filepath.Join(storage, name), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.Symlink(filepath.Join(storage, name), filepath.Join(project, name)); err != nil {
			t.Fatal(err)
		}
	}
	source, _ := json.Marshal(worktree.StorageSource{Project: project, CommonDir: app.CommonDir, StorageRoot: storage})
	sum := sha256.Sum256([]byte(owner.Root()))
	ownerDir := filepath.Join(app.CommonDir, "noodle-contexts", hex.EncodeToString(sum[:]))
	if err = os.MkdirAll(ownerDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(storage, "source.json"), filepath.Join(ownerDir, "source.json")} {
		if err = os.WriteFile(path, source, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return owner, app, storage
}

func TestEnsureWorktreeResumesOriginalCustodyAfterPublicationFailure(t *testing.T) {
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	owner, app, storage := durableReadbackFixture(t, project)
	bindings := filepath.Join(storage, "worktree-bindings")
	if err = os.WriteFile(bindings, []byte("temporarily unavailable"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = app.Create("worker"); err == nil {
		t.Fatal("publication failure did not reject worker preparation")
	}
	logical := worktree.WorktreePath(project, "worker")
	if _, err = worktree.ReadCustody(t.Context(), owner, logical); err == nil {
		t.Fatal("unpublished worker accepted")
	}
	dirty := filepath.Join(logical, "retained.txt")
	if err = os.WriteFile(dirty, []byte("original bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(bindings); err != nil {
		t.Fatal(err)
	}
	l := &Loop{projectDir: project, deps: Dependencies{Worktree: app}}
	created, err := l.ensureWorktree("worker")
	if err != nil || created {
		t.Fatalf("original owner did not resume publication: %v %v", created, err)
	}
	if _, err = worktree.ReadCustody(t.Context(), owner, logical); err != nil {
		t.Fatal(err)
	}
	if created, err = l.ensureWorktree("worker"); err != nil || created {
		t.Fatalf("repeated owner readback differs: %v %v", created, err)
	}
	data, err := os.ReadFile(dirty)
	if err != nil || string(data) != "original bytes" {
		t.Fatal("recovery changed original worker data")
	}
}
