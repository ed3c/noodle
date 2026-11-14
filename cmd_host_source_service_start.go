package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
)

func verifySourceServiceRetirement(j restrictedMaintenance) error {
	p, progress := j.Plan, j.SourceService
	if p.SourcePolicy == nil || (p.SourcePolicy.Operation != "operator-service-restore" && p.SourcePolicy.Operation != "operator-service-upgrade") || j.Phase != "source-policy-retired" || progress == nil || progress.Compute == nil || progress.GatewayExit == nil || progress.Quota == nil {
		return errors.New("original Source retirement phase is incomplete")
	}
	if err := validateSourcePolicyPlan(p); err != nil {
		return err
	}
	if err := verifySourceServiceDrain(j); err != nil {
		return err
	}
	op, err := readSourcePolicySnapshot(p.SourcePolicy.Snapshot)
	if err != nil {
		return err
	}
	if err = sourceServiceActorsRetired(op.NativeScope); err != nil {
		return err
	}
	var quota struct {
		Schema  int                        `json:"schema"`
		Policy  string                     `json:"worker_admission_policy"`
		Maximum int                        `json:"maximum"`
		Leases  map[string]json.RawMessage `json:"leases"`
	}
	quotaName := "source-quota-migrated.json"
	if p.SourcePolicy.Operation == "operator-service-upgrade" {
		quotaName = "source-quota-empty.json"
	}
	ref, err := loopruntime.ReadSessionReadbackFile(progress.Quota.Path, &quota)
	if err != nil || ref != *progress.Quota || progress.Quota.Path != filepath.Join(p.Root, "maintenance", p.PlanSHA, quotaName) || quota.Schema != 3 || quota.Policy != admission.WorkerAdmissionPolicy || quota.Maximum != admission.DefaultWorkerLimit || quota.Leases == nil || len(quota.Leases) != 0 {
		return errors.New("original Source migrated empty quota differs")
	}
	if err = verifySourcePolicyHoldFiles(op); err != nil {
		return err
	}
	if err = verifySourceServiceBinaries(op.NativeScope); err != nil {
		return err
	}
	return loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"custody": op.Custody, "native-config": op.NativeScope.GatewayConfig})
}

func verifySourceServiceDrain(j restrictedMaintenance) error {
	if j.SourceService == nil || j.SourceService.Compute == nil || j.SourceService.GatewayExit == nil {
		return errors.New("original Source drain proof is missing")
	}
	progress := j.SourceService
	op, err := readSourcePolicySnapshot(j.Plan.SourcePolicy.Snapshot)
	if err != nil {
		return err
	}
	var compute sourceComputeObservation
	ref, err := loopruntime.ReadSessionReadbackFile(progress.Compute.Path, &compute)
	if err != nil || ref != *progress.Compute || compute.State != "observed_zero" || compute.ObservedAt.IsZero() || compute.History == nil || compute.History.Custody != op.Custody {
		return errors.New("original qualified Source compute observation differs")
	}
	var normal sourcePolicyNormalExit
	ref, err = loopruntime.ReadSessionReadbackFile(progress.GatewayExit.Path, &normal)
	if err != nil || ref != *progress.GatewayExit || normal.State != "observed" || normal.Method != "kernel-zombie" || normal.KernelExit == nil || normal.Parent == nil {
		return errors.New("original Source normal gateway exit is unproved")
	}
	exit, gateway := normal.KernelExit, op.NativeScope.Gateway.Process
	if !exit.NormalZero() || exit.PID != gateway.PID || exit.UID != gateway.UID || exit.ParentPID != gateway.ParentPID || exit.Group != gateway.Group || exit.StartSec != gateway.StartSec || exit.StartUsec != gateway.StartUsec || exit.Version != gateway.Version || !normal.Parent.SameGeneration(op.NativeScope.Host.Process) || !normal.Parent.Stopped {
		return errors.New("Source kernel normal exit differs from its original gateway and stopped parent")
	}
	return nil
}

func sourceServicePoolHeld(pool hostPool, p hostMaintenancePlan) error {
	if p.SourcePolicy != nil && p.SourcePolicy.Operation == "operator-service-upgrade" {
		base, err := sourceServiceBusinessPlan(p)
		if err != nil {
			return err
		}
		for _, owner := range pool.Owners {
			if owner.Current.HostInstanceID == p.Source.Instance && owner.Current.ContextState != "detached" {
				return errors.New("Source-only upgrade has an unexpected business owner")
			}
		}
		p = base
	}
	for _, cp := range p.Contexts {
		owner, ok := pool.Owners[cp.Original.RequestID]
		request := bootstrapTaskRequest(hostBootstrapTask{Original: cp.Original})
		if !ok || owner.MaintenanceHold != p.PlanSHA || !hostReceiptMatches(request, owner.Current) || owner.Current.HostInstanceID != p.Source.Instance || owner.Current.HostPID != p.Source.PID || owner.Current.BinarySHA256 != cp.Original.BinarySHA256 {
			return errors.New("original Source held business owner differs")
		}
	}
	return nil
}

func retireSourceServicePool(p hostMaintenancePlan) error {
	return updateHostPool(p.Root, func(pool *hostPool) error {
		if pool.Active != p.Source.Instance && pool.Active != "" {
			return errors.New("Source selected generation changed")
		}
		if err := sourceServicePoolHeld(*pool, p); err != nil {
			return err
		}
		generation, ok := pool.Generations[p.Source.Instance]
		if !ok || generation.Process.PID != p.Source.PID {
			return errors.New("original Source generation differs")
		}
		generation.State = "exited"
		pool.Generations[p.Source.Instance] = generation
		pool.Active = ""
		return nil
	})
}

func launchSourceServiceGeneration(ctx context.Context, p hostMaintenancePlan, retired loopruntime.SessionReadbackFileRef) (hostGeneration, error) {
	return launchHostGenerationWithSource(ctx, p.Root, p.Target, p.TargetSHA, &retired)
}

func readOriginalSourceServiceLaunch(p hostMaintenancePlan) (hostGeneration, error) {
	ref, err := loopruntime.ReadSessionReadbackFile(filepath.Join(p.Root, "maintenance", p.PlanSHA, "source-retired-phase.json"), nil)
	if err != nil {
		return hostGeneration{}, err
	}
	pool, err := readHostPool(p.Root)
	if err != nil {
		return hostGeneration{}, err
	}
	var selected hostGeneration
	for _, g := range pool.Generations {
		if g.SourceService == nil || *g.SourceService != ref || g.Binary != p.Target || g.SHA != p.TargetSHA {
			continue
		}
		if selected.ID != "" {
			return hostGeneration{}, errors.New("original Source launch identity is ambiguous; no spawn repeated")
		}
		selected = g
	}
	if selected.ID == "" {
		return selected, errors.New("original Source launch outcome unknown; no spawn repeated")
	}
	return selected, nil
}

// The new generation consumes the original retired phase before listening.
// Business contexts and the old queue are never activated in this service role.
func (h *noodleHost) startSourceService(ctx context.Context) error {
	if h.sourceService == nil {
		return nil
	}
	var retired restrictedMaintenance
	ref, err := loopruntime.ReadSessionReadbackFile(h.sourceService.Path, &retired)
	if err != nil || ref != *h.sourceService {
		return errors.New("original Source startup phase changed")
	}
	if err = verifySourceServiceRetirement(retired); err != nil {
		return err
	}
	p := retired.Plan
	if h.controlRoot() != p.Root || h.binaryPath != p.Target || h.binarySHA != p.TargetSHA || h.dir != filepath.Join(p.Root, "g", h.instanceID) || h.instanceID == p.Source.Instance || len(h.contexts) != 0 || len(h.receipts) != 0 {
		return errors.New("Source service startup is outside the bound new generation")
	}
	pool, err := readHostPool(p.Root)
	if err != nil {
		return err
	}
	if err = sourceServicePoolHeld(pool, p); err != nil {
		return err
	}
	business, err := sourceServiceBusinessPlan(p)
	if err != nil {
		return err
	}
	op, err := readSourcePolicySnapshot(business.SourcePolicy.Snapshot)
	if err != nil {
		return err
	}
	if err = verifySourceServiceBinaries(op.NativeScope); err != nil {
		return err
	}
	h.sourceServiceScope = op.NativeScope
	quota, err := loopruntime.ReadSessionReadbackFile(op.Quota.Ledger.Path, nil)
	if err != nil || quota.SHA256 != retired.SourceService.Quota.SHA256 {
		return errors.New("Source startup quota no longer matches the retired empty ledger")
	}
	intent := filepath.Join(h.dir, "source-service-start.json")
	if _, err := os.Lstat(intent); !os.IsNotExist(err) {
		return errors.New("Source service start already recorded or unknown; no replay")
	}
	owner, err := loopruntime.NewOpenShellSourcePolicyService(h.residents, h.budget, *op.OperatorPolicy, op.Custody, p.Root)
	if owner != nil {
		h.openShell = owner
	}
	if err != nil {
		return err
	}
	if _, err = saveSourceServiceObservation(h.dir, "source-service-start.json", map[string]any{"retired": ref, "generation": h.instanceID, "binary": h.binarySHA}); err != nil {
		return err
	}
	h.budget.Register("source-service-" + p.PlanSHA)
	observation, err := owner.AcquireSourceService(ctx, "source-service-"+p.PlanSHA)
	if err == nil {
		err = verifySourceServiceDeployment(ctx, h.sourceServiceScope, observation.Resident)
	}
	// Keep the original Host and child on unknown readiness; explicit readback
	// may prove that same start. Returning from serve would lose its true handle.
	if err != nil {
		h.sourceServiceDiagnostic = err.Error()
	}
	return nil
}

func (h *noodleHost) sourceServiceReadback(ctx context.Context) any {
	result := map[string]any{"status": "unknown", "host_instance_id": h.instanceID, "host_pid": os.Getpid(), "binary_sha256": h.binarySHA, "effects": []string{}}
	if h.sourceService == nil || h.openShell == nil {
		result["error"] = "original Source service owner is unavailable"
		return result
	}
	observation, err := h.openShell.ReadSourceService(ctx)
	if err == nil {
		err = verifySourceServiceDeployment(ctx, h.sourceServiceScope, observation.Resident)
	}
	result["observation"] = observation
	if err != nil {
		result["error"] = err.Error()
		h.sourceServiceDiagnostic = err.Error()
		return result
	}
	result["status"], h.sourceServiceDiagnostic = "healthy", ""
	return result
}

func verifySourceServiceBinaries(scope *hostOperatorNativeScope) error {
	if scope == nil {
		return errors.New("original Source deployment scope is unavailable")
	}
	for _, expected := range []loopruntime.SessionReadbackFileRef{scope.Gateway.Binary, scope.Driver.Binary} {
		current, err := hostReadbackBinaryRef(expected.Path)
		if err != nil || current != expected {
			return errors.New("original Source native executable changed; service start/readback refused")
		}
	}
	return nil
}

func verifySourceServiceDeployment(ctx context.Context, scope *hostOperatorNativeScope, resident admission.Resident) error {
	if err := verifySourceServiceBinaries(scope); err != nil {
		return err
	}
	observe := func(pid int, expected loopruntime.SessionReadbackFileRef) (procx.NativeIdentity, error) {
		identity, err := procx.ObserveNativeIdentity(pid)
		if err != nil || identity.Zombie || identity.UID != os.Getuid() {
			return identity, errors.New("Source service actor native identity unavailable")
		}
		image, err := procx.ObserveNativeImage(identity)
		if err != nil {
			return identity, err
		}
		actual, err := hostReadbackBinaryRef(image)
		if err != nil || actual != expected {
			return identity, errors.New("Source service actor image differs from its original executable")
		}
		return identity, nil
	}
	gateway, err := observe(resident.PID, scope.Gateway.Binary)
	if err != nil {
		return err
	}
	if gateway.ParentPID != os.Getpid() || gateway.Group != resident.ProcessGroup {
		return errors.New("Source gateway is outside its original new Host child")
	}
	rows, err := admission.ObserveVisibleProcesses(ctx)
	if err != nil {
		return err
	}
	var driver procx.NativeIdentity
	for _, row := range rows {
		if row.ParentPID != gateway.PID {
			continue
		}
		if driver.PID != 0 {
			return errors.New("Source gateway has an unbound additional direct child")
		}
		driver, err = observe(row.PID, scope.Driver.Binary)
		if err != nil {
			return err
		}
		if driver.ParentPID != gateway.PID {
			return errors.New("Source driver parent changed")
		}
	}
	if driver.PID == 0 {
		return errors.New("Source driver native child is unavailable")
	}
	for _, actor := range []procx.NativeIdentity{gateway, driver} {
		current, err := procx.ObserveNativeIdentity(actor.PID)
		if err != nil || !actor.SameGeneration(current) || current.Zombie || current.ParentPID != actor.ParentPID || current.Group != actor.Group {
			return errors.New("Source actor generation changed during deployment readback")
		}
	}
	return verifySourceServiceBinaries(scope)
}

func publishSourceServiceGeneration(ctx context.Context, j restrictedMaintenance) error {
	g, p := j.Generation, j.Plan
	if g == nil || g.SourceService == nil {
		return errors.New("original Source generation lacks its retired phase binding")
	}
	ref, err := loopruntime.ReadSessionReadbackFile(filepath.Join(p.Root, "maintenance", p.PlanSHA, "source-retired-phase.json"), nil)
	if err != nil || ref != *g.SourceService {
		return errors.New("original Source generation retired phase binding changed")
	}
	raw, err := nativeHostRPC(ctx, g.Directory, hostRequest{Operation: "source_service_readback"})
	if err != nil {
		return err
	}
	var result struct {
		Status   string `json:"status"`
		Instance string `json:"host_instance_id"`
		Binary   string `json:"binary_sha256"`
		PID      int    `json:"host_pid"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Status != "healthy" || result.Instance != g.ID || result.Binary != p.TargetSHA || result.PID != g.Process.PID {
		return errors.New("original new Source service health is unresolved; readback only")
	}
	if err = updateHostPool(p.Root, func(pool *hostPool) error {
		if pool.Active != "" && pool.Active != g.ID {
			return errors.New("Source publication selected generation changed")
		}
		actual, ok := pool.Generations[g.ID]
		if !ok || actual.SHA != p.TargetSHA || actual.Process.PID != g.Process.PID || actual.SourceService == nil || *actual.SourceService != *g.SourceService {
			return errors.New("original Source launch identity changed")
		}
		if err := sourceServicePoolHeld(*pool, p); err != nil {
			return err
		}
		actual.State, pool.Active = "active", g.ID
		pool.Generations[g.ID] = actual
		return nil
	}); err != nil {
		return err
	}
	marker, err := readHostBootstrap(p.Root)
	if err != nil || (marker.Phase != "source-policy-"+p.PlanSHA && marker.Phase != "active") || marker.Source.ID != p.Source.Instance || marker.TargetSHA != p.TargetSHA {
		return errors.New("original Source publication marker changed")
	}
	marker.Phase = "active"
	if err = saveHostBootstrap(p.Root, marker); err != nil {
		return err
	}
	return publishPoolSocket(p.Root, g.Directory)
}
