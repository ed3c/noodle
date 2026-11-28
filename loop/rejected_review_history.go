package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/poteto/noodle/internal/orderx"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/statever"
	loopruntime "github.com/poteto/noodle/runtime"
)

type RejectedReviewHistory struct {
	Sources  map[string]loopruntime.SessionReadbackFileRef
	Attempts []RejectedReviewAttempt
	Head     string
}

type RejectedReviewAttempt struct {
	StageIndex int
	AttemptID  string
	SessionID  string
}

// ReadRejectedReviewHistory selects only the native local task's reviewed
// attempts. A current pending rejection and a prior rejection followed by
// requeue have different original event witnesses.
func ReadRejectedReviewHistory(runtimeDir, orderID string) (RejectedReviewHistory, error) {
	var snapshot reducer.DurableSnapshot
	snapshotRef, err := loopruntime.ReadSessionReadbackFile(filepath.Join(runtimeDir, "state.snapshot.json"), &snapshot)
	if err != nil {
		return RejectedReviewHistory{}, err
	}
	order, found := snapshot.State.Orders[orderID]
	review, pending := snapshot.State.PendingReviews[orderID]
	if !found || !pending || orderID == "" || order.OrderID != orderID || order.Status != state.OrderFailed ||
		snapshot.State.SchemaVersion != statever.Current || !orderx.ValidOrderRevision(snapshot.OrderRevision) ||
		snapshot.GeneratedAt.IsZero() || snapshot.State.LastEventID == "" || review.OrderID != orderID ||
		review.Runtime != "openshell" || review.StageIndex < 0 || review.StageIndex >= len(order.Stages) ||
		!reducer.RetainedTaskData(snapshot.EffectLedger, orderID) {
		return RejectedReviewHistory{}, fmt.Errorf("original failed OpenShell review or explicit checkout retention differs")
	}
	stage := order.Stages[review.StageIndex]
	if stage.StageIndex != review.StageIndex || stage.Status != state.StageFailed || stage.Runtime != review.Runtime ||
		(stage.TaskKey != "local-task" && stage.Skill != "local-task") || len(stage.Attempts) == 0 {
		return RejectedReviewHistory{}, fmt.Errorf("original local task rejected stage differs")
	}
	current := len(stage.Attempts) - 1
	attempt := stage.Attempts[current]
	var binding requestChangesBinding
	if err = decodeStoppedReview(stage.Extra[state.RequestChangesKey], &binding); err != nil {
		return RejectedReviewHistory{}, err
	}
	if binding.Attempt != current || binding.AttemptID != attempt.AttemptID || binding.SessionID != attempt.SessionID ||
		review.SessionID != attempt.SessionID || !reflect.DeepEqual(review.ReturnHandoff, attempt.ReturnHandoff) ||
		binding.WorktreeName != attempt.WorktreeName || binding.WorktreePath != review.WorktreePath ||
		!filepath.IsAbs(binding.WorktreePath) || !reconciliationSHA.MatchString(binding.Head) {
		return RejectedReviewHistory{}, fmt.Errorf("original request-changes candidate or checkout differs")
	}
	eventRef, err := loopruntime.ReadSessionReadbackFile(filepath.Join(runtimeDir, "loop-events.ndjson"), nil)
	if err != nil {
		return RejectedReviewHistory{}, err
	}
	events, err := os.ReadFile(eventRef.Path)
	if err != nil || recoveryDigest(events) != eventRef.SHA256 {
		return RejectedReviewHistory{}, fmt.Errorf("original failed review event history changed: %v", err)
	}
	history := RejectedReviewHistory{
		Sources: map[string]loopruntime.SessionReadbackFileRef{"canonical_snapshot": snapshotRef, "rejected_review_events": eventRef},
		Head:    binding.Head,
	}
	seenSessions, seenAttempts := map[string]bool{}, map[string]bool{}
	for i, prior := range stage.Attempts {
		if !cleanupReviewRejected(prior) || !cleanupSessionName(prior.SessionID) || prior.AttemptID == "" ||
			prior.CompletedAt.IsZero() || prior.WorktreeName != binding.WorktreeName ||
			seenSessions[prior.SessionID] || seenAttempts[prior.AttemptID] {
			return RejectedReviewHistory{}, fmt.Errorf("original rejected review attempt is missing, mixed or ambiguous")
		}
		seenSessions[prior.SessionID], seenAttempts[prior.AttemptID] = true, true
		if i == current {
			err = validateRequestChangesFailure(events, requestChangesPacket{Order: order, Review: review, Binding: binding})
		} else {
			err = verifyHistoricalRejection(events, order, stage, i)
		}
		if err != nil {
			return RejectedReviewHistory{}, fmt.Errorf("original rejected session %s: %w", prior.SessionID, err)
		}
		history.Attempts = append(history.Attempts, RejectedReviewAttempt{stage.StageIndex, prior.AttemptID, prior.SessionID})
	}
	return history, loopruntime.VerifySessionReadbackFiles(history.Sources)
}
