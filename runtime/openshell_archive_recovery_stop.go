package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/lockfile"
)

type OpenShellArchiveRecoveryStop struct {
	Schema          int                         `json:"schema"`
	Kind            string                      `json:"kind"`
	OriginalReturn  string                      `json:"original_return"`
	RequestID       string                      `json:"request_id"`
	Review          SessionReadbackFileRef      `json:"review"`
	Capture         SessionReadbackFileRef      `json:"capture"`
	OriginalRequest SessionReadbackFileRef      `json:"original_request"`
	Reservation     SessionReadbackFileRef      `json:"reservation"`
	NativeStop      SessionReadbackFileRef      `json:"native_stop"`
	Before          SessionReadbackFileRef      `json:"before_inventory"`
	After           SessionReadbackFileRef      `json:"after_inventory"`
	DispatchHold    ArchiveRecoveryDispatchHold `json:"dispatch_hold"`
}

type ArchiveRecoveryDispatchHold struct {
	Acknowledgements SessionReadbackFileRef `json:"acknowledgements"`
	PauseCommandID   string                 `json:"pause_command_id"`
}

// StopOpenShellArchiveRecovery must be called under the original control lock
// after independently verifying the reviewed archive and the original paused owner.
func StopOpenShellArchiveRecovery(ctx context.Context, p ProgressMaintenance, profile OpenShellProfile, review, captureRef SessionReadbackFileRef, hold ArchiveRecoveryDispatchHold, output string, checkOwner func() error) (SessionReadbackFileRef, error) {
	if output != filepath.Join(p.SessionDirectory, "archive-recovery-stop") {
		return SessionReadbackFileRef{}, fmt.Errorf("archive recovery Stop output must remain in the original session")
	}
	if hold.PauseCommandID == "" || hold.Acknowledgements.Path != filepath.Join(filepath.Dir(filepath.Dir(p.SessionDirectory)), "control-ack.ndjson") {
		return SessionReadbackFileRef{}, fmt.Errorf("archive recovery Stop requires the original native dispatch hold")
	}
	if err := maintenanceVerify(hold.Acknowledgements, nil); err != nil {
		return SessionReadbackFileRef{}, err
	}
	lock, err := lockfile.TryLock(filepath.Join(p.SessionDirectory, "maintenance.lock"))
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	defer lock.Close()
	capture, in, err := archiveRecoveryStopInputs(p, profile, review, captureRef, false, false)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	conn, err := newOpenShellConnection(profile.Gateway.Binding.Gateway, profile.Gateway.TLS)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	defer conn.Close()
	return stopOpenShellArchiveRecovery(ctx, pb.NewOpenShellClient(conn), in, p, review, captureRef, hold, output, func() error {
		if err := VerifySessionReadbackFiles(capture.Sources); err != nil {
			return err
		}
		if err := maintenanceVerify(captureRef, nil); err != nil {
			return err
		}
		if err := maintenanceVerify(review, nil); err != nil {
			return err
		}
		if err := verifyRetainedArchiveRecoveryBytes(capture.Archive); err != nil {
			return err
		}
		return checkOwner()
	})
}

// ObserveOpenShellArchiveRecovery checks the original VM identity and phase without sending Stop.
// A Stopped phase still requires the original Stop intent and acknowledgement.
func ObserveOpenShellArchiveRecovery(ctx context.Context, p ProgressMaintenance, profile OpenShellProfile, review, captureRef SessionReadbackFileRef, hold *ArchiveRecoveryDispatchHold) error {
	// A confirmed Stop receipt witnessed the exact archive before the effect.
	// Re-verify the current custody through that witness if its bytes moved.
	return observeBoundOpenShellArchiveRecovery(ctx, p, profile, review, captureRef, hold, hold != nil)
}

// ObserveOpenShellArchiveRecapture checks the same original Ready VM. The
// caller must separately compare the historical archive and its verified tree.
func ObserveOpenShellArchiveRecapture(ctx context.Context, p ProgressMaintenance, profile OpenShellProfile, review, captureRef SessionReadbackFileRef) error {
	return observeBoundOpenShellArchiveRecovery(ctx, p, profile, review, captureRef, nil, true)
}

func observeBoundOpenShellArchiveRecovery(ctx context.Context, p ProgressMaintenance, profile OpenShellProfile, review, captureRef SessionReadbackFileRef, hold *ArchiveRecoveryDispatchHold, historical bool) error {
	_, in, err := archiveRecoveryStopInputs(p, profile, review, captureRef, historical, hold != nil)
	if err != nil {
		return err
	}
	conn, err := newOpenShellConnection(profile.Gateway.Binding.Gateway, profile.Gateway.TLS)
	if err != nil {
		return err
	}
	defer conn.Close()
	return observeOpenShellArchiveRecovery(ctx, pb.NewOpenShellClient(conn), in, p, review, captureRef, hold)
}

func observeOpenShellArchiveRecovery(ctx context.Context, client pb.OpenShellClient, in storageFailureInputs, p ProgressMaintenance, review, captureRef SessionReadbackFileRef, hold *ArchiveRecoveryDispatchHold) error {
	if hold != nil {
		var intent OpenShellArchiveRecoveryStop
		reservation, err := ReadSessionReadbackFile(filepath.Join(p.SessionDirectory, "archive-recovery-stop-intent.json"), &intent)
		if err != nil {
			return err
		}
		if intent.DispatchHold != *hold {
			return fmt.Errorf("archive recovery Stop original dispatch hold differs")
		}
		if _, err := readArchiveRecoveryStop(in, p, review, captureRef, filepath.Join(p.SessionDirectory, "archive-recovery-stop"), reservation, intent); err != nil {
			return err
		}
	}
	scope := &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: in.record.Worker.Identity.Sandbox.Workspace}}
	response, err := client.GetSandbox(ctx, &pb.GetSandboxRequest{Name: in.record.Name, WorkspaceScope: scope})
	if err != nil {
		return err
	}
	if hold != nil {
		return verifyArchiveRecoveryStopped(in.record, response)
	}
	if err := maintenanceNative(in.record, response.GetSandbox()); err != nil {
		return err
	}
	if response.GetSandbox().GetStatus().GetPhase() != pb.SandboxPhase_SANDBOX_PHASE_READY {
		return fmt.Errorf("archive recovery original VM phase %s is required", pb.SandboxPhase_SANDBOX_PHASE_READY)
	}
	return nil
}

func archiveRecoveryStopInputs(p ProgressMaintenance, profile OpenShellProfile, review, captureRef SessionReadbackFileRef, historical, afterArchiveStop bool) (OpenShellWorkspaceCapture, storageFailureInputs, error) {
	capture, err := ReadOpenShellWorkspaceCapture(captureRef)
	if err != nil {
		return OpenShellWorkspaceCapture{}, storageFailureInputs{}, err
	}
	record, baseline, sources, err := workspaceCaptureInputs(p, profile, capture.OrderID, afterArchiveStop)
	if err != nil {
		return OpenShellWorkspaceCapture{}, storageFailureInputs{}, err
	}
	sources["request"] = p.Request
	for name, source := range sources {
		if historical && name == "custody" && capture.Sources[name].Path == source.Path && len(capture.Sources[name].SHA256) == 64 {
			continue
		}
		if capture.Sources[name] != source {
			return OpenShellWorkspaceCapture{}, storageFailureInputs{}, fmt.Errorf("archive recovery original source differs: %s", name)
		}
	}
	if capture.SessionID != record.Task.SessionID || capture.NativeAttemptID != record.Task.AttemptID || capture.SandboxID != record.Worker.Identity.Sandbox.SandboxID || capture.MainProcessInstanceID != record.MainProcessInstanceID || capture.BaselineHead != baseline.Head || capture.BaselineIndexSHA256 != baseline.IndexSHA256 {
		return OpenShellWorkspaceCapture{}, storageFailureInputs{}, fmt.Errorf("archive recovery candidate does not bind the original attempt")
	}
	var custody OpenShellCustodyState
	if err = maintenanceVerify(sources["custody"], &custody); err != nil {
		return OpenShellWorkspaceCapture{}, storageFailureInputs{}, err
	}
	if err = maintenanceVerify(review, nil); err != nil {
		return OpenShellWorkspaceCapture{}, storageFailureInputs{}, err
	}
	return capture, storageFailureInputs{state: custody, record: record}, nil
}

func stopOpenShellArchiveRecovery(ctx context.Context, client pb.OpenShellClient, in storageFailureInputs, p ProgressMaintenance, review, capture SessionReadbackFileRef, hold ArchiveRecoveryDispatchHold, output string, checkOwner func() error) (SessionReadbackFileRef, error) {
	var empty SessionReadbackFileRef
	reservationPath := filepath.Join(p.SessionDirectory, "archive-recovery-stop-intent.json")
	completePath := filepath.Join(output, "archive-recovery-stop.json")
	var intent OpenShellArchiveRecoveryStop
	reservation, err := ReadSessionReadbackFile(reservationPath, &intent)
	if err == nil {
		if intent.DispatchHold != hold {
			return empty, fmt.Errorf("archive recovery Stop original dispatch hold differs")
		}
		return readArchiveRecoveryStop(in, p, review, capture, output, reservation, intent)
	}
	if !os.IsNotExist(err) {
		return empty, err
	}
	if err = checkOwner(); err != nil {
		return empty, err
	}
	if err = os.Mkdir(output, 0700); err != nil {
		return empty, err
	}
	reserved := false
	defer func() {
		if !reserved {
			_ = os.RemoveAll(output)
		}
	}()
	before, err := observeStorageFailureInventory(ctx, client, in, output, "archive-before", pb.SandboxPhase_SANDBOX_PHASE_READY)
	if err != nil {
		return empty, err
	}
	beforeRef, err := writeStorageFailureInventory(output, "archive-before", before)
	if err != nil {
		return empty, err
	}
	id, err := openShellUUID()
	if err != nil {
		return empty, err
	}
	intent = OpenShellArchiveRecoveryStop{Schema: 1, Kind: "archive_recovery_stop", OriginalReturn: "UNKNOWN", RequestID: id, OriginalRequest: p.Request, Review: review, Capture: capture, Before: beforeRef, DispatchHold: hold}
	if err = checkOwner(); err != nil {
		return empty, err
	}
	reserved = true
	if err = maintenanceWrite(reservationPath, intent); err != nil {
		return empty, err
	}
	reservation, err = ReadSessionReadbackFile(reservationPath, nil)
	if err != nil {
		return empty, err
	}
	scope := &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: in.record.Worker.Identity.Sandbox.Workspace}}
	fresh, err := client.GetSandbox(ctx, &pb.GetSandboxRequest{Name: in.record.Name, WorkspaceScope: scope})
	if err != nil {
		return empty, err
	}
	if err = maintenanceNative(in.record, fresh.GetSandbox()); err != nil || fresh.GetSandbox().GetStatus().GetPhase() != pb.SandboxPhase_SANDBOX_PHASE_READY {
		return empty, fmt.Errorf("archive recovery original Ready VM changed before Stop")
	}
	stop, err := recordMaintenanceNativeStop(ctx, client, in.record, output, ProgressMaintenanceStop{Plan: review, RequestID: id})
	if err != nil {
		return empty, err
	}
	after, err := observeStorageFailureInventory(ctx, client, in, output, "archive-after", pb.SandboxPhase_SANDBOX_PHASE_STOPPED)
	if err != nil {
		return empty, err
	}
	intent.After, err = writeStorageFailureInventory(output, "archive-after", after)
	if err != nil {
		return empty, err
	}
	if err = checkOwner(); err != nil {
		return empty, err
	}
	intent.NativeStop, intent.Reservation = stop, reservation
	if err = maintenanceWrite(completePath, intent); err != nil {
		return empty, err
	}
	return ReadSessionReadbackFile(completePath, nil)
}

// A recorded intent without exact completion is UNKNOWN. Re-entry never sends
// a second native Stop, including when the first acknowledgement was lost.
func readArchiveRecoveryStop(in storageFailureInputs, p ProgressMaintenance, review, capture SessionReadbackFileRef, output string, reservation SessionReadbackFileRef, intent OpenShellArchiveRecoveryStop) (SessionReadbackFileRef, error) {
	var complete OpenShellArchiveRecoveryStop
	ref, err := ReadSessionReadbackFile(filepath.Join(output, "archive-recovery-stop.json"), &complete)
	if err != nil {
		return SessionReadbackFileRef{}, fmt.Errorf("archive recovery Stop is UNKNOWN; original Owner must read back the reserved effect: %w", err)
	}
	if intent.Schema != 1 || intent.Kind != "archive_recovery_stop" || intent.OriginalReturn != "UNKNOWN" || intent.RequestID == "" || intent.Review != review || intent.Capture != capture || intent.OriginalRequest != p.Request || complete.Reservation != reservation {
		return SessionReadbackFileRef{}, fmt.Errorf("archive recovery Stop reservation differs")
	}
	intent.NativeStop, intent.Reservation, intent.After = complete.NativeStop, complete.Reservation, complete.After
	if complete != intent {
		return SessionReadbackFileRef{}, fmt.Errorf("archive recovery Stop completion differs")
	}
	var raw ProgressMaintenanceStop
	if err := maintenanceVerify(complete.NativeStop, &raw); err != nil {
		return SessionReadbackFileRef{}, err
	}
	if raw.Plan != review || raw.RequestID != intent.RequestID || raw.Acknowledgement.Path != filepath.Join(output, "native-stop-ack.json") || raw.Stopped.Path != filepath.Join(output, "native-stopped.json") {
		return SessionReadbackFileRef{}, fmt.Errorf("archive recovery native Stop binding differs")
	}
	var ack, stopped pb.SandboxResponse
	for _, result := range []struct {
		source SessionReadbackFileRef
		native *pb.SandboxResponse
	}{{raw.Acknowledgement, &ack}, {raw.Stopped, &stopped}} {
		if err := maintenanceReadNative(result.source, result.native); err != nil {
			return SessionReadbackFileRef{}, err
		}
		if err := maintenanceNative(in.record, result.native.GetSandbox()); err != nil {
			return SessionReadbackFileRef{}, err
		}
	}
	if err := verifyArchiveRecoveryStopped(in.record, &stopped); err != nil {
		return SessionReadbackFileRef{}, err
	}
	for _, check := range []struct {
		source SessionReadbackFileRef
		phase  pb.SandboxPhase
	}{{complete.Before, pb.SandboxPhase_SANDBOX_PHASE_READY}, {complete.After, pb.SandboxPhase_SANDBOX_PHASE_STOPPED}} {
		var inventory StorageFailureInventory
		if err := maintenanceVerify(check.source, &inventory); err != nil {
			return SessionReadbackFileRef{}, err
		}
		if err := validateStorageFailureInventory(in, inventory, check.phase); err != nil {
			return SessionReadbackFileRef{}, err
		}
	}
	return ref, nil
}
