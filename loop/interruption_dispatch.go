package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

type InterruptionSuccessor struct {
	AttemptID string `json:"attempt_id"`
	SessionID string `json:"session_id"`
}

type interruptionDispatch struct {
	Digest    string `json:"custody_sha256"`
	AttemptID string `json:"attempt_id"`
	SessionID string `json:"session_id,omitempty"`
}

func interruptionStage(s state.State, i interruptionIntent) (state.StageNode, interruptionBinding, error) {
	var b interruptionBinding
	o, st, ok := s.LookupStage(i.Custody.OrderID, i.Custody.StageIndex)
	if !ok || json.Unmarshal(st.Extra[interruptionKey], &b) != nil || b.Digest != i.Digest || b.Subject != i.Custody.Subject || b.PriorAttemptID != i.Custody.AttemptID {
		return st, b, fmt.Errorf("original interruption binding differs")
	}
	var after reducer.DurableSnapshot
	if err := decodeStoppedReview(i.After, &after); err != nil {
		return st, b, err
	}
	_, prior, _ := after.State.LookupStage(i.Custody.OrderID, i.Custody.StageIndex)
	if len(st.Attempts) < len(prior.Attempts) || !reflect.DeepEqual(st.Attempts[:len(prior.Attempts)], prior.Attempts) || b.NextAttemptID != dispatchAttemptID(o.OrderID, st.StageIndex, len(prior.Attempts)) {
		return st, b, fmt.Errorf("interruption attempt lineage differs")
	}
	if st.TaskKey != prior.TaskKey || st.Skill != prior.Skill || st.Provider != prior.Provider || st.Model != prior.Model || st.Runtime != prior.Runtime || st.Group != prior.Group {
		return st, b, fmt.Errorf("interruption carrier differs")
	}
	if _, err := interruptionSubject(st.Prompt, b.Subject); err != nil {
		return st, b, err
	}
	return st, b, nil
}

func interruptionDispatchReadback(dir, path string, i interruptionIntent, current []byte) (*InterruptionSuccessor, error) {
	raw, err := readAdmissionFile(filepath.Join(path, "dispatch-offered.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var offered interruptionDispatch
	if err := decodeStoppedReview(raw, &offered); err != nil {
		return nil, err
	}
	var s reducer.DurableSnapshot
	if err := decodeStoppedReview(current, &s); err != nil {
		return nil, err
	}
	st, b, err := interruptionStage(s.State, i)
	if err != nil {
		return nil, err
	}
	if offered.Digest != i.Digest || offered.AttemptID != b.NextAttemptID || offered.SessionID != "" {
		return nil, fmt.Errorf("dispatch offer identity differs")
	}
	raw, err = readAdmissionFile(filepath.Join(path, "dispatch-result.json"))
	if err != nil {
		return nil, fmt.Errorf("interruption dispatch outcome unknown; original process/session readback required: %w", err)
	}
	var result interruptionDispatch
	if err := decodeStoppedReview(raw, &result); err != nil {
		return nil, err
	}
	if result.Digest != i.Digest || result.AttemptID != offered.AttemptID || result.SessionID == "" {
		return nil, fmt.Errorf("dispatch result identity differs")
	}
	found := false
	for _, a := range st.Attempts {
		if a.AttemptID == result.AttemptID && a.SessionID == result.SessionID && a.WorktreeName == i.Custody.WorktreeName {
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("dispatch canonical successor differs")
	}
	again, err := readAdmissionFile(filepath.Join(dir, "state.snapshot.json"))
	if err != nil || !bytes.Equal(current, again) {
		return nil, fmt.Errorf("dispatch readback changed")
	}
	return &InterruptionSuccessor{AttemptID: result.AttemptID, SessionID: result.SessionID}, nil
}

// The marker permits one preserved dispatch. An offered launch never becomes retryable from process absence alone.
func (l *Loop) interruptionForDispatch(cand dispatchCandidate, ordinal int) (*interruptionIntent, error) {
	_, st, ok := l.canonical.LookupStage(cand.OrderID, cand.StageIndex)
	if !ok {
		return nil, nil
	}
	raw, exists := st.Extra[interruptionKey]
	if !exists {
		return nil, nil
	}
	var b interruptionBinding
	if err := decodeStoppedReview(raw, &b); err != nil {
		return nil, err
	}
	path := interruptionPath(l.projectDir, cand.OrderID, b.Subject)
	i, err := readInterruptionIntent(path)
	if err != nil {
		return nil, err
	}
	st, b, err = interruptionStage(l.canonical, i)
	if err != nil {
		return nil, err
	}
	if cand.Stage.Prompt != st.Prompt || cand.Stage.TaskKey != st.TaskKey || cand.Stage.Skill != st.Skill || cand.Stage.Provider != st.Provider || cand.Stage.Model != st.Model || cand.Stage.Runtime != st.Runtime {
		return nil, fmt.Errorf("interruption dispatch projection differs")
	}
	if b.NextAttemptID != dispatchAttemptID(cand.OrderID, cand.StageIndex, ordinal) {
		return nil, fmt.Errorf("interruption continuation was already consumed; original owner readback required")
	}
	if st.Status != state.StagePending || len(st.Attempts) != ordinal {
		return nil, fmt.Errorf("interruption dispatch is not the prepared attempt")
	}
	if _, err := os.Lstat(filepath.Join(path, "dispatch-offered.json")); err == nil {
		return nil, fmt.Errorf("interruption dispatch was already offered; readback required")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := validateInterruptionFiles(l.projectDir, i.Custody); err != nil {
		return nil, err
	}
	// Startup may refresh derived metadata. Raw session and process evidence stay fixed.
	if err := interruptionProcessAbsent(l.runtimeDir, i.Custody.SessionID); err != nil {
		return nil, err
	}
	return &i, nil
}

func offerInterruptionDispatch(project string, i interruptionIntent, attemptID string) error {
	if err := validateInterruptionFiles(project, i.Custody); err != nil {
		return err
	}
	path := filepath.Join(interruptionPath(project, i.Custody.OrderID, i.Custody.Subject), "dispatch-offered.json")
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("interruption dispatch already offered")
	} else if !os.IsNotExist(err) {
		return err
	}
	raw, err := json.Marshal(interruptionDispatch{Digest: i.Digest, AttemptID: attemptID})
	if err != nil {
		return err
	}
	return filex.WriteFileAtomicDurable(path, raw)
}

func recordInterruptionDispatch(project string, i interruptionIntent, attemptID, sessionID string) error {
	raw, err := json.Marshal(interruptionDispatch{Digest: i.Digest, AttemptID: attemptID, SessionID: sessionID})
	if err != nil {
		return err
	}
	return filex.WriteFileAtomicDurable(filepath.Join(interruptionPath(project, i.Custody.OrderID, i.Custody.Subject), "dispatch-result.json"), raw)
}

func interruptionProcessAbsent(dir, session string) error {
	pid, err := procx.ReadPIDFile(filepath.Join(dir, "sessions", session, "process.json"))
	if err != nil {
		return err
	}
	absent, err := procx.ProcessGroupAbsent(pid)
	if err != nil {
		return err
	}
	if !absent {
		return fmt.Errorf("original session process or group is present")
	}
	return nil
}
