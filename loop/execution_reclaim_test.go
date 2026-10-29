package loop

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

type reclaimingRuntime struct {
	*cleanupReadbackRuntime
	reclaim func(context.Context, loopruntime.CompletedSessionBinding, string, loopruntime.SessionStatus, string, loopruntime.SessionReadbackFileRef) (loopruntime.CleanupSessionReadback, error)
}

func (r *reclaimingRuntime) ReclaimCleanupSession(ctx context.Context, b loopruntime.CompletedSessionBinding, project string, status loopruntime.SessionStatus, reason string, authority loopruntime.SessionReadbackFileRef) (loopruntime.CleanupSessionReadback, error) {
	return r.reclaim(ctx, b, project, status, reason, authority)
}

type reclaimingWorktree struct {
	*fakeWorktree
	remove func(string, string) error
}

func (w *reclaimingWorktree) RemoveSettledExecutionCopy(name, head string) error {
	return w.remove(name, head)
}

func TestExecutionReclamationFollowsOriginalClosureAndRuntimeEvidence(t *testing.T) {
	for _, variant := range []string{"closed", "runtime-error", "worktree-error"} {
		t.Run(variant, func(t *testing.T) {
			l, cook, request := failedClosureFixture(t)
			if err := l.closeFailedTask(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			sequence := []string{}
			reader := l.deps.Runtimes["openshell"].(*cleanupReadbackRuntime)
			rt := &reclaimingRuntime{cleanupReadbackRuntime: reader}
			rt.reclaim = func(ctx context.Context, b loopruntime.CompletedSessionBinding, project string, status loopruntime.SessionStatus, reason string, authority loopruntime.SessionReadbackFileRef) (loopruntime.CleanupSessionReadback, error) {
				if b.OrderID != cook.orderID || b.SessionID != request.SessionID || status != loopruntime.StatusFailed || reason == "" {
					t.Fatal("wrong original closure binding")
				}
				if err := loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"original_closure": authority}); err != nil {
					t.Fatal(err)
				}
				sequence = append(sequence, "runtime-preserve-and-retire")
				if variant == "runtime-error" {
					return loopruntime.CleanupSessionReadback{}, errors.New("native delete acknowledgement unknown")
				}
				observed, err := reader.read(b, project, status)
				observed.Execution.Sources["native_custody"] = request.Diagnostics
				observed.Retirement = &loopruntime.CleanupRetirementReadback{ObservationBasis: "historical_stopped_with_fresh_native_absence", Closure: authority, DeleteRequestID: "original-native-delete", DeleteAcknowledged: true, AbsenceObserved: true}
				return observed, err
			}
			l.deps.Runtimes["openshell"] = rt
			l.deps.Worktree = &reclaimingWorktree{fakeWorktree: &fakeWorktree{}, remove: func(name, head string) error {
				if name != cook.worktreeName || head != "published-progress" {
					t.Fatal("worktree binding differs")
				}
				sequence = append(sequence, "worktree-remove")
				if variant == "worktree-error" {
					return errors.New("worktree still registered")
				}
				return nil
			}}
			err := l.cleanupCompleted(t.Context(), true)
			if (err == nil) != (variant == "closed") {
				t.Fatalf("unexpected reclamation result: %v", err)
			}
			expected := []string{"runtime-preserve-and-retire"}
			if variant != "runtime-error" {
				expected = append(expected, "worktree-remove")
			}
			if !reflect.DeepEqual(sequence, expected) {
				t.Fatalf("unsafe reclamation ordering: %v", sequence)
			}
			if l.canonical.Orders[cook.orderID].Status != state.OrderFailed {
				t.Fatal("cleanup promoted failure")
			}
			if variant != "closed" && l.cleanupUnknown == nil {
				t.Fatal("reclamation failure not retained for readback")
			}
		})
	}
}
