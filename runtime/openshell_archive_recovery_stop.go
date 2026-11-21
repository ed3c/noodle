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
	capture, err := ReadOpenShellWorkspaceCapture(captureRef)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	record, baseline, sources, err := workspaceCaptureInputs(p, profile, capture.OrderID)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	sources["request"] = p.Request
	for name, source := range sources {
		if capture.Sources[name] != source {
			return SessionReadbackFileRef{}, fmt.Errorf("archive recovery original source differs: %s", name)
		}
	}
	if capture.SessionID != record.Task.SessionID || capture.NativeAttemptID != record.Task.AttemptID || capture.SandboxID != record.Worker.Identity.Sandbox.SandboxID || capture.MainProcessInstanceID != record.MainProcessInstanceID || capture.BaselineHead != baseline.Head || capture.BaselineIndexSHA256 != baseline.IndexSHA256 {
		return SessionReadbackFileRef{}, fmt.Errorf("archive recovery candidate does not bind the original attempt")
	}
	var custody OpenShellCustodyState
	if err = maintenanceVerify(sources["custody"], &custody); err != nil {
		return SessionReadbackFileRef{}, err
	}
	if err = maintenanceVerify(review, nil); err != nil {
		return SessionReadbackFileRef{}, err
	}
	conn, err := newOpenShellConnection(profile.Gateway.Binding.Gateway, profile.Gateway.TLS)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	defer conn.Close()
	return stopOpenShellArchiveRecovery(ctx, pb.NewOpenShellClient(conn), storageFailureInputs{state: custody, record: record}, p, review, captureRef, hold, output, func() error {
		if err := VerifySessionReadbackFiles(capture.Sources); err != nil {
			return err
		}
		if err := maintenanceVerify(captureRef, nil); err != nil {
			return err
		}
		if err := maintenanceVerify(review, nil); err != nil {
			return err
		}
		before, err := os.Lstat(capture.Archive.Path)
		if err != nil || !before.Mode().IsRegular() {
			return fmt.Errorf("archive recovery retained workspace is unavailable")
		}
		digest, err := pairingDigestFile(capture.Archive.Path)
		if err != nil || digest != capture.Archive.SHA256 {
			return fmt.Errorf("archive recovery retained workspace bytes changed")
		}
		after, err := os.Lstat(capture.Archive.Path)
		if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			return fmt.Errorf("archive recovery retained workspace changed during readback")
		}
		return checkOwner()
	})
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
	if stopped.GetSandbox().GetStatus().GetPhase() != pb.SandboxPhase_SANDBOX_PHASE_STOPPED {
		return SessionReadbackFileRef{}, fmt.Errorf("archive recovery original Stopped evidence is missing")
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
