package state

import "fmt"

// ReturnHandoffForAttempt reads the first known intent. Missing legacy history
// is not proof that a callback was never attempted. Only a new dispatch records
// that positive fact; approval and every subsequent park retain it as consumed.
func (s State) ReturnHandoffForAttempt(orderID string, stageIndex int, attemptID, sessionID string) (*ReturnHandoffIntent, bool, error) {
	order, stage, ok := s.LookupStage(orderID, stageIndex)
	if !ok || order.Status.IsTerminal() {
		return nil, false, nil
	}
	var attempt *AttemptNode
	sessionMatches := 0
	for i := range stage.Attempts {
		if stage.Attempts[i].SessionID == sessionID {
			sessionMatches++
		}
		if stage.Attempts[i].AttemptID == attemptID {
			attempt = &stage.Attempts[i]
		}
	}
	var saved *ReturnHandoffIntent
	if attempt != nil {
		saved = attempt.ReturnHandoff
	}
	review, pending := s.PendingReviews[orderID]
	if pending && review.ReturnHandoff != nil {
		if review.OrderID != orderID || review.StageIndex != stageIndex || review.SessionID != sessionID || attempt == nil || attempt.SessionID != sessionID {
			return nil, false, fmt.Errorf("return handoff original attempt/session differs; original Owner readback required")
		}
		if saved != nil && *saved != *review.ReturnHandoff {
			return nil, false, fmt.Errorf("return handoff histories differ; original Owner readback required")
		}
		saved = review.ReturnHandoff
	}
	if saved != nil {
		if attemptID == "" || sessionID == "" || attempt == nil || sessionMatches != 1 || attempt.SessionID != sessionID || saved.BindingPath == "" || len(saved.BindingSHA256) != 64 || saved.EventID == "" || saved.Status != "unknown" {
			return nil, false, fmt.Errorf("return handoff original intent unavailable; original Owner readback required")
		}
		intent := *saved
		return &intent, false, nil
	}
	return nil, attemptID != "" && sessionID != "" && attempt != nil && sessionMatches == 1 && attempt.SessionID == sessionID && attempt.ReturnHandoffUnattempted && !pending, nil
}
