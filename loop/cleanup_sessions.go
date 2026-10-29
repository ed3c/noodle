package loop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
func (l *Loop) observeCleanupSessions(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	expected := map[string]cleanupAttempt{}
	for key, order := range l.canonical.Orders {
		if err := ctx.Err(); err != nil {
			return err
		}
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
	refs := map[string]loopruntime.SessionReadbackFileRef{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
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
			var observed loopruntime.CleanupSessionReadback
			err = l.cleanupPhase(ctx, "session_readback", attempt.binding.OrderID, entry.Name(), func() error {
				var readErr error
				observed, readErr = reader.ReadCleanupSession(ctx, attempt.binding, project, attempt.status)
				return readErr
			})
			if err != nil {
				return fmt.Errorf("session %s: %w", entry.Name(), err)
			}
			if observed.OutcomeStatus != attempt.status || observed.PublishedHead == "" || len(observed.Execution.Sources) == 0 || !observed.Execution.StopAcknowledged || observed.Execution.NativePhase != "Stopped" {
				return fmt.Errorf("cleanup original runtime returned incomplete outcome: %s", entry.Name())
			}
			if observed.Diagnostic != nil {
				if err := validateCleanupNotStarted(attempt, observed); err != nil {
					return fmt.Errorf("cleanup diagnostic session %s: %w", entry.Name(), err)
				}
			} else if observed.Execution.NativeExecExitCode != 0 {
				return fmt.Errorf("cleanup original runtime returned incomplete execution: %s", entry.Name())
			}
			for name, ref := range observed.Execution.Sources {
				refs[entry.Name()+"/"+name] = ref
			}
		default:
			return fmt.Errorf("cleanup observation unavailable for runtime %q", attempt.runtime)
		}
	}
	return l.cleanupPhase(ctx, "source_verification", "", "", func() error {
		return loopruntime.VerifySessionReadbackFiles(refs)
	})
}

// The original runtime proves the live custody record has Create/Stop ACKs and
// no Exec intent. Bind that diagnostic to this canonical attempt; -1 preserves
// non-execution and cannot stand in for successful execution or saved progress.
func validateCleanupNotStarted(attempt cleanupAttempt, observed loopruntime.CleanupSessionReadback) error {
	diagnostic, execution := observed.Diagnostic, observed.Execution
	if attempt.status != loopruntime.StatusFailed || diagnostic.ExecutionBasis != "not_started" || diagnostic.Outcome.Status != loopruntime.StatusFailed || diagnostic.Outcome.ExitCode != -1 || strings.TrimSpace(diagnostic.Outcome.Reason) == "" || execution.NativeExecExitCode != -1 || execution.CommandSHA256 != "" || execution.CandidateHead != "" || execution.Instructions != nil {
		return fmt.Errorf("original not-started failure outcome differs")
	}
	id := diagnostic.Observation.Identity
	for _, value := range []string{id.AttemptID, id.Sandbox.Gateway, id.Sandbox.Workspace, id.Sandbox.SandboxID, id.SandboxName, id.ConfigSource, execution.MainProcessInstanceID, execution.ProfileSHA256} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("original not-started custody identity is incomplete")
		}
	}
	if id.OrderID != attempt.binding.OrderID || id.SessionID != attempt.binding.SessionID || id.AttemptID != execution.NativeAttemptID {
		return fmt.Errorf("original not-started task binding differs")
	}
	want := loopruntime.OpenShellCompletedObservation{ExecutionBasis: "not_started", Identity: id,
		Task:         loopruntime.OpenShellTask{OrderID: id.OrderID, AttemptID: id.AttemptID, SessionID: id.SessionID},
		ExecExitCode: -1, MainProcessInstanceID: execution.MainProcessInstanceID, Phase: "Stopped", CreateAcknowledged: true, StopAcknowledged: true}
	if diagnostic.Observation != want {
		return fmt.Errorf("original not-started Create/Stop/main or no-Exec observation differs")
	}
	if _, ok := execution.Sources["native_custody"]; !ok {
		return fmt.Errorf("original not-started custody source is missing")
	}
	return nil
}

func cleanupSessionName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name
}
