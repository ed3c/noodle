package admission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

type machinePreparation struct {
	Source PreparationSource `json:"source"`
	Claim  ResourceClaim     `json:"claim"`
	SHA256 string            `json:"source_sha256"`
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
	if lease.Preparation == nil {
		return nil
	}
	bound, err := preparationBinding(lease.Preparation.Source, lease.Preparation.Claim)
	if err != nil || !reflect.DeepEqual(bound, lease.Preparation) {
		return errors.New("original machine preparation source changed")
	}
	if lease.Preparation.Source.Purpose == "host-resident" && (lease.ContextID != "" || lease.OrderID != "" || lease.Binding != nil || lease.Settlement != nil) {
		return errors.New("resident lifetime cannot become a worker session")
	}
	return nil
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
		if observed := b.resourceSources["original-native-preparation"]; observed != nil && observed.source == claim.Source && observed.policy == claim.Policy {
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
		if !exists || lease.Owner != b.machine.owner || lease.Preparation == nil || lease.Preparation.SHA256 != ref.SHA256 || lease.ContextID != "" || lease.Settlement != nil {
			return false, errors.New("original machine preparation effect owner differs")
		}
		if lease.EffectsPossible {
			return false, nil
		}
		lease.EffectsPossible = true
		state.Leases[ref.ID] = lease
		return true, nil
	})
}
