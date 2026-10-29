package runtime

import (
	"fmt"
	"path/filepath"
	"reflect"
)

func maintainedSources(d openShellDispatchReceipt, record OpenShellCustodyRecord) (map[string]SessionReadbackFileRef, error) {
	c, err := maintenanceCompletion(d)
	if err != nil {
		return nil, err
	}
	var p ProgressMaintenance
	if err := maintenanceVerify(c.Plan, &p); err != nil {
		return nil, err
	}
	if p.ContextID != c.ContextID || p.Originals["dispatch"] != c.OriginalDispatch || p.Originals["custody"] != c.OriginalCustody {
		return nil, fmt.Errorf("maintenance original source bindings differ")
	}
	if err := VerifySessionReadbackFiles(p.Originals); err != nil {
		return nil, err
	}
	var original OpenShellCustodyState
	if err := maintenanceVerify(c.OriginalCustody, &original); err != nil {
		return nil, err
	}
	var before OpenShellCustodyRecord
	count := 0
	for _, r := range original.Records {
		if r.Worker.Identity == record.Worker.Identity {
			before = r
			count++
		}
	}
	if count != 1 {
		return nil, fmt.Errorf("maintenance original native operation is missing or ambiguous")
	}
	proof, err := maintenanceStopped(c.Plan, p, before)
	if err != nil {
		return nil, err
	}
	stopRef, stop := proof.Reference, proof.Stop
	if stopRef != c.Stop || stop.RequestID != record.StopRequestID || !record.StopAcknowledged {
		return nil, fmt.Errorf("maintenance original native operation differs")
	}
	comparison := record
	comparison.StopRequestID, comparison.StopAcknowledged, comparison.Stopped = before.StopRequestID, before.StopAcknowledged, before.Stopped
	comparison.Worker.Boundary, comparison.Worker.StopPending, comparison.Worker.StopError = before.Worker.Boundary, before.Worker.StopPending, before.Worker.StopError
	if !reflect.DeepEqual(comparison, before) {
		return nil, fmt.Errorf("maintenance changed the original native execution")
	}
	refs := map[string]SessionReadbackFileRef{"maintenance_completion": *d.Maintenance, "maintenance_plan": c.Plan, "maintenance_stop": c.Stop, "maintenance_original_dispatch": c.OriginalDispatch, "maintenance_original_custody": c.OriginalCustody, "maintenance_stop_ack": stop.Acknowledgement, "maintenance_stopped": stop.Stopped}
	for name, ref := range p.Originals {
		refs["maintenance_original_"+name] = ref
	}
	var h ProgressMaintenanceHostHandoff
	handoffRef, err := ReadSessionReadbackFile(filepath.Join(filepath.Dir(c.Plan.Path), "host-handoff.json"), &h)
	if err != nil {
		return nil, err
	}
	repairRefs, err := maintenanceRestoreRepairSources(*c, h)
	if err != nil {
		return nil, err
	}
	for name, ref := range repairRefs {
		refs[name] = ref
	}
	refs["maintenance_host_handoff"], refs["maintenance_host_before"], refs["maintenance_host_after"] = handoffRef, h.HostReceiptsBefore, h.HostReceiptsAfter
	refs["maintenance_release"], refs["maintenance_host_exit"], refs["maintenance_launcher"] = h.Release, h.HostExit, p.Launcher
	var release ProgressMaintenanceRelease
	var exit ProgressMaintenanceExit
	if err := maintenanceVerify(h.Release, &release); err != nil {
		return nil, err
	}
	if err := maintenanceVerify(h.HostExit, &exit); err != nil {
		return nil, err
	}
	refs["maintenance_released_resources"], refs["maintenance_original_wait_result"] = release.Resources, exit.WaitResult
	refs["maintenance_launcher_call"], refs["maintenance_wait_call"] = p.LauncherCall, exit.WaitCall
	refs["maintenance_release_intent"] = release.Intent
	if release.Response != nil {
		refs["maintenance_release_response"] = *release.Response
	}
	for key, ref := range proof.Intents {
		refs[key] = ref
	}
	return refs, nil
}
