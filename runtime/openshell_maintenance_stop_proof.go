package runtime

import (
	"fmt"
	"path/filepath"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
)

type maintenanceStopProof struct {
	Reference SessionReadbackFileRef
	Stop      ProgressMaintenanceStop
	Stopped   *pb.Sandbox
	Intents   map[string]SessionReadbackFileRef
}

func maintenanceStopped(ref SessionReadbackFileRef, p ProgressMaintenance, record OpenShellCustodyRecord) (maintenanceStopProof, error) {
	var stop ProgressMaintenanceStop
	fail := func(err error) (maintenanceStopProof, error) {
		return maintenanceStopProof{}, err
	}
	directory := filepath.Dir(ref.Path)
	stopRef, err := ReadSessionReadbackFile(filepath.Join(directory, "stop-complete.json"), &stop)
	if err != nil {
		return fail(err)
	}
	if stop.Plan != ref || stop.RequestID == "" {
		return fail(fmt.Errorf("maintenance stop does not bind this plan"))
	}
	intents := map[string]SessionReadbackFileRef{}
	for key, name := range map[string]string{"maintenance_stop_intent": filepath.Join(directory, "stop-intent.json"), "maintenance_stop_reservation": filepath.Join(p.SessionDirectory, "maintenance-stop-intent.json")} {
		var intent ProgressMaintenanceStop
		intentRef, err := ReadSessionReadbackFile(name, &intent)
		if err != nil {
			return fail(err)
		}
		if intent.Plan != stop.Plan || intent.RequestID != stop.RequestID {
			return fail(fmt.Errorf("maintenance stop acknowledgement differs from original intent"))
		}
		intents[key] = intentRef
	}
	var ack, stopped pb.SandboxResponse
	if err := maintenanceReadNative(stop.Acknowledgement, &ack); err != nil {
		return fail(err)
	}
	if err := maintenanceReadNative(stop.Stopped, &stopped); err != nil {
		return fail(err)
	}
	if err := maintenanceNative(record, ack.GetSandbox()); err != nil {
		return fail(err)
	}
	if err := maintenanceNative(record, stopped.GetSandbox()); err != nil {
		return fail(err)
	}
	if stopped.GetSandbox().GetStatus().GetPhase() != pb.SandboxPhase_SANDBOX_PHASE_STOPPED {
		return fail(fmt.Errorf("maintenance has no fresh Stopped evidence"))
	}
	return maintenanceStopProof{Reference: stopRef, Stop: stop, Stopped: stopped.GetSandbox(), Intents: intents}, nil
}

func ValidateProgressMaintenanceStop(ref SessionReadbackFileRef, profile OpenShellProfile) (SessionReadbackFileRef, error) {
	var p ProgressMaintenance
	if err := maintenanceVerify(ref, &p); err != nil {
		return SessionReadbackFileRef{}, err
	}
	p.Sources = p.Originals
	_, _, _, record, _, err := maintenanceInputs(p, profile)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	proof, err := maintenanceStopped(ref, p, record)
	return proof.Reference, err
}
