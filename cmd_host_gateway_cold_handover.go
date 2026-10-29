package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
)

type hostColdHandoverPlan struct {
	Recovery, StopPlan, Stop, Profile, Inventory loopruntime.SessionReadbackFileRef
	Actors                                       []procx.NativeIdentity
	Absent                                       *hostColdAbsentSource `json:",omitempty"`
}

func coldFailurePlan(p hostMaintenancePlan, recovery string) (*hostColdHandoverPlan, error) {
	cold := &hostColdHandoverPlan{}
	var original struct {
		Schema int
		Stop   loopruntime.SessionReadbackFileRef
	}
	var err error
	cold.Recovery, err = loopruntime.ReadSessionReadbackFile(recovery, &original)
	if err != nil {
		return nil, err
	}
	if original.Schema != 1 {
		return nil, errors.New("original failure recovery schema differs")
	}
	var stop loopruntime.StorageFailureStopPlan
	ref, err := loopruntime.ReadSessionReadbackFile(original.Stop.Path, &stop)
	if err != nil || ref != original.Stop {
		return nil, errors.New("original qualified failure plan changed")
	}
	cold.StopPlan, cold.Profile = original.Stop, stop.Profile
	if stop.HostPID != p.Source.PID || stop.HostInstance != p.Source.Instance || stop.Custody != p.Handover.Custody || !slices.ContainsFunc(p.Contexts, func(cp hostMaintenanceContextPlan) bool { return cp.Original.ContextID == stop.ContextID }) {
		return nil, errors.New("qualified failed compute is not bound to this original Host, custody and context")
	}
	profile, err := loopruntime.LoadOpenShellProfile(cold.Profile.Path)
	if err != nil {
		return nil, err
	}
	if profile.Gateway.Binding != p.Handover.Binding {
		return nil, errors.New("failure profile uses a different gateway")
	}
	cold.Stop, err = loopruntime.ValidateStorageFailureStopReceipt(cold.StopPlan, profile)
	return cold, err
}

// This is deliberately the inspected 119 persistence-fault shape, not a generic
// timeout or unknown-effect waiver. Native VM and actor checks follow separately.
func coldResourceScope(p hostMaintenancePlan, raw []byte) (admission.Resident, error) {
	var row struct {
		Residents struct {
			Error   any                  `json:"error"`
			Records []admission.Resident `json:"records"`
		} `json:"residents"`
	}
	var current legacyHostResources
	if json.Unmarshal(raw, &row) != nil || json.Unmarshal(raw, &current) != nil || current.Instance != p.Source.Instance || current.PID != p.Source.PID || current.Contexts != len(p.Contexts) || current.Resources.Workers != 1 || current.Resources.Residents != 1 || current.Resources.Allocations != 2 || current.Resources.Unresolved != 0 {
		return admission.Resident{}, errors.New("cold retirement has other work or a different resource responsibility")
	}
	message, ok := row.Residents.Error.(string)
	if !ok || !strings.Contains(message, "no space left on device") || !strings.Contains(message, filepath.Join(p.Root, "residents")) || len(row.Residents.Records) != 1 {
		return admission.Resident{}, errors.New("original resident persistence fault is not the qualified storage failure")
	}
	resident := row.Residents.Records[0]
	if resident.ID != p.Handover.Binding.Owner || resident.Name != p.Handover.Binding.Owner || resident.State != "running" || resident.PID <= 1 || resident.Started == "" || resident.ProcessGroup != resident.PID || len(resident.Contexts) == 0 || len(resident.ConsumerLeases) != 0 {
		return resident, errors.New("cold retirement resident identity or native consumers differ")
	}
	for _, id := range resident.Contexts {
		if !slices.ContainsFunc(p.Contexts, func(cp hostMaintenanceContextPlan) bool { return cp.Original.ContextID == id }) {
			return resident, errors.New("gateway still serves a context outside the confirmed task set")
		}
	}
	return resident, nil
}

func observeColdActors(ctx context.Context, p hostMaintenancePlan, resident admission.Resident, absent *hostColdAbsentSource) ([]procx.NativeIdentity, error) {
	if p.Native == nil || !procx.NativeFixtureControlAvailable() {
		return nil, errors.New("kernel generation-bound recovery control unavailable")
	}
	rows, err := admission.ObserveVisibleProcesses(ctx)
	if err != nil {
		return nil, err
	}
	expected := map[int]bool{p.Native.PID: true, resident.PID: true}
	var descendants []admission.ProcessObservation
	residentFound := false
	for _, row := range rows {
		if row.PID == resident.PID {
			residentFound = true
		}
		gatewayChild := slices.Contains(row.AncestorPIDs, resident.PID)
		if slices.Contains(row.AncestorPIDs, p.Native.PID) && row.PID != resident.PID && !gatewayChild {
			return nil, fmt.Errorf("original Host has another live local child: %d", row.PID)
		}
		if row.PID == resident.PID && (row.Started != resident.Started || (absent == nil && row.ParentPID != p.Native.PID)) {
			return nil, errors.New("original resident ancestry changed")
		}
		if gatewayChild {
			if !row.AncestryComplete || row.UID == nil || *row.UID != os.Getuid() {
				return nil, errors.New("gateway descendant ownership is incomplete")
			}
			expected[row.PID] = true
			descendants = append(descendants, row)
		}
	}
	if !residentFound {
		return nil, errors.New("original resident process metadata unavailable")
	}
	sort.Slice(descendants, func(i, j int) bool { return len(descendants[i].AncestorPIDs) > len(descendants[j].AncestorPIDs) })
	ids := []int{p.Native.PID}
	for _, row := range descendants {
		ids = append(ids, row.PID)
	}
	ids = append(ids, resident.PID)
	actors := []procx.NativeIdentity{}
	for _, pid := range ids {
		if pid == p.Native.PID && absent != nil {
			actors = append(actors, *p.Native)
			continue
		}
		actor, err := procx.ObserveNativeIdentity(pid)
		if err != nil || !actor.SameGeneration(actor) || actor.Zombie || actor.UID != os.Getuid() || !expected[pid] {
			return nil, errors.New("cold actor kernel generation is unavailable")
		}
		if pid == p.Native.PID && !actor.SameGeneration(*p.Native) {
			return nil, errors.New("original Host kernel generation changed")
		}
		actors = append(actors, actor)
	}
	return actors, nil
}

func observeColdInventory(ctx context.Context, cold *hostColdHandoverPlan) (loopruntime.SessionReadbackFileRef, error) {
	profile, err := loopruntime.LoadOpenShellProfile(cold.Profile.Path)
	if err != nil {
		return loopruntime.SessionReadbackFileRef{}, err
	}
	ref, err := loopruntime.ReadSessionReadbackFile(cold.Profile.Path, nil)
	if err != nil || ref != cold.Profile {
		return ref, errors.New("original failure profile changed")
	}
	return loopruntime.ObserveStorageFailureStopped(ctx, cold.StopPlan, profile)
}

func planColdGatewayHandover(ctx context.Context, p hostMaintenancePlan, recovery string, absent *hostColdAbsentSource) (*hostColdHandoverPlan, error) {
	cold, err := coldFailurePlan(p, recovery)
	if err != nil {
		return nil, err
	}
	cold.Absent = absent
	raw, err := coldSourceResources(ctx, p, cold, p.Root)
	if err != nil {
		return nil, err
	}
	resident, err := coldResourceScope(p, raw)
	if err != nil {
		return nil, err
	}
	cold.Actors, err = observeColdActors(ctx, p, resident, absent)
	if err != nil {
		return nil, err
	}
	cold.Inventory, err = observeColdInventory(ctx, cold)
	return cold, err
}

func verifyColdBindings(p hostMaintenancePlan) error {
	cold := p.Handover.Cold
	if cold == nil || len(cold.Actors) < 2 || !cold.Actors[0].SameGeneration(*p.Native) {
		return errors.New("cold plan actor scope is incomplete")
	}
	other, err := coldFailurePlan(p, cold.Recovery.Path)
	if err != nil {
		return err
	}
	if other.Recovery != cold.Recovery || other.StopPlan != cold.StopPlan || other.Stop != cold.Stop || other.Profile != cold.Profile {
		return errors.New("cold original failure sources changed")
	}
	for _, cp := range p.Contexts {
		for path, digest := range cp.Files {
			ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
			if err != nil || ref.SHA256 != digest {
				return errors.New("cold original accepted inputs changed")
			}
		}
	}
	ref, err := loopruntime.ReadSessionReadbackFile(p.Handover.Custody.Path, nil)
	if err != nil || ref != p.Handover.Custody {
		return errors.New("cold original custody changed before resource settlement")
	}
	return nil
}

// An intent is persisted before the only signal. Resumption reads the original
// generation's outcome. A live generation after intent never triggers a replay.
func retireColdActor(ctx context.Context, actor *loopruntime.OpenShellRetiredActor, save func() error) error {
	if !actor.ExitedAt.IsZero() {
		return nil
	}
	gone, err := procx.ObserveNativeExit(actor.Process)
	if !actor.IntentAt.IsZero() {
		if !gone {
			return errors.New("cold control outcome remains unconfirmed; no signal repeated")
		}
		actor.ExitedAt = time.Now().UTC()
		return save()
	}
	if gone && err == nil {
		actor.IntentAt = time.Now().UTC()
		actor.ExitedAt = actor.IntentAt
		return save()
	}
	if err != nil {
		return errors.New("cold actor changed before its control intent")
	}
	actor.IntentAt = time.Now().UTC()
	if err = save(); err != nil {
		return err
	}
	if err = procx.SignalNativeIdentity(actor.Process, true); err != nil {
		return err
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		gone, err = procx.ObserveNativeExit(actor.Process)
		if err == nil && gone {
			actor.ExitedAt = time.Now().UTC()
			return save()
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("cold actor exit not yet observed; inspect the original intent")
		case <-ticker.C:
		}
	}
}

func runColdGatewayHandover(ctx context.Context, p hostMaintenancePlan, dir, backing string, j *restrictedMaintenance, out *json.Encoder) error {
	cold := p.Handover.Cold
	path := filepath.Join(dir, "cold-retirement.json")
	var retirement loopruntime.OpenShellColdRetirement
	save := func() error {
		raw, err := json.Marshal(retirement)
		if err != nil {
			return err
		}
		return filex.WriteFileAtomicDurable(path, raw)
	}
	if j.Phase == "handover-cleanup" {
		if err := verifyColdBindings(p); err != nil {
			return err
		}
		raw, err := coldSourceResources(ctx, p, cold, backing)
		if err != nil {
			return err
		}
		resident, err := coldResourceScope(p, raw)
		if err != nil {
			return err
		}
		actors, err := observeColdActors(ctx, p, resident, cold.Absent)
		if err != nil || !reflect.DeepEqual(actors, cold.Actors) {
			return errors.New("cold actor set changed after admission fence")
		}
		inventory, err := observeColdInventory(ctx, cold)
		if err != nil {
			return err
		}
		if err = saveHostStorageBytes(filepath.Join(dir, "retired-resource-accounts.json"), raw); err != nil {
			return err
		}
		resources, err := loopruntime.ReadSessionReadbackFile(filepath.Join(dir, "retired-resource-accounts.json"), nil)
		if err != nil {
			return err
		}
		plan, err := loopruntime.ReadSessionReadbackFile(filepath.Join(dir, "plan.json"), nil)
		if err != nil {
			return err
		}
		before, err := loopruntime.ReadSessionReadbackFile(filepath.Join(dir, "original-custody.json"), nil)
		if err != nil {
			return err
		}
		retirement = loopruntime.OpenShellColdRetirement{Schema: 1, Plan: plan, Custody: before, Resources: resources, BeforeInventory: inventory}
		for _, actor := range cold.Actors {
			retirement.Actors = append(retirement.Actors, loopruntime.OpenShellRetiredActor{Process: actor})
		}
		if _, err = os.Lstat(path); err == nil {
			var prior loopruntime.OpenShellColdRetirement
			if _, err = loopruntime.ReadSessionReadbackFile(path, &prior); err != nil {
				return err
			}
			if prior.Plan != retirement.Plan || prior.Custody != retirement.Custody || prior.Resources != retirement.Resources || len(prior.Actors) != len(retirement.Actors) {
				return errors.New("cold preparation belongs to another source")
			}
			for i, actor := range prior.Actors {
				if !actor.Process.SameGeneration(retirement.Actors[i].Process) || !actor.IntentAt.IsZero() || !actor.ExitedAt.IsZero() {
					return errors.New("cold control intent exists before journal phase; inspect original outcome")
				}
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if err = save(); err != nil {
			return err
		}
		j.Phase = "handover-cold-retiring"
		if err = saveRestrictedMaintenance(dir, *j); err != nil {
			return err
		}
	} else {
		if _, err := loopruntime.ReadSessionReadbackFile(path, &retirement); err != nil {
			return err
		}
	}
	if retirement.Schema != 1 || len(retirement.Actors) != len(cold.Actors) {
		return errors.New("cold retirement journal actor set differs")
	}
	for i, actor := range retirement.Actors {
		if !actor.Process.SameGeneration(cold.Actors[i]) {
			return errors.New("cold retirement journal actor identity differs")
		}
	}
	if j.Phase == "handover-cold-retiring" {
		if cold.Absent != nil && retirement.Actors[0].IntentAt.IsZero() {
			if err := verifyColdAbsentSource(p); err != nil {
				return err
			}
			retirement.Actors[0].IntentAt = time.Now().UTC()
			retirement.Actors[0].ExitedAt = retirement.Actors[0].IntentAt
			if err := save(); err != nil {
				return err
			}
		}
		if err := retireColdActor(ctx, &retirement.Actors[0], save); err != nil {
			return err
		}
		if retirement.FinalInventory.Path == "" {
			if err := verifyColdBindings(p); err != nil {
				return err
			}
			inventory, err := observeColdInventory(ctx, cold)
			if err != nil {
				return err
			}
			retirement.FinalInventory = inventory
			if err = save(); err != nil {
				return err
			}
		}
		if err := verifyColdGatewayDescendants(ctx, cold); err != nil {
			return err
		}
		for i := 1; i < len(retirement.Actors); i++ {
			if err := retireColdActor(ctx, &retirement.Actors[i], save); err != nil {
				return err
			}
		}
		// The old source can no longer create work or account for this gateway. Its
		// nonzero counters are archived above, never rewritten as an empty snapshot.
		j.Phase = "handover-cold-releasing"
		if err := saveRestrictedMaintenance(dir, *j); err != nil {
			return err
		}
	}
	if err := verifyColdGatewayDescendants(ctx, cold); err != nil {
		return err
	}
	retirementRef, err := loopruntime.ReadSessionReadbackFile(path, nil)
	if err != nil {
		return err
	}
	release := loopruntime.OpenShellHostRelease{Schema: 3, SourceRoot: p.Root, SourceInstance: p.Source.Instance, ControlRoot: p.Handover.SharedRoot, Process: *p.Native, Plan: retirement.Plan, EmptyResources: retirement.Resources, Cold: &loopruntime.OpenShellColdRelease{StopPlan: cold.StopPlan, Profile: cold.Profile, Inventory: retirement.FinalInventory, Retirement: retirementRef}}
	raw, err := json.Marshal(release)
	if err != nil {
		return err
	}
	if err = saveHostStorageBytes(filepath.Join(dir, "release.json"), raw); err != nil {
		return err
	}
	releaseRef, err := loopruntime.ReadSessionReadbackFile(filepath.Join(dir, "release.json"), nil)
	if err != nil {
		return err
	}
	if _, err = loopruntime.UpgradeOpenShellCustodyV3(p.Handover.Binding, p.Handover.SharedRoot, retirement.Custody, releaseRef, filepath.Join(dir, "custody-handover.json")); err != nil {
		return err
	}
	j.Phase = "handover-completed"
	if err = saveRestrictedMaintenance(dir, *j); err != nil {
		return err
	}
	return out.Encode(j)
}

func validateColdPlanShape(p hostMaintenancePlan) error {
	cold := p.Handover.Cold
	for _, ref := range []loopruntime.SessionReadbackFileRef{cold.Recovery, cold.StopPlan, cold.Stop, cold.Profile, cold.Inventory} {
		if !filepath.IsAbs(ref.Path) || len(ref.SHA256) != 64 {
			return errors.New("cold failure source reference is incomplete")
		}
	}
	if len(cold.Actors) < 2 || !cold.Actors[0].SameGeneration(*p.Native) {
		return errors.New("cold actor set is incomplete")
	}
	seen := map[int]bool{}
	for _, actor := range cold.Actors {
		if seen[actor.PID] || !actor.SameGeneration(actor) || actor.UID != os.Getuid() {
			return errors.New("cold actor generation differs or is duplicated")
		}
		seen[actor.PID] = true
	}
	return nil
}

// Unknown descendants block settlement. Sampling never grants authority to
// signal a new process that was not in the confirmed kernel-bound actor set.
func verifyColdGatewayDescendants(ctx context.Context, cold *hostColdHandoverPlan) error {
	rows, err := admission.ObserveVisibleProcesses(ctx)
	if err != nil {
		return err
	}
	gateway := cold.Actors[len(cold.Actors)-1]
	for _, row := range rows {
		if row.PID != gateway.PID && !slices.Contains(row.AncestorPIDs, gateway.PID) && row.ProcessGroup != gateway.Group {
			continue
		}
		var bound *procx.NativeIdentity
		for i := range cold.Actors {
			if cold.Actors[i].PID == row.PID {
				bound = &cold.Actors[i]
				break
			}
		}
		current, err := procx.ObserveNativeIdentity(row.PID)
		if err == nil && current.Zombie {
			continue
		}
		if err != nil || bound == nil || !bound.SameGeneration(current) {
			return errors.New("gateway has an unconfirmed descendant or process generation")
		}
	}
	return nil
}
