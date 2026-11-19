package admission

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
	for _, role := range []string{"main_ack", "checkout_packet", "checkout_removal", "checkout_verification_release"} {
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
		for id, lease := range state.Leases {
			if lease.Owner != b.machine.owner || lease.Settlement != nil {
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
		return false, nil
	})
	return result, err
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
	return b.machine.access(false, func(state *machineState) (bool, error) {
		lease, exists := state.Leases[original.ID]
		if !exists || lease.Owner != original.Owner || lease.Source != original.Source || lease.Binding == nil ||
			*lease.Binding != proof.Binding || !lease.EffectsPossible {
			return false, errors.New("original machine settlement lease differs")
		}
		if lease.Settlement != nil {
			return false, nil
		}
		source := b.resourceSources[proof.Binding.ContextID]
		if source == nil || source.read == nil {
			return false, errors.New("original machine profile source unavailable")
		}
		claim, err := source.read(proof.Binding.Runtime)
		if err != nil || validateResourceClaim(claim) != nil || claim.Source != lease.Source || state.Capacity == nil || !sameAdmissionCapacity(claim.Policy, *state.Capacity) {
			return false, errors.New("original machine profile source changed")
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
