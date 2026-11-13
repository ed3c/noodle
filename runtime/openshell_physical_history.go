package runtime

import (
	"fmt"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
)

// These are historical physical observations, not current compute absence,
// business completion, maintenance eligibility or permission to send controls.
type OpenShellPhysicalHistory struct {
	Custody         SessionReadbackFileRef
	Handover        *SessionReadbackFileRef
	OriginalRecords int
	LaterStopped    []OpenShellPhysicalRecord
}

type OpenShellPhysicalRecord struct {
	Name, SandboxID, MainProcessInstanceID, StopRequestID string
}

// InspectOpenShellPhysicalHistory reuses the original custody proof. It does
// not reconstruct execution outcomes or send RPCs, and never modifies a ledger.
func InspectOpenShellPhysicalHistory(ref SessionReadbackFileRef) (OpenShellPhysicalHistory, error) {
	result := OpenShellPhysicalHistory{Custody: ref, LaterStopped: []OpenShellPhysicalRecord{}}
	var state OpenShellCustodyState
	if err := maintenanceVerify(ref, &state); err != nil {
		return result, err
	}
	if state.Version != openShellCustodyVersion || len(state.Credentials) != 0 || len(state.PendingRetained) != 0 {
		return result, fmt.Errorf("Source physical history requires original v4 custody without pending retained or credential ownership")
	}
	if err := (&OpenShellCustody{state: state}).validateLedger(); err != nil {
		return result, err
	}
	originalNames := map[string]bool{}
	if state.Handover != nil {
		if err := verifyCustodyHandover(*state.Handover, state); err != nil {
			return result, err
		}
		var proof OpenShellCustodyHandover
		if err := maintenanceVerify(*state.Handover, &proof); err != nil {
			return result, err
		}
		var before OpenShellCustodyState
		if err := maintenanceVerify(proof.Before, &before); err != nil {
			return result, err
		}
		var release OpenShellHostRelease
		if err := maintenanceVerify(proof.Release, &release); err != nil {
			return result, err
		}
		if release.Schema != 3 || release.Cold == nil {
			return result, fmt.Errorf("Source physical history has no selected cold inventory boundary")
		}
		var inventory StorageFailureInventory
		if err := maintenanceVerify(release.Cold.Inventory, &inventory); err != nil {
			return result, err
		}
		byName := map[string]OpenShellCustodyRecord{}
		for _, record := range state.Records {
			byName[record.Name] = record
		}
		// The old inventory remains the native ID/main/spec oracle. Later
		// business diagnostics may progress without changing those identities.
		current := before
		current.Records = make([]OpenShellCustodyRecord, 0, len(before.Records))
		for _, record := range before.Records {
			selected, ok := byName[record.Name]
			if !ok {
				return result, fmt.Errorf("original Source physical record is missing")
			}
			originalNames[record.Name] = true
			current.Records = append(current.Records, selected)
		}
		if err := validateStorageFailureInventory(storageFailureInputs{state: current}, inventory, pb.SandboxPhase_SANDBOX_PHASE_STOPPED); err != nil {
			return result, err
		}
		result.OriginalRecords, result.Handover = len(before.Records), state.Handover
	}
	for _, record := range state.Records {
		if originalNames[record.Name] {
			continue
		}
		if !record.CreateAcknowledged || record.StopRequestID == "" || !record.StopAcknowledged || record.Stopped == nil || record.Worker.StopPending || record.MainProcessInstanceID == "" {
			return result, fmt.Errorf("later Source physical Stop evidence is incomplete: %s", record.Name)
		}
		result.LaterStopped = append(result.LaterStopped, OpenShellPhysicalRecord{record.Name, record.Worker.Identity.Sandbox.SandboxID, record.MainProcessInstanceID, record.StopRequestID})
	}
	return result, maintenanceVerify(ref, nil)
}
