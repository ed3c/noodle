package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
)

var sourceServicePhases = []string{"source-host-stop-intent", "source-driver-stop-intent", "source-compute-qualification", "source-gateway-term-intent", "source-host-retire-intent", "source-quota-return", "source-quota-migrate", "source-policy-retired", "source-service-launch-intent", "source-service-active"}

var errSourceRetirementPending = errors.New("original Source retirement still pending")

type sourceServiceProgress struct {
	Compute, GatewayExit, Quota *loopruntime.SessionReadbackFileRef
}

func prepareSourceServiceRestore(ctx context.Context, snapshot, target string) (hostMaintenancePlan, error) {
	p, err := prepareSourcePolicyHold(snapshot)
	if err != nil {
		return p, err
	}
	binary, err := hostReadbackBinaryRef(target)
	if err != nil {
		return p, err
	}
	if err = verifyRollingTarget(ctx, binary.Path, binary.SHA256); err != nil {
		return p, err
	}
	if err = verifySourceServiceTarget(ctx, binary); err != nil {
		return p, err
	}
	p.SourcePolicy.Operation = "operator-service-restore"
	p.Target, p.TargetSHA, p.Policy = binary.Path, binary.SHA256, admission.WorkerAdmissionPolicy
	if err = sourceServiceQuotaWritersRetired(p, true); err != nil {
		return p, err
	}
	p.Observations = []string{"Only the original Source generation is retired; all business outcomes remain held and unknown", "Historical custody bytes stay unchanged; the restored operator service has its own bound deployment identity"}
	p.Steps = []string{"Hold original admission and preserve all selected contexts", "Suspend the original Host and VM driver once, then qualify current zero compute", "Request original gateway normal shutdown and retain its kernel normal-zero result", "Retire the still-suspended original Host without resuming old work", "Return the exact retired resident allocation and migrate only the empty original quota", "Launch a dedicated Source service generation with original business history held; verify service separately from operator and worker acceptance"}
	p.PlanSHA = ""
	raw, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	p.PlanSHA = hostDigest(raw)
	return p, validateSourcePolicyPlan(p)
}

func verifySourceServiceTarget(ctx context.Context, target loopruntime.SessionReadbackFileRef) error {
	current, err := hostReadbackBinaryRef(target.Path)
	if err != nil || current != target {
		return errors.New("Source service target executable changed")
	}
	raw, err := exec.CommandContext(ctx, target.Path, "host", "capabilities").Output()
	if err != nil {
		return err
	}
	var capabilities struct {
		SourceService int `json:"source_service_restore"`
	}
	if json.Unmarshal(raw, &capabilities) != nil || capabilities.SourceService != 1 {
		return errors.New("selected target has no bound Source service startup role")
	}
	final, err := hostReadbackBinaryRef(target.Path)
	if err != nil || final != target {
		return errors.New("Source service target changed during capability readback")
	}
	return nil
}

func validateSourceServiceRestorePlan(p hostMaintenancePlan, op hostOperatorPlan) error {
	if p.Policy != admission.WorkerAdmissionPolicy || !filepath.IsAbs(p.Target) || len(p.TargetSHA) != 64 || op.OperatorPolicy == nil || op.Quota == nil || op.Quota.Schema != 2 || op.Quota.ResidentReturn == nil {
		return errors.New("full Source restore lacks its target, auth-only policy or original quota allocation")
	}
	policy := op.OperatorPolicy
	verified, err := loopruntime.InspectSourceOperatorPolicy(policy.Before, policy.After, policy.BeforeSource)
	if err != nil {
		return err
	}
	if verified != *policy || policy.Before != op.Profile || policy.BeforeSource != op.ProfileSource || policy.BeforeBinding != op.Binding {
		return errors.New("Source restore policy differs from its original deployment")
	}
	var custody loopruntime.OpenShellCustodyState
	ref, err := loopruntime.ReadSessionReadbackFile(op.Custody.Path, &custody)
	if err != nil || ref != op.Custody || custody.Version != 4 || len(custody.Credentials) != 0 || custody.Binding != policy.BeforeBinding || custody.ControlRoot != p.Root || op.Custody.Path != filepath.Join(custody.Binding.StateRoot, "owner.json") {
		return errors.New("Source restore historical custody is not the exact original credential-free v4 deployment")
	}
	return verifySourceServiceBinaries(op.NativeScope)
}

func saveSourceServiceObservation(dir, name string, value any) (loopruntime.SessionReadbackFileRef, error) {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return loopruntime.SessionReadbackFileRef{}, err
	}
	path := filepath.Join(dir, name)
	if err = saveHostStorageBytes(path, raw); err != nil {
		return loopruntime.SessionReadbackFileRef{}, err
	}
	return loopruntime.ReadSessionReadbackFile(path, nil)
}

// Intent is durable before one generation-bound signal. Re-entry only reads;
// an intent without the expected state never permits sending that signal again.
func sourceServiceSignalOnce(dir, name string, actor procx.NativeIdentity, action string) error {
	want, err := json.Marshal(struct {
		Actor  procx.NativeIdentity
		Action string
	}{actor, action})
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name+".json")
	if prior, err := os.ReadFile(path); err == nil {
		if string(prior) != string(want) {
			return errors.New("original Source control intent differs")
		}
		ack, err := os.ReadFile(filepath.Join(dir, name+"-ack.json"))
		if err != nil || string(ack) != string(want) {
			return errors.New("original Source control ACK missing or changed; intent remains unknown, no signal repeated")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	current, err := procx.ObserveNativeIdentity(actor.PID)
	if err != nil || !actor.SameGeneration(current) || current.Zombie || current.ParentPID != actor.ParentPID || current.Group != actor.Group {
		return errors.New("original Source control actor generation or ancestry changed")
	}
	if action != "stop" && action != "term" && action != "kill" {
		return errors.New("unknown Source maintenance control")
	}
	if action == "stop" && current.Stopped {
		return errors.New("Source actor was already stopped outside this control intent")
	}
	if action == "kill" && !current.Stopped {
		return errors.New("Source Host is no longer suspended; retirement refused")
	}
	if err = saveHostStorageBytes(path, want); err != nil {
		return err
	}
	if action == "stop" {
		err = procx.SetNativeSuspended(actor, true)
	} else {
		err = procx.SignalNativeIdentity(actor, action == "kill")
	}
	if err != nil {
		return err
	}
	return saveHostStorageBytes(filepath.Join(dir, name+"-ack.json"), want)
}

func sourceServiceStopped(actor procx.NativeIdentity) error {
	current, err := procx.ObserveNativeIdentity(actor.PID)
	if err != nil || !actor.SameGeneration(current) || current.Zombie || !current.Stopped || current.ParentPID != actor.ParentPID || current.Group != actor.Group {
		return errors.New("original Source producer suspension is unconfirmed; control readback only")
	}
	return nil
}

func sourceServiceActorsRetired(scope *hostOperatorNativeScope) error {
	for _, actor := range []hostOperatorActor{scope.Host, scope.Gateway, scope.Driver} {
		gone, err := procx.ObserveNativeExit(actor.Process)
		if err != nil {
			return err
		}
		if !gone {
			return errSourceRetirementPending
		}
		absent, err := procx.ProcessGroupAbsent(actor.Process.Group)
		if err != nil {
			return err
		}
		if !absent {
			return errSourceRetirementPending
		}
	}
	return nil
}

func sourceServiceQuotaWritersRetired(p hostMaintenancePlan, sourceMayBeLive bool) error {
	pool, err := readHostPool(p.Root)
	if err != nil {
		return err
	}
	for id, generation := range pool.Generations {
		if sourceMayBeLive && id == p.Source.Instance {
			if generation.Process.PID != p.Source.PID {
				return errors.New("original Source quota writer identity changed")
			}
			continue
		}
		if generation.Process.PID <= 1 {
			return errors.New("original quota writer has no conclusive native identity")
		}
		absent, err := generation.Process.GroupAbsent()
		if err != nil || !absent {
			return errors.New("an original quota writer is not retired")
		}
	}
	return nil
}

func runSourceServiceRestore(ctx context.Context, p hostMaintenancePlan, dir string, out *json.Encoder) error {
	if err := verifySourceServiceTarget(ctx, loopruntime.SessionReadbackFileRef{Path: p.Target, SHA256: p.TargetSHA}); err != nil {
		return err
	}
	lock, err := lockfile.TryLock(filepath.Join(p.Root, "maintenance.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	op, err := readSourcePolicySnapshot(p.SourcePolicy.Snapshot)
	if err != nil {
		return err
	}
	j, err := readRestrictedMaintenance(dir)
	if os.IsNotExist(err) || (err == nil && (j.Phase == "source-policy-prepared" || j.Phase == "source-policy-fencing" || j.Phase == "source-policy-admission-held")) {
		if err = runSourcePolicyHoldLocked(ctx, p, dir, json.NewEncoder(io.Discard)); err != nil {
			return err
		}
		j, err = readRestrictedMaintenance(dir)
	}
	if err != nil {
		return err
	}
	if j.Plan.PlanSHA != p.PlanSHA {
		return errors.New("original full Source epoch differs")
	}
	if j.Phase == "source-service-active" {
		return out.Encode(j)
	}
	gate, err := openHostMaintenanceGate(p.Root, filepath.Join(p.Root, "g", p.Source.Instance))
	if err != nil {
		return err
	}
	defer gate.Close()
	advance := func(phase string) error { j.Phase = phase; return saveRestrictedMaintenance(dir, j) }
	if j.Phase == "source-policy-admission-held" {
		if err = sourceServiceQuotaWritersRetired(p, true); err != nil {
			return err
		}
		if err = verifySourcePolicyAdmissionHold(p.Root, p); err != nil {
			return err
		}
		if err = verifySourcePolicyHoldFiles(op); err != nil {
			return err
		}
		if _, err = loopruntime.InspectOpenShellPhysicalHistory(op.Custody); err != nil {
			return err
		}
		if err = loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"quota": op.Quota.Ledger}); err != nil {
			return err
		}
		if err = saveSourceServiceQuotaBefore(dir, op); err != nil {
			return err
		}
		j.SourceService = &sourceServiceProgress{}
		if err = advance("source-host-stop-intent"); err != nil {
			return err
		}
	}
	if j.SourceService == nil {
		return errors.New("original full Source progress is missing")
	}
	if j.Phase == "source-host-stop-intent" {
		if err = sourceServiceSignalOnce(dir, "source-host-stop", op.NativeScope.Host.Process, "stop"); err != nil {
			return err
		}
		if err = sourceServiceStopped(op.NativeScope.Host.Process); err != nil {
			return err
		}
		if err = advance("source-driver-stop-intent"); err != nil {
			return err
		}
	}
	if j.Phase == "source-driver-stop-intent" {
		if err = sourceServiceStopped(op.NativeScope.Host.Process); err != nil {
			return err
		}
		if err = sourceServiceSignalOnce(dir, "source-driver-stop", op.NativeScope.Driver.Process, "stop"); err != nil {
			return err
		}
		if err = sourceServiceStopped(op.NativeScope.Driver.Process); err != nil {
			return err
		}
		if err = advance("source-compute-qualification"); err != nil {
			return err
		}
	}
	if j.Phase == "source-compute-qualification" {
		if err = verifySourcePolicyAdmissionHold(p.Root, p); err != nil {
			return err
		}
		if err = advance("source-gateway-term-intent"); err != nil {
			return err
		}
	}
	if j.Phase == "source-gateway-term-intent" {
		if err = sourceServiceStopped(op.NativeScope.Host.Process); err != nil {
			return err
		}
		if _, err := os.Lstat(filepath.Join(dir, "source-gateway-term.json")); os.IsNotExist(err) {
			if err = verifySourcePolicyAdmissionHold(p.Root, p); err != nil {
				return err
			}
			compute := readSourceComputeStatus(ctx, p)
			if compute.State != "observed_zero" {
				return fmt.Errorf("Source compute is not qualified: %s", compute.Diagnostic)
			}
			raw, err := json.MarshalIndent(compute, "", "  ")
			if err != nil {
				return err
			}
			ref, err := saveSourceServiceObservation(dir, "source-compute-"+hostDigest(raw)+".json", compute)
			if err != nil {
				return err
			}
			j.SourceService.Compute = &ref
			if err = saveRestrictedMaintenance(dir, j); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		gone, err := procx.ObserveNativeExit(op.NativeScope.Driver.Process)
		if err != nil {
			return err
		}
		if !gone {
			if err = sourceServiceStopped(op.NativeScope.Driver.Process); err != nil {
				return err
			}
		}
		if err = sourceServiceSignalOnce(dir, "source-gateway-term", op.NativeScope.Gateway.Process, "term"); err != nil {
			return err
		}
		_, normal, readErr := readSourcePolicyStatus(ctx, p)
		if readErr != nil {
			current, err := procx.ObserveNativeIdentity(op.Resident.PID)
			if err == nil && op.NativeScope.Gateway.Process.SameGeneration(current) && !current.Zombie {
				return out.Encode(j)
			}
			return readErr
		}
		if normal.State != "observed" {
			return errors.New("original gateway normal-zero drain completion unproved")
		}
		gone, err = procx.ObserveNativeExit(op.NativeScope.Driver.Process)
		if err != nil || !gone {
			return out.Encode(j)
		}
		ref, err := saveSourceServiceObservation(dir, "source-gateway-normal-exit.json", normal)
		if err != nil {
			return err
		}
		j.SourceService.GatewayExit = &ref
		if err = advance("source-host-retire-intent"); err != nil {
			return err
		}
	}
	if j.Phase == "source-host-retire-intent" {
		if err = verifySourceServiceDrain(j); err != nil {
			return err
		}
		if err = verifySourcePolicyAdmissionHold(p.Root, p); err != nil {
			return err
		}
		gone, err := procx.ObserveNativeExit(op.NativeScope.Driver.Process)
		if err != nil || !gone {
			return errors.New("original Source driver retirement is unconfirmed")
		}
		if err = sourceServiceSignalOnce(dir, "source-host-kill", op.NativeScope.Host.Process, "kill"); err != nil {
			return err
		}
		if err = sourceServiceActorsRetired(op.NativeScope); err != nil {
			if errors.Is(err, errSourceRetirementPending) {
				return out.Encode(j)
			}
			return err
		}
		if err = sourceServiceQuotaWritersRetired(p, false); err != nil {
			return err
		}
		if err = advance("source-quota-return"); err != nil {
			return err
		}
	}
	if j.Phase == "source-quota-return" {
		if err = sourceServiceActorsRetired(op.NativeScope); err != nil {
			return err
		}
		if err = sourceServiceQuotaWritersRetired(p, false); err != nil {
			return err
		}
		before, err := os.ReadFile(filepath.Join(dir, "source-quota-before.json"))
		if err != nil || hostDigest(before) != op.Quota.Ledger.SHA256 {
			return errors.New("original Source quota before-image differs")
		}
		if err = admission.ReturnSourceResidentQuota(ctx, filepath.Join(p.Root, "quota"), dir, before, *op.Quota.ResidentReturn); err != nil {
			return err
		}
		empty, err := os.ReadFile(op.Quota.Ledger.Path)
		if err != nil {
			return err
		}
		if err = saveHostStorageBytes(filepath.Join(dir, "source-quota-empty.json"), empty); err != nil {
			return err
		}
		if err = advance("source-quota-migrate"); err != nil {
			return err
		}
	}
	if j.Phase == "source-quota-migrate" {
		if err = sourceServiceActorsRetired(op.NativeScope); err != nil {
			return err
		}
		if err = sourceServiceQuotaWritersRetired(p, false); err != nil {
			return err
		}
		empty, err := os.ReadFile(filepath.Join(dir, "source-quota-empty.json"))
		if err != nil {
			return err
		}
		if err = admission.MigrateEmptySourceQuotaV2(ctx, filepath.Join(p.Root, "quota"), dir, empty); err != nil {
			return err
		}
		raw, err := os.ReadFile(op.Quota.Ledger.Path)
		if err != nil {
			return err
		}
		if err = saveHostStorageBytes(filepath.Join(dir, "source-quota-migrated.json"), raw); err != nil {
			return err
		}
		ref, err := loopruntime.ReadSessionReadbackFile(filepath.Join(dir, "source-quota-migrated.json"), nil)
		if err != nil {
			return err
		}
		j.SourceService.Quota = &ref
		if err = advance("source-policy-retired"); err != nil {
			return err
		}
	}
	if j.Phase == "source-policy-retired" {
		if err = verifySourceServiceRetirement(j); err != nil {
			return err
		}
		if err = retireSourceServicePool(p); err != nil {
			return err
		}
		ref, err := saveSourceServiceObservation(dir, "source-retired-phase.json", j)
		if err != nil {
			return err
		}
		if err = advance("source-service-launch-intent"); err != nil {
			return err
		}
		g, launchErr := launchSourceServiceGeneration(ctx, p, ref)
		if validPoolID(g.ID) {
			j.Generation = &g
		}
		if err = saveRestrictedMaintenance(dir, j); err != nil {
			return err
		}
		if launchErr != nil {
			return launchErr
		}
	}
	if j.Phase == "source-service-launch-intent" {
		if j.Generation == nil {
			g, err := readOriginalSourceServiceLaunch(p)
			if err != nil {
				return err
			}
			j.Generation = &g
			if err = saveRestrictedMaintenance(dir, j); err != nil {
				return err
			}
		}
		if err = publishSourceServiceGeneration(ctx, j); err != nil {
			return err
		}
		if err = advance("source-service-active"); err != nil {
			return err
		}
	}
	return out.Encode(j)
}

func saveSourceServiceQuotaBefore(dir string, op hostOperatorPlan) error {
	raw, err := os.ReadFile(op.Quota.Ledger.Path)
	if err != nil || hostDigest(raw) != op.Quota.Ledger.SHA256 {
		return errors.New("original Source quota changed")
	}
	return saveHostStorageBytes(filepath.Join(dir, "source-quota-before.json"), raw)
}
