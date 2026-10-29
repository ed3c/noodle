package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
)

// An already exited source has no live resource RPC. Preserve the original
// CLI's kernel identity and qualified recovery snapshot as historical evidence.
type hostColdAbsentSource struct {
	Plan, ResourceFile, History loopruntime.SessionReadbackFileRef
	Resources                   legacyHostResources
	Native                      procx.NativeIdentity
}

func nativeGenerationAbsent(identity procx.NativeIdentity) bool {
	gone, err := procx.ObserveNativeExit(identity)
	return err == nil && gone
}

func readColdAbsentSource(root, recovery, sourcePlan string) (*hostColdAbsentSource, error) {
	var prior hostMaintenancePlan
	ref, err := loopruntime.ReadSessionReadbackFile(sourcePlan, &prior)
	if err != nil {
		return nil, err
	}
	sealed, err := sealGatewayHandoverPlan(prior)
	// A blocked original plan still carries a valid native observation; its
	// blockers never become authorization for this new, qualified cold plan.
	if err != nil || sealed.PlanSHA != prior.PlanSHA || prior.Root != root || prior.Native == nil || !prior.Native.SameGeneration(*prior.Native) || prior.Native.UID != os.Getuid() || prior.Native.PID != prior.Source.PID || !nativeGenerationAbsent(*prior.Native) {
		return nil, errors.New("original CLI source plan or kernel exit observation differs")
	}
	var original struct {
		Schema                     int
		HostResources, HostHistory loopruntime.SessionReadbackFileRef
	}
	if _, err = loopruntime.ReadSessionReadbackFile(recovery, &original); err != nil {
		return nil, err
	}
	if original.Schema != 1 {
		return nil, errors.New("original failure recovery schema differs")
	}
	var resources legacyHostResources
	actual, err := loopruntime.ReadSessionReadbackFile(original.HostResources.Path, &resources)
	if err != nil || actual != original.HostResources || resources.Instance != prior.Source.Instance || resources.PID != prior.Source.PID || resources.Contexts != prior.Source.Contexts || resources.Resources != prior.Source.Resources {
		return nil, errors.New("original absent Host resource snapshot differs")
	}
	actual, err = loopruntime.ReadSessionReadbackFile(original.HostHistory.Path, nil)
	if err != nil || actual != original.HostHistory || actual.SHA256 != prior.Files["receipts.json"] {
		return nil, errors.New("original absent Host history snapshot differs")
	}
	current, err := loopruntime.ReadSessionReadbackFile(filepath.Join(root, "receipts.json"), nil)
	if err != nil || current.SHA256 != actual.SHA256 {
		return nil, errors.New("absent Host receipt history changed")
	}
	return &hostColdAbsentSource{Plan: ref, ResourceFile: original.HostResources, History: original.HostHistory, Resources: resources, Native: *prior.Native}, nil
}

func verifyColdAbsentSource(p hostMaintenancePlan) error {
	cold := p.Handover.Cold
	if cold == nil || cold.Absent == nil {
		return errors.New("absent source binding missing")
	}
	current, err := readColdAbsentSource(p.Root, cold.Recovery.Path, cold.Absent.Plan.Path)
	if err != nil {
		return err
	}
	old, _ := json.Marshal(cold.Absent)
	now, _ := json.Marshal(current)
	if string(old) != string(now) || !current.Native.SameGeneration(*p.Native) {
		return errors.New("absent source evidence changed")
	}
	return nil
}

func coldSourceResources(ctx context.Context, p hostMaintenancePlan, cold *hostColdHandoverPlan, root string) ([]byte, error) {
	if cold.Absent == nil {
		return nativeHostRPC(ctx, root, hostRequest{Operation: "resources"})
	}
	if err := verifyColdAbsentSourceWithPlan(p, cold); err != nil {
		return nil, err
	}
	return os.ReadFile(cold.Absent.ResourceFile.Path)
}

func verifyColdAbsentSourceWithPlan(p hostMaintenancePlan, cold *hostColdHandoverPlan) error {
	// Preparation has not assigned Cold to the enclosing plan yet.
	copy := *p.Handover
	copy.Cold = cold
	p.Handover = &copy
	return verifyColdAbsentSource(p)
}

func fenceColdAbsentSource(p hostMaintenancePlan, backing string) error {
	if err := verifyColdAbsentSource(p); err != nil {
		return err
	}
	marker := hostBootstrap{Schema: 1, Phase: "gateway-handover", Target: p.Target, TargetSHA: p.TargetSHA, ProcessIdentity: "native-plan:" + p.Handover.Cold.Absent.Plan.SHA256, Source: hostGeneration{ID: p.Source.Instance, Directory: backing, Binary: p.Handover.SourceBinary.Path, SHA: p.Handover.SourceBinary.SHA256}}
	if err := saveHostBootstrap(p.Root, marker); err != nil {
		return err
	}
	source, target := filepath.Join(p.Root, "host.sock"), filepath.Join(backing, "host.sock")
	if _, err := os.Lstat(target); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	info, err := os.Lstat(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errors.New("absent source listener is indirect or replaced")
	}
	return os.Rename(source, target)
}
