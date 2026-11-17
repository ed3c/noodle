package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

// The base owns business provenance. This epoch owns only the current physical
// service retirement; it never replaces the original held task identities.
func sourceServiceBusinessPlan(p hostMaintenancePlan) (hostMaintenancePlan, error) {
	if p.SourcePolicy == nil || p.SourcePolicy.Operation != "operator-service-upgrade" {
		return p, nil
	}
	if p.SourcePolicy.Base == nil {
		return p, errors.New("Source upgrade lacks original held provenance")
	}
	var base restrictedMaintenance
	ref, err := loopruntime.ReadSessionReadbackFile(p.SourcePolicy.Base.Path, &base)
	if err != nil || ref != *p.SourcePolicy.Base || base.Plan.SourcePolicy == nil || base.Plan.SourcePolicy.Operation != "operator-service-restore" || base.Plan.SourcePolicy.Base != nil || base.Plan.Root != p.Root {
		return p, errors.New("Source upgrade base differs from original restore")
	}
	if err = verifySourceServiceRetirement(base); err != nil {
		return p, err
	}
	return base.Plan, nil
}

func verifySourceServiceMaintenanceTarget(ctx context.Context, p hostMaintenancePlan) error {
	target := loopruntime.SessionReadbackFileRef{Path: p.Target, SHA256: p.TargetSHA}
	if err := verifySourceServiceTarget(ctx, target); err != nil {
		return err
	}
	if p.SourcePolicy.Operation == "operator-service-upgrade" {
		raw, err := runSourceUpgradeCapabilities(ctx, target.Path)
		if err != nil || raw != 1 {
			return errors.New("selected target has no bound Source service upgrade consumer")
		}
		current, err := hostReadbackBinaryRef(target.Path)
		if err != nil || current != target {
			return errors.New("Source upgrade target changed during capability readback")
		}
	}
	return nil
}

func validateSourceServiceUpgradePlan(p hostMaintenancePlan, op hostOperatorPlan) error {
	base, err := sourceServiceBusinessPlan(p)
	if err != nil {
		return err
	}
	original, err := readSourcePolicySnapshot(base.SourcePolicy.Snapshot)
	if err != nil {
		return err
	}
	if len(p.Contexts) != 0 || len(op.Contexts) != 0 || p.Source.Contexts != 0 || p.Source.Instance == base.Source.Instance || p.Policy != admission.WorkerAdmissionPolicy || !filepath.IsAbs(p.Target) || len(p.TargetSHA) != 64 || op.OperatorPolicy == nil || *op.OperatorPolicy != *original.OperatorPolicy || op.Custody != original.Custody || op.Request != original.Request || op.Config != original.Config || op.History != original.History || op.Profile != original.OperatorPolicy.After || op.ProfileSource != op.Profile.Path || op.Binding != original.OperatorPolicy.AfterBinding || op.Quota == nil || op.Quota.Schema != 3 || op.Quota.ResidentReturn == nil || op.Quota.Ledger.Path != filepath.Join(p.Root, "quota", "quota.json") || op.Quota.ResidentReturn.Owner != p.Source.Instance || op.NativeScope.Gateway.Binary != original.NativeScope.Gateway.Binary || op.NativeScope.Driver.Binary != original.NativeScope.Driver.Binary || op.NativeScope.GatewayConfig != original.NativeScope.GatewayConfig || op.NativeScope.StateDirectory != original.NativeScope.StateDirectory || op.NativeScope.Socket != original.NativeScope.Socket {
		return errors.New("Source upgrade differs from current service or original held provenance")
	}
	return verifySourcePolicyHoldFiles(op)
}

func observeSourceServiceUpgrade(ctx context.Context, root string) (hostOperatorPlan, loopruntime.SessionReadbackFileRef, error) {
	var empty loopruntime.SessionReadbackFileRef
	pool, err := readHostPool(root)
	if err != nil {
		return hostOperatorPlan{}, empty, err
	}
	g, ok := pool.Generations[pool.Active]
	if !ok || g.State != "active" || g.SourceService == nil {
		return hostOperatorPlan{}, empty, errors.New("active Source-only service generation is required")
	}
	var current restrictedMaintenance
	ref, err := loopruntime.ReadSessionReadbackFile(g.SourceService.Path, &current)
	if err != nil || ref != *g.SourceService {
		return hostOperatorPlan{}, empty, errors.New("current Source retirement binding changed")
	}
	if err = verifySourceServiceRetirement(current); err != nil {
		return hostOperatorPlan{}, empty, err
	}
	baseRef := *g.SourceService
	if current.Plan.SourcePolicy.Operation == "operator-service-upgrade" {
		baseRef = *current.Plan.SourcePolicy.Base
	}
	base, err := sourceServiceBusinessPlan(current.Plan)
	if err != nil {
		return hostOperatorPlan{}, empty, err
	}
	op, err := readSourcePolicySnapshot(base.SourcePolicy.Snapshot)
	if err != nil {
		return op, empty, err
	}
	if err = sourceServicePoolHeld(pool, base); err != nil {
		return op, empty, err
	}
	op.Profile, op.ProfileSource, op.Binding = op.OperatorPolicy.After, op.OperatorPolicy.After.Path, op.OperatorPolicy.AfterBinding
	op.Contexts = []hostReceipt{}
	op.Source, err = nativeHostRPC(ctx, g.Directory, hostRequest{Operation: "resources"})
	if err != nil {
		return op, empty, err
	}
	if err = verifySourceUpgradeResources(op.Source, g, *g.SourceService); err != nil {
		return op, empty, err
	}
	var resources struct {
		Residents struct {
			Records []admission.Resident `json:"records"`
		} `json:"residents"`
	}
	if err = json.Unmarshal(op.Source, &resources); err != nil {
		return op, empty, err
	}
	op.Resident = resources.Residents.Records[0]
	op.Pool, err = loopruntime.ReadSessionReadbackFile(filepath.Join(root, "pool.json"), nil)
	if err != nil {
		return op, empty, err
	}
	op.NativeScope, err = collectSourceNativeScope(ctx, op)
	if err != nil {
		return op, empty, err
	}
	op.Quota, err = observeSourceQuota(op)
	if err != nil {
		return op, empty, err
	}
	raw, err := os.ReadFile(op.Quota.Ledger.Path)
	if err != nil || hostDigest(raw) != op.Quota.Ledger.SHA256 {
		return op, empty, errors.New("Source upgrade quota changed during readback")
	}
	if err = verifySourceServiceUpgradeQuota(raw); err != nil {
		return op, empty, err
	}
	selected, err := admission.PrepareSourceResidentQuotaReturnV3(raw, g.ID, op.Resident.Contexts, op.Resident.ID)
	if err != nil {
		return op, empty, err
	}
	op.Quota.ResidentReturn = &selected
	fresh, err := nativeHostRPC(ctx, g.Directory, hostRequest{Operation: "resources"})
	if err != nil {
		return op, empty, err
	}
	if err = verifySourceUpgradeResources(fresh, g, *g.SourceService); err != nil {
		return op, empty, err
	}
	var after struct {
		Residents struct {
			Records []admission.Resident `json:"records"`
		} `json:"residents"`
	}
	if json.Unmarshal(fresh, &after) != nil || !reflect.DeepEqual(after.Residents.Records, resources.Residents.Records) {
		return op, empty, errors.New("Source resident changed during upgrade readback")
	}
	if err = loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"pool": op.Pool, "quota": op.Quota.Ledger, "custody": op.Custody}); err != nil {
		return op, empty, err
	}
	return op, baseRef, nil
}

func verifySourceUpgradeResources(raw json.RawMessage, g hostGeneration, retired loopruntime.SessionReadbackFileRef) error {
	var r struct {
		legacyHostResources
		Root      string `json:"host_root"`
		Directory string `json:"host_dir"`
		Binary    string `json:"binary_path"`
		SHA       string `json:"binary_sha256"`
		Admission struct {
			Status    string `json:"status"`
			CanAccept bool   `json:"can_accept"`
		} `json:"admission"`
		Source struct {
			Retired    loopruntime.SessionReadbackFileRef `json:"retired_phase"`
			Diagnostic string                             `json:"diagnostic"`
		} `json:"source_service"`
		Residents struct {
			Records []admission.Resident `json:"records"`
			Error   json.RawMessage      `json:"error"`
		} `json:"residents"`
		Resources struct {
			Workers   int `json:"active_workers"`
			Residents int `json:"active_residents"`
			Maximum   int `json:"maximum_workers"`
		} `json:"resources"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Protocol != hostProtocol || r.Rolling != 1 || r.Instance != g.ID || r.PID != g.Process.PID || r.Contexts != 0 || r.Root != filepath.Dir(filepath.Dir(g.Directory)) || r.Directory != g.Directory || r.Binary != g.Binary || r.SHA != g.SHA || r.Admission.Status != "service_only" || r.Admission.CanAccept || r.Source.Retired != retired || r.Source.Diagnostic != "" || string(r.Residents.Error) != "null" || len(r.Residents.Records) != 1 || r.Residents.Records[0].State != "running" || r.Resources.Workers != 0 || r.Resources.Residents != 1 || r.Resources.Maximum != admission.DefaultWorkerLimit {
		return errors.New("current Source role, generation or sole resident is unresolved")
	}
	return nil
}

func prepareSourceServiceUpgrade(ctx context.Context, root, target, snapshot string) (hostMaintenancePlan, error) {
	var p hostMaintenancePlan
	if !filepath.IsAbs(snapshot) {
		return p, errors.New("Source upgrade requires an absolute immutable snapshot output")
	}
	op, base, err := observeSourceServiceUpgrade(ctx, root)
	if err != nil {
		return p, err
	}
	var source legacyHostResources
	if err = json.Unmarshal(op.Source, &source); err != nil {
		return p, err
	}
	binary, err := hostReadbackBinaryRef(target)
	if err != nil {
		return p, err
	}
	p = hostMaintenancePlan{Schema: 1, Root: root, Source: source, Native: &op.NativeScope.Host.Process, Target: binary.Path, TargetSHA: binary.SHA256, Policy: admission.WorkerAdmissionPolicy, SourcePolicy: &hostSourcePolicyPlan{Operation: "operator-service-upgrade", Base: &base}, Contexts: []hostMaintenanceContextPlan{}, Files: map[string]string{}, Blockers: []string{}, Effects: []string{}, Executable: true,
		Observations: []string{"Upgrade only the current Source service; original business holds, unknown outcomes and historical diagnostics remain unchanged"}, Steps: []string{"Bind current Source-only actors and exact schema3 resident allocation", "Fence the current service, qualify zero compute and retain normal gateway exit", "Retire only the stopped current Host and return its exact resident row without migration", "Start the immutable replacement with original business provenance and publish only verified Source service health"}}
	if err = verifyRollingTarget(ctx, binary.Path, binary.SHA256); err != nil {
		return p, err
	}
	if err = verifySourceServiceMaintenanceTarget(ctx, p); err != nil {
		return p, err
	}
	if err = sourceServiceQuotaWritersRetired(p, true); err != nil {
		return p, err
	}
	p.SourcePolicy.Snapshot, err = saveSourceServiceObservation(filepath.Dir(snapshot), filepath.Base(snapshot), op)
	if err != nil {
		return p, err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	p.PlanSHA = hostDigest(raw)
	return p, validateSourcePolicyPlan(p)
}

func initializeSourceServiceMaintenance(ctx context.Context, p hostMaintenancePlan, dir string, out *json.Encoder) error {
	if p.SourcePolicy.Operation != "operator-service-upgrade" {
		return runSourcePolicyHoldLocked(ctx, p, dir, out)
	}
	op, err := readSourcePolicySnapshot(p.SourcePolicy.Snapshot)
	if err != nil {
		return err
	}
	j, err := readRestrictedMaintenance(dir)
	if os.IsNotExist(err) {
		marker, err := readHostBootstrap(p.Root)
		if err != nil || marker.Phase != "active" {
			return errors.New("another Source maintenance marker is present")
		}
		if err = os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		for name, ref := range map[string]loopruntime.SessionReadbackFileRef{"original-pool.json": op.Pool, "original-bootstrap.json": {Path: filepath.Join(p.Root, "bootstrap.json")}, "original-history.json": op.History, "original-request.json": op.Request, "original-config.toml": op.Config, "original-profile.json": op.Profile, "original-custody.json": op.Custody} {
			raw, err := os.ReadFile(ref.Path)
			if err != nil {
				return err
			}
			if ref.SHA256 != "" && hostDigest(raw) != ref.SHA256 {
				return errors.New("Source upgrade before-image changed")
			}
			if err = saveHostStorageBytes(filepath.Join(dir, name), raw); err != nil {
				return err
			}
		}
		j = restrictedMaintenance{Schema: 1, Plan: p, Phase: "source-policy-prepared"}
		if err = saveRestrictedMaintenance(dir, j); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if j.Plan.PlanSHA != p.PlanSHA {
		return errors.New("Source upgrade epoch changed")
	}
	// The running role is already admission-closed. Read back the original
	// generation before publishing this epoch's separate maintenance marker.
	pool, err := readHostPool(p.Root)
	if err != nil {
		return err
	}
	g, ok := pool.Generations[p.Source.Instance]
	if !ok || g.SourceService == nil || pool.Active != g.ID {
		return errors.New("Source upgrade generation changed")
	}
	raw, err := nativeHostRPC(ctx, g.Directory, hostRequest{Operation: "resources"})
	if err != nil {
		return err
	}
	if err = verifySourceUpgradeResources(raw, g, *g.SourceService); err != nil {
		return err
	}
	scope, err := collectSourceNativeScope(ctx, op)
	if err != nil || !reflect.DeepEqual(scope, op.NativeScope) {
		return errors.New("Source upgrade actors changed before fencing")
	}
	var previous restrictedMaintenance
	previousRef, err := loopruntime.ReadSessionReadbackFile(g.SourceService.Path, &previous)
	if err != nil || previousRef != *g.SourceService {
		return errors.New("current Source retirement reference changed")
	}
	if err = verifySourceServiceRetirement(previous); err != nil {
		return err
	}
	baseRef := *g.SourceService
	if previous.Plan.SourcePolicy.Operation == "operator-service-upgrade" {
		baseRef = *previous.Plan.SourcePolicy.Base
	}
	if p.SourcePolicy.Base == nil || baseRef != *p.SourcePolicy.Base {
		return errors.New("Source upgrade substituted original held provenance")
	}
	var selected struct {
		Source struct {
			Retired loopruntime.SessionReadbackFileRef `json:"retired_phase"`
		} `json:"source_service"`
	}
	if json.Unmarshal(op.Source, &selected) != nil || selected.Source.Retired != *g.SourceService {
		return errors.New("Source upgrade previous generation binding differs")
	}
	if err = sourceServicePoolHeld(pool, p); err != nil {
		return err
	}
	if err = loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"custody": op.Custody, "quota": op.Quota.Ledger}); err != nil {
		return err
	}
	quotaRaw, err := os.ReadFile(op.Quota.Ledger.Path)
	if err != nil || hostDigest(quotaRaw) != op.Quota.Ledger.SHA256 {
		return errors.New("Source quota changed before upgrade fencing")
	}
	if err = verifySourceServiceUpgradeQuota(quotaRaw); err != nil {
		return err
	}
	if err = sourcePolicyMarker(p.Root, dir, p); err != nil {
		return err
	}
	if err = publishMaintenanceSocket(p.Root); err != nil {
		return err
	}
	if err = verifySourceServiceAdmissionHold(p); err != nil {
		return err
	}
	j.Phase = "source-policy-admission-held"
	if err = saveRestrictedMaintenance(dir, j); err != nil {
		return err
	}
	return out.Encode(j)
}

func verifySourceServiceAdmissionHold(p hostMaintenancePlan) error {
	if p.SourcePolicy.Operation != "operator-service-upgrade" {
		return verifySourcePolicyAdmissionHold(p.Root, p)
	}
	marker, err := readHostBootstrap(p.Root)
	if err != nil || marker.Phase != "source-policy-"+p.PlanSHA || marker.Source.ID != p.Source.Instance || marker.Source.Process.PID != p.Source.PID {
		return errors.New("Source upgrade admission marker differs")
	}
	alias, err := os.Readlink(filepath.Join(p.Root, "host.sock"))
	if err != nil || alias != filepath.Join(p.Root, "maintenance.sock") {
		return errors.New("Source upgrade public gate differs")
	}
	pool, err := readHostPool(p.Root)
	if err != nil {
		return err
	}
	if pool.Active != p.Source.Instance {
		return errors.New("Source upgrade selected generation changed")
	}
	return sourceServicePoolHeld(pool, p)
}

func returnSourceServiceQuota(ctx context.Context, p hostMaintenancePlan, dir string, before []byte, row admission.SourceResidentQuotaReturn) error {
	if p.SourcePolicy.Operation == "operator-service-upgrade" {
		return admission.ReturnSourceResidentQuotaV3(ctx, filepath.Join(p.Root, "quota"), dir, before, row)
	}
	return admission.ReturnSourceResidentQuota(ctx, filepath.Join(p.Root, "quota"), dir, before, row)
}

func runSourceUpgradeCapabilities(ctx context.Context, path string) (int, error) {
	raw, err := exec.CommandContext(ctx, path, "host", "capabilities").Output()
	if err != nil {
		return 0, err
	}
	var c struct {
		Upgrade int `json:"source_service_upgrade"`
	}
	err = json.Unmarshal(raw, &c)
	return c.Upgrade, err
}

func verifySourceServiceUpgradeQuota(raw []byte) error {
	var q struct {
		Schema  int                        `json:"schema"`
		Maximum int                        `json:"maximum"`
		Policy  string                     `json:"worker_admission_policy"`
		Leases  map[string]json.RawMessage `json:"leases"`
	}
	if json.Unmarshal(raw, &q) != nil || q.Schema != 3 || q.Maximum != admission.DefaultWorkerLimit || q.Policy != admission.WorkerAdmissionPolicy || len(q.Leases) != 1 {
		return errors.New("Source upgrade requires exactly its sole resident and unchanged nine-worker policy")
	}
	return nil
}
