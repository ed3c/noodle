package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/lockfile"
)

func maintenanceNative(record OpenShellCustodyRecord, s *pb.Sandbox) error {
	ref := record.Worker.Identity.Sandbox
	if !sameOpenShellSandbox(s, record.Name, ref.SandboxID, ref.Workspace) || s.GetSpec() == nil || s.GetStatus().GetMainProcessInstanceId() != record.MainProcessInstanceID || record.MainProcessInstanceID == "" {
		return fmt.Errorf("maintenance native sandbox identity changed")
	}
	spec, err := openShellBytes(s.GetSpec())
	if err != nil || !bytes.Equal(spec, record.Spec) {
		return fmt.Errorf("maintenance native sandbox policy changed")
	}
	return nil
}

// StopProgressMaintenance never takes over a resident process. The existing
// paused Host keeps that child handle until its normal guarded release.
func StopProgressMaintenance(ctx context.Context, planRef SessionReadbackFileRef, profile OpenShellProfile) (SessionReadbackFileRef, error) {
	var p ProgressMaintenance
	if err := maintenanceVerify(planRef, &p); err != nil {
		return SessionReadbackFileRef{}, err
	}
	directory := filepath.Dir(planRef.Path)
	lock, err := lockfile.TryLock(filepath.Join(p.SessionDirectory, "maintenance.lock"))
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	defer lock.Close()
	reservation := filepath.Join(p.SessionDirectory, "maintenance-stop-intent.json")
	if _, err := os.Lstat(reservation); !os.IsNotExist(err) {
		return SessionReadbackFileRef{}, fmt.Errorf("original attempt already reserved maintenance stop; no replay")
	}
	profile, _, state, record, _, err := maintenanceInputs(p, profile)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	conn, err := newOpenShellConnection(profile.Gateway.Binding.Gateway, profile.Gateway.TLS)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	defer conn.Close()
	client := pb.NewOpenShellClient(conn)
	scope := &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: state.Binding.Workspace}}
	before, err := client.GetSandbox(ctx, &pb.GetSandboxRequest{Name: record.Name, WorkspaceScope: scope})
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	if err := maintenanceNative(record, before.GetSandbox()); err != nil {
		return SessionReadbackFileRef{}, err
	}
	if before.GetSandbox().GetStatus().GetPhase() != pb.SandboxPhase_SANDBOX_PHASE_READY {
		return SessionReadbackFileRef{}, fmt.Errorf("maintenance original sandbox is not Ready")
	}
	if err := ValidateProgressMaintenance(p, profile); err != nil {
		return SessionReadbackFileRef{}, err
	}
	id, err := openShellUUID()
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	intent := ProgressMaintenanceStop{Plan: planRef, RequestID: id}
	transactions := filepath.Join(p.HostDir, "progress-maintenance")
	if err := os.MkdirAll(transactions, 0700); err != nil {
		return SessionReadbackFileRef{}, err
	}
	if err := maintenanceWrite(filepath.Join(transactions, filepath.Base(p.SessionDirectory)+".json"), planRef); err != nil {
		return SessionReadbackFileRef{}, err
	}
	if err := maintenanceWrite(reservation, intent); err != nil {
		return SessionReadbackFileRef{}, err
	}
	if err := maintenanceWrite(filepath.Join(directory, "stop-intent.json"), intent); err != nil {
		return SessionReadbackFileRef{}, fmt.Errorf("maintenance stop already attempted or intent unavailable: %w", err)
	}
	ack, err := client.StopSandbox(ctx, &pb.StopSandboxRequest{Name: record.Name, WorkspaceScope: scope, RequestId: id})
	if err != nil {
		return SessionReadbackFileRef{}, fmt.Errorf("maintenance stop acknowledgement unresolved; never replay: %w", err)
	}
	if err := maintenanceNative(record, ack.GetSandbox()); err != nil {
		return SessionReadbackFileRef{}, err
	}
	ackPath := filepath.Join(directory, "native-stop-ack.json")
	if err := maintenanceWriteNative(ackPath, ack); err != nil {
		return SessionReadbackFileRef{}, err
	}
	intent.Acknowledgement, err = ReadSessionReadbackFile(ackPath, nil)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	for {
		after, err := client.GetSandbox(ctx, &pb.GetSandboxRequest{Name: record.Name, WorkspaceScope: scope})
		if err != nil {
			return SessionReadbackFileRef{}, fmt.Errorf("maintenance fresh stop observation unresolved: %w", err)
		}
		if err := maintenanceNative(record, after.GetSandbox()); err != nil {
			return SessionReadbackFileRef{}, err
		}
		if after.GetSandbox().GetStatus().GetPhase() == pb.SandboxPhase_SANDBOX_PHASE_STOPPED {
			name := filepath.Join(directory, "native-stopped.json")
			if err := maintenanceWriteNative(name, after); err != nil {
				return SessionReadbackFileRef{}, err
			}
			intent.Stopped, err = ReadSessionReadbackFile(name, nil)
			if err != nil {
				return SessionReadbackFileRef{}, err
			}
			name = filepath.Join(directory, "stop-complete.json")
			if err := maintenanceWrite(name, intent); err != nil {
				return SessionReadbackFileRef{}, err
			}
			return ReadSessionReadbackFile(name, nil)
		}
		select {
		case <-ctx.Done():
			return SessionReadbackFileRef{}, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
