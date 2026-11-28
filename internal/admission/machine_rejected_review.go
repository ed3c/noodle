package admission

import (
	"errors"
	"maps"
)

const RetainedRejectedReviewCheckout = "rejected_review_retain_checkout"

// ReadRejectedReviewMachineSettlement observes the exact original nonce. A
// qualified held lease returns no settlement; a missing lease is an error.
func ReadRejectedReviewMachineSettlement(directory string, original MachineSessionLease, release MachineSettlementFile) (*MachineReclamation, error) {
	state, err := readArchivedMachineJournal(directory)
	if err != nil {
		return nil, err
	}
	return rejectedReviewSettlementIn(&state, original, release)
}

func rejectedReviewSettlementIn(state *machineState, original MachineSessionLease, release MachineSettlementFile) (*MachineReclamation, error) {
	if !validArchiveRecoveryLeaseSelection(original, release) {
		return nil, errors.New("original rejected review machine owner or release binding differs")
	}
	lease, exists := state.Leases[original.ID]
	if !exists || lease.Owner != original.Owner || lease.Source != original.Source || lease.Binding == nil ||
		*lease.Binding != *original.Binding || !lease.EffectsPossible {
		return nil, errors.New("original rejected review machine lease differs or is missing")
	}
	for id, row := range state.Leases {
		if id != original.ID && row.Owner == original.Owner && row.Binding != nil && *row.Binding == *original.Binding {
			return nil, errors.New("original rejected review machine lease is ambiguous")
		}
	}
	if err := validateMachineLeaseSettlement(lease); err != nil || lease.Settlement == nil {
		return nil, err
	}
	if lease.Settlement.CheckoutOperation != RetainedRejectedReviewCheckout || lease.Settlement.Sources["rejected_review_release"] != release {
		return nil, errors.New("original rejected review machine lease was settled for a different release")
	}
	proof := *lease.Settlement
	proof.Sources = maps.Clone(proof.Sources)
	return &proof, nil
}
