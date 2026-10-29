package loop

import "testing"

func TestBoundContinuationSeparatesTaskTypes(t *testing.T) {
	l, cook, _ := localTaskReviewFixture(t)
	if err := l.controlRequestChanges(cook.orderID, "correct result"); err != nil {
		t.Fatal(err)
	}
	if err := l.controlRequeue(cook.orderID); err != nil {
		t.Fatal(err)
	}
	stage := l.canonical.Orders[cook.orderID].Stages[0]
	cand := dispatchCandidate{OrderID: cook.orderID, StageIndex: 0, Stage: Stage{TaskKey: stage.TaskKey, Skill: stage.Skill, Provider: stage.Provider, Runtime: stage.Runtime}}
	if id, err := l.resumeTaskSessionID(cand, 1); err != nil || id != cook.session.ID() {
		t.Fatalf("same task continuation=%q %v", id, err)
	}
	cand.Stage.TaskKey = "review"
	if _, err := l.resumeTaskSessionID(cand, 1); err == nil {
		t.Fatal("review reused implementation conversation")
	}
	cand.Stage.TaskKey = stage.TaskKey
	if _, err := l.resumeTaskSessionID(cand, 2); err == nil {
		t.Fatal("another attempt borrowed original continuation")
	}
}
