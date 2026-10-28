package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

type cleanupSessionReader interface {
	ReadCleanupSession(context.Context, loopruntime.CompletedSessionBinding, string, loopruntime.SessionStatus) (loopruntime.CleanupSessionReadback, error)
}

type cleanupAttempt struct {
	binding  loopruntime.CompletedSessionBinding
	worktree string
	runtime  string
	status   loopruntime.SessionStatus
}

// Called by the quiescent original Loop actor. No recovery or runtime creation
// is available here. Final gateway inventory/quiescence belongs to lease release.
func (l *Loop) observeCleanupSessions() error {
	expected := map[string]cleanupAttempt{}
	for key, order := range l.canonical.Orders {
		for index, stage := range order.Stages {
			for retry, attempt := range stage.Attempts {
				if key != order.OrderID || index != stage.StageIndex || attempt.AttemptID == "" || !cleanupSessionName(attempt.SessionID) {
					return fmt.Errorf("cleanup canonical session binding is ambiguous")
				}
				if _, exists := expected[attempt.SessionID]; exists {
					return fmt.Errorf("cleanup canonical session is duplicated: %s", attempt.SessionID)
				}
				var status loopruntime.SessionStatus
				switch attempt.Status {
				case state.AttemptCompleted:
					status = loopruntime.StatusCompleted
				case state.AttemptFailed:
					status = loopruntime.StatusFailed
				case state.AttemptCancelled:
					status = loopruntime.StatusCancelled
				default:
					return fmt.Errorf("cleanup attempt is unresolved: %s", attempt.SessionID)
				}
				expected[attempt.SessionID] = cleanupAttempt{
					binding:  loopruntime.CompletedSessionBinding{OrderID: order.OrderID, SessionID: attempt.SessionID, Provider: stage.Provider, Model: stage.Model, StageIndex: index, RetryCount: retry},
					worktree: attempt.WorktreeName, runtime: stage.Runtime, status: status,
				}
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join(l.runtimeDir, "sessions"))
	if err != nil {
		return err
	}
	if len(entries) != len(expected) {
		return fmt.Errorf("cleanup local sessions and canonical attempts differ")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	refs := map[string]loopruntime.SessionReadbackFileRef{}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("cleanup session entry is ambiguous: %s", entry.Name())
		}
		attempt, ok := expected[entry.Name()]
		if !ok {
			return fmt.Errorf("cleanup session has no canonical attempt: %s", entry.Name())
		}
		switch attempt.runtime {
		case "", "process":
			if err = observeAdmissionProcessSession(l.runtimeDir, entry.Name()); err != nil {
				return err
			}
		case "openshell":
			reader, ok := l.deps.Runtimes[attempt.runtime].(cleanupSessionReader)
			if !ok {
				return fmt.Errorf("original OpenShell cleanup observer unavailable")
			}
			if !cleanupSessionName(attempt.worktree) {
				return fmt.Errorf("cleanup original worktree identity missing")
			}
			project, err := canonicalDirectory(l.projectDir)
			if err != nil {
				return err
			}
			// The original directory may have been removed after merge. Its lexical
			// identity is still bound by canonical attempt, spawn and dispatch evidence.
			attempt.binding.WorktreePath = filepath.Join(project, ".worktrees", attempt.worktree)
			observed, err := reader.ReadCleanupSession(ctx, attempt.binding, project, attempt.status)
			if err != nil {
				return fmt.Errorf("session %s: %w", entry.Name(), err)
			}
			if observed.OutcomeStatus != attempt.status || observed.PublishedHead == "" || len(observed.Execution.Sources) == 0 || !observed.Execution.StopAcknowledged || observed.Execution.NativePhase != "Stopped" || observed.Execution.NativeExecExitCode != 0 {
				return fmt.Errorf("cleanup original runtime returned incomplete outcome: %s", entry.Name())
			}
			for name, ref := range observed.Execution.Sources {
				refs[entry.Name()+"/"+name] = ref
			}
		default:
			return fmt.Errorf("cleanup observation unavailable for runtime %q", attempt.runtime)
		}
	}
	return loopruntime.VerifySessionReadbackFiles(refs)
}

func cleanupSessionName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name
}
