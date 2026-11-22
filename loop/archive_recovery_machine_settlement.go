package loop

import (
	"fmt"
	"reflect"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree/archive"
)

// settleArchiveRecoveryMachineSession leaves the historical attempt unmodified.
// It commits only the existing resource owner's exact reclamation to its lease.
func settleArchiveRecoveryMachineSession(budget *admission.Budget, lease admission.MachineSessionLease, disposition loopruntime.SessionReadbackFileRef, checkoutOperation string, checkout archive.CheckoutReclamationEvidence, vm loopruntime.ArchiveRecoveryVMRetirementReadback) error {
	var accepted struct {
		Schema          int                                `json:"schema"`
		Kind            string                             `json:"kind"`
		OriginalReturn  string                             `json:"original_return"`
		Lease           admission.MachineSessionLease      `json:"lease"`
		Checkout        archive.CheckoutIdentity           `json:"checkout"`
		LandingProof    loopruntime.SessionReadbackFileRef `json:"landing_proof"`
		Stop            loopruntime.SessionReadbackFileRef `json:"stop"`
		SessionID       string                             `json:"session_id"`
		NativeAttemptID string                             `json:"native_attempt_id"`
		SandboxID       string                             `json:"sandbox_id"`
	}
	observed, err := loopruntime.ReadSessionReadbackFile(disposition.Path, &accepted)
	if err != nil {
		return err
	}
	if observed != disposition || accepted.Schema != 1 || accepted.Kind != "archive_recovery_cleanup_disposition" ||
		accepted.OriginalReturn != "UNKNOWN" || budget == nil || lease.Binding == nil || !reflect.DeepEqual(accepted.Lease, lease) ||
		accepted.Checkout != checkout.Identity || checkoutOperation == "" || checkout.Identity.Path != lease.Binding.WorktreePath ||
		vm.OriginalReturn != "UNKNOWN" || vm.Retirement.SessionID != accepted.SessionID || accepted.SessionID != lease.Binding.SessionID ||
		vm.Retirement.NativeAttemptID != accepted.NativeAttemptID || vm.Retirement.SandboxID != accepted.SandboxID {
		return fmt.Errorf("archive recovery machine settlement differs from original disposition")
	}
	if err = loopruntime.VerifyArchiveRecoveryVMRetirementReadback(disposition, vm); err != nil {
		return err
	}
	source := admission.MachineSettlementFile(disposition)
	settled, err := budget.VerifyArchiveRecoveryMachineLease(lease, source)
	if err != nil || settled {
		return err
	}
	var packet archive.CheckoutPacket
	packetSource, err := loopruntime.ReadSessionReadbackFile(checkout.Packet.Path, &packet)
	if err != nil {
		return err
	}
	if packetSource != loopruntime.SessionReadbackFileRef(checkout.Packet) || packet.Request.Operation != checkoutOperation {
		return fmt.Errorf("archive recovery machine settlement checkout operation differs")
	}
	reclaimed, err := archive.ReadCheckoutReclamation(packet)
	if err != nil || reclaimed != checkout {
		return fmt.Errorf("archive recovery machine settlement checkout proof differs: %v", err)
	}
	proof := admission.MachineReclamation{
		Binding: *lease.Binding, CheckoutOperation: checkoutOperation, RuntimeState: vm.Retirement.State,
		NativeAttemptID: vm.Retirement.NativeAttemptID, SandboxID: vm.Retirement.SandboxID, NativeDeletionOutcome: vm.Retirement.NativeDeletionOutcome,
		Sources: map[string]admission.MachineSettlementFile{
			"archive_recovery_disposition":  source,
			"archive_recovery_landing":      admission.MachineSettlementFile(accepted.LandingProof),
			"archive_recovery_stop":         admission.MachineSettlementFile(accepted.Stop),
			"native_custody":                admission.MachineSettlementFile(vm.Custody),
			"checkout_packet":               admission.MachineSettlementFile(checkout.Packet),
			"checkout_removal":              admission.MachineSettlementFile(checkout.Removal),
			"checkout_verification_release": admission.MachineSettlementFile(checkout.VerificationRelease),
		},
	}
	if err = prepareArchiveRecoveryReclamation(disposition, lease, &proof); err != nil {
		return err
	}
	if err = budget.SettleMachineSession(lease, proof); err != nil {
		return err
	}
	settled, err = budget.VerifyArchiveRecoveryMachineLease(lease, source)
	if err != nil {
		return err
	}
	if !settled {
		return fmt.Errorf("archive recovery machine settlement readback is missing")
	}
	return nil
}

// SettleExitedArchiveRecovery seals the same resource disposition after its
// original checkout and VM have been reclaimed, without a live worker drain.
func SettleExitedArchiveRecovery(budget *admission.Budget, scope ArchiveRecoveryReclamation, checkout archive.CheckoutReclamationEvidence, vm loopruntime.ArchiveRecoveryVMRetirementReadback) (ArchiveRecoveryReclamationResult, error) {
	result := ArchiveRecoveryReclamationResult{Status: "archive_recovery_reclamation_waiting", Disposition: scope.Disposition,
		OriginalReturn: "UNKNOWN", Effects: []string{}, Checkout: &checkout, VM: &vm}
	operation := "archive-recovery-checkout/" + scope.Disposition.SHA256
	err := finalizeArchiveRecoveryReclamation(&result, budget, scope, operation, checkout, vm)
	return result, err
}

func finalizeArchiveRecoveryReclamation(result *ArchiveRecoveryReclamationResult, budget *admission.Budget, scope ArchiveRecoveryReclamation, operation string, checkout archive.CheckoutReclamationEvidence, vm loopruntime.ArchiveRecoveryVMRetirementReadback) error {
	if err := settleArchiveRecoveryMachineSession(budget, scope.Lease, scope.Disposition, operation, checkout, vm); err != nil {
		return err
	}
	settlement, err := budget.ReadArchiveRecoverySettlement(scope.Lease, admission.MachineSettlementFile(scope.Disposition))
	if err != nil {
		return err
	}
	result.Effects = []string{"original_machine_lease_settled"}
	return sealArchiveRecoveryReclamation(result, scope, settlement)
}
