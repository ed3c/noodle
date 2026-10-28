package loop

import (
	"context"
	"github.com/poteto/noodle/internal/state"
	"testing"
)

func TestOpenShellStoppedTaskPreservesFailedAttemptInReview(t *testing.T) {
	for _, status := range []StageResultStatus{StageResultCancelled, StageResultFailed} {
		t.Run(string(status), func(t *testing.T) {
			l, cook, _ := localTaskReviewFixtureRoute(t, "candidate.txt", "candidate.txt", "openshell", "pi")
			if err := l.handleCompletion(context.Background(), cook, status, string(status)); err != nil {
				t.Fatal(err)
			}
			order := l.canonical.Orders[cook.orderID]
			stage := order.Stages[0]
			if order.Status != state.OrderActive || stage.Status != state.StageReview || len(stage.Attempts) != 1 || string(stage.Attempts[0].Status) != string(status) {
				t.Fatalf("stopped attempt history was lost: %+v", order)
			}
			if _, ok := l.cooks.pendingReview[cook.orderID]; !ok {
				t.Fatal("saved worktree lost review owner")
			}
		})
	}
}
