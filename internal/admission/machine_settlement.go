package admission

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
)

// MachineSessionBinding comes from the original dispatch, not a GC scan.
type MachineSessionBinding struct {
	ContextID    string `json:"context_id"`
	ProjectDir   string `json:"project_dir"`
	OrderID      string `json:"order_id"`
	StageIndex   int    `json:"stage_index"`
	RetryCount   int    `json:"retry_count"`
	AttemptID    string `json:"attempt_id"`
	SessionID    string `json:"session_id"`
	Runtime      string `json:"runtime"`
	WorktreePath string `json:"worktree_path"`
}

type MachineSettlementFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// The original cleanup actor supplies its source-bound reclamation and runtime
// reader result. A completed worker, removed checkout or native ACK alone cannot
// produce this proof.
type MachineReclamation struct {
	Binding               MachineSessionBinding            `json:"binding"`
	CheckoutOperation     string                           `json:"checkout_operation"`
	Sources               map[string]MachineSettlementFile `json:"sources"`
	RuntimeState          string                           `json:"runtime_state"`
	NativeAttemptID       string                           `json:"native_attempt_id"`
	SandboxID             string                           `json:"sandbox_id"`
	NativeDeletionOutcome string                           `json:"native_deletion_outcome"`
}

type MachineSessionLease struct {
	ContextID  string                 `json:"context_id"`
	ID         string                 `json:"id"`
	Owner      string                 `json:"owner"`
	Source     string                 `json:"source"`
	Binding    *MachineSessionBinding `json:"binding"`
	State      string                 `json:"state"`
	HoldReason string                 `json:"hold_reason"`
}

func validateMachineSession(binding MachineSessionBinding) error {
	for _, id := range []string{binding.ContextID, binding.OrderID, binding.AttemptID, binding.SessionID, binding.Runtime} {
		if id == "" || strings.ContainsRune(id, 0) {
			return errors.New("original machine session identity missing")
		}
	}
	if binding.StageIndex < 0 || binding.RetryCount < 0 || !filepath.IsAbs(binding.ProjectDir) ||
		filepath.Clean(binding.ProjectDir) != binding.ProjectDir || !filepath.IsAbs(binding.WorktreePath) ||
		filepath.Clean(binding.WorktreePath) != binding.WorktreePath {
		return errors.New("original machine session paths or attempt differ")
	}
	return nil
}

func validateMachineReclamation(proof MachineReclamation) error {
	if err := validateMachineSession(proof.Binding); err != nil {
		return err
	}
	if proof.CheckoutOperation == "" {
		return errors.New("original checkout reclamation operation missing")
	}
	required := []string{"checkout_packet", "checkout_removal", "checkout_verification_release"}
	if proof.CheckoutOperation == RetainedRejectedReviewCheckout {
		if proof.Binding.Runtime != "openshell" {
			return errors.New("rejected review requires original OpenShell session settlement")
		}
		required = []string{"canonical_snapshot", "rejected_review_events", "rejected_review_release", "native_custody"}
	} else if _, recovered := proof.Sources["archive_recovery_disposition"]; recovered {
		if proof.Binding.Runtime != "openshell" {
			return errors.New("archive recovery requires original OpenShell session settlement")
		}
		required = append(required, "archive_recovery_disposition", "archive_recovery_landing", "archive_recovery_stop", "native_custody")
	} else {
		required = append(required, "main_ack")
	}
	for _, role := range required {
		ref := proof.Sources[role]
		digest, err := hex.DecodeString(ref.SHA256)
		if !filepath.IsAbs(ref.Path) || filepath.Clean(ref.Path) != ref.Path || len(digest) != sha256.Size || err != nil {
			return fmt.Errorf("original machine reclamation source missing: %s", role)
		}
	}
	switch proof.Binding.Runtime {
	case "process":
		if proof.RuntimeState != "process_exited" || proof.NativeAttemptID != "" || proof.SandboxID != "" || proof.NativeDeletionOutcome != "" {
			return errors.New("original process settlement outcome differs")
		}
	case "openshell":
		if proof.RuntimeState != "retired" || proof.NativeAttemptID == "" || proof.SandboxID == "" || proof.NativeDeletionOutcome == "" {
			return errors.New("original VM retirement and disk absence are unresolved")
		}
	default:
		return errors.New("original runtime settlement observer unavailable")
	}
	return nil
}

func verifyMachineSettlementFiles(refs map[string]MachineSettlementFile) error {
	for _, ref := range refs {
		if err := canonicalCapacityPath(ref.Path); err != nil {
			return err
		}
		before, err := os.Lstat(ref.Path)
		if err != nil || !before.Mode().IsRegular() || before.Size() > 64<<20 {
			return errors.New("original machine settlement source unavailable or unbounded")
		}
		file, err := os.Open(ref.Path)
		if err != nil {
			return err
		}
		digest := sha256.New()
		_, readErr := io.Copy(digest, io.LimitReader(file, before.Size()+1))
		after, statErr := file.Stat()
		closeErr := file.Close()
		current, pathErr := os.Lstat(ref.Path)
		if err := errors.Join(readErr, statErr, closeErr, pathErr); err != nil {
			return err
		}
		if !os.SameFile(before, after) || !os.SameFile(before, current) || before.Size() != after.Size() ||
			before.Size() != current.Size() || !before.ModTime().Equal(after.ModTime()) ||
			!before.ModTime().Equal(current.ModTime()) || hex.EncodeToString(digest.Sum(nil)) != ref.SHA256 {
			return errors.New("original machine settlement source changed")
		}
	}
	return nil
}

// BindSession retains the original lease nonce even if its write ACK is lost.
// A crash before this binding is durable leaves a named hold, never an expiry.
func (p *Permit) BindSession(binding MachineSessionBinding) error {
	if p == nil {
		return nil
	}
	b := p.allocation.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if p.allocation.machineID == "" {
		return nil
	}
	if p.released {
		return errors.New("original resource permit already returned")
	}
	if err := validateMachineSession(binding); err != nil {
		return err
	}
	if attempt := p.allocation.machineAttempt; attempt != nil &&
		(attempt.StageIndex != binding.StageIndex || attempt.RetryCount != binding.RetryCount) {
		return errors.New("original machine session attempt differs from admission")
	}
	return b.machine.access(false, func(state *machineState) (bool, error) {
		lease, ok := state.Leases[p.allocation.machineID]
		if !ok || lease.Owner != b.machine.owner || !lease.EffectsPossible || lease.Settlement != nil ||
			lease.ContextID != binding.ContextID || lease.OrderID != binding.OrderID || lease.Runtime != binding.Runtime {
			return false, errors.New("original machine session lease differs")
		}
		if lease.Binding != nil {
			if *lease.Binding != binding {
				return false, errors.New("original machine session binding changed")
			}
			return false, nil
		}
		lease.Binding = &binding
		state.Leases[p.allocation.machineID] = lease
		return true, nil
	})
}

// MachineSessionLeases reads only this generation's original known set. Old
// unbound rows remain observable holds; another generation cannot release them.
func (b *Budget) MachineSessionLeases(contextID, project string) ([]MachineSessionLease, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.machine == nil {
		return nil, nil
	}
	var result []MachineSessionLease
	err := b.machine.access(false, func(state *machineState) (bool, error) {
		result = selectMachineSessionLeases(*state, b.machine.owner, contextID, project)
		return false, nil
	})
	return result, err
}

// ReadMachineSessionLeases observes the original held rows without creating a
// Budget, initializing a machine ledger or binding a replacement lease. An
// omitted row cannot establish prior settlement or absence of native effects.
func ReadMachineSessionLeases(directory, owner, contextID, project string) ([]MachineSessionLease, error) {
	if owner == "" || contextID == "" || !filepath.IsAbs(project) || filepath.Clean(project) != project {
		return nil, errors.New("original machine session scope missing")
	}
	state, err := readArchivedMachineJournal(directory)
	if err != nil {
		return nil, err
	}
	result := selectMachineSessionLeases(state, owner, contextID, project)
	for _, selected := range result {
		row := state.Leases[selected.ID]
		if selected.ID == "" || row.Source == "" || row.MemoryBytes == 0 || row.CPUs == 0 || len(row.Disks) == 0 {
			return nil, errors.New("original selected machine lease is incomplete")
		}
		for volume, size := range row.Disks {
			if volume == "" || size == 0 {
				return nil, errors.New("original selected machine disk lease is incomplete")
			}
		}
		if err := errors.Join(validateMachinePreparation(row), validateMachineLeaseSettlement(row)); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func selectMachineSessionLeases(state machineState, owner, contextID, project string) []MachineSessionLease {
	var result []MachineSessionLease
	for id, lease := range state.Leases {
		if lease.Owner != owner || lease.Settlement != nil {
			continue
		}
		row := MachineSessionLease{ContextID: lease.ContextID, ID: id, Owner: lease.Owner, Source: lease.Source, State: "held"}
		if lease.Binding == nil {
			if lease.ContextID != "" && lease.ContextID != contextID {
				continue
			}
			row.HoldReason = "original_machine_session_binding_missing"
		} else {
			if lease.Binding.ContextID != contextID || lease.Binding.ProjectDir != project {
				continue
			}
			binding := *lease.Binding
			row.Binding = &binding
		}
		result = append(result, row)
	}
	return result
}

// VerifyArchiveRecoveryMachineLease observes the exact original worker nonce
// before checkout or VM effects. A settled result names this same disposition.
func (b *Budget) VerifyArchiveRecoveryMachineLease(original MachineSessionLease, disposition MachineSettlementFile) (bool, error) {
	proof, err := b.ReadArchiveRecoverySettlement(original, disposition)
	return proof != nil, err
}

// ReadArchiveRecoverySettlement returns only the recorded effect for the exact
// original nonce and disposition. A still-held but qualified lease has no proof.
func (b *Budget) ReadArchiveRecoverySettlement(original MachineSessionLease, disposition MachineSettlementFile) (proof *MachineReclamation, err error) {
	return b.ReadArchiveRecoverySettlementForCleanup(original, disposition, nil)
}

// ReadArchiveRecoverySettlementForCleanup admits one exact retained in-memory
// permit for the archive-only Stop/retirement path. Settlement itself still
// requires the joined permit and its observable original shared quota return.
func (b *Budget) ReadArchiveRecoverySettlementForCleanup(original MachineSessionLease, disposition MachineSettlementFile, permit *Permit) (proof *MachineReclamation, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.machine == nil || original.Owner != b.machine.owner || !validArchiveRecoveryLeaseSelection(original, disposition) {
		return nil, errors.New("original archive recovery machine owner or lease binding differs")
	}
	var retained *allocation
	if permit != nil {
		retained = permit.allocation
		if retained == nil || retained.budget != b || retained.machineID != original.ID || retained.refs != 1 ||
			!retained.effectsPossible || retained.resident || permit.released || permit.replacementReserved ||
			b.machineAllocations[retained.machineKey] != retained {
			return nil, errors.New("archive recovery retained permit differs from original lease")
		}
	}
	for _, active := range b.machineAllocations {
		if active.machineID == original.ID && active.refs > 0 {
			if active != retained {
				return nil, errors.New("original worker resource permit is still active")
			}
		}
	}
	if b.shared != nil {
		if err = b.shared.access(0, false, func(state *quotaState) error {
			found := false
			for id, row := range state.Leases {
				if row.Context != original.ContextID || row.Resident {
					continue
				}
				if retained == nil || id != retained.sharedID || row.Owner != b.shared.owner {
					return errors.New("original archive recovery shared worker quota has not returned")
				}
				found = true
			}
			if retained != nil && !found {
				return errors.New("original archive recovery retained permit lost its shared quota binding")
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	err = b.machine.access(false, func(state *machineState) (bool, error) {
		lease, recorded, err := archiveRecoverySettlementIn(state, original, disposition)
		if recorded != nil && retained != nil {
			return false, errors.New("settled archive recovery cannot retain an active permit")
		}
		if err != nil || recorded != nil {
			proof = recorded
			return false, err
		}
		return false, b.verifyMachineSettlementSource(state, lease, *original.Binding)
	})
	return proof, err
}

// ReconcileExitedArchiveWorkerQuota is called only while the original Host
// generation, request, runtime and custody are excluded. The caller must have
// checked native Stop before offering cleanup and retirement before release.
func (b *Budget) ReconcileExitedArchiveWorkerQuota(original MachineSessionLease, disposition MachineSettlementFile, generation string, release bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.machine == nil || b.shared == nil || generation == "" || b.shared.owner != generation ||
		original.Owner != b.machine.owner || !validArchiveRecoveryLeaseSelection(original, disposition) {
		return errors.New("original exited archive accounting owner differs")
	}
	if len(b.machineAllocations) != 0 || b.active != 0 {
		return errors.New("exited archive accounting cannot carry active permits")
	}
	if err := b.machine.access(false, func(state *machineState) (bool, error) {
		lease, settled, err := archiveRecoverySettlementIn(state, original, disposition)
		if err != nil || settled != nil {
			if err == nil {
				err = errors.New("original exited archive lease is already settled; use terminal readback")
			}
			return false, err
		}
		for id, row := range state.Leases {
			if id != original.ID && row.ContextID == original.ContextID && row.EffectsPossible && row.Settlement == nil && row.PreparationClosed == nil {
				return false, errors.New("another unsettled lease shares the original exited archive context")
			}
		}
		return false, b.verifyMachineSettlementSource(state, lease, *original.Binding)
	}); err != nil {
		return err
	}
	var selectedID string
	if err := b.shared.access(0, false, func(state *quotaState) error {
		for id, row := range state.Leases {
			if row.Resident || (row.Owner != generation && row.Context != original.ContextID) {
				continue
			}
			if row.Owner != generation || row.Context != original.ContextID || selectedID != "" {
				return errors.New("original exited archive worker quota is foreign or ambiguous")
			}
			selectedID = id
		}
		return nil
	}); err != nil || !release || selectedID == "" {
		return err
	}
	return b.shared.update(0, func(state *quotaState) error {
		for id, row := range state.Leases {
			if row.Resident || (row.Owner != generation && row.Context != original.ContextID) {
				continue
			}
			if id != selectedID || row.Owner != generation || row.Context != original.ContextID {
				return errors.New("original exited archive worker quota changed before return")
			}
		}
		if row, ok := state.Leases[selectedID]; ok {
			if row.Resident || row.Owner != generation || row.Context != original.ContextID {
				return errors.New("original exited archive worker quota identity changed before return")
			}
			delete(state.Leases, selectedID)
		}
		return nil
	})
}

func validArchiveRecoveryLeaseSelection(original MachineSessionLease, disposition MachineSettlementFile) bool {
	return original.Binding != nil && original.Binding.Runtime == "openshell" && original.ContextID == original.Binding.ContextID &&
		original.ID != "" && original.Owner != "" && original.Source != "" && filepath.IsAbs(disposition.Path) && len(disposition.SHA256) == 64
}

func archiveRecoverySettlementIn(state *machineState, original MachineSessionLease, disposition MachineSettlementFile) (machineLease, *MachineReclamation, error) {
	lease, exists := state.Leases[original.ID]
	if !exists || lease.Owner != original.Owner || lease.Source != original.Source || lease.Binding == nil ||
		*lease.Binding != *original.Binding || !lease.EffectsPossible {
		return lease, nil, errors.New("original archive recovery machine lease differs or is missing")
	}
	for id, row := range state.Leases {
		if id != original.ID && row.Owner == original.Owner && row.Binding != nil && *row.Binding == *original.Binding {
			return lease, nil, errors.New("original archive recovery machine lease is ambiguous")
		}
	}
	if err := validateMachineLeaseSettlement(lease); err != nil {
		return lease, nil, err
	}
	if lease.Settlement == nil {
		return lease, nil, nil
	}
	if lease.Settlement.Sources["archive_recovery_disposition"] != disposition {
		return lease, nil, errors.New("original archive recovery machine lease was settled for a different disposition")
	}
	copy := *lease.Settlement
	copy.Sources = maps.Clone(copy.Sources)
	return lease, &copy, nil
}

// ReadArchivedArchiveRecoverySettlement observes a completed receipt without a
// controller, lease or writable lock. A concurrently replaced ledger requires
// fresh readback, even if the caller's remembered settlement looked valid.
func ReadArchivedArchiveRecoverySettlement(directory string, original MachineSessionLease, disposition MachineSettlementFile) (*MachineReclamation, error) {
	if !validArchiveRecoveryLeaseSelection(original, disposition) {
		return nil, errors.New("original archive recovery machine owner or lease binding differs")
	}
	state, err := readArchivedMachineJournal(directory)
	if err != nil {
		return nil, err
	}
	_, proof, err := archiveRecoverySettlementIn(&state, original, disposition)
	return proof, err
}

// The immutable snapshot path verifies the machine identity and original inode
// window without taking the machine writer lock. Scoped observations must
// validate their selected row. Effectful admission still validates all rows.
func readArchivedMachineJournal(directory string) (machineState, error) {
	var state machineState
	if err := canonicalCapacityPath(directory); err != nil {
		return state, fmt.Errorf("original machine capacity directory %s: %w", directory, err)
	}
	if err := capacityFileIdentity(directory, true); err != nil {
		return state, err
	}
	if err := capacityFileIdentity(filepath.Join(directory, "capacity.lock"), false); err != nil {
		return state, err
	}
	path := filepath.Join(directory, "capacity.json")
	if err := capacityFileIdentity(path, false); err != nil {
		return state, err
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() > 64<<20 {
		return state, errors.New("original machine ledger is unavailable or unbounded")
	}
	file, err := os.Open(path)
	if err != nil {
		return state, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, (64<<20)+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	current, currentErr := os.Lstat(path)
	if err = errors.Join(readErr, statErr, closeErr, currentErr); err != nil {
		return state, err
	}
	if int64(len(raw)) != before.Size() || !os.SameFile(before, after) || !os.SameFile(before, current) ||
		before.Size() != after.Size() || before.Size() != current.Size() ||
		!before.ModTime().Equal(after.ModTime()) || !before.ModTime().Equal(current.ModTime()) {
		return state, errors.New("original machine ledger changed during readback")
	}
	if err = capacityFileIdentity(path, false); err != nil {
		return state, err
	}
	if err = uniqueCapacityJSON(json.NewDecoder(bytes.NewReader(raw))); err != nil {
		return state, err
	}
	reader := json.NewDecoder(bytes.NewReader(raw))
	reader.DisallowUnknownFields()
	if err = reader.Decode(&state); err != nil {
		return machineState{}, err
	}
	if err = reader.Decode(new(any)); err != io.EOF {
		return machineState{}, errors.New("original machine ledger has trailing content")
	}
	identity, err := nativeMachineIdentity()
	if err != nil {
		return machineState{}, err
	}
	if state.Schema != 1 || state.Identity != identity || state.Policy != MachineAdmissionPolicy || state.Capacity == nil ||
		state.Leases == nil || validateCapacityPolicy(*state.Capacity) != nil {
		return machineState{}, errors.New("original machine ledger policy or identity differs")
	}
	return state, nil
}

func (b *Budget) verifyMachineSettlementSource(state *machineState, lease machineLease, binding MachineSessionBinding) error {
	source := b.resourceSources[binding.ContextID]
	if source == nil || source.read == nil {
		return errors.New("original machine profile source unavailable")
	}
	claim, err := source.read(binding.Runtime)
	if err != nil || validateResourceClaim(claim) != nil || claim.Source != lease.Source || state.Capacity == nil || !sameAdmissionCapacity(claim.Policy, *state.Capacity) {
		return errors.New("original machine profile source changed")
	}
	return nil
}

// SettleMachineSession updates the same original row. Its immutable settlement
// serves as the release ACK, so a lost result is read back without a new nonce.
func (b *Budget) SettleMachineSession(original MachineSessionLease, proof MachineReclamation) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.machine == nil || original.Binding == nil || original.Owner != b.machine.owner || *original.Binding != proof.Binding {
		return errors.New("original machine settlement owner or binding differs")
	}
	for _, active := range b.machineAllocations {
		if active.machineID == original.ID && active.refs > 0 {
			return errors.New("original worker resource permit is still active")
		}
	}
	if proof.CheckoutOperation == RetainedRejectedReviewCheckout {
		if b.shared == nil {
			return errors.New("original rejected review shared quota unavailable")
		}
		if err := b.shared.access(0, false, func(state *quotaState) error {
			for _, row := range state.Leases {
				if row.Context == original.ContextID {
					return errors.New("original rejected review shared quota has not returned")
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return b.machine.access(false, func(state *machineState) (bool, error) {
		lease, exists := state.Leases[original.ID]
		if !exists || lease.Owner != original.Owner || lease.Source != original.Source || lease.Binding == nil ||
			*lease.Binding != proof.Binding || !lease.EffectsPossible {
			return false, errors.New("original machine settlement lease differs")
		}
		if proof.CheckoutOperation == RetainedRejectedReviewCheckout {
			recorded, err := rejectedReviewSettlementIn(state, original, proof.Sources["rejected_review_release"])
			if err != nil || recorded != nil {
				return false, err
			}
		}
		if lease.Settlement != nil {
			if lease.Settlement.CheckoutOperation == RetainedRejectedReviewCheckout {
				return false, errors.New("original machine lease was settled for retained rejected review")
			}
			return false, nil
		}
		if err := b.verifyMachineSettlementSource(state, lease, proof.Binding); err != nil {
			return false, err
		}
		if err := validateMachineReclamation(proof); err != nil {
			return false, err
		}
		if err := verifyMachineSettlementFiles(proof.Sources); err != nil {
			return false, err
		}
		lease.Settlement = &proof
		state.Leases[original.ID] = lease
		return true, nil
	})
}

func validateMachineLeaseSettlement(lease machineLease) error {
	if lease.Binding != nil {
		if err := validateMachineSession(*lease.Binding); err != nil {
			return err
		}
		if lease.ContextID != lease.Binding.ContextID || lease.OrderID != lease.Binding.OrderID || lease.Runtime != lease.Binding.Runtime || !lease.EffectsPossible {
			return errors.New("original machine lease/session differs")
		}
	}
	if lease.Settlement != nil {
		if lease.Binding == nil || *lease.Binding != lease.Settlement.Binding {
			return errors.New("original machine settled identity differs")
		}
		return validateMachineReclamation(*lease.Settlement)
	}
	return nil
}
