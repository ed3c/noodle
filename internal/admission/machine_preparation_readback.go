package admission

import "errors"

// PreparationObservation describes the original recorded fence and closure.
// It grants no authority to start, settle or release the lifetime.
type PreparationObservation struct {
	Reference       PreparationRef    `json:"reference"`
	Source          PreparationSource `json:"-"`
	Owner           string            `json:"-"`
	ContextID       string            `json:"-"`
	OrderID         string            `json:"-"`
	Runtime         string            `json:"-"`
	EffectsPossible bool              `json:"effects_possible"`
	Closed          bool              `json:"closed"`
	Promoted        bool              `json:"promoted"`
	SessionBound    bool              `json:"session_bound"`
}

// ReadMachinePreparations reads all requested lifetimes under the same native
// lock. It never initializes the ledger or claims a lifetime.
func ReadMachinePreparations(dir string, refs ...PreparationRef) (map[PreparationRef]PreparationObservation, error) {
	identity, err := nativeMachineIdentity()
	if err != nil {
		return nil, err
	}
	machine := &machineCapacity{dir: dir, identity: identity}
	observed := make(map[PreparationRef]PreparationObservation, len(refs))
	err = machine.access(false, func(state *machineState) (bool, error) {
		for _, ref := range refs {
			lease, ok := state.Leases[ref.ID]
			if !ok || lease.Preparation == nil || lease.Preparation.SHA256 != ref.SHA256 {
				return false, errors.New("original preparation reference differs")
			}
			observed[ref] = PreparationObservation{
				Reference: ref, Source: lease.Preparation.Source, Owner: lease.Owner, ContextID: lease.ContextID,
				OrderID: lease.OrderID, Runtime: lease.Runtime, EffectsPossible: lease.EffectsPossible,
				Closed:   lease.PreparationClosed != nil || lease.Settlement != nil,
				Promoted: lease.PreparationPromoted, SessionBound: lease.Binding != nil,
			}
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return observed, nil
}
