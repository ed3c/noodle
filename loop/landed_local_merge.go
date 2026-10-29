package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/workspaceartifacts"
	loopruntime "github.com/poteto/noodle/runtime"
)

type LandedLocalMergeProof struct {
	Schema          int                                `json:"schema"`
	Observation     loopruntime.SessionReadbackFileRef `json:"observation"`
	Response        loopruntime.SessionReadbackFileRef `json:"response"`
	ObserverIntent  loopruntime.SessionReadbackFileRef `json:"observer_intent"`
	ObserverReceipt loopruntime.SessionReadbackFileRef `json:"observer_receipt"`
}

type LandedLocalMergeResult struct {
	Schema           int                                           `json:"schema"`
	Owner            string                                        `json:"owner"`
	Status           string                                        `json:"status"`
	Version          string                                        `json:"version"`
	OrderID          string                                        `json:"order_id"`
	StageIndex       int                                           `json:"stage_index"`
	AttemptID        string                                        `json:"attempt_id"`
	SessionID        string                                        `json:"session_id"`
	Candidate        localTaskFileRef                              `json:"candidate"`
	Head             string                                        `json:"head"`
	Tree             string                                        `json:"tree"`
	OriginalHost     landedMergeHost                               `json:"original_host"`
	RetiredEffectIDs []string                                      `json:"retired_effect_ids"`
	Sources          map[string]loopruntime.SessionReadbackFileRef `json:"sources"`
	Effects          []string                                      `json:"effects"`
}

type landedMergeHost struct {
	InstanceID string                             `json:"host_instance_id"`
	ContextID  string                             `json:"context_id"`
	RequestID  string                             `json:"request_id"`
	Binary     loopruntime.SessionReadbackFileRef `json:"binary"`
}

type landedMergeIntent struct {
	Schema      int                                `json:"schema"`
	Project     string                             `json:"project"`
	Proof       loopruntime.SessionReadbackFileRef `json:"proof"`
	Version     string                             `json:"version"`
	Before      []byte                             `json:"before_snapshot"`
	After       []byte                             `json:"after_snapshot"`
	Integration loopruntime.SessionReadbackFileRef `json:"integration"`
}

// This operation never starts a Host or touches a VM. The retained original
// execution proof and an exact already-landed Git commit authorize only a local
// canonical projection repair while the original context lock is unowned.
func InspectLandedLocalMerge(project, proofPath, proofSHA string) (LandedLocalMergeResult, error) {
	return reconcileLandedLocalMerge(project, proofPath, proofSHA, "", nil)
}
func ReconcileLandedLocalMerge(project, proofPath, proofSHA, expectedVersion string) (LandedLocalMergeResult, error) {
	if len(expectedVersion) != 64 {
		return LandedLocalMergeResult{}, fmt.Errorf("landed merge expected version is missing")
	}
	return reconcileLandedLocalMerge(project, proofPath, proofSHA, expectedVersion, nil)
}

func reconcileLandedLocalMerge(project, proofPath, proofSHA, expected string, barrier func(string) error) (r LandedLocalMergeResult, err error) {
	r = LandedLocalMergeResult{Schema: 1, Owner: "Noodle stopped local merge", Status: "refused", Effects: []string{}, Sources: map[string]loopruntime.SessionReadbackFileRef{}}
	project, err = canonicalDirectory(project)
	if err != nil {
		return r, err
	}
	dir := filepath.Join(project, ".noodle")
	lock, err := lockfile.TryLock(filepath.Join(dir, "noodle.lock"))
	if err != nil {
		return r, err
	}
	defer lock.Close()
	controlLock, err := lockfile.TryLock(filepath.Join(dir, "control.lock"))
	if err != nil {
		return r, err
	}
	defer controlLock.Close()
	proofRef := loopruntime.SessionReadbackFileRef{Path: proofPath, SHA256: proofSHA}
	var proof LandedLocalMergeProof
	if err = landedMergeReadRef(proofRef, &proof); err != nil {
		return r, err
	}
	if proof.Schema != 1 {
		return r, fmt.Errorf("landed merge proof schema differs")
	}
	snapshotPath := filepath.Join(dir, "state.snapshot.json")
	current, err := readAdmissionFile(snapshotPath)
	if err != nil {
		return r, err
	}
	evidenceDir := filepath.Join(dir, "landed-local-merge", proofSHA)
	intentPath := filepath.Join(evidenceDir, "intent.json")
	var intent landedMergeIntent
	previous, readErr := readAdmissionFile(intentPath)
	exists := readErr == nil
	if readErr != nil && !os.IsNotExist(readErr) {
		return r, readErr
	}
	before := current
	if exists {
		if err = decodeStoppedReview(previous, &intent); err != nil {
			return r, err
		}
		if intent.Schema != 1 || intent.Project != project || intent.Proof != proofRef || (!bytes.Equal(current, intent.Before) && !bytes.Equal(current, intent.After)) {
			return r, fmt.Errorf("landed merge intent or original snapshot changed")
		}
		before = intent.Before
	}
	var snapshot reducer.DurableSnapshot
	if err = decodeStoppedReview(before, &snapshot); err != nil {
		return r, err
	}
	r, err = validateLandedLocalMerge(project, snapshot, proof)
	if err != nil {
		return r, err
	}
	integration, err := loopruntime.ReadSessionReadbackFile(filepath.Join(dir, "integration-branch.json"), nil)
	if err != nil {
		return r, err
	}
	identity, err := json.Marshal(struct {
		Project     string
		Proof       loopruntime.SessionReadbackFileRef
		SnapshotSHA string
		Integration loopruntime.SessionReadbackFileRef
	}{project, proofRef, recoveryDigest(before), integration})
	if err != nil {
		return r, err
	}
	var history = map[string]loopruntime.SessionReadbackFileRef{}
	for _, name := range []string{"control-ack.ndjson", "loop-events.ndjson"} {
		source, sourceErr := loopruntime.ReadSessionReadbackFile(filepath.Join(dir, name), nil)
		if sourceErr != nil {
			return r, sourceErr
		}
		history[name] = source
		r.Sources[name] = source
	}
	identity, err = json.Marshal(struct {
		Identity json.RawMessage
		History  map[string]loopruntime.SessionReadbackFileRef
	}{identity, history})
	if err != nil {
		return r, err
	}
	r.Version = recoveryDigest(identity)
	r.Sources["proof"] = proofRef
	r.Sources["canonical_before"] = loopruntime.SessionReadbackFileRef{Path: snapshotPath, SHA256: recoveryDigest(before)}
	r.Sources["integration"] = integration
	if exists && (intent.Version != r.Version || intent.Integration != integration) {
		return r, fmt.Errorf("landed merge original identity changed")
	}
	if expected != "" && expected != r.Version {
		return r, fmt.Errorf("landed merge expected version changed")
	}
	if !exists {
		intent = landedMergeIntent{Schema: 1, Project: project, Proof: proofRef, Version: r.Version, Before: before, Integration: integration}
		intent.After, err = landedLocalMergeAfter(snapshot, r, time.Now().UTC())
		if err != nil {
			return r, err
		}
	} else {
		var after reducer.DurableSnapshot
		if err = decodeStoppedReview(intent.After, &after); err != nil {
			return r, err
		}
		calculated, calcErr := landedLocalMergeAfter(snapshot, r, after.GeneratedAt)
		if calcErr != nil || !bytes.Equal(calculated, intent.After) {
			return r, fmt.Errorf("landed merge recorded transition differs: %v", calcErr)
		}
	}
	if exists {
		if err = landedMergeArchiveSources(evidenceDir, intent, &r); err != nil {
			return r, err
		}
	}
	receiptPath := filepath.Join(evidenceDir, "receipt.json")
	if expected == "" {
		r.Status = "observed"
		if data, e := readAdmissionFile(receiptPath); e == nil {
			var completed LandedLocalMergeResult
			expectedReceipt := r
			expectedReceipt.Status = "reconciled"
			expectedReceipt.Effects = []string{"local_canonical_reconciliation"}
			if json.Unmarshal(data, &completed) != nil || !recoveryJSONEqual(completed, expectedReceipt) || !bytes.Equal(current, intent.After) {
				return r, fmt.Errorf("landed merge completion receipt differs")
			}
			r.Status = "reconciled"
			r.Sources["receipt"], err = loopruntime.ReadSessionReadbackFile(receiptPath, nil)
			return r, err
		} else if !os.IsNotExist(e) {
			return r, e
		}
		return r, nil
	}
	r.Effects = []string{"local_canonical_reconciliation"}
	if !exists {
		if err = os.MkdirAll(evidenceDir, 0700); err != nil {
			return r, err
		}
		if err = writeLandedMergeBytes(filepath.Join(evidenceDir, "before.json"), intent.Before); err != nil {
			return r, err
		}
		if err = writeLandedMergeBytes(filepath.Join(evidenceDir, "after.json"), intent.After); err != nil {
			return r, err
		}
		if err = writeLandedMergeJSON(intentPath, intent); err != nil {
			return r, err
		}
	}
	r.Status = "unknown"
	if barrier != nil {
		if err = barrier("after_intent"); err != nil {
			return r, err
		}
	}
	// Recheck all external sources and raw worktree bytes immediately before the
	// single canonical write. No Git merge, index refresh, filters or hooks run.
	if _, err = validateLandedLocalMerge(project, snapshot, proof); err != nil {
		return r, err
	}
	if err = landedMergeReadRef(proofRef, nil); err != nil {
		return r, err
	}
	if err = landedMergeReadRef(integration, nil); err != nil {
		return r, err
	}
	for _, source := range history {
		if err = landedMergeReadRef(source, nil); err != nil {
			return r, err
		}
	}
	fresh, err := readAdmissionFile(snapshotPath)
	if err != nil {
		return r, err
	}
	if !bytes.Equal(fresh, current) {
		return r, fmt.Errorf("canonical snapshot changed before landed merge reconciliation")
	}
	if bytes.Equal(current, intent.Before) {
		if err = filex.WriteFileAtomicDurable(snapshotPath, intent.After); err != nil {
			return r, err
		}
	}
	if barrier != nil {
		if err = barrier("after_canonical"); err != nil {
			return r, err
		}
	}
	var after reducer.DurableSnapshot
	if err = decodeStoppedReview(intent.After, &after); err != nil {
		return r, err
	}
	l := &Loop{runtimeDir: dir, canonical: after.State, canonicalLoaded: true}
	if err = l.writeProjectionState(); err != nil {
		return r, err
	}
	if err = l.writePendingReview(); err != nil {
		return r, err
	}
	if err = landedMergeArchiveSources(evidenceDir, intent, &r); err != nil {
		return r, err
	}
	r.Status = "reconciled"
	if err = writeLandedMergeJSON(receiptPath, r); err != nil {
		r.Status = "unknown"
		return r, err
	}
	r.Sources["receipt"], err = loopruntime.ReadSessionReadbackFile(receiptPath, nil)
	return r, err
}

func landedMergeArchiveSources(dir string, intent landedMergeIntent, r *LandedLocalMergeResult) error {
	for name, want := range map[string][]byte{"before.json": intent.Before, "after.json": intent.After} {
		data, err := readAdmissionFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if !bytes.Equal(data, want) {
			return fmt.Errorf("landed merge retained snapshot differs: %s", name)
		}
		key := "canonical_before"
		if name == "after.json" {
			key = "canonical_after"
		}
		r.Sources[key] = loopruntime.SessionReadbackFileRef{Path: filepath.Join(dir, name), SHA256: recoveryDigest(data)}
	}
	ref, err := loopruntime.ReadSessionReadbackFile(filepath.Join(dir, "intent.json"), nil)
	if err != nil {
		return err
	}
	r.Sources["intent"] = ref
	return nil
}

func landedMergeReadRef(ref loopruntime.SessionReadbackFileRef, value any) error {
	if !filepath.IsAbs(ref.Path) || len(ref.SHA256) != 64 {
		return fmt.Errorf("landed merge source reference is invalid")
	}
	got, err := loopruntime.ReadSessionReadbackFile(ref.Path, value)
	if err != nil {
		return err
	}
	if got != ref {
		return fmt.Errorf("landed merge source changed: %s", ref.Path)
	}
	return nil
}
func writeLandedMergeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return writeLandedMergeBytes(path, raw)
}
func writeLandedMergeBytes(path string, raw []byte) error {
	if prior, e := readAdmissionFile(path); e == nil {
		if !bytes.Equal(prior, raw) {
			return fmt.Errorf("landed merge evidence already differs: %s", path)
		}
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	return filex.WriteFileAtomicDurable(path, raw)
}

func landedLocalMergeAfter(s reducer.DurableSnapshot, r LandedLocalMergeResult, at time.Time) ([]byte, error) {
	s.State = s.State.Clone()
	s.EffectLedger = append([]reducer.EffectLedgerRecord(nil), s.EffectLedger...)
	order := s.State.Orders[r.OrderID]
	stage := order.Stages[0]
	if stage.Extra == nil {
		stage.Extra = map[string]json.RawMessage{}
	}
	fact, err := json.Marshal(map[string]any{"version": r.Version, "head": r.Head, "tree": r.Tree, "candidate": r.Candidate, "original_host": r.OriginalHost, "retired_effect_ids": r.RetiredEffectIDs})
	if err != nil {
		return nil, err
	}
	stage.Extra["landed_local_merge_reconciliation"] = fact
	stage.Status, stage.Merge = state.StageCompleted, nil
	order.Stages[0], order.Status, order.UpdatedAt = stage, state.OrderCompleted, at
	s.State.Orders[r.OrderID] = order
	delete(s.State.PendingReviews, r.OrderID)
	for i := range s.EffectLedger {
		record := &s.EffectLedger[i]
		for _, id := range r.RetiredEffectIDs {
			if record.EffectID == id {
				record.Status = reducer.EffectLedgerDone
				record.Result = &reducer.EffectResult{EffectID: id, Status: reducer.EffectResultCancelled, Timestamp: at, Error: "retired now by exact landed local merge reconciliation " + r.Version}
				break
			}
		}
	}
	s.GeneratedAt = at
	if err = validateAdmissionSnapshot(s); err != nil {
		return nil, err
	}
	encoded, err := json.MarshalIndent(s, "", "  ")
	return append(encoded, '\n'), err
}

func validateLandedMergeGit(project string, binding localTaskMergeBinding, branch string) error {
	observed, err := workspaceartifacts.ReadCleanIdentity(context.Background(), project)
	if err != nil {
		return err
	}
	if observed.Head != binding.Head || observed.Tree != binding.Tree || observed.Branch != branch {
		return fmt.Errorf("landed integration identity differs")
	}
	return nil
}
