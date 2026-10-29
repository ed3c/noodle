package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/internal/reducer"
)

// Preparation is read-only, including on refusal. This plan is evidence for
// review, never an authorization token or a replacement for an original request.
type hostMaintenancePlan struct {
	Schema       int                          `json:"schema"`
	Root         string                       `json:"host_dir"`
	Target       string                       `json:"target"`
	TargetSHA    string                       `json:"target_sha256"`
	Policy       string                       `json:"worker_admission_policy"`
	Source       legacyHostResources          `json:"native_source"`
	Native       *procx.NativeIdentity        `json:"process_generation,omitempty"`
	Files        map[string]string            `json:"source_sha256"`
	Contexts     []hostMaintenanceContextPlan `json:"contexts"`
	Blockers     []string                     `json:"blockers"`
	Observations []string                     `json:"observations"`
	Steps        []string                     `json:"proposed_steps"`
	Effects      []string                     `json:"effects"`
	Executable   bool                         `json:"executable"`
	PlanSHA      string                       `json:"plan_sha256"`
	Handover     *hostGatewayHandoverPlan     `json:"handover,omitempty"`
}

type hostMaintenanceContextPlan struct {
	Original         hostReceipt       `json:"original"`
	RequestFile      string            `json:"request_file,omitempty"`
	ConfigSource     string            `json:"config_source,omitempty"`
	RecoveryRequired bool              `json:"native_checkpoint_recovery"`
	EffectiveMode    string            `json:"effective_mode,omitempty"`
	DispatchState    string            `json:"dispatch_state,omitempty"`
	Files            map[string]string `json:"source_sha256"`
	Blockers         []string          `json:"blockers"`
}

func existingHostDirectory() (string, error) {
	dir := os.Getenv("NOODLE_HOST_DIR")
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(cache, "noodle", "host")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("original Host directory is not a real directory")
	}
	return dir, nil
}

func planHostMaintenance(ctx context.Context, root, target, digest string, requestFiles []string) (hostMaintenancePlan, error) {
	p := hostMaintenancePlan{Schema: 1, Root: root, Target: target, TargetSHA: digest,
		Policy: admission.WorkerAdmissionPolicy, Files: map[string]string{},
		Contexts: []hostMaintenanceContextPlan{}, Blockers: []string{}, Observations: []string{}, Effects: []string{},
		Steps: []string{
			"Confirm a source-bound plan; this read-only result grants no control authority",
			"Preserve old maintenance, pool, request and quota bytes; do not replay the stale bootstrap",
			"Fence new admission through the original Host entry, then pause each verified original context once",
			"Read each original pause ACK and durable checkpoint; drain existing work without business dispatch",
			"Detach each context once through its original owner; uncertain responses permit status readback only",
			"Confirm zero owned resources and retire the exact original Host generation through normal shutdown",
			"After all old quota writers are proved retired, migrate an unchanged empty quota ledger under its lock",
			"Import original requests as held; do not restore business dispatch or rebuild missing inputs",
			"Read back the new Host policy and 15,000,000,000-byte ceiling before publishing the selected entry",
		},
	}
	if !filepath.IsAbs(root) || !filepath.IsAbs(target) {
		return p, errors.New("maintenance plan requires absolute original Host and target paths")
	}
	if err := verifyRollingTarget(ctx, target, digest); err != nil {
		return p, err
	}
	capRaw, err := exec.CommandContext(ctx, target, "host", "capabilities").Output()
	if err != nil {
		return p, err
	}
	var cap struct {
		Policy string `json:"worker_admission_policy"`
	}
	if err = json.Unmarshal(capRaw, &cap); err != nil || cap.Policy != p.Policy {
		return p, errors.New("target does not expose the selected worker admission policy")
	}
	r, err := readLegacyResources(ctx, root)
	if err != nil {
		return p, err
	}
	p.Source = r
	if r.Resources.Workers != 0 || r.Resources.Residents != 0 || r.Resources.Allocations != 0 || r.Resources.Unresolved != 0 || r.Residents.Error != nil {
		p.Blockers = append(p.Blockers, "original owner has active or unresolved resources; only readback is currently eligible")
	}
	for _, name := range []string{"bootstrap.json", "pool.json", "quota/quota.json", "receipts.json"} {
		raw, readErr := os.ReadFile(filepath.Join(root, name))
		if readErr != nil {
			p.Blockers = append(p.Blockers, fmt.Sprintf("%s: %v", name, readErr))
			continue
		}
		p.Files[name] = hostDigest(raw)
	}
	// The old single bootstrap cannot accept a second source/target epoch.
	// Preserve it; a different source is not permission to overwrite its history.
	if old, readErr := readHostBootstrap(root); readErr == nil {
		if old.Phase != "active" {
			p.Blockers = append(p.Blockers, "original bootstrap is unresolved; readback only")
		} else {
			p.Observations = append(p.Observations, "old bootstrap will be preserved byte-for-byte in a separate maintenance epoch")
		}
	} else if !os.IsNotExist(readErr) {
		p.Blockers = append(p.Blockers, "old bootstrap cannot be validated: "+readErr.Error())
	}
	if pool, readErr := readHostPool(root); readErr == nil {
		p.Observations = append(p.Observations, fmt.Sprintf("preserve %d old owners held and %d old version requests without replay", len(pool.Owners), len(pool.Queue)))
		for id, g := range pool.Generations {
			absent, absenceErr := g.Process.GroupAbsent()
			if absenceErr != nil || !absent {
				p.Blockers = append(p.Blockers, "old quota writer retirement unconfirmed: "+id)
			}
		}
	} else if !os.IsNotExist(readErr) {
		p.Blockers = append(p.Blockers, "old pool cannot be validated: "+readErr.Error())
	}
	if raw, readErr := os.ReadFile(filepath.Join(root, "quota", "quota.json")); readErr == nil {
		var q struct {
			Schema    int                        `json:"schema"`
			Maximum   int                        `json:"maximum"`
			SwapLimit uint64                     `json:"swap_used_limit_bytes"`
			Leases    map[string]json.RawMessage `json:"leases"`
		}
		if json.Unmarshal(raw, &q) != nil || q.Schema != 1 || q.Maximum <= 0 || q.SwapLimit != admission.DefaultSwapUsedLimit || q.Leases == nil || len(q.Leases) != 0 {
			p.Blockers = append(p.Blockers, "quota is not a verified empty original schema-1 ledger with the selected ceiling")
		}
	}
	raw, err := os.ReadFile(filepath.Join(root, "receipts.json"))
	if err != nil {
		return p, err
	}
	var receipts []hostReceipt
	if err = json.Unmarshal(raw, &receipts); err != nil {
		return p, err
	}
	seen := map[string]bool{}
	for _, receipt := range receipts {
		if receipt.HostInstanceID != r.Instance || receipt.ContextState == "detached" {
			continue
		}
		if receipt.HostPID != r.PID || seen[receipt.ContextID] || !validPoolID(receipt.ContextID) || receipt.ContextState != "active" {
			return p, errors.New("original active receipt identities are inconsistent")
		}
		seen[receipt.ContextID] = true
		if err = verifyLegacyBootstrapBinary(receipt.BinaryPath, receipt.BinarySHA256); err != nil {
			return p, err
		}
		if _, err = legacyProcessIdentity(ctx, r.PID, receipt.BinaryPath); err != nil {
			return p, err
		}
		native, nativeErr := procx.ObserveNativeIdentity(r.PID)
		if nativeErr != nil || native.Zombie || !native.SameGeneration(native) || !procx.NativeFixtureControlAvailable() {
			p.Blockers = append(p.Blockers, "kernel-bound original owner generation unavailable")
		} else if p.Native != nil && !p.Native.SameGeneration(native) {
			return p, errors.New("original Host generation changed during plan readback")
		} else {
			p.Native = &native
		}
		cp := planHostMaintenanceContext(root, receipt, requestFiles)
		req := bootstrapTaskRequest(hostBootstrapTask{Original: receipt})
		req.Operation = "status"
		observedRaw, statusErr := nativeHostRPC(ctx, root, req)
		var observed hostResponse
		if statusErr != nil || json.Unmarshal(observedRaw, &observed) != nil || observed.Status != "ok" || observed.hostReceipt != receipt {
			cp.Blockers = append(cp.Blockers, "original context does not match live owner status; readback only")
		}
		p.Contexts = append(p.Contexts, cp)
	}
	if len(p.Contexts) != r.Contexts || len(p.Contexts) == 0 {
		p.Blockers = append(p.Blockers, "original live context count differs from retained receipt evidence")
	}
	for name, digest := range p.Files {
		data, readErr := os.ReadFile(filepath.Join(root, name))
		if readErr != nil || hostDigest(data) != digest {
			p.Blockers = append(p.Blockers, "source changed during plan readback: "+name)
		}
	}
	sort.Slice(p.Contexts, func(i, j int) bool { return p.Contexts[i].Original.ContextID < p.Contexts[j].Original.ContextID })
	p.Executable = len(p.Blockers) == 0
	for _, cp := range p.Contexts {
		if len(cp.Blockers) != 0 {
			p.Executable = false
		}
	}
	sort.Strings(p.Blockers)
	encoded, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	p.PlanSHA = hostDigest(encoded)
	return p, nil
}

func planHostMaintenanceContext(root string, receipt hostReceipt, paths []string) hostMaintenanceContextPlan {
	p := planHostMaintenanceContextInputs(root, receipt, paths)
	dir := filepath.Join(receipt.ProjectDir, ".noodle")
	for _, name := range []string{"status.json", "state.snapshot.json", "control.ndjson", "control-ack.ndjson", "orders-next.json", "noodle.lock"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			if os.IsNotExist(err) {
				p.Files[name] = "absent"
			}
			if os.IsNotExist(err) && (name == "status.json" || name == "state.snapshot.json") && p.RequestFile != "" && p.ConfigSource != "" {
				p.RecoveryRequired = true
				p.Files[name] = "absent"
			} else if name == "status.json" || name == "state.snapshot.json" || !os.IsNotExist(err) {
				p.Blockers = append(p.Blockers, name+": "+err.Error())
			}
			continue
		}
		p.Files[name] = hostDigest(data)
		if name == "status.json" {
			mode, dispatch, statusErr := readMaintenanceStatus(data)
			if statusErr != nil {
				p.Blockers = append(p.Blockers, statusErr.Error())
			} else {
				p.EffectiveMode, p.DispatchState = mode, dispatch
			}
		}
		if (name == "control.ndjson" || name == "orders-next.json") && len(data) != 0 {
			p.Blockers = append(p.Blockers, name+": pending original input; read back its owner first")
		}
	}
	snapshot, err := reducer.ReadSnapshot(filepath.Join(dir, "state.snapshot.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		p.Blockers = append(p.Blockers, "canonical checkpoint cannot be read: "+err.Error())
	} else if err == nil && (snapshot.EffectLedger == nil || snapshot.GeneratedAt.IsZero()) {
		p.Blockers = append(p.Blockers, "canonical checkpoint is incomplete")
	}
	sort.Strings(p.Blockers)
	return p
}

func readMaintenanceStatus(raw []byte) (string, string, error) {
	var status struct {
		Mode     string `json:"effective_mode"`
		Override string `json:"mode_override"`
		Dispatch string `json:"loop_state"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return "", "", err
	}
	if (status.Mode != "manual" && status.Mode != "supervised" && status.Mode != "auto") || (status.Override != "" && status.Override != status.Mode) {
		return "", "", errors.New("original effective mode is missing or conflicting")
	}
	switch status.Dispatch {
	case "running", "paused", "draining", "idle":
		return status.Mode, status.Dispatch, nil
	default:
		return "", "", errors.New("original dispatch state is missing or invalid")
	}
}

// Resource retirement preserves original request/config bindings without importing or resuming task checkpoints.
func planHostMaintenanceContextInputs(root string, receipt hostReceipt, paths []string) hostMaintenanceContextPlan {
	p := hostMaintenanceContextPlan{Original: receipt, Files: map[string]string{}, Blockers: []string{}}
	if receipt.ConfigPath != "" {
		paths = append(append([]string{}, paths...), filepath.Join(filepath.Dir(receipt.ConfigPath), "host-request.json"))
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			p.Blockers = append(p.Blockers, fmt.Sprintf("original request %s: %v", path, err))
			continue
		}
		var b hostRequestBinding
		if err = json.Unmarshal(raw, &b); err != nil {
			p.Blockers = append(p.Blockers, fmt.Sprintf("original request %s: %v", path, err))
			continue
		}
		if b.RequestID != receipt.RequestID {
			continue
		}
		req := hostRequest{ProjectDir: b.ProjectDir, ConfigPath: b.ConfigPath, ConfigSHA256: b.ConfigSHA256, Mode: b.Mode, RequestID: b.RequestID, CleanupCompleted: b.CleanupCompleted}
		if b.Schema != 1 || b.Status != "accepted" || b.Operation != "attach" || b.HostDir != root || b.BinarySHA256 != receipt.BinarySHA256 || b.EnvironmentSHA256 == "" || !hostReceiptMatches(req, receipt) {
			p.Blockers = append(p.Blockers, "original accepted request conflicts with native context")
			continue
		}
		if p.RequestFile != "" && p.Files[p.RequestFile] != hostDigest(raw) {
			p.Blockers = append(p.Blockers, "conflicting original accepted request bytes")
			continue
		}
		p.RequestFile = path
		p.Files[path] = hostDigest(raw)
	}
	if p.RequestFile == "" {
		p.Blockers = append(p.Blockers, "original accepted request envelope unavailable; do not reconstruct from a receipt")
	}
	configPath := receipt.ConfigPath
	if configPath == "" {
		configPath = filepath.Join(receipt.ProjectDir, ".noodle.toml")
	}
	if data, err := os.ReadFile(configPath); err == nil && hostDigest(data) == receipt.ConfigSHA256 {
		p.ConfigSource = configPath
	} else if p.RequestFile != "" {
		// The accepted envelope remains unchanged. This is only an independently
		// verified copy for custody, never a replacement execution binding.
		path := filepath.Join(filepath.Dir(p.RequestFile), "noodle.toml")
		if data, err := os.ReadFile(path); err == nil && hostDigest(data) == receipt.ConfigSHA256 {
			p.ConfigSource = path
		}
	}
	if p.ConfigSource == "" {
		p.Blockers = append(p.Blockers, "original config bytes unavailable or changed; import must remain held")
	} else {
		p.Files[p.ConfigSource] = receipt.ConfigSHA256
	}
	return p
}
