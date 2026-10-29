package runtime

import (
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/poteto/noodle/internal/procx"
)

// Cold retirement settles resource responsibility only. Original commands,
// failed outcomes and missing execution results remain unchanged in custody.
type OpenShellColdRelease struct {
	StopPlan, Profile, Inventory, Retirement SessionReadbackFileRef
}

type OpenShellRetiredActor struct {
	Process            procx.NativeIdentity
	IntentAt, ExitedAt time.Time
}

type OpenShellColdRetirement struct {
	Schema                                                    int
	Plan, Custody, Resources, BeforeInventory, FinalInventory SessionReadbackFileRef
	Actors                                                    []OpenShellRetiredActor
}

func verifyOpenShellColdRelease(release OpenShellHostRelease) (storageFailureInputs, error) {
	var in storageFailureInputs
	if release.Cold == nil {
		return in, fmt.Errorf("cold release proof is absent")
	}
	cold := release.Cold
	var plan struct {
		Handover struct {
			Cold *struct {
				StopPlan, Profile SessionReadbackFileRef
				Actors            []procx.NativeIdentity
			}
		}
	}
	if err := maintenanceVerify(release.Plan, &plan); err != nil {
		return in, err
	}
	bound := plan.Handover.Cold
	if bound == nil || bound.StopPlan != cold.StopPlan || bound.Profile != cold.Profile || len(bound.Actors) < 2 || !bound.Actors[0].SameGeneration(release.Process) {
		return in, fmt.Errorf("cold release differs from the confirmed actor and failure scope")
	}
	profile, err := LoadOpenShellProfile(cold.Profile.Path)
	if err != nil {
		return in, err
	}
	if err = maintenanceVerify(cold.Profile, nil); err != nil {
		return in, err
	}
	var stop StorageFailureStopPlan
	if err = maintenanceVerify(cold.StopPlan, &stop); err != nil {
		return in, err
	}
	if stop.HostPID != release.Process.PID || stop.HostInstance != release.SourceInstance {
		return in, fmt.Errorf("cold stop belongs to another Host")
	}
	in, err = readStorageFailureInputs(stop, profile, false)
	if err != nil {
		return in, err
	}
	var retired OpenShellColdRetirement
	if err = maintenanceVerify(cold.Retirement, &retired); err != nil {
		return in, err
	}
	if retired.Schema != 1 || retired.Plan != release.Plan || retired.Custody.SHA256 != stop.Custody.SHA256 || retired.Resources != release.EmptyResources || retired.FinalInventory != cold.Inventory || len(retired.Actors) != len(bound.Actors) {
		return in, fmt.Errorf("cold retirement binding is incomplete")
	}
	if err = maintenanceVerify(retired.Custody, nil); err != nil {
		return in, err
	}
	var before, final StorageFailureInventory
	for _, item := range []struct {
		ref   SessionReadbackFileRef
		value *StorageFailureInventory
	}{{retired.BeforeInventory, &before}, {retired.FinalInventory, &final}} {
		if err = maintenanceVerify(item.ref, item.value); err != nil {
			return in, err
		}
		if err = ValidateStorageFailureInventory(cold.StopPlan, profile, *item.value); err != nil {
			return in, err
		}
	}
	for i, actor := range retired.Actors {
		if !actor.Process.SameGeneration(bound.Actors[i]) || actor.Process.UID != os.Getuid() || actor.IntentAt.IsZero() || actor.ExitedAt.Before(actor.IntentAt) || before.ObservedAt.After(actor.IntentAt) {
			return in, fmt.Errorf("cold actor retirement is unproved")
		}
		if i > 0 && (actor.IntentAt.Before(retired.Actors[0].ExitedAt) || final.ObservedAt.Before(retired.Actors[0].ExitedAt) || final.ObservedAt.After(actor.IntentAt)) {
			return in, fmt.Errorf("cold inventory does not bracket Host exit and gateway retirement")
		}
	}
	return in, nil
}

// Only the exact historical failed record qualifies. A new attempt of its order
// still follows Create's original unresolved-effect refusal.
func (c *OpenShellCustody) coldResourceSettled(record OpenShellCustodyRecord) (bool, error) {
	if c.state.Handover == nil {
		return false, nil
	}
	var handover OpenShellCustodyHandover
	if err := maintenanceVerify(*c.state.Handover, &handover); err != nil {
		return false, err
	}
	release, err := verifyOpenShellHostRelease(handover.Release)
	if err != nil {
		return false, err
	}
	if release.Schema != 3 {
		return false, nil
	}
	in, err := verifyOpenShellColdRelease(release)
	if err != nil {
		return false, err
	}
	if record.Name != in.record.Name {
		return false, nil
	}
	record.Stopped = in.record.Stopped
	if !reflect.DeepEqual(record, in.record) {
		return false, fmt.Errorf("cold resource record changed beyond a native Stopped observation")
	}
	return true, nil
}
