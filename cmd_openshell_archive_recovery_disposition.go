package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/filex"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree/archive"
)

const archiveRecoveryCleanupReserveOperation = "reserve-workspace-capture-cleanup"
const archiveRecoveryCleanupDispositionFile = "archive-recovery-cleanup-disposition.json"

type archiveRecoveryCleanupDisposition struct {
	archiveRecoveryAppliedIdentity
	Schema                int                                     `json:"schema"`
	Kind                  string                                  `json:"kind"`
	Candidate             loopruntime.SessionReadbackFileRef      `json:"candidate"`
	LandingProof          loopruntime.SessionReadbackFileRef      `json:"landing_proof"`
	Request               loopruntime.SessionReadbackFileRef      `json:"request"`
	Stop                  loopruntime.SessionReadbackFileRef      `json:"stop"`
	Archive               loopruntime.SessionReadbackFileRef      `json:"archive"`
	Hold                  loopruntime.ArchiveRecoveryDispatchHold `json:"hold"`
	Checkout              archive.CheckoutIdentity                `json:"checkout"`
	Lease                 admission.MachineSessionLease           `json:"lease"`
	HostInstanceID        string                                  `json:"host_instance_id"`
	ContextID             string                                  `json:"context_id"`
	NativeAttemptID       string                                  `json:"native_attempt_id"`
	SandboxID             string                                  `json:"sandbox_id"`
	MainProcessInstanceID string                                  `json:"main_process_instance_id"`
	OriginalReturn        string                                  `json:"original_return"`
}

func newArchiveRecoveryCleanupDisposition(plan archiveRecoveryCleanupPlan, p loopruntime.ProgressMaintenance, hold loopruntime.ArchiveRecoveryDispatchHold, checkout archive.CheckoutIdentity, lease admission.MachineSessionLease) archiveRecoveryCleanupDisposition {
	return archiveRecoveryCleanupDisposition{
		archiveRecoveryAppliedIdentity: archiveRecoveryAppliedIdentity{OrderID: plan.OrderID, AttemptID: plan.AttemptID,
			SessionID: plan.SessionID, Head: plan.Head, Tree: plan.Tree},
		Schema: 1, Kind: "archive_recovery_cleanup_disposition", Candidate: plan.Candidate, LandingProof: plan.LandingProof,
		Request: plan.Request, Stop: plan.Stop, Archive: plan.Archive, HostInstanceID: p.HostInstance, ContextID: p.ContextID,
		NativeAttemptID: plan.NativeAttemptID, SandboxID: plan.SandboxID, MainProcessInstanceID: plan.MainProcessInstanceID,
		Hold: hold, Checkout: checkout, Lease: lease, OriginalReturn: "UNKNOWN",
	}
}

// The writer holds the original control and maintenance locks. Re-entry compares
// the same immutable disposition and never replaces even a refused reservation.
func archiveRecoveryCleanupContinue(session string, result *archiveRecoveryCleanupPlan, disposition archiveRecoveryCleanupDisposition, expected string) error {
	raw, err := json.Marshal(disposition)
	if err != nil {
		return err
	}
	digest := hostDigest(raw)
	result.DispositionSHA256 = digest
	if expected != "" && expected != digest {
		return fmt.Errorf("archive recovery cleanup planned disposition changed")
	}
	path := filepath.Join(session, archiveRecoveryCleanupDispositionFile)
	ref := loopruntime.SessionReadbackFileRef{Path: path, SHA256: digest}
	err = readArchiveCleanupRef(ref, nil)
	if err == nil {
		if expected != "" {
			if err = filex.SyncDir(session); err != nil {
				return err
			}
		}
		result.Disposition, result.WaitFor = &ref, "original_archive_recovery_reclamation_owner"
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("archive recovery cleanup original disposition cannot be replaced: %w", err)
	}
	if expected == "" {
		result.WaitFor = ""
		result.Next, err = archiveRecoveryCleanupReserveNext(*result)
		return err
	}
	if err = writeArchiveRecoveryCleanupDisposition(path, raw); err != nil {
		return err
	}
	if err = readArchiveCleanupRef(ref, nil); err != nil {
		return err
	}
	result.Disposition, result.WaitFor = &ref, "original_archive_recovery_reclamation_owner"
	result.Effects = []string{"archive_recovery_cleanup_disposition_recorded"}
	return nil
}

// Publish only fully synced bytes, and leave an existing disposition untouched.
func writeArchiveRecoveryCleanupDisposition(path string, raw []byte) error {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".archive-cleanup-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(raw)
	if err == nil {
		err = file.Sync()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return err
	}
	if err = os.Link(file.Name(), path); err != nil {
		return err
	}
	return filex.SyncDir(directory)
}

func archiveRecoveryCleanupReserveNext(plan archiveRecoveryCleanupPlan) (*archiveRecoveryNext, error) {
	path, err := os.Executable()
	if err != nil {
		return nil, err
	}
	binary, err := hostReadbackBinaryRef(path)
	if err != nil {
		return nil, err
	}
	return &archiveRecoveryNext{Owner: "Noodle", Kind: "executable", Operation: archiveRecoveryCleanupReserveOperation, Executable: binary,
		Argv: []string{binary.Path, "openshell", "progress-maintenance", archiveRecoveryCleanupReserveOperation,
			"--candidate-file", plan.Candidate.Path, "--expected-candidate-sha256", plan.Candidate.SHA256,
			"--landing-proof-file", plan.LandingProof.Path, "--expected-landing-proof-sha256", plan.LandingProof.SHA256,
			"--expected-disposition-sha256", plan.DispositionSHA256}}, nil
}
