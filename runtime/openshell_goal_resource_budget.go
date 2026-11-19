package runtime

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"

	"github.com/poteto/noodle/internal/admission"
)

const ConservativeGoalResourceBudget = "conservative-v1"
const QualificationGoalResourceBudget = "qualification-small-no-deps-v1"
const qualificationImage = "nvcr.io/nvidia/base/ubuntu@sha256:7e0f726d635d440d28a7f18e6498d3bdb76c7038bd4c90a090c21eb7c9f40686"

// Physical capacity and native component geometry are facts. Extra allowances
// are an explicitly selected operator policy, not RSS/maxseen or a hard cap.
func conservativeGoalResourceBudget(p OpenShellProfile, hardware admission.Sample, class string) (map[string]any, error) {
	if hardware.Unsupported || hardware.SwapUsedBytes > math.MaxInt64 {
		return nil, errors.New("native Swap sample unavailable")
	}
	if class != ConservativeGoalResourceBudget && class != QualificationGoalResourceBudget {
		return nil, errors.New("unsupported Goal resource budget class")
	}
	var cfg openShellResourceConfig
	if err := readOpenShellResourcePolicy(p, &cfg); err != nil {
		return nil, err
	}
	if p.Gateway.ComputeDriver != "vm" {
		return nil, errors.New("conservative class requires native VM configuration")
	}
	// OpenShell719 VmDriverConfig defaults, driver.rs:114-116. New qualification
	// explicitly selects its smaller geometry; old profile bytes are untouched.
	memory, cpus, overlay := int64(2048), int64(2), int64(4096)
	vm := cfg.OpenShell.Drivers.VM
	if vm.MemoryMiB != nil {
		memory = *vm.MemoryMiB
	}
	if vm.CPUs != nil {
		cpus = *vm.CPUs
	}
	if vm.OverlayMiB != nil {
		overlay = *vm.OverlayMiB
	}
	checkout, dependencies, returned, restore, archive := int64(1024), int64(4096), int64(2048), int64(2048), int64(8192)
	prepMemory, prepDisk, residentMemory, residentDisk := int64(1024), int64(4096), int64(1024), int64(1024)
	image := "operator reviewed images within declared cold-cache allowance"
	workload := "reviewed coding tasks: checkout<=1GiB, dependencies<=4GiB, return/restore<=2GiB each, archive/CAS writes<=8GiB"
	if class == QualificationGoalResourceBudget {
		if goruntime.GOARCH != "arm64" {
			return nil, errors.New("qualification cold-image Source only covers arm64")
		}
		memory, cpus, overlay = 2048, 2, 1024
		checkout, dependencies, returned, restore, archive = 64, 0, 128, 256, 256
		prepDisk, residentMemory, residentDisk = 512, 512, 256
		image = qualificationImage
		workload = "qualification only: reviewed repository<=64MiB; no dependency installation; return<=128MiB; restore<=256MiB; archive/CAS<=256MiB; immutable payload<=768MiB"
		if len(p.Routes) == 0 {
			return nil, errors.New("qualification routes absent")
		}
		for _, route := range p.Routes {
			if route.Spec.GetTemplate().GetImage() != qualificationImage || len(route.Spec.GetTemplate().GetDriverConfig().GetFields()) != 0 {
				return nil, errors.New("qualification requires original pinned image and no foreign driver configuration")
			}
		}
		var payload uint64
		if err := filepath.WalkDir(p.PayloadDirectory, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type().IsRegular() {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				payload += uint64(info.Size())
				if payload > 768<<20 {
					return errors.New("qualification immutable payload exceeds declared geometry")
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	if memory <= 0 || memory > 1<<30 || cpus <= 0 || cpus > 255 || overlay <= 0 || overlay > 1<<40 {
		return nil, errors.New("native guest component outside conservative class")
	}
	memoryMiB := int64(hardware.TotalMemoryBytes >> 20)
	memoryBudget := memoryMiB / 4 * 3                 // legacy physical RAM metadata keeps existing publications readable
	cpuBudget := int64(max(hardware.CPUs, 0) / 4 * 3) // legacy count metadata only
	baselineMiB := int64(hardware.SwapUsedBytes >> 20)
	if hardware.SwapUsedBytes%(1<<20) != 0 {
		baselineMiB++
	}
	swapLimit := max(memoryMiB/4, baselineMiB+1024)
	if memoryMiB == 0 {
		swapLimit = baselineMiB + 3072
	}
	// Native719 driver.rs:4051,7427 uses max(overlay,4*OCI-layout+1GiB).
	// The pinned ARM64 manifest f659db60... has 31,340,110 payload bytes; 32MiB
	// includes descriptors/layout metadata. Bootstrap1GiB and staging1GiB are
	// conservative policy (original observed bootstrap logical size512MiB).
	prepared := max(overlay, int64(1152))
	coldCache := int64(1024) + prepared + overlay + 1024
	retention := archive + coldCache
	peak := func(memory, disk int64) map[string]any {
		return map[string]any{"schema": int64(1), "memory_peak_mib": memory, "cpus": int64(1), "disk_peak_mib": disk}
	}
	return map[string]any{
		"noodle_worker_resources": map[string]any{"schema": int64(1), "memory_peak_mib": memory + 2048, "disk_peak_mib": overlay + checkout + dependencies + returned + restore + retention,
			"checkout_peak_mib": checkout, "dependency_peak_mib": dependencies, "return_tar_peak_mib": returned, "restore_peak_mib": restore, "retention_peak_mib": retention,
			"memory_budget_mib": memoryBudget, "cpu_budget": cpuBudget, "disk_safety_mib": int64(2048), "max_swap_used_mib": swapLimit, "max_swapout_bytes_per_second": int64(1 << 20), "recovery_samples": int64(2), "require_io_observation": false},
		"noodle_preparation_resources": peak(prepMemory, prepDisk), "noodle_resident_resources": peak(residentMemory, residentDisk),
		"noodle_resource_budget": map[string]any{"schema": int64(1), "class": class, "kind": "operator_reservation_not_hard_limit", "hardware_memory_mib": memoryMiB, "hardware_cpus": int64(hardware.CPUs), "initial_swap_used_bytes": int64(hardware.SwapUsedBytes),
			"guest_memory_mib": memory, "guest_vcpus": cpus, "guest_overlay_mib": overlay, "image": image, "cold_native_cache_peak_mib": coldCache, "cold_cache_plan": "bootstrap<=1GiB + max(overlay,1152MiB) prepared rootfs + full overlay template + 1GiB staging; no clone credit",
			"return_budget_source":    "original A1 session tar83312KiB + result12640KiB observation; qualification return128MiB/restore256MiB are conservative policy, not implied by repo size",
			"native_component_source": "OpenShell719 configured guest/defaults; pinned image manifest; original bootstrap logical512MiB observation; native hardware sampler",
			"extra_budget_source":     "operator-selected conservative policy; not RSS, du, maxseen or mathematical peak", "workload": workload,
			"lifetime":  "preparation through original worker cleanup; resident until original exit and group absence; already materialized retained data counted by physical free space",
			"retention": "keep failures/Frozen/unknown/pinned; native image caches stay materialized; no unlimited retention guarantee; insufficient space queues",
			"limits":    "programs and append-only logs are not hard capped; applicability excess requires original Owner review; unknown never reclaimed as completed"},
	}, nil
}

func goalResourceBudgetWithMachinePolicy(p OpenShellProfile, hardware admission.Sample, class string, policy *admission.MachineCapacityPolicyReadback) (map[string]any, error) {
	budget, err := conservativeGoalResourceBudget(p, hardware, class)
	if err != nil || policy == nil {
		return budget, err
	}
	fixed := policy.Capacity
	if !filepath.IsAbs(policy.Source) || filepath.Clean(policy.Source) != policy.Source || fixed.DiskSafetyBytes == 0 || fixed.RecoverySamples < 2 || fixed.RecoverySamples > admission.MaxRecoverySamples {
		return nil, errors.New("original machine capacity policy invalid")
	}
	for _, bytes := range []uint64{fixed.MemoryBytes, fixed.DiskSafetyBytes, fixed.MaxSwapUsedBytes} {
		if bytes > math.MaxInt64 || bytes%(1<<20) != 0 {
			return nil, errors.New("original machine capacity policy cannot be represented exactly in MiB")
		}
	}
	if fixed.CPUs > math.MaxInt64 || fixed.MaxSwapoutBytesPerSecond > math.MaxInt64 {
		return nil, errors.New("original machine capacity policy overflows resource publication")
	}
	worker := budget["noodle_worker_resources"].(map[string]any)
	worker["memory_budget_mib"] = int64(fixed.MemoryBytes >> 20)
	worker["cpu_budget"] = int64(fixed.CPUs)
	worker["disk_safety_mib"] = int64(fixed.DiskSafetyBytes >> 20)
	worker["max_swap_used_mib"] = int64(fixed.MaxSwapUsedBytes >> 20)
	worker["max_swapout_bytes_per_second"] = int64(fixed.MaxSwapoutBytesPerSecond)
	worker["recovery_samples"] = int64(fixed.RecoverySamples)
	worker["require_io_observation"] = fixed.RequireIOObservation
	// Keep the sampled stock truthful; the hash-bound snapshot, not that stock,
	// explains the inherited ceiling when later publications verify themselves.
	budget["noodle_resource_budget"].(map[string]any)["machine_policy"] = map[string]any{
		"source": policy.Source,
		"capacity": map[string]any{
			"memory_bytes": int64(fixed.MemoryBytes), "cpus": int64(fixed.CPUs), "disk_safety_bytes": int64(fixed.DiskSafetyBytes),
			"max_swap_used_bytes": int64(fixed.MaxSwapUsedBytes), "max_swapout_bytes_per_second": int64(fixed.MaxSwapoutBytesPerSecond),
			"recovery_samples": int64(fixed.RecoverySamples), "require_io_observation": fixed.RequireIOObservation,
		},
	}
	return budget, nil
}

func markCPUObservationOnly(budget map[string]any) {
	metadata := budget["noodle_resource_budget"].(map[string]any)
	metadata["cpu_policy"] = "observation_only"
	metadata["cpu_count_role"] = "hardware_and_guest_configuration_and_legacy_policy_not_busy_percent_or_admission_capacity"
}

// GoalResourceBudgetBaseline reads the original hash-bound Swap baseline.
func GoalResourceBudgetBaseline(p OpenShellProfile, original OpenShellProfile, hardware admission.Sample) (uint64, bool, error) {
	saved, ok, err := ReadGoalResourceBudgetHardware(p, original)
	if ok && saved.TotalMemoryBytes>>20 != hardware.TotalMemoryBytes>>20 {
		return 0, false, nil
	}
	return saved.SwapUsedBytes, ok, err
}

// ReadGoalResourceBudgetHardware verifies the original publication and returns
// its saved hardware counts and Swap baseline for reentry.
func ReadGoalResourceBudgetHardware(p OpenShellProfile, original OpenShellProfile) (admission.Sample, bool, error) {
	var saved admission.Sample
	var cfg struct {
		Budget struct {
			Class              string
			Memory             int64                                    `toml:"hardware_memory_mib"`
			CPUs               int64                                    `toml:"hardware_cpus"`
			Swap               int64                                    `toml:"initial_swap_used_bytes"`
			MachinePolicy      *admission.MachineCapacityPolicyReadback `toml:"machine_policy"`
			CPUObservationOnly string                                   `toml:"cpu_policy"`
		} `toml:"noodle_resource_budget"`
	}
	if err := readOpenShellResourcePolicy(p, &cfg); err != nil {
		return saved, false, err
	}
	if cfg.Budget.Class == "" {
		return saved, false, nil
	}
	if cfg.Budget.Class != ConservativeGoalResourceBudget && cfg.Budget.Class != QualificationGoalResourceBudget {
		return saved, false, errors.New("original resource budget class unsupported")
	}
	receipt, err := ReadGoalOpenShellProfile(filepath.Dir(filepath.Dir(p.SourcePath)))
	if err != nil {
		return saved, false, err
	}
	if receipt.Template.Path != original.SourcePath || receipt.TemplatePolicySHA256 != original.SourceSHA256 {
		return saved, false, nil
	}
	if receipt.Profile != (SessionReadbackFileRef{Path: p.SourcePath, SHA256: p.sourceFiles[p.SourcePath]}) || receipt.Template != (SessionReadbackFileRef{Path: original.SourcePath, SHA256: original.sourceFiles[original.SourcePath]}) {
		return saved, false, errors.New("original resource publication FileRef differs")
	}
	if cfg.Budget.Swap < 0 || cfg.Budget.Memory < 0 || cfg.Budget.Memory > math.MaxUint64>>20 {
		return saved, false, errors.New("original native memory or Swap observation invalid")
	}
	hardware := admission.Sample{
		TotalMemoryBytes: uint64(cfg.Budget.Memory) << 20,
		SwapUsedBytes:    uint64(cfg.Budget.Swap),
		CPUs:             int(cfg.Budget.CPUs),
	}
	expected, err := goalResourceBudgetWithMachinePolicy(original, hardware, cfg.Budget.Class, cfg.Budget.MachinePolicy)
	if err != nil {
		return saved, false, err
	}
	if cfg.Budget.CPUObservationOnly != "" {
		if cfg.Budget.CPUObservationOnly != "observation_only" {
			return saved, false, errors.New("original CPU observation policy unsupported")
		}
		markCPUObservationOnly(expected)
	}
	var actual map[string]any
	if err = readOpenShellResourcePolicy(p, &actual); err != nil {
		return saved, false, err
	}
	for key, policy := range expected {
		if !reflect.DeepEqual(actual[key], policy) {
			return saved, false, errors.New("original resource publication policy semantics differ")
		}
	}
	return hardware, true, nil
}

// This reuses the original isolated profile producer, never creates a gateway
// or VM and never rewrites the original template or an existing policy.
func MaterializeBudgetedGoalOpenShellProfile(template, hostRoot, class string, hardware admission.Sample, readers ...func() (*admission.MachineCapacityPolicyReadback, error)) (GoalProfileReceipt, error) {
	if len(readers) > 1 || (len(readers) == 1 && readers[0] == nil) {
		return GoalProfileReceipt{}, errors.New("exactly one non-nil machine capacity policy reader is required when supplied")
	}
	readPolicy := func() (*admission.MachineCapacityPolicyReadback, error) {
		dir, err := admission.DefaultMachineCapacityDirectory()
		if err != nil {
			return nil, err
		}
		return admission.ReadMachineCapacityPolicy(dir)
	}
	if len(readers) == 1 {
		readPolicy = readers[0]
	}
	source, err := ReadSessionReadbackFile(template, nil)
	if err != nil {
		return GoalProfileReceipt{}, err
	}
	template = source.Path
	p, err := LoadOpenShellProfile(template)
	if err != nil {
		return GoalProfileReceipt{}, err
	}
	var machinePolicy *admission.MachineCapacityPolicyReadback
	cpuOnly := true
	if _, err = os.Lstat(filepath.Join(hostRoot, "openshell")); err == nil {
		receipt, e := ReadGoalOpenShellProfile(hostRoot)
		if e != nil {
			return receipt, e
		}
		prior, e := LoadOpenShellProfile(receipt.Profile.Path)
		if e != nil {
			return receipt, e
		}
		observed, ok, e := ReadGoalResourceBudgetHardware(prior, p)
		if e != nil {
			return receipt, e
		}
		if ok {
			hardware.TotalMemoryBytes, hardware.SwapUsedBytes = observed.TotalMemoryBytes, observed.SwapUsedBytes
		}
		var saved struct {
			Budget struct {
				MachinePolicy      *admission.MachineCapacityPolicyReadback `toml:"machine_policy"`
				CPUs               int                                      `toml:"hardware_cpus"`
				CPUObservationOnly string                                   `toml:"cpu_policy"`
			} `toml:"noodle_resource_budget"`
		}
		if e = readOpenShellResourcePolicy(prior, &saved); e != nil {
			return receipt, e
		}
		machinePolicy = saved.Budget.MachinePolicy
		hardware.CPUs = saved.Budget.CPUs
		cpuOnly = saved.Budget.CPUObservationOnly == "observation_only"
	} else if !os.IsNotExist(err) {
		return GoalProfileReceipt{}, err
	} else {
		machinePolicy, err = readPolicy()
		if err != nil {
			return GoalProfileReceipt{}, fmt.Errorf("%w: original machine capacity policy: %v", admission.ErrDeferred, err)
		}
	}
	budget, err := goalResourceBudgetWithMachinePolicy(p, hardware, class, machinePolicy)
	if err != nil {
		return GoalProfileReceipt{}, fmt.Errorf("%w: %v", admission.ErrDeferred, err)
	}
	if cpuOnly {
		markCPUObservationOnly(budget)
	}
	receipt, err := materializeGoalOpenShellProfile(template, hostRoot, "", budget)
	if err != nil {
		return receipt, err
	}
	loaded, err := LoadOpenShellProfile(receipt.Profile.Path)
	if err != nil {
		return receipt, err
	}
	var actual map[string]any
	if err = readOpenShellResourcePolicy(loaded, &actual); err != nil {
		return receipt, err
	}
	for key, expected := range budget {
		if !reflect.DeepEqual(actual[key], expected) {
			return receipt, errors.New("existing Goal template resource budget differs; no overwrite")
		}
	}
	if _, err = OpenShellPreparedWorkerResourceClaim(loaded, hostRoot); err != nil {
		return receipt, err
	}
	for _, purpose := range []string{"task-preparation", "host-resident"} {
		if _, err = OpenShellLifecycleResourceClaim(loaded, purpose, hostRoot); err != nil {
			return receipt, err
		}
	}
	return receipt, nil
}
