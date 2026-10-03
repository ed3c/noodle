package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

const interruptionKey = "interrupted_execution"

type InterruptionCustody struct {
	OrderID           string            `json:"order_id"`
	StageIndex        int               `json:"stage_index"`
	AttemptID         string            `json:"attempt_id"`
	SessionID         string            `json:"session_id"`
	Subject           string            `json:"subject"`
	EnvelopeSHA256    string            `json:"envelope_sha256"`
	WorktreeName      string            `json:"worktree_name"`
	WorktreePath      string            `json:"worktree_path"`
	Branch            string            `json:"branch"`
	Head              string            `json:"head"`
	SessionSHA256     map[string]string `json:"session_sha256"`
	CandidateManifest candidateManifest `json:"candidate_manifest"`
}

type InterruptionInspection struct {
	Owner              string                 `json:"owner"`
	Status             string                 `json:"status"`
	Successor          *InterruptionSuccessor `json:"successor"`
	CandidateUnchanged bool                   `json:"candidate_unchanged"`
	CandidateInvalid   string                 `json:"candidate_invalid"`
	Custody            InterruptionCustody    `json:"custody"`
	Digest             string                 `json:"custody_sha256"`
	EvidencePath       string                 `json:"evidence_path"`
	Invalid            string                 `json:"invalid"`
	Next               AdmissionNext          `json:"next"`
}

type interruptionIntent struct {
	Custody InterruptionCustody `json:"custody"`
	Before  []byte              `json:"before_snapshot"`
	After   []byte              `json:"after_snapshot"`
	Digest  string              `json:"custody_sha256"`
}

type interruptionBinding struct {
	Digest         string `json:"custody_sha256"`
	Subject        string `json:"subject"`
	PriorAttemptID string `json:"prior_attempt_id"`
	NextAttemptID  string `json:"next_attempt_id"`
}

func InspectInterruption(project, binary, order, subject string) InterruptionInspection {
	return prepareInterruption(project, binary, order, subject, "", nil)
}

func PrepareInterruption(project, binary, order, subject, digest string) InterruptionInspection {
	if len(digest) != 64 {
		return interruptionRefusal(interruptionResult(project, binary, order, subject), fmt.Errorf("custody_sha256 must be exact"))
	}
	return prepareInterruption(project, binary, order, subject, digest, nil)
}

func interruptionResult(project, binary, order, subject string) InterruptionInspection {
	return InterruptionInspection{Owner: "Noodle interrupted execution", Next: AdmissionNext{
		Argv: []string{}, ReadbackArgv: []string{binary, "--project-dir", project, "interruption", "inspect", order, subject},
	}}
}

func interruptionRefusal(r InterruptionInspection, err error) InterruptionInspection {
	r.Status, r.Invalid = "refused", err.Error()
	r.Next.Argv = []string{}
	r.Next.Required = err.Error()
	r.Next.ProvidedBy = "Supervisor of the original Noodle order"
	return r
}

func interruptionPath(project, order, subject string) string {
	return filepath.Join(project, ".noodle", "interruptions", publicationDigest([]byte(order+"\n"+subject)))
}

func interruptionDigest(i interruptionIntent) string {
	raw, _ := json.Marshal(struct {
		Custody InterruptionCustody
		Before  []byte
	}{i.Custody, i.Before})
	return publicationDigest(raw)
}

func readInterruptionIntent(path string) (interruptionIntent, error) {
	var i interruptionIntent
	raw, err := readAdmissionFile(filepath.Join(path, "intent.json"))
	if err != nil {
		return i, err
	}
	if err := decodeStoppedReview(raw, &i); err != nil {
		return i, err
	}
	if i.Digest != interruptionDigest(i) {
		return i, fmt.Errorf("interruption intent digest differs")
	}
	var after reducer.DurableSnapshot
	if err := decodeStoppedReview(i.After, &after); err != nil {
		return i, err
	}
	expected, err := interruptionAfter(i, after.GeneratedAt)
	if err != nil {
		return i, err
	}
	if !bytes.Equal(expected, i.After) {
		return i, fmt.Errorf("interruption transition differs")
	}
	return i, nil
}

func prepareInterruption(project, binary, order, subject, digest string, barrier func(string)) InterruptionInspection {
	if digest == "" {
		return inspectInterruption(project, binary, order, subject)
	}
	r := interruptionResult(project, binary, order, subject)
	refuse := func(err error) InterruptionInspection { return interruptionRefusal(r, err) }
	project, err := canonicalDirectory(project)
	if err != nil {
		return refuse(err)
	}
	dir := filepath.Join(project, ".noodle")
	lock, err := lockfile.TryLock(filepath.Join(dir, "noodle.lock"))
	if err != nil {
		return refuse(err)
	}
	defer lock.Close()
	r.EvidencePath = interruptionPath(project, order, subject)
	intent, err := readInterruptionIntent(r.EvidencePath)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return refuse(err)
	}
	if !exists {
		intent, err = captureInterruption(project, order, subject)
		if err != nil {
			return refuse(err)
		}
	}
	if intent.Custody.OrderID != order || intent.Custody.Subject != subject {
		return refuse(fmt.Errorf("interruption identity differs"))
	}
	r.Custody, r.Digest = intent.Custody, intent.Digest
	if digest != intent.Digest {
		return refuse(fmt.Errorf("custody_sha256 changed"))
	}
	current, err := readAdmissionFile(filepath.Join(dir, "state.snapshot.json"))
	if err != nil {
		return refuse(err)
	}
	if exists {
		dispatched, err := interruptionDispatchReadback(dir, r.EvidencePath, intent, current)
		if err != nil {
			return refuse(err)
		}
		if dispatched != nil {
			return dispatchedInterruptionReadback(project, r, intent, dispatched)
		}
	}
	before := bytes.Equal(current, intent.Before)
	after := exists && bytes.Equal(current, intent.After)
	if !before && !after {
		if exists {
			return readPreparedInterruption(project, r, intent, current)
		}
		return refuse(fmt.Errorf("canonical snapshot changed outside interruption transition"))
	}
	if err := validateInterruptionFiles(project, intent.Custody); err != nil {
		return refuse(err)
	}
	var snapshot reducer.DurableSnapshot
	if err := decodeStoppedReview(current, &snapshot); err != nil {
		return refuse(err)
	}
	if err := observeAdmissionSessions(dir, snapshot.State); err != nil {
		return refuse(err)
	}
	r.CandidateUnchanged = true
	r.Status = "recoverable"
	r.Next.Argv = []string{binary, "--project-dir", project, "interruption", "prepare", order, subject, r.Digest}
	if after {
		if err := stoppedReviewProjection(dir, intent.After, false); err == nil {
			r.CandidateUnchanged = true
			r.Status = "prepared"
			r.Next.Argv = []string{}
			r.Next.ProvidedBy = "Original Noodle lifecycle supervisor"
			r.Next.Required = "selected held start for the prepared original order"
			return r
		}
	}
	if !exists {
		intent.After, err = interruptionAfter(intent, time.Now().UTC())
		if err != nil {
			return refuse(err)
		}
		if err := os.MkdirAll(r.EvidencePath, 0700); err != nil {
			return refuse(err)
		}
		raw, err := json.MarshalIndent(intent, "", "  ")
		if err != nil {
			return refuse(err)
		}
		if err := filex.WriteFileAtomicDurable(filepath.Join(r.EvidencePath, "intent.json"), append(raw, '\n')); err != nil {
			return refuse(err)
		}
	}
	if barrier != nil {
		barrier("after_intent")
	}
	if before {
		fresh, err := captureInterruption(project, order, subject)
		if err != nil {
			return refuse(err)
		}
		if fresh.Digest != intent.Digest {
			return refuse(fmt.Errorf("interruption custody changed after intent"))
		}
		if err := filex.WriteFileAtomicDurable(filepath.Join(dir, "state.snapshot.json"), intent.After); err != nil {
			return refuse(err)
		}
	}
	if barrier != nil {
		barrier("after_canonical")
	}
	if err := stoppedReviewProjection(dir, intent.After, true); err != nil {
		return refuse(err)
	}
	if err := validateInterruptionFiles(project, intent.Custody); err != nil {
		return refuse(err)
	}
	r.CandidateUnchanged = true
	r.Status = "prepared"
	r.Next.Argv = []string{}
	r.Next.ProvidedBy = "Original Noodle lifecycle supervisor"
	r.Next.Required = "selected held start for the prepared original order"
	return r
}

func interruptionAfter(i interruptionIntent, at time.Time) ([]byte, error) {
	var s reducer.DurableSnapshot
	if err := decodeStoppedReview(i.Before, &s); err != nil {
		return nil, err
	}
	c := i.Custody
	order, stage, ok := s.State.LookupStage(c.OrderID, c.StageIndex)
	if !ok || stage.Status != state.StageRunning || len(stage.Attempts) == 0 {
		return nil, fmt.Errorf("original running stage absent")
	}
	last := len(stage.Attempts) - 1
	if stage.Attempts[last].AttemptID != c.AttemptID || stage.Attempts[last].SessionID != c.SessionID || stage.Attempts[last].Status != state.AttemptRunning {
		return nil, fmt.Errorf("original running attempt differs")
	}
	stage.Attempts[last].Status = state.AttemptCancelled
	stage.Attempts[last].CompletedAt = at
	stage.Attempts[last].ExitCode = nil
	stage.Attempts[last].Error = "owner observed interrupted execution; process and group absent; exit unknown"
	stage.Status = state.StagePending
	if stage.Extra == nil {
		stage.Extra = map[string]json.RawMessage{}
	}
	if _, exists := stage.Extra[interruptionKey]; exists {
		return nil, fmt.Errorf("original stage already has interruption binding")
	}
	binding := interruptionBinding{Digest: i.Digest, Subject: c.Subject, PriorAttemptID: c.AttemptID, NextAttemptID: dispatchAttemptID(c.OrderID, c.StageIndex, len(stage.Attempts))}
	raw, err := json.Marshal(binding)
	if err != nil {
		return nil, err
	}
	stage.Extra[interruptionKey] = raw
	order.Stages[c.StageIndex] = stage
	order.Status, order.UpdatedAt = state.OrderActive, at
	s.State.Orders[c.OrderID] = order
	s.GeneratedAt = at
	raw, err = json.MarshalIndent(s, "", "  ")
	return append(raw, '\n'), err
}

func validateInterruptionSession(project string, c InterruptionCustody) error {
	for name, digest := range c.SessionSHA256 {
		if filepath.Base(name) != name {
			return fmt.Errorf("invalid session evidence path")
		}
		raw, err := readAdmissionFile(filepath.Join(project, ".noodle", "sessions", c.SessionID, name))
		if err != nil {
			return err
		}
		if publicationDigest(raw) != digest {
			return fmt.Errorf("original session %s changed", name)
		}
	}
	return nil
}

func validateInterruptionFiles(project string, c InterruptionCustody) error {
	if err := validateInterruptionSession(project, c); err != nil {
		return err
	}
	return validateInterruptionCandidate(project, c)
}

func validateInterruptionCandidate(project string, c InterruptionCustody) error {
	manifest, branch, head, err := captureCandidate(project, c.WorktreeName)
	if err != nil {
		return err
	}
	if c.WorktreePath != filepath.Join(project, ".worktrees", c.WorktreeName) || branch != c.Branch || head != c.Head || !reflect.DeepEqual(manifest, c.CandidateManifest) {
		return fmt.Errorf("candidate custody changed")
	}
	return nil
}

func readPreparedInterruption(project string, r InterruptionInspection, i interruptionIntent, current []byte) InterruptionInspection {
	refuse := func(err error) InterruptionInspection { return interruptionRefusal(r, err) }
	r.Custody, r.Digest = i.Custody, i.Digest
	dir := filepath.Join(project, ".noodle")
	successor, err := interruptionDispatchReadback(dir, r.EvidencePath, i, current)
	if err != nil {
		return refuse(err)
	}
	if successor != nil {
		return dispatchedInterruptionReadback(project, r, i, successor)
	}
	var s reducer.DurableSnapshot
	if err := decodeStoppedReview(current, &s); err != nil {
		return refuse(err)
	}
	st, _, err := interruptionStage(s.State, i)
	if err != nil {
		return refuse(err)
	}
	if st.Status != state.StagePending || st.Attempts[len(st.Attempts)-1].AttemptID != i.Custody.AttemptID {
		return refuse(fmt.Errorf("interruption is not prepared"))
	}
	if err := validateInterruptionFiles(project, i.Custody); err != nil {
		return refuse(err)
	}
	if err := interruptionProcessAbsent(dir, i.Custody.SessionID); err != nil {
		return refuse(err)
	}
	again, err := readAdmissionFile(filepath.Join(dir, "state.snapshot.json"))
	if err != nil || !bytes.Equal(current, again) {
		return refuse(fmt.Errorf("prepared readback changed"))
	}
	r.CandidateUnchanged = true
	r.Status = "prepared"
	r.Next.Argv = []string{}
	r.Next.ProvidedBy = "Original Noodle lifecycle supervisor"
	r.Next.Required = "selected held start for the prepared original order"
	return r
}

func dispatchedInterruptionReadback(project string, r InterruptionInspection, i interruptionIntent, successor *InterruptionSuccessor) InterruptionInspection {
	if err := validateInterruptionSession(project, i.Custody); err != nil {
		return interruptionRefusal(r, err)
	}
	r.Status = "dispatched"
	r.Successor = successor
	if err := validateInterruptionCandidate(project, i.Custody); err != nil {
		r.CandidateInvalid = err.Error()
	} else {
		r.CandidateUnchanged = true
	}
	return r
}

func inspectInterruption(project, binary, order, subject string) InterruptionInspection {
	r := interruptionResult(project, binary, order, subject)
	refuse := func(err error) InterruptionInspection { return interruptionRefusal(r, err) }
	project, err := canonicalDirectory(project)
	if err != nil {
		return refuse(err)
	}
	r.EvidencePath = interruptionPath(project, order, subject)
	i, err := readInterruptionIntent(r.EvidencePath)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return refuse(err)
	}
	if !exists {
		i, err = captureInterruption(project, order, subject)
		if err != nil {
			return refuse(err)
		}
	}
	if i.Custody.OrderID != order || i.Custody.Subject != subject {
		return refuse(fmt.Errorf("interruption identity differs"))
	}
	r.Custody, r.Digest = i.Custody, i.Digest
	current, err := readAdmissionFile(filepath.Join(project, ".noodle", "state.snapshot.json"))
	if err != nil {
		return refuse(err)
	}
	if exists && !bytes.Equal(current, i.Before) {
		if bytes.Equal(current, i.After) {
			if err := stoppedReviewProjection(filepath.Join(project, ".noodle"), i.After, false); err != nil {
				r.Status = "recoverable"
				r.Next.Argv = []string{binary, "--project-dir", project, "interruption", "prepare", order, subject, i.Digest}
				return r
			}
		}
		return readPreparedInterruption(project, r, i, current)
	}
	fresh, err := captureInterruption(project, order, subject)
	if err != nil {
		return refuse(err)
	}
	if fresh.Digest != i.Digest {
		return refuse(fmt.Errorf("interruption custody changed"))
	}
	r.Status = "recoverable"
	r.CandidateUnchanged = true
	r.Next.Argv = []string{binary, "--project-dir", project, "interruption", "prepare", order, subject, i.Digest}
	return r
}
