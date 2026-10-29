package main

import (
	"context"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
)

type hostGatewayHandoverPlan struct {
	SharedRoot, SharedInstance     string
	SourceBinary, Profile, Custody loopruntime.SessionReadbackFileRef
	Binding                        loopruntime.OpenShellCustodyBinding
	Cold                           *hostColdHandoverPlan `json:",omitempty"`
}

// This contract is deliberately separate from the earlier same-root bootstrap.
// Only original completed-context cleanup, readback and custody release are used.
func verifyGatewayHandoverBinary(ref loopruntime.SessionReadbackFileRef) error {
	actual, err := loopruntime.ReadSessionReadbackFile(ref.Path, nil)
	if err != nil || actual != ref {
		return errors.New("gateway handover source executable changed")
	}
	info, err := buildinfo.ReadFile(ref.Path)
	if err != nil {
		return err
	}
	settings := map[string]string{}
	for _, item := range info.Settings {
		settings[item.Key] = item.Value
	}
	if settings["vcs.revision"] != "119036b6828556ebae3f7470491ab6811c4e911d" || settings["vcs.modified"] != "false" {
		return errors.New("legacy executable has no verified completed-owner handover contract")
	}
	return nil
}

func handoverSharedReadback(ctx context.Context, root string, releaseSchema int) (string, error) {
	raw, err := nativeHostRPC(ctx, root, hostRequest{Operation: "resources"})
	if err != nil {
		return "", err
	}
	var row struct {
		BinaryPath string `json:"binary_path"`
		BinarySHA  string `json:"binary_sha256"`
		Shutdown   *struct{ Requested *bool }
		Resources  struct {
			Policy string                 `json:"worker_admission_policy"`
			Limit  uint64                 `json:"swap_used_limit_bytes"`
			Quota  *struct{ Maximum int } `json:"shared_quota"`
		}
	}
	// Use native field names rather than accepting Go's zero defaults.
	var identity struct {
		Protocol string `json:"protocol"`
		Root     string `json:"host_root"`
		Instance string `json:"host_instance_id"`
		Rolling  int    `json:"rolling_host_version"`
	}
	if json.Unmarshal(raw, &row) != nil || json.Unmarshal(raw, &identity) != nil || identity.Protocol != hostProtocol || identity.Root != root || !validPoolID(identity.Instance) || identity.Rolling != 1 || row.Shutdown == nil || row.Shutdown.Requested == nil || *row.Shutdown.Requested || row.Resources.Policy != admission.WorkerAdmissionPolicy || row.Resources.Limit != admission.DefaultSwapUsedLimit || row.Resources.Quota == nil || row.Resources.Quota.Maximum != 4 {
		return "", errors.New("gateway handover target is not the existing shared 15 GB Swap owner")
	}
	ref, err := loopruntime.ReadSessionReadbackFile(row.BinaryPath, nil)
	if err != nil || ref.SHA256 != row.BinarySHA {
		return "", errors.New("shared OpenShell Host executable source differs")
	}
	capsRaw, err := hostCapabilityCommand(ctx, ref.Path).Output()
	var caps struct {
		Custody  int `json:"openshell_custody_version"`
		Handover int `json:"gateway_host_handover"`
		Release  int `json:"gateway_custody_release"`
	}
	if err != nil || json.Unmarshal(capsRaw, &caps) != nil || caps.Custody != 4 || caps.Handover != 1 {
		return "", errors.New("shared OpenShell Host requires the custody v4 compatible deployment")
	}
	if releaseSchema >= 2 && caps.Release < releaseSchema {
		return "", fmt.Errorf("shared OpenShell Host cannot read custody release schema %d; deploy compatible reader before handover", releaseSchema)
	}
	return identity.Instance, nil
}

func sealGatewayHandoverPlan(p hostMaintenancePlan) (hostMaintenancePlan, error) {
	sort.Strings(p.Blockers)
	p.Executable = len(p.Blockers) == 0
	for _, cp := range p.Contexts {
		p.Executable = p.Executable && len(cp.Blockers) == 0
	}
	p.PlanSHA = ""
	raw, err := json.Marshal(p)
	if err == nil {
		p.PlanSHA = hostDigest(raw)
	}
	return p, err
}

func planGatewayHandover(ctx context.Context, root, shared, target, digest string, requests []string) (hostMaintenancePlan, error) {
	return planGatewayHandoverWithRecovery(ctx, root, shared, target, digest, requests, "", "")
}

func planGatewayHandoverWithRecovery(ctx context.Context, root, shared, target, digest string, requests []string, recovery, sourcePlan string) (hostMaintenancePlan, error) {
	p := hostMaintenancePlan{Schema: 1, Root: root, Target: target, TargetSHA: digest, Policy: admission.WorkerAdmissionPolicy,
		Files: map[string]string{}, Contexts: []hostMaintenanceContextPlan{}, Blockers: []string{}, Observations: []string{}, Effects: []string{},
		Steps: []string{"Wait for original completed-only cleanup eligibility; do not pause or interrupt existing work", "Fence new admissions through the original maintenance socket and recheck every original context", "Release completed contexts once and verify empty resources; do not signal the source Host", "Acquire released custody exclusively and preserve v3 history while upgrading to shared control-root v4; native old-client rejection fences reacquisition"}}
	canonical, err := filepath.EvalSymlinks(shared)
	if err != nil || !filepath.IsAbs(shared) || canonical != shared || shared == root {
		return p, errors.New("handover-root is not a distinct canonical existing directory")
	}
	p.Handover = &hostGatewayHandoverPlan{SharedRoot: shared}
	if err = verifyRollingTarget(ctx, target, digest); err != nil {
		return p, err
	}
	schema := 2
	if recovery != "" {
		schema = 3
	}
	if p.Handover.SharedInstance, err = handoverSharedReadback(ctx, shared, schema); err != nil {
		p.Blockers = append(p.Blockers, err.Error())
	}
	r, err := readLegacyResources(ctx, root)
	var absent *hostColdAbsentSource
	if sourcePlan != "" {
		absent, err = readColdAbsentSource(root, recovery, sourcePlan)
		if err == nil {
			r = absent.Resources
			p.Native = &absent.Native
		}
	}
	if err != nil {
		p.Blockers = append(p.Blockers, "original owner readback unavailable: "+err.Error())
		return sealGatewayHandoverPlan(p)
	}
	p.Source = r
	if recovery == "" && (r.Resources.Workers != 0 || r.Resources.Unresolved != 0 || r.Residents.Error != nil) {
		p.Blockers = append(p.Blockers, "original work or unknown resources remain; no control is eligible")
	}
	if absent == nil {
		native, err := procx.ObserveNativeIdentity(r.PID)
		if err != nil || !native.SameGeneration(native) || native.Zombie || native.UID != os.Getuid() {
			p.Blockers = append(p.Blockers, "original native process generation unavailable")
		} else {
			p.Native = &native
		}
	}

	var receipts []hostReceipt
	ref, err := loopruntime.ReadSessionReadbackFile(filepath.Join(root, "receipts.json"), &receipts)
	if err != nil {
		return p, err
	}
	p.Files["receipts.json"] = ref.SHA256
	for _, name := range []string{"bootstrap.json", "pool.json", "quota/quota.json"} {
		raw, readErr := os.ReadFile(filepath.Join(root, name))
		if os.IsNotExist(readErr) {
			p.Files[name] = "absent"
		} else if readErr != nil {
			p.Blockers = append(p.Blockers, name+": "+readErr.Error())
		} else {
			p.Files[name] = hostDigest(raw)
		}
	}
	if pool, readErr := readHostPool(root); readErr == nil {
		for id, generation := range pool.Generations {
			if generation.Process.PID == r.PID {
				continue
			}
			absent, absenceErr := generation.Process.GroupAbsent()
			if absenceErr != nil || !absent {
				p.Blockers = append(p.Blockers, "previous original quota writer retirement unconfirmed: "+id)
			}
		}
	} else if !os.IsNotExist(readErr) {
		p.Blockers = append(p.Blockers, "original pool readback unavailable: "+readErr.Error())
	}
	latest := map[string]hostReceipt{}
	for _, receipt := range receipts {
		if receipt.HostInstanceID == r.Instance {
			latest[receipt.ContextID] = receipt
			p.Handover.SourceBinary = loopruntime.SessionReadbackFileRef{Path: receipt.BinaryPath, SHA256: receipt.BinarySHA256}
		}
	}
	if err = verifyGatewayHandoverBinary(p.Handover.SourceBinary); err != nil {
		p.Blockers = append(p.Blockers, err.Error())
	}
	if absent == nil {
		if _, err = legacyProcessIdentity(ctx, r.PID, p.Handover.SourceBinary.Path); err != nil {
			p.Blockers = append(p.Blockers, "original executable/process binding differs: "+err.Error())
		}
	}
	for _, receipt := range latest {
		if receipt.ContextState == "detached" {
			if receipt.TerminalCause != loop.TerminalAutoCompleted && receipt.TerminalCause != loop.TerminalExplicitCleanup && receipt.AdmissionRefusal == "" {
				p.Blockers = append(p.Blockers, "original detached context has no completed cleanup boundary")
				continue
			}
			if profileErr := bindGatewayHandoverProfile(&p, receipt.ConfigPath, receipt.ConfigSHA256); profileErr != nil {
				p.Blockers = append(p.Blockers, profileErr.Error())
			}
			continue
		}
		if receipt.ContextState != "active" || receipt.HostPID != r.PID || receipt.BinarySHA256 != p.Handover.SourceBinary.SHA256 {
			p.Blockers = append(p.Blockers, "original context identity is unsettled")
			continue
		}
		cp := hostMaintenanceContextPlan{Original: receipt, Files: map[string]string{}, Blockers: []string{}}
		if len(requests) == 0 {
			cp.Blockers = append(cp.Blockers, "current accepted request FileRef required from the original owner continuation")
		} else {
			if recovery != "" {
				cp = planHostMaintenanceContextInputs(root, receipt, requests)
			} else {
				cp = planHostMaintenanceContext(root, receipt, requests)
			}
		}
		if recovery == "" {

			req := bootstrapTaskRequest(hostBootstrapTask{Original: receipt})
			req.Operation = "cleanup"
			raw, readErr := nativeHostRPC(ctx, root, req)
			var observed hostResponse
			if readErr != nil {
				cp.Blockers = append(cp.Blockers, "original cleanup readback unavailable: "+readErr.Error())
			} else if err := json.Unmarshal(raw, &observed); err != nil {
				cp.Blockers = append(cp.Blockers, "original cleanup response invalid: "+err.Error())
			} else if observed.Status != "eligible" || !hostReceiptMatches(req, observed.hostReceipt) {
				cp.Blockers = append(cp.Blockers, fmt.Sprintf("original cleanup not confirmed: status=%q error=%q; retain owner and frozen contract", observed.Status, observed.Error))
			}
		}
		if cp.ConfigSource != "" {
			if profileErr := bindGatewayHandoverProfile(&p, cp.ConfigSource, receipt.ConfigSHA256); profileErr != nil {
				cp.Blockers = append(cp.Blockers, profileErr.Error())
			}
		}
		p.Contexts = append(p.Contexts, cp)
	}
	sort.Slice(p.Contexts, func(i, j int) bool { return p.Contexts[i].Original.ContextID < p.Contexts[j].Original.ContextID })
	if len(p.Contexts) != r.Contexts {
		p.Blockers = append(p.Blockers, "native context count changed during readback")
	}
	if p.Handover.Profile.Path == "" {
		p.Blockers = append(p.Blockers, "original gateway profile FileRef unavailable; no profile is reconstructed")
	} else {
		p.Handover.Custody, err = loopruntime.ReadSessionReadbackFile(filepath.Join(p.Handover.Binding.StateRoot, "owner.json"), nil)
		if err != nil {
			p.Blockers = append(p.Blockers, err.Error())
		}
	}
	if recovery != "" {
		p.Handover.Cold, err = planColdGatewayHandover(ctx, p, recovery, absent)
		if err != nil {
			p.Blockers = append(p.Blockers, err.Error())
		}
		p.Steps = []string{"Fence managed new admission and verify qualified stopped compute, exact original task inputs and process generations", "Retire only the confirmed failed Host and dedicated gateway actors; persist intent, read back each exit, never replay an unknown control", "Preserve failed outcomes, unknown Exec and nonzero old accounts; settle only their retired resource responsibility", "Upgrade original custody to shared control-root v4 with release schema 3; new Host performs fresh native audit before R3"}
	}
	return sealGatewayHandoverPlan(p)
}

func bindGatewayHandoverProfile(p *hostMaintenancePlan, path, digest string) error {
	data, err := os.ReadFile(path)
	if err != nil || hostDigest(data) != digest {
		return errors.New("original OpenShell config source differs")
	}
	cfg, err := config.Parse(data)
	if err != nil || !filepath.IsAbs(cfg.Runtime.OpenShell.Profile) {
		return errors.New("original OpenShell profile source unavailable")
	}
	profile, err := loopruntime.LoadOpenShellProfile(cfg.Runtime.OpenShell.Profile)
	if err != nil {
		return err
	}
	ref, err := loopruntime.ReadSessionReadbackFile(profile.SourcePath, nil)
	if err != nil {
		return err
	}
	if p.Handover.Profile.Path != "" && p.Handover.Binding != profile.Gateway.Binding {
		return errors.New("original contexts use different gateway bindings")
	}
	p.Handover.Profile, p.Handover.Binding = ref, profile.Gateway.Binding
	return nil
}

func validateGatewayHandoverPlan(p hostMaintenancePlan) error {
	digest := p.PlanSHA
	p.PlanSHA = ""
	raw, err := json.Marshal(p)
	if err != nil || digest == "" || hostDigest(raw) != digest || p.Schema != 1 || !p.Executable || len(p.Blockers) != 0 || p.Native == nil || !p.Native.SameGeneration(*p.Native) || p.Native.PID != p.Source.PID || p.Native.UID != os.Getuid() || p.Handover == nil || p.Handover.SharedRoot == p.Root || !filepath.IsAbs(p.Root) || !filepath.IsAbs(p.Handover.SharedRoot) || !validPoolID(p.Source.Instance) || !validPoolID(p.Handover.SharedInstance) || len(p.Contexts) != p.Source.Contexts || p.Policy != admission.WorkerAdmissionPolicy || len(p.TargetSHA) != 64 || !filepath.IsAbs(p.Target) {
		return errors.New("gateway handover plan is blocked, incomplete or changed")
	}
	for _, ref := range []loopruntime.SessionReadbackFileRef{p.Handover.SourceBinary, p.Handover.Profile, p.Handover.Custody} {
		if !filepath.IsAbs(ref.Path) || len(ref.SHA256) != 64 {
			return errors.New("gateway handover source reference is incomplete")
		}
	}
	if p.Handover.Cold != nil {
		if err := validateColdPlanShape(p); err != nil {
			return err
		}
	}
	for _, cp := range p.Contexts {
		if len(cp.Blockers) != 0 || cp.RequestFile == "" || cp.RecoveryRequired {
			return errors.New("gateway handover context is incomplete")
		}
	}
	return nil
}

func verifyGatewayHandoverSources(ctx context.Context, p hostMaintenancePlan) error {
	if err := verifyRollingTarget(ctx, p.Target, p.TargetSHA); err != nil {
		return err
	}
	if err := verifyGatewayHandoverBinary(p.Handover.SourceBinary); err != nil {
		return err
	}
	for _, ref := range []loopruntime.SessionReadbackFileRef{p.Handover.Profile, p.Handover.Custody} {
		current, err := loopruntime.ReadSessionReadbackFile(ref.Path, nil)
		if err != nil || current != ref {
			return errors.New("gateway handover source changed")
		}
	}
	for name, digest := range p.Files {
		raw, err := os.ReadFile(filepath.Join(p.Root, name))
		if digest == "absent" && os.IsNotExist(err) {
			continue
		}
		if err != nil || hostDigest(raw) != digest {
			return errors.New("gateway handover original Host changed")
		}
	}
	for _, cp := range p.Contexts {
		for name, digest := range cp.Files {
			path := name
			if !filepath.IsAbs(path) {
				path = filepath.Join(cp.Original.ProjectDir, ".noodle", name)
			}
			raw, err := os.ReadFile(path)
			if digest == "absent" && os.IsNotExist(err) {
				continue
			}
			if err != nil || hostDigest(raw) != digest {
				return fmt.Errorf("gateway handover original context changed: %s", path)
			}
		}
	}
	if p.Handover.Cold != nil && p.Handover.Cold.Absent != nil {
		if err := verifyColdAbsentSource(p); err != nil {
			return err
		}
	} else {

		native, err := procx.ObserveNativeIdentity(p.Source.PID)
		if err != nil || !p.Native.SameGeneration(native) || native.Zombie {
			return errors.New("gateway handover original process changed")
		}
		if _, err = legacyProcessIdentity(ctx, p.Source.PID, p.Handover.SourceBinary.Path); err != nil {
			return err
		}
	}
	schema := 2
	if p.Handover.Cold != nil {
		schema = 3
	}
	instance, err := handoverSharedReadback(ctx, p.Handover.SharedRoot, schema)
	if err != nil {
		return err
	}
	if instance != p.Handover.SharedInstance {
		return errors.New("gateway handover shared generation changed")
	}
	return nil
}

func runGatewayHandover(ctx context.Context, p hostMaintenancePlan, dir string, out *json.Encoder) error {
	lock, err := lockfile.TryLock(filepath.Join(p.Root, "maintenance.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	sharedLock, err := lockfile.TryLock(filepath.Join(p.Handover.SharedRoot, "maintenance.lock"))
	if err != nil {
		return err
	}
	defer sharedLock.Close()
	if p.Handover.Cold != nil && p.Handover.Cold.Absent != nil {
		fence, err := lockfile.TryLock(filepath.Join(p.Root, "host.lock"))
		if err != nil {
			return err
		}
		defer fence.Close()
	}
	j, err := readRestrictedMaintenance(dir)
	if os.IsNotExist(err) {
		if err = verifyGatewayHandoverSources(ctx, p); err != nil {
			return err
		}
		if err = os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		for name, digest := range p.Files {
			if digest == "absent" {
				continue
			}
			raw, readErr := os.ReadFile(filepath.Join(p.Root, name))
			if readErr != nil {
				return readErr
			}
			if err = saveHostStorageBytes(filepath.Join(dir, "original-"+filepath.Base(name)), raw); err != nil {
				return err
			}
		}
		for _, item := range []struct {
			Name string
			Ref  loopruntime.SessionReadbackFileRef
		}{{"original-custody.json", p.Handover.Custody}, {"plan.json", loopruntime.SessionReadbackFileRef{}}} {
			var raw []byte
			if item.Name == "plan.json" {
				raw, err = json.Marshal(p)
			} else {
				raw, err = os.ReadFile(item.Ref.Path)
			}
			if err != nil {
				return err
			}
			if err = saveHostStorageBytes(filepath.Join(dir, item.Name), raw); err != nil {
				return err
			}
		}
		j = restrictedMaintenance{Schema: 1, Plan: p, Phase: "handover-prepared"}
		for _, cp := range p.Contexts {
			j.Tasks = append(j.Tasks, restrictedTask{Plan: cp, Phase: "prepared"})
		}
		if err = saveRestrictedMaintenance(dir, j); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if j.Plan.PlanSHA != p.PlanSHA {
		return errors.New("gateway handover journal belongs to another plan")
	}
	if j.Phase == "handover-completed" {
		return out.Encode(j)
	}
	backing := filepath.Join(p.Root, "g", p.Source.Instance)
	gate, err := openHostMaintenanceGate(p.Root, backing)
	if err != nil {
		return err
	}
	defer gate.Close()
	if j.Phase == "handover-prepared" {
		if err = verifyGatewayHandoverSources(ctx, p); err != nil {
			return err
		}
		if err = os.MkdirAll(backing, 0700); err != nil {
			return err
		}
		j.Phase = "handover-fencing"
		if err = saveRestrictedMaintenance(dir, j); err != nil {
			return err
		}
	}
	if j.Phase == "handover-fencing" && p.Handover.Cold != nil && p.Handover.Cold.Absent != nil {
		if err := fenceColdAbsentSource(p, backing); err != nil {
			return err
		}
		if err := publishMaintenanceSocket(p.Root); err != nil {
			return err
		}
		j.Phase = "handover-cleanup"
		if err := saveRestrictedMaintenance(dir, j); err != nil {
			return err
		}
	}
	if j.Phase == "handover-fencing" {
		if _, statErr := os.Lstat(filepath.Join(backing, "host.sock")); os.IsNotExist(statErr) {
			native, observeErr := procx.ObserveNativeIdentity(p.Source.PID)
			if observeErr != nil || !p.Native.SameGeneration(native) {
				return errors.New("original owner changed before socket fence")
			}
			current, readErr := readLegacyResources(ctx, p.Root)
			if readErr != nil || current.Instance != p.Source.Instance || current.PID != p.Source.PID {
				return errors.New("original public socket outcome is unknown; no rename repeated")
			}
			info, readErr := os.Lstat(filepath.Join(p.Root, "host.sock"))
			if readErr != nil || info.Mode()&os.ModeSocket == 0 {
				return errors.New("original public socket is no longer the native listener")
			}
			identity, readErr := legacyProcessIdentity(ctx, p.Source.PID, p.Handover.SourceBinary.Path)
			if readErr != nil {
				return readErr
			}
			marker := hostBootstrap{Schema: 1, Phase: "gateway-handover", Target: p.Target, TargetSHA: p.TargetSHA, Source: hostGeneration{ID: p.Source.Instance, Directory: backing, Binary: p.Handover.SourceBinary.Path, SHA: p.Handover.SourceBinary.SHA256}, ProcessIdentity: identity}
			if err = saveHostBootstrap(p.Root, marker); err != nil {
				return err
			}
			if err = os.Rename(filepath.Join(p.Root, "host.sock"), filepath.Join(backing, "host.sock")); err != nil {
				return err
			}
			if err = filex.SyncDir(backing); err != nil {
				return err
			}
		} else if statErr != nil {
			return statErr
		}
		r, readErr := readLegacyResources(ctx, backing)
		if readErr != nil {
			return readErr
		}
		if r.Instance != p.Source.Instance || r.PID != p.Source.PID {
			return errors.New("gateway handover fenced owner differs")
		}
		if err = publishMaintenanceSocket(p.Root); err != nil {
			return err
		}
		j.Phase = "handover-cleanup"
		if err = saveRestrictedMaintenance(dir, j); err != nil {
			return err
		}
	}
	if p.Handover.Cold != nil && (j.Phase == "handover-cleanup" || j.Phase == "handover-cold-retiring" || j.Phase == "handover-cold-releasing") {
		gate.freeze()
		return runColdGatewayHandover(ctx, p, dir, backing, &j, out)
	}
	if j.Phase == "handover-cleanup" {
		for i := range j.Tasks {
			task := &j.Tasks[i]
			if task.Phase == "detached" {
				continue
			}
			req := bootstrapTaskRequest(hostBootstrapTask{Original: task.Plan.Original})
			req.Operation = "cleanup"
			if task.Phase == "prepared" {
				raw, readErr := nativeHostRPC(ctx, backing, req)
				var observed hostResponse
				if readErr != nil || json.Unmarshal(raw, &observed) != nil || observed.Status != "eligible" || !hostReceiptMatches(req, observed.hostReceipt) {
					return fmt.Errorf("original cleanup not confirmed: status=%q error=%q readback=%v; no control sent", observed.Status, observed.Error, readErr)
				}
				task.Phase = "cleanup-intent"
				if err = saveRestrictedMaintenance(dir, j); err != nil {
					return err
				}
				req.Apply = true
				if _, err = nativeHostRPC(ctx, backing, req); err != nil {
					return err
				}
			}
			req.Operation = "status"
			req.Apply = false
			raw, readErr := nativeHostRPC(ctx, backing, req)
			var observed hostResponse
			if readErr != nil || json.Unmarshal(raw, &observed) != nil || observed.Status != "ok" || !hostReceiptMatches(req, observed.hostReceipt) || observed.ContextState != "detached" || observed.TerminalCause != loop.TerminalExplicitCleanup {
				return errors.New("original cleanup effect is unconfirmed; readback only, no retry")
			}
			task.Phase = "detached"
			task.Detached = &observed.hostReceipt
			if err = saveRestrictedMaintenance(dir, j); err != nil {
				return err
			}
		}
		gate.freeze()
		raw, readErr := nativeHostRPC(ctx, backing, hostRequest{Operation: "resources"})
		if readErr != nil {
			return readErr
		}
		var r legacyHostResources
		if json.Unmarshal(raw, &r) != nil || r.Instance != p.Source.Instance || r.PID != p.Source.PID || r.Contexts != 0 || r.Resources.Workers != 0 || r.Resources.Residents != 0 || r.Resources.Allocations != 0 || r.Resources.Unresolved != 0 || r.Residents.Error != nil {
			return errors.New("original Host has active or unknown resources; custody handover refused")
		}
		if err = saveHostStorageBytes(filepath.Join(dir, "empty-resources.json"), raw); err != nil {
			return err
		}
		j.Phase = "handover-releasing"
		if err = saveRestrictedMaintenance(dir, j); err != nil {
			return err
		}
	}
	// Resume an older stop intent by readback only; new handovers send no signal.
	if j.Phase == "handover-stopping" {
		_, readErr := procx.ObserveNativeIdentity(p.Source.PID)
		if readErr == nil {
			return out.Encode(j)
		}
		if !errors.Is(readErr, procx.ErrProcessGone) {
			return errors.New("original Host exit remains unknown; shutdown was not repeated")
		}
		absent, groupErr := procx.ProcessGroupAbsent(p.Native.Group)
		if groupErr != nil || !absent {
			return errors.New("original Host process group exit remains unknown")
		}
		j.Phase = "handover-stopped"
		if err = saveRestrictedMaintenance(dir, j); err != nil {
			return err
		}
	}
	if j.Phase == "handover-releasing" || j.Phase == "handover-stopped" {
		planRef, readErr := loopruntime.ReadSessionReadbackFile(filepath.Join(dir, "plan.json"), nil)
		if readErr != nil {
			return readErr
		}
		emptyRef, readErr := loopruntime.ReadSessionReadbackFile(filepath.Join(dir, "empty-resources.json"), nil)
		if readErr != nil {
			return readErr
		}
		schema := 2
		if j.Phase == "handover-stopped" {
			schema = 1
		}
		release := loopruntime.OpenShellHostRelease{Schema: schema, SourceRoot: p.Root, SourceInstance: p.Source.Instance, ControlRoot: p.Handover.SharedRoot, Process: *p.Native, Plan: planRef, EmptyResources: emptyRef}
		raw, marshalErr := json.Marshal(release)
		if marshalErr != nil {
			return marshalErr
		}
		if err = saveHostStorageBytes(filepath.Join(dir, "release.json"), raw); err != nil {
			return err
		}
		releaseRef, readErr := loopruntime.ReadSessionReadbackFile(filepath.Join(dir, "release.json"), nil)
		if readErr != nil {
			return readErr
		}
		if _, statErr := os.Lstat(filepath.Join(dir, "released-custody.json")); os.IsNotExist(statErr) {
			settled, readErr := os.ReadFile(filepath.Join(p.Handover.Binding.StateRoot, "owner.json"))
			if readErr != nil {
				return readErr
			}
			if err = saveHostStorageBytes(filepath.Join(dir, "released-custody.json"), settled); err != nil {
				return err
			}
		} else if statErr != nil {
			return statErr
		}
		before, readErr := loopruntime.ReadSessionReadbackFile(filepath.Join(dir, "released-custody.json"), nil)
		if readErr != nil {
			return readErr
		}
		if _, err = loopruntime.UpgradeOpenShellCustodyV3(p.Handover.Binding, p.Handover.SharedRoot, before, releaseRef, filepath.Join(dir, "custody-handover.json")); err != nil {
			return err
		}
		j.Phase = "handover-completed"
		if err = saveRestrictedMaintenance(dir, j); err != nil {
			return err
		}
	}
	return out.Encode(j)
}
