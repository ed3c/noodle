package admission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// PreparationSource is fixed by the original producer from its bound inputs and
// planned roots. It identifies a lifetime, not a fabricated worker or session.
type PreparationSource struct {
	Purpose  string            `json:"purpose"`
	Bindings map[string]string `json:"bindings"`
}

type PreparationRef struct {
	ID     string `json:"id"`
	SHA256 string `json:"source_sha256"`
}

const MaxPreparationObservationWindow = time.Duration(MaxRecoverySamples)*PollInterval + time.Duration(MaxRecoverySamples+1)*SampleTimeout + time.Second

type machinePreparation struct {
	Source PreparationSource `json:"source"`
	Claim  ResourceClaim     `json:"claim"`
	SHA256 string            `json:"source_sha256"`
}

// ReadPreparation returns only the original pool's bound lifetime for native
// generation/Goal readback. It neither adopts nor starts an unknown lifetime.
func (b *Budget) ReadPreparation(ref PreparationRef) (source PreparationSource, claim ResourceClaim, effects bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.machine == nil {
		return source, claim, false, errors.New("original preparation owner unavailable")
	}
	err = b.machine.access(false, func(state *machineState) (bool, error) {
		lease, exists := state.Leases[ref.ID]
		if !exists || lease.Preparation == nil || lease.Preparation.SHA256 != ref.SHA256 || lease.Settlement != nil || lease.PreparationClosed != nil {
			return false, errors.New("original preparation lifetime missing or closed")
		}
		source, claim, effects = lease.Preparation.Source, lease.Preparation.Claim, lease.EffectsPossible
		return false, nil
	})
	return source, claim, effects, err
}

// VerifyPreparationSource is a readback for the original native producer and
// Host consumer. The FileRef in a caller's JSON alone cannot select a lifetime.
func (b *Budget) VerifyPreparationSource(ref PreparationRef, source PreparationSource, claim ResourceClaim) error {
	observed, exists, err := b.preparationRead(source, claim)
	if err != nil {
		return err
	}
	if !exists || observed != ref {
		return errors.New("original preparation source or nonce differs")
	}
	return nil
}

func (b *Budget) CancelUnstartedPreparation(ref PreparationRef) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.machine == nil {
		return errors.New("original preparation owner unavailable")
	}
	return b.machine.access(false, func(state *machineState) (bool, error) {
		lease, exists := state.Leases[ref.ID]
		if err := refuseManagedSwitchCancellation(state, ref); err != nil {
			return false, err
		}
		if !exists || lease.Owner != b.machine.owner || lease.Preparation == nil || lease.Preparation.SHA256 != ref.SHA256 || lease.EffectsPossible {
			return false, errors.New("original preparation cannot be proved unstarted")
		}
		delete(state.Leases, ref.ID)
		return true, nil
	})
}

func preparationBinding(source PreparationSource, claim ResourceClaim) (*machinePreparation, error) {
	if source.Purpose != "task-preparation" && source.Purpose != "host-resident" {
		return nil, errors.New("machine preparation lifetime unsupported")
	}
	if len(source.Bindings) == 0 {
		return nil, errors.New("machine preparation original sources missing")
	}
	for key, value := range source.Bindings {
		if key == "" || value == "" || strings.ContainsRune(key+value, 0) {
			return nil, errors.New("machine preparation original source invalid")
		}
	}
	if err := validateResourceClaim(claim); err != nil {
		return nil, err
	}
	bound := &machinePreparation{Source: source, Claim: claim}
	raw, err := json.Marshal(bound)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	bound.SHA256 = hex.EncodeToString(digest[:])
	return bound, nil
}

func validateMachinePreparation(lease machineLease) error {
	if err := validateManagedResidentSwitch(lease); err != nil {
		return err
	}
	if err := validateEmptyResidentRetirement(lease); err != nil {
		return err
	}
	if lease.Preparation == nil {
		if lease.PreparationPromoted || lease.PreparationClosed != nil {
			return errors.New("promoted preparation source missing")
		}
		return nil
	}
	bound, err := preparationBinding(lease.Preparation.Source, lease.Preparation.Claim)
	if err != nil || !reflect.DeepEqual(bound, lease.Preparation) {
		return errors.New("original machine preparation source changed")
	}
	if lease.Preparation.Source.Purpose == "host-resident" && (lease.ContextID != "" || lease.OrderID != "" || lease.Binding != nil || lease.Settlement != nil) {
		return errors.New("resident lifetime cannot become a worker session")
	}
	if lease.PreparationPromoted && (lease.ContextID == "" || lease.OrderID == "" || lease.Runtime == "" || !lease.EffectsPossible) {
		return errors.New("promoted preparation original worker identity missing")
	}
	if lease.PreparationClosed != nil {
		if lease.Preparation.Source.Purpose != "host-resident" || !lease.EffectsPossible || lease.PreparationPromoted || len(lease.PreparationClosed) != 1 {
			return errors.New("original resident closure lifetime differs")
		}
		ref := lease.PreparationClosed["resident_exit"]
		digest, err := hex.DecodeString(ref.SHA256)
		if !filepath.IsAbs(ref.Path) || filepath.Clean(ref.Path) != ref.Path || err != nil || len(digest) != sha256.Size {
			return errors.New("original resident exit source missing")
		}
	}
	return nil
}

// The original pool owner reads its own exit witness and freshly confirms the
// native process-group absence inside this return operation. No caller JSON
// can replace that native readback or turn a running/unknown lifetime into GC.
func (b *Budget) CloseResidentPreparation(ref PreparationRef, readback func(PreparationSource) (map[string]MachineSettlementFile, error)) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.machine == nil || readback == nil {
		return errors.New("original resident exit reader missing")
	}
	return b.machine.access(false, func(state *machineState) (bool, error) {
		lease, exists := state.Leases[ref.ID]
		if !exists || lease.Owner != b.machine.owner || lease.Preparation == nil || lease.Preparation.SHA256 != ref.SHA256 || lease.Preparation.Source.Purpose != "host-resident" || !lease.EffectsPossible || lease.ContextID != "" {
			return false, errors.New("original resident lifetime differs")
		}
		if lease.PreparationClosed != nil {
			if lease.EmptyRetirement != nil {
				return false, verifyEmptyResidentRetirementFiles(*lease.EmptyRetirement)
			}
			return false, nil
		}
		refs, err := readback(lease.Preparation.Source)
		if err != nil {
			return false, err
		}
		lease.PreparationClosed = refs
		if err = validateMachinePreparation(lease); err != nil {
			return false, err
		}
		if err = verifyMachineSettlementFiles(refs); err != nil {
			return false, err
		}
		state.Leases[ref.ID] = lease
		return true, nil
	})
}

// BindPreparation is called by the native Host after verifying its original
// control journal. The same row moves to the real context/order; no capacity is
// returned and no session identity is invented before dispatch.
func (b *Budget) BindPreparation(ref PreparationRef, contextID, orderID, runtime string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.machine == nil || contextID == "" || orderID == "" || runtime == "" || strings.ContainsRune(contextID+orderID+runtime, 0) {
		return errors.New("original preparation worker destination missing")
	}
	key := contextID + "\x00" + orderID
	if prior, exists := b.preparedMachine[key]; exists && prior != ref {
		return errors.New("original worker preparation reference changed")
	}
	err := b.machine.access(false, func(state *machineState) (bool, error) {
		lease, exists := state.Leases[ref.ID]
		if !exists || lease.Preparation == nil || lease.Preparation.SHA256 != ref.SHA256 || lease.Preparation.Source.Purpose != "task-preparation" || !lease.EffectsPossible || lease.Settlement != nil {
			return false, errors.New("original task preparation missing or unresolved")
		}
		if err := refuseManagedSwitchSource(state, lease.Preparation.Source); err != nil {
			return false, err
		}
		if lease.ContextID != "" {
			if lease.Owner != b.machine.owner || lease.ContextID != contextID || lease.OrderID != orderID || lease.Runtime != runtime {
				return false, errors.New("original preparation already belongs to another worker")
			}
			if lease.Binding != nil {
				return false, nil // The exact first attempt can coexist with later leases.
			}
		}
		for id, existing := range state.Leases {
			if id != ref.ID && existing.Owner == b.machine.owner && existing.ContextID == contextID && existing.OrderID == orderID && existing.Settlement == nil {
				return false, errors.New("original worker already has a different machine lifetime")
			}
		}
		if lease.ContextID != "" {
			return false, nil
		}
		lease.Owner, lease.ContextID, lease.OrderID, lease.Runtime = b.machine.owner, contextID, orderID, runtime
		state.Leases[ref.ID] = lease
		return true, nil
	})
	if err != nil {
		return err
	}
	if b.preparedMachine == nil {
		b.preparedMachine = make(map[string]PreparationRef)
	}
	b.preparedMachine[key] = ref
	return nil
}

// MachineAttempt identifies the original Loop position before a session exists.
type MachineAttempt struct {
	StageIndex int
	RetryCount int
}

var errPreparationDispatched = errors.New("original preparation already has a dispatched attempt")

func (m *machineCapacity) promotePreparation(ref PreparationRef, contextID, orderID, runtime string, claim ResourceClaim, sample Sample, rate float64, rateValid bool, normals int, now time.Time, reconcile bool) error {
	if err := validateResourceClaim(claim); err != nil {
		return err
	}
	return m.access(false, func(state *machineState) (bool, error) {
		original, exists := state.Leases[ref.ID]
		if !exists || original.Owner != m.owner || original.ContextID != contextID || original.OrderID != orderID || original.Runtime != runtime || original.Preparation == nil || original.Preparation.SHA256 != ref.SHA256 || !original.EffectsPossible || original.Settlement != nil {
			return false, errors.New("original preparation handoff differs")
		}
		if claim.Policy != original.Preparation.Claim.Policy {
			return false, errors.New("original preparation operator policy changed")
		}
		if original.PreparationPromoted {
			if original.Binding != nil {
				return false, errPreparationDispatched
			}
			if !reconcile {
				return false, errors.New("original promoted preparation requires dispatch readback")
			}
			if original.Source != claim.Source || original.MemoryBytes != claim.MemoryBytes || original.CPUs != claim.CPUs || state.Capacity == nil || !sameAdmissionCapacity(*state.Capacity, claim.Policy) {
				return false, errors.New("original promoted worker resource source changed")
			}
			volumes := map[string]uint64{}
			for _, path := range claim.DiskPaths {
				disk, err := m.probe(path)
				if err != nil {
					return false, err
				}
				volumes[disk.Volume] = claim.DiskBytes
			}
			if !reflect.DeepEqual(original.Disks, volumes) {
				return false, errors.New("original promoted worker disk source changed")
			}
			return false, nil
		}
		// Replacement is only in this locked in-memory state. A failed check
		// writes nothing; no other process can observe a release/reacquire gap.
		delete(state.Leases, ref.ID)
		if _, err := m.reserve(state, ref.ID, contextID, orderID, runtime, claim, sample, rate, rateValid, normals, now); err != nil {
			return false, err
		}
		promoted := state.Leases[ref.ID]
		promoted.Preparation, promoted.PreparationPromoted, promoted.EffectsPossible = original.Preparation, true, true
		state.Leases[ref.ID] = promoted
		return true, nil
	})
}

// Once a preparation is assigned, reserve a different worker lease under the
// same journal lock. An existing attempt or an unknown binding cannot qualify.
func (m *machineCapacity) acquirePreparedSuccessor(ref PreparationRef, nonce, contextID, orderID, runtime string, attempt MachineAttempt, claim ResourceClaim, sample Sample, rate float64, rateValid bool, normals int, now time.Time) error {
	return m.access(false, func(state *machineState) (bool, error) {
		original := state.Leases[ref.ID]
		if nonce == ref.ID || original.Owner != m.owner || original.ContextID != contextID || original.OrderID != orderID || original.Runtime != runtime ||
			original.Preparation == nil || original.Preparation.SHA256 != ref.SHA256 || !original.PreparationPromoted || original.Binding == nil || original.Settlement != nil ||
			original.Preparation.Claim.Policy != claim.Policy || original.Source != claim.Source || original.MemoryBytes != claim.MemoryBytes || original.CPUs != claim.CPUs {
			return false, errors.New("original prepared attempt lineage differs")
		}
		if err := refuseRetiredPreparationHost(state, original.Preparation.Source); err != nil {
			return false, err
		}
		for id, lease := range state.Leases {
			if id == nonce || lease.ContextID != contextID || lease.OrderID != orderID {
				continue
			}
			bound := lease.Binding
			if lease.Owner != m.owner || lease.Runtime != runtime || !lease.EffectsPossible || bound == nil || validateMachineSession(*bound) != nil ||
				bound.ContextID != contextID || bound.OrderID != orderID || bound.Runtime != runtime || bound.ProjectDir != original.Binding.ProjectDir {
				return false, errors.New("original prepared attempt requires session binding readback")
			}
			if attempt.StageIndex < bound.StageIndex || attempt.StageIndex == bound.StageIndex && attempt.RetryCount <= bound.RetryCount {
				return false, errors.New("original prepared attempt already dispatched or stale")
			}
		}
		return m.reserve(state, nonce, contextID, orderID, runtime, claim, sample, rate, rateValid, normals, now)
	})
}

// PreparationWindow is the original sampler's bounded recovery window. The
// first sample has no rate; every recovery sample needs the preceding sample.
func PreparationWindow(policy CapacityPolicy) (time.Duration, error) {
	if err := validateCapacityPolicy(policy); err != nil {
		return 0, err
	}
	return time.Duration(policy.RecoverySamples)*PollInterval + time.Duration(policy.RecoverySamples+1)*SampleTimeout + time.Second, nil
}

// PrepareMachineResources uses the original sampler without requiring a Host
// that does not exist yet. It never consumes any Host worker quota.
func (b *Budget) PrepareMachineResources(ctx context.Context, source PreparationSource, claim ResourceClaim, sampler Sampler) (PreparationRef, error) {
	if sampler == nil {
		return PreparationRef{}, fmt.Errorf("%w: machine bootstrap sampler missing", ErrDeferred)
	}
	if _, err := preparationBinding(source, claim); err != nil {
		return PreparationRef{}, fmt.Errorf("%w: %v", ErrDeferred, err)
	}
	// Lost ACK and restart must read the original nonce before any new sampling
	// or creation. A pressure change does not erase an already-held lifetime.
	if ref, exists, err := b.preparationRead(source, claim); err != nil || exists {
		return ref, err
	}
	window, _ := PreparationWindow(claim.Policy)
	ctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	const observer = "original-native-preparation"
	b.BindResourceSource(observer, "preparation", func(string) (ResourceClaim, error) { return claim, nil })
	for index := 0; index <= claim.Policy.RecoverySamples; index++ {
		if index != 0 {
			timer := time.NewTimer(PollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return PreparationRef{}, fmt.Errorf("%w: machine bootstrap window: %v", ErrDeferred, ctx.Err())
			case <-timer.C:
			}
		}
		readCtx, readCancel := context.WithTimeout(ctx, SampleTimeout)
		sample, err := sampler.Read(readCtx)
		readCancel()
		b.Observe(sample, err)
	}
	return b.reservePreparation(source, claim)
}

func (b *Budget) preparationRead(source PreparationSource, claim ResourceClaim) (ref PreparationRef, exists bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	bound, err := preparationBinding(source, claim)
	if err != nil {
		return ref, false, err
	}
	if b.machine == nil {
		return ref, false, fmt.Errorf("%w: native machine owner unavailable", ErrDeferred)
	}
	err = b.machine.access(false, func(state *machineState) (bool, error) {
		if err := refuseRetiredPreparationHost(state, source); err != nil {
			return false, err
		}
		for id, lease := range state.Leases {
			if lease.Preparation == nil || lease.Preparation.SHA256 != bound.SHA256 {
				continue
			}
			if exists || !reflect.DeepEqual(lease.Preparation, bound) {
				return false, errors.New("original machine preparation is ambiguous")
			}
			ref, exists = PreparationRef{id, bound.SHA256}, true
		}
		return false, nil
	})
	return ref, exists, err
}

// reservePreparation performs lookup, nonce minting and all volume claims in
// the same existing pool lock. No lookup-before-mint race crosses that lock.
func (b *Budget) reservePreparation(source PreparationSource, claim ResourceClaim) (ref PreparationRef, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	bound, err := preparationBinding(source, claim)
	if err != nil {
		return ref, err
	}
	if b.machine == nil {
		return ref, fmt.Errorf("%w: native machine owner unavailable", ErrDeferred)
	}
	err = b.machine.access(false, func(state *machineState) (bool, error) {
		if err := refuseRetiredPreparationHost(state, source); err != nil {
			return false, err
		}
		for id, lease := range state.Leases {
			if lease.Preparation != nil && lease.Preparation.SHA256 == bound.SHA256 {
				if !reflect.DeepEqual(lease.Preparation, bound) || ref.ID != "" {
					return false, errors.New("original machine preparation is ambiguous")
				}
				ref = PreparationRef{id, bound.SHA256}
			}
		}
		if ref.ID != "" {
			return false, nil
		}
		id, err := machineNonce()
		if err != nil {
			return false, err
		}
		sample := Sample{}
		if b.previous != nil {
			sample = *b.previous
		}
		normals := 0
		if observed := b.resourceSources["original-native-preparation"]; observed != nil && observed.source == claim.Source && sameAdmissionCapacity(observed.policy, claim.Policy) {
			normals = observed.normals
		}
		if _, err = b.machine.reserve(state, id, "", "", "", claim, sample, b.rate, b.rateValid, normals, b.now()); err != nil {
			return false, fmt.Errorf("%w: %v", ErrDeferred, err)
		}
		lease := state.Leases[id]
		lease.Preparation = bound
		state.Leases[id] = lease
		ref = PreparationRef{id, bound.SHA256}
		return true, nil
	})
	return ref, err
}

// StartPreparationEffects fences the very first producer effect. It never
// changes back to unstarted; failures retain the original source and nonce.
func (b *Budget) StartPreparationEffects(ref PreparationRef) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.machine == nil {
		return fmt.Errorf("%w: native machine owner unavailable", ErrDeferred)
	}
	return b.machine.access(false, func(state *machineState) (bool, error) {
		lease, exists := state.Leases[ref.ID]
		if !exists || lease.Owner != b.machine.owner || lease.Preparation == nil || lease.Preparation.SHA256 != ref.SHA256 || lease.ContextID != "" || lease.Settlement != nil || lease.PreparationClosed != nil {
			return false, errors.New("original machine preparation effect owner differs")
		}
		if err := refuseManagedSwitchSource(state, lease.Preparation.Source); err != nil {
			return false, err
		}
		for _, old := range state.Leases {
			if old.ManagedSwitch != nil && old.ManagedSwitch.Preparation == ref && old.PreparationClosed == nil {
				return false, errors.New("original managed predecessor exit is not closed")
			}
		}
		if lease.EffectsPossible {
			return false, nil
		}
		lease.EffectsPossible = true
		state.Leases[ref.ID] = lease
		return true, nil
	})
}
