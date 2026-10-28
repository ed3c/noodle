package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/workspaceartifacts"
)

// RestoreProgressMaintenance is called only while the CLI owns the old Host
// and project locks and has proved its resident service closed.
func RestoreProgressMaintenance(ctx context.Context, planRef SessionReadbackFileRef, profile OpenShellProfile, finalize func(SessionReadbackFileRef) error) (SessionReadbackFileRef, error) {
	if finalize == nil {
		return SessionReadbackFileRef{}, fmt.Errorf("maintenance Host finalizer is missing")
	}
	var p ProgressMaintenance
	if err := maintenanceVerify(planRef, &p); err != nil {
		return SessionReadbackFileRef{}, err
	}
	lock, err := lockfile.TryLock(filepath.Join(profile.Gateway.Binding.StateRoot, "owner.lock"))
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	defer lock.Close()
	liveSources := p.Sources
	p.Sources = p.Originals
	_, d, before, record, saved, err := maintenanceInputs(p, profile)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	directory := filepath.Dir(planRef.Path)
	proof, err := maintenanceStopped(planRef, p, record)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	stopRef, stop, stopped := proof.Reference, proof.Stop, proof.Stopped
	var current OpenShellCustodyState
	ownerPath := filepath.Join(profile.Gateway.Binding.StateRoot, "owner.json")
	_, err = ReadSessionReadbackFile(ownerPath, &current)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	// The original release guard may add fresh Stopped observations, but cannot
	// introduce another operation or change a command during the handover.
	comparison := current
	comparison.Records = append([]OpenShellCustodyRecord(nil), current.Records...)
	if len(comparison.Records) != len(before.Records) {
		return SessionReadbackFileRef{}, fmt.Errorf("maintenance custody history changed")
	}
	index := -1
	for i := range comparison.Records {
		comparison.Records[i].Stopped = before.Records[i].Stopped
		if comparison.Records[i].Worker.Identity == record.Worker.Identity {
			index = i
		}
	}
	if index < 0 || !reflect.DeepEqual(comparison, before) {
		return SessionReadbackFileRef{}, fmt.Errorf("maintenance original custody changed")
	}
	for key, ref := range liveSources {
		if key != "custody" {
			if err := maintenanceVerify(ref, nil); err != nil {
				return SessionReadbackFileRef{}, err
			}
		}
	}
	currentData, err := os.ReadFile(ownerPath)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	currentPath := filepath.Join(directory, "custody-before-restore.json")
	if err := maintenanceWriteBytes(currentPath, currentData); err != nil {
		return SessionReadbackFileRef{}, err
	}
	currentRef, err := ReadSessionReadbackFile(currentPath, nil)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	if err := maintenanceWrite(filepath.Join(directory, "restore-intent.json"), map[string]any{"plan": planRef, "stop": stopRef, "custody": currentRef}); err != nil {
		return SessionReadbackFileRef{}, fmt.Errorf("maintenance restore already attempted; inspect original effects: %w", err)
	}
	for _, archive := range saved.Archives {
		dest := filepath.Join(p.SessionDirectory, "returned")
		if archive.Name == "noodle-session.tar" {
			dest = filepath.Join(p.SessionDirectory, "guest-session")
		}
		if err := workspaceartifacts.Extract(ctx, p.Archives[archive.Name].Path, dest); err != nil {
			return SessionReadbackFileRef{}, err
		}
	}
	staging, err := os.MkdirTemp(p.SessionDirectory, "progress-result-")
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	data, err := os.ReadFile(p.Archives["result.json"].Path)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	if err := filex.WriteFileAtomicDurable(filepath.Join(staging, "result.json"), data); err != nil {
		return SessionReadbackFileRef{}, err
	}
	ref, err := ReadSessionReadbackFile(filepath.Join(staging, "result.json"), nil)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	d.Progress, d.ProgressSource = &saved, &ref
	delivery, err := workspaceartifacts.Apply(ctx, d.Target, d.Baseline, filepath.Join(p.SessionDirectory, "returned"), filepath.Join(p.SessionDirectory, "delivery"))
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	head, err := workspaceartifacts.Publish(ctx, d.Target, d.Baseline, delivery, fmt.Sprintf("chore: preserve interrupted OpenShell progress\n\nRecover the verified archives for original session %s after explicit native stop and owner handover. The original return failure remains in the maintenance journal; this is not task acceptance.", d.SessionID))
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	recovered := &current.Records[index]
	recovered.StopRequestID, recovered.StopAcknowledged = stop.RequestID, true
	recovered.Stopped = openShellStoppedObservation(stopped)
	recovered.Worker.Boundary, recovered.Worker.StopPending, recovered.Worker.StopError = OpenShellBoundaryStopped, false, ""
	d.Worker = &recovered.Worker
	d.Outcome = SessionOutcome{Status: StatusCancelled, Reason: "OpenShell interrupted; archived progress recovered through explicit maintenance", ExitCode: saved.ExitCode}
	d.ProgressHead, d.Phase, d.Error, d.Delivery = head, "settled", "", delivery
	completion := ProgressMaintenanceCompletion{Schema: 1, Plan: planRef, Stop: stopRef, OriginalDispatch: p.Originals["dispatch"], OriginalCustody: p.Originals["custody"], ContextID: d.ContextID, SessionID: d.SessionID, OrderID: d.OrderID, ProgressHead: head}
	completePath := filepath.Join(directory, "runtime-complete.json")
	if err := maintenanceWrite(completePath, completion); err != nil {
		return SessionReadbackFileRef{}, err
	}
	completeRef, err := ReadSessionReadbackFile(completePath, nil)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	d.Maintenance = &completeRef
	if err := maintenanceWrite(filepath.Join(p.SessionDirectory, "maintenance-completion.json"), completeRef); err != nil {
		return SessionReadbackFileRef{}, err
	}
	ownerData, err := json.Marshal(current)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	if err := filex.WriteFileAtomicDurable(ownerPath, ownerData); err != nil {
		return SessionReadbackFileRef{}, err
	}
	// Preserve terminal metadata bytes before replacing its failed projection.
	metaPath := filepath.Join(p.SessionDirectory, "meta.json")
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	if err := maintenanceWriteBytes(filepath.Join(directory, "original-meta.json"), metaBytes); err != nil {
		return SessionReadbackFileRef{}, err
	}
	var meta map[string]any
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return SessionReadbackFileRef{}, err
	}
	meta["status"], meta["alive"], meta["exit_pending"] = "exited", false, false
	metaData, err := json.Marshal(meta)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	if err := filex.WriteFileAtomicDurable(metaPath, metaData); err != nil {
		return SessionReadbackFileRef{}, err
	}
	data, err = json.Marshal(d)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	if err := filex.WriteFileAtomicDurable(liveSources["dispatch"].Path, data); err != nil {
		return SessionReadbackFileRef{}, err
	}
	if err := finalize(completeRef); err != nil {
		return SessionReadbackFileRef{}, err
	}
	if _, err := maintenanceCompletion(d); err != nil {
		return SessionReadbackFileRef{}, err
	}
	return completeRef, nil
}

func maintenanceCompletion(d openShellDispatchReceipt) (*ProgressMaintenanceCompletion, error) {
	if d.Maintenance == nil {
		return nil, fmt.Errorf("dispatch has no explicit maintenance handover")
	}
	var c ProgressMaintenanceCompletion
	if err := maintenanceVerify(*d.Maintenance, &c); err != nil {
		return nil, err
	}
	if c.Schema != 1 || c.ContextID != d.ContextID || c.SessionID != d.SessionID || c.OrderID != d.OrderID || c.ProgressHead != d.ProgressHead || c.ProgressHead == "" {
		return nil, fmt.Errorf("maintenance completion differs from dispatch")
	}
	var original openShellDispatchReceipt
	if err := maintenanceVerify(c.OriginalDispatch, &original); err != nil {
		return nil, err
	}
	if original.Phase != "progress-unknown" || original.Error == "" || original.ContextID != d.ContextID || original.SessionID != d.SessionID || original.AttemptID != d.AttemptID || original.ProfileSHA256 != d.ProfileSHA256 || original.Target != d.Target || !reflect.DeepEqual(original.Baseline, d.Baseline) {
		return nil, fmt.Errorf("maintenance predecessor differs")
	}
	if err := maintenanceVerify(c.Plan, nil); err != nil {
		return nil, err
	}
	if err := maintenanceVerify(c.Stop, nil); err != nil {
		return nil, err
	}
	var handoff ProgressMaintenanceHostHandoff
	if _, err := ReadSessionReadbackFile(filepath.Join(filepath.Dir(c.Plan.Path), "host-handoff.json"), &handoff); err != nil {
		return nil, fmt.Errorf("maintenance Host handover is incomplete: %w", err)
	}
	if handoff.Schema != 1 || handoff.Completion != *d.Maintenance {
		return nil, fmt.Errorf("maintenance Host handover differs")
	}
	if err := maintenanceVerify(handoff.HostReceiptsBefore, nil); err != nil {
		return nil, err
	}
	if err := maintenanceVerify(handoff.HostReceiptsAfter, nil); err != nil {
		return nil, err
	}
	if err := validateMaintenanceHostHandoff(c, handoff); err != nil {
		return nil, err
	}
	return &c, nil
}
