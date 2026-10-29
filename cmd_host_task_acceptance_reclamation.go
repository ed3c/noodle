package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"

	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
)

// Keep only the original read-only project owner after the runtime detaches.
// Its immutable environment remains the one used at admission, without a lease.
type hostTaskAcceptanceReadbackOwner struct {
	worktrees      *worktree.App
	environmentSHA string
}

func (h *noodleHost) retainTaskAcceptanceReadbackOwner(id string, e *execution.Context, environmentSHA string) {
	if h.taskReadbacks == nil {
		h.taskReadbacks = map[string]hostTaskAcceptanceReadbackOwner{}
	}
	h.taskReadbacks[id] = hostTaskAcceptanceReadbackOwner{worktrees: &worktree.App{Root: e.Root(), Execution: e}, environmentSHA: environmentSHA}
}

func readHostTaskAcceptanceReclamation(req hostRequest, authority loopruntime.SessionReadbackFileRef) (loopruntime.ExecutionReclamationReceipt, loopruntime.SessionReadbackFileRef, error) {
	path := filepath.Join(req.ProjectDir, ".noodle", "execution-closures", hostDigest([]byte(req.ReadbackOrderID))+".reclaimed.json")
	value, ref, err := loopruntime.ReadExecutionReclamationReceipt(path, req.ReadbackOrderID, authority)
	if err != nil {
		return value, ref, err
	}
	if err := verifyHostTaskReclamationOrder(req, authority, value); err != nil {
		return value, ref, err
	}
	return value, ref, nil
}

func verifyHostTaskReclamationOrder(req hostRequest, authority loopruntime.SessionReadbackFileRef, value loopruntime.ExecutionReclamationReceipt) error {
	var closure struct {
		Order state.OrderNode `json:"order"`
	}
	actual, err := loopruntime.ReadSessionReadbackFile(authority.Path, &closure)
	if err != nil {
		return err
	}
	if actual != authority {
		return fmt.Errorf("original task closure changed during reclamation readback")
	}
	snapshot, err := reducer.ReadSnapshot(filepath.Join(req.ProjectDir, ".noodle", "state.snapshot.json"))
	if err != nil {
		return err
	}
	current, ok := snapshot.State.Orders[req.ReadbackOrderID]
	if !ok {
		return fmt.Errorf("original reclaimed canonical order missing")
	}
	// Unrelated orders and snapshot timestamps may change; this order may not.
	retainedJSON, err := json.Marshal(closure.Order)
	if err != nil {
		return err
	}
	currentJSON, err := json.Marshal(current)
	if err != nil {
		return err
	}
	if !bytes.Equal(retainedJSON, currentJSON) {
		return fmt.Errorf("original reclaimed canonical order changed")
	}
	sessions := map[string]loopruntime.CleanupSessionReadback{}
	for _, session := range value.Sessions {
		sessions[session.SessionID] = session.Readback
	}
	heads := map[string]string{}
	count := 0
	for _, stage := range current.Stages {
		if stage.Runtime != "openshell" {
			return fmt.Errorf("original reclaimed runtime differs")
		}
		for _, attempt := range stage.Attempts {
			readback, ok := sessions[attempt.SessionID]
			if !ok || attempt.WorktreeName == "" {
				return fmt.Errorf("original reclamation attempt coverage incomplete")
			}
			var outcome loopruntime.SessionStatus
			switch attempt.Status {
			case state.AttemptCompleted:
				outcome = loopruntime.StatusCompleted
			case state.AttemptFailed:
				outcome = loopruntime.StatusFailed
			case state.AttemptCancelled:
				outcome = loopruntime.StatusCancelled
			default:
				return fmt.Errorf("original reclaimed attempt unresolved")
			}
			if readback.OutcomeStatus != outcome {
				return fmt.Errorf("original reclaimed attempt outcome differs")
			}
			heads[attempt.WorktreeName] = readback.PublishedHead
			delete(sessions, attempt.SessionID)
			count++
		}
	}
	if count != len(value.Sessions) || !reflect.DeepEqual(heads, value.Worktrees) {
		return fmt.Errorf("original reclamation session or worktree coverage differs")
	}
	acceptance, err := loopruntime.ReadTaskDeliveryAcceptance(req.TaskAcceptance)
	if err != nil {
		return err
	}
	for _, session := range value.Sessions {
		if session.SessionID == req.ReadbackSessionID && session.Readback.PublishedHead != acceptance.CandidateHead {
			return fmt.Errorf("original reclaimed accepted candidate differs")
		}
	}
	return loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"authority": authority})
}
