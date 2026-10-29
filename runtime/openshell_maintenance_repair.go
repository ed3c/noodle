package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/workspaceartifacts"
)

type progressMaintenanceRestoreIntent struct {
	Plan, Stop, Custody SessionReadbackFileRef
}

func maintenanceRestoreRepairSources(c ProgressMaintenanceCompletion, h ProgressMaintenanceHostHandoff) (map[string]SessionReadbackFileRef, error) {
	if c.RestoreRepair == nil {
		return map[string]SessionReadbackFileRef{}, nil
	}
	repair, refs, err := readMaintenanceRestoreRepair(*c.RestoreRepair, c.Plan, c.Stop)
	if err != nil {
		return nil, err
	}
	if h.HostReceiptsBefore != repair.HostBefore {
		return nil, fmt.Errorf("maintenance repair changed the original Host before-image")
	}
	var accepted SessionReadbackFileRef
	intent, err := ReadSessionReadbackFile(filepath.Join(filepath.Dir(c.Plan.Path), "restore-repair-intent.json"), &accepted)
	if err != nil {
		return nil, err
	}
	if accepted != *c.RestoreRepair {
		return nil, fmt.Errorf("maintenance repair does not match the accepted continuation")
	}
	refs["maintenance_restore_repair_intent"] = intent
	return refs, nil
}

func readMaintenanceRestoreRepair(ref, plan, stop SessionReadbackFileRef) (ProgressMaintenanceRestoreRepair, map[string]SessionReadbackFileRef, error) {
	var repair ProgressMaintenanceRestoreRepair
	if err := maintenanceVerify(ref, &repair); err != nil {
		return repair, nil, err
	}
	if repair.Schema != 1 || repair.Plan != plan {
		return repair, nil, fmt.Errorf("maintenance restore repair belongs to a different plan")
	}
	directory := filepath.Dir(plan.Path)
	if repair.RestoreIntent.Path != filepath.Join(directory, "restore-intent.json") || repair.CustodyBefore.Path != filepath.Join(directory, "custody-before-restore.json") || repair.HostBefore.Path != filepath.Join(directory, "host-receipts-before.json") {
		return repair, nil, fmt.Errorf("maintenance restore repair does not bind the original before-images")
	}
	var intent progressMaintenanceRestoreIntent
	if err := maintenanceVerify(repair.RestoreIntent, &intent); err != nil {
		return repair, nil, err
	}
	if intent.Plan != plan || intent.Stop != stop || intent.Custody != repair.CustodyBefore {
		return repair, nil, fmt.Errorf("maintenance original restore intent differs")
	}
	refs := map[string]SessionReadbackFileRef{"maintenance_restore_repair": ref, "maintenance_restore_intent": repair.RestoreIntent, "maintenance_restore_custody_before": repair.CustodyBefore, "maintenance_restore_host_before": repair.HostBefore}
	history, err := maintenanceHostHistorySources(repair)
	if err != nil {
		return repair, nil, err
	}
	for name, source := range history {
		refs[name] = source
	}
	return repair, refs, VerifySessionReadbackFiles(refs)
}

// The first restore did not pin its whole Host snapshot in the original intent.
// Independently retained native responses must account for every history row;
// two matching mutable copies alone cannot establish that before-image.
func maintenanceHostHistorySources(repair ProgressMaintenanceRestoreRepair) (map[string]SessionReadbackFileRef, error) {
	var history ProgressMaintenanceHostHistory
	if err := maintenanceVerify(repair.HostHistory, &history); err != nil {
		return nil, err
	}
	var before []map[string]any
	if err := maintenanceVerify(repair.HostBefore, &before); err != nil {
		return nil, err
	}
	if history.Schema != 1 || len(before) == 0 || len(history.Receipts) != len(before) {
		return nil, fmt.Errorf("maintenance original Host history is incomplete")
	}
	refs := map[string]SessionReadbackFileRef{"maintenance_host_history": repair.HostHistory}
	seen := make(map[string]bool)
	for i, source := range history.Receipts {
		if source.Path == repair.HostBefore.Path || seen[source.Path] {
			return nil, fmt.Errorf("maintenance original Host receipt source is not independent")
		}
		seen[source.Path] = true
		var row map[string]any
		if err := maintenanceVerify(source, &row); err != nil {
			return nil, err
		}
		delete(row, "status")
		delete(row, "error")
		if !reflect.DeepEqual(row, before[i]) {
			return nil, fmt.Errorf("maintenance Host before-image differs from original receipt %d", i)
		}
		refs[fmt.Sprintf("maintenance_host_history_%d", i)] = source
	}
	return refs, nil
}

// Inspection recognizes only the proven pre-publication transfer failure. An
// applied patch, changed index, partial repository, or later phase stays unknown.
func InspectProgressMaintenanceRestore(ctx context.Context, planRef SessionReadbackFileRef, profile OpenShellProfile, hostBefore, hostHistory SessionReadbackFileRef) (SessionReadbackFileRef, error) {
	var p ProgressMaintenance
	if err := maintenanceVerify(planRef, &p); err != nil {
		return SessionReadbackFileRef{}, err
	}
	lock, err := lockfile.TryLock(filepath.Join(profile.Gateway.Binding.StateRoot, "owner.lock"))
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	defer lock.Close()
	original := p
	original.Sources = p.Originals
	_, d, _, record, _, err := maintenanceInputs(original, profile)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	proof, err := maintenanceStopped(planRef, p, record)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	directory := filepath.Dir(planRef.Path)
	intent, err := ReadSessionReadbackFile(filepath.Join(directory, "restore-intent.json"), nil)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	custody, err := ReadSessionReadbackFile(filepath.Join(directory, "custody-before-restore.json"), nil)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	repair := ProgressMaintenanceRestoreRepair{Schema: 1, Plan: planRef, RestoreIntent: intent, CustodyBefore: custody, HostBefore: hostBefore, HostHistory: hostHistory}
	name := filepath.Join(directory, "restore-repair-packet.json")
	if _, err := os.Lstat(name); err == nil {
		ref, err := ReadSessionReadbackFile(name, nil)
		if err != nil {
			return ref, err
		}
		var prior ProgressMaintenanceRestoreRepair
		if err := maintenanceVerify(ref, &prior); err != nil {
			return ref, err
		}
		if prior != repair {
			return ref, fmt.Errorf("maintenance restore inspection inputs changed")
		}
		if err := validateProgressMaintenanceRestoreRepair(ctx, planRef, d, profile, ref); err != nil {
			return ref, err
		}
		return ref, nil
	} else if !os.IsNotExist(err) {
		return SessionReadbackFileRef{}, err
	}
	// Keep the packet immutable even if validation fails. It never reserves a
	// publication or permits replay; the consumer repeats every live predicate.
	if err := maintenanceWrite(name, repair); err != nil {
		return SessionReadbackFileRef{}, err
	}
	ref, err := ReadSessionReadbackFile(name, nil)
	if err != nil {
		return ref, err
	}
	if _, _, err := readMaintenanceRestoreRepair(ref, planRef, proof.Reference); err != nil {
		return ref, err
	}
	if err := validateProgressMaintenanceRestoreRepair(ctx, planRef, d, profile, ref); err != nil {
		return ref, err
	}
	return ref, nil
}

func validateProgressMaintenanceRestoreRepair(ctx context.Context, planRef SessionReadbackFileRef, d openShellDispatchReceipt, profile OpenShellProfile, repairRef SessionReadbackFileRef) error {
	var p ProgressMaintenance
	if err := maintenanceVerify(planRef, &p); err != nil {
		return err
	}
	stop, err := ReadSessionReadbackFile(filepath.Join(filepath.Dir(planRef.Path), "stop-complete.json"), nil)
	if err != nil {
		return err
	}
	repair, _, err := readMaintenanceRestoreRepair(repairRef, planRef, stop)
	if err != nil {
		return err
	}
	for _, binding := range []struct {
		live   string
		before SessionReadbackFileRef
	}{
		{filepath.Join(profile.Gateway.Binding.StateRoot, "owner.json"), repair.CustodyBefore},
		{filepath.Join(p.HostDir, "receipts.json"), repair.HostBefore},
	} {
		current, err := ReadSessionReadbackFile(binding.live, nil)
		if err != nil {
			return err
		}
		if current.SHA256 != binding.before.SHA256 {
			return fmt.Errorf("maintenance pre-publication owner state changed: %s", binding.live)
		}
	}
	for key, ref := range p.Sources {
		if key != "custody" {
			if err := maintenanceVerify(ref, nil); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{
		filepath.Join(filepath.Dir(planRef.Path), "runtime-complete.json"),
		filepath.Join(filepath.Dir(planRef.Path), "restore-repair-intent.json"),
		filepath.Join(filepath.Dir(planRef.Path), "original-meta.json"),
		filepath.Join(p.SessionDirectory, "maintenance-completion.json"),
		filepath.Join(p.SessionDirectory, "delivery"),
		filepath.Join(p.SessionDirectory, "guest-session"),
	} {
		if _, err := os.Lstat(name); !os.IsNotExist(err) {
			return fmt.Errorf("maintenance restore advanced beyond the inspected transfer phase: %s", name)
		}
	}
	entries, err := os.ReadDir(p.SessionDirectory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "progress-archive-") {
			for _, name := range []string{"noodle-result.tar", "noodle-session.tar"} {
				if _, err := os.Lstat(filepath.Join(p.SessionDirectory, entry.Name(), name)); !os.IsNotExist(err) {
					return fmt.Errorf("maintenance archive retention already has effects")
				}
			}
		}
	}
	if err := workspaceartifacts.VerifyBaseline(ctx, d.Target, d.Baseline); err != nil {
		return err
	}
	return workspaceartifacts.VerifyExtracted(ctx, p.Archives["noodle-result.tar"].Path, filepath.Join(p.SessionDirectory, "returned"))
}
