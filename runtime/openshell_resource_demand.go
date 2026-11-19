package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/poteto/noodle/internal/admission"
)

// SDK ResourceRequirements describes GPUs, not RAM or disk. Whole-worker peaks
// are explicit operator policy in the original profile-bound gateway config.
type openShellResourceConfig struct {
	OpenShell struct {
		Drivers struct {
			VM struct {
				MemoryMiB *int64 `toml:"mem_mib"`
				CPUs *int64 `toml:"vcpus"`
				OverlayMiB *int64 `toml:"overlay_disk_mib"`
				StateDir string `toml:"state_dir"`
			}
		}
	}
	Worker struct {
		Schema int `toml:"schema"`
		MemoryPeakMiB *int64 `toml:"memory_peak_mib"`
		DiskPeakMiB *int64 `toml:"disk_peak_mib"`
		CheckoutMiB *int64 `toml:"checkout_peak_mib"`
		DependencyMiB *int64 `toml:"dependency_peak_mib"`
		ReturnTarMiB *int64 `toml:"return_tar_peak_mib"`
		RestoreMiB *int64 `toml:"restore_peak_mib"`
		MemoryBudgetMiB *int64 `toml:"memory_budget_mib"`
		CPUBudget any `toml:"cpu_budget"`
		DiskSafetyMiB *int64 `toml:"disk_safety_mib"`
		MaxSwapUsedMiB *int64 `toml:"max_swap_used_mib"`
		MaxSwapoutBytesPerSecond *int64 `toml:"max_swapout_bytes_per_second"`
		RecoverySamples int `toml:"recovery_samples"`
		RequireIOObservation bool `toml:"require_io_observation"`
	} `toml:"noodle_worker_resources"`
}

func resourceMiB(value *int64, name string, zero bool) (uint64, error) {
	if value == nil || *value < 0 || (!zero && *value == 0) || uint64(*value) > math.MaxUint64/(1<<20) {
		return 0, fmt.Errorf("trusted_worker_peak_missing_or_invalid: %s", name)
	}
	return uint64(*value)*(1<<20), nil
}

// OpenShellWorkerResourceClaim uses the frozen producer's dependency closure.
// Changed policy requires a new profile/context; old bytes are never backfilled.
func OpenShellWorkerResourceClaim(p OpenShellProfile, diskRoots ...string) (admission.ResourceClaim, error) {
	var claim admission.ResourceClaim
	if p.Gateway.ComputeDriver != "vm" { return claim, errors.New("worker_resource_runtime_unsupported") }
	if p.SourcePath == "" || p.SourceSHA256 == "" || len(diskRoots) == 0 { return claim, errors.New("worker_resource_source_unknown") }
	readBound := func(path string) ([]byte, error) {
		expected := p.sourceFiles[path]
		if expected == "" { return nil, errors.New("worker_resource_source_not_bound") }
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || resolved != path { return nil, errors.New("worker_resource_source_alias_or_unavailable") }
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() { return nil, errors.New("worker_resource_source_not_regular") }
		raw, err := os.ReadFile(path)
		if err != nil { return nil, err }
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != expected { return nil, errors.New("worker_resource_source_changed") }
		return raw, nil
	}
	if _, err := readBound(p.SourcePath); err != nil { return claim, err }
	configPath := p.Gateway.Binding.ConfigSource
	raw, err := readBound(configPath)
	if err != nil { return claim, err }
	var cfg openShellResourceConfig
	if _, err = toml.Decode(string(raw), &cfg); err != nil { return claim, fmt.Errorf("worker_resource_config_invalid: %w", err) }
	vm, policy := cfg.OpenShell.Drivers.VM, cfg.Worker
	if policy.Schema != 1 { return claim, errors.New("operator_worker_resource_policy_missing_or_unsupported") }
	memory, err := resourceMiB(vm.MemoryMiB, "mem_mib", false)
	if err != nil { return claim, err }
	claim.MemoryBytes, err = resourceMiB(policy.MemoryPeakMiB, "memory_peak_mib", false)
	if err != nil { return claim, err }
	if claim.MemoryBytes < memory || vm.CPUs == nil || *vm.CPUs <= 0 { return claim, errors.New("worker_resource_peak_below_native_vm") }
	claim.CPUs = uint64(*vm.CPUs)
	for _, component := range []struct { value *int64; name string; zero bool }{
		{vm.OverlayMiB, "overlay_disk_mib", false},
		{policy.CheckoutMiB, "checkout_peak_mib", false},
		{policy.DependencyMiB, "dependency_peak_mib", true},
		{policy.ReturnTarMiB, "return_tar_peak_mib", false},
		{policy.RestoreMiB, "restore_peak_mib", false},
	} {
		size, e := resourceMiB(component.value, component.name, component.zero)
		if e != nil { return claim, e }
		if size > math.MaxUint64-claim.DiskBytes { return claim, errors.New("worker_disk_peak_overflow") }
		claim.DiskBytes += size
	}
	minimumDisk := claim.DiskBytes
	claim.DiskBytes, err = resourceMiB(policy.DiskPeakMiB, "disk_peak_mib", false)
	if err != nil { return claim, err }
	if claim.DiskBytes < minimumDisk { return claim, errors.New("worker_disk_peak_below_native_and_return_restore_components") }
	claim.Policy.MemoryBytes, err = resourceMiB(policy.MemoryBudgetMiB, "memory_budget_mib", true)
	if err != nil { return claim, err }
	claim.Policy.DiskSafetyBytes, err = resourceMiB(policy.DiskSafetyMiB, "disk_safety_mib", false)
	if err != nil { return claim, err }
	claim.Policy.MaxSwapUsedBytes, err = resourceMiB(policy.MaxSwapUsedMiB, "max_swap_used_mib", true)
	if err != nil { return claim, err }
	if policy.MaxSwapoutBytesPerSecond == nil || *policy.MaxSwapoutBytesPerSecond < 0 || policy.RecoverySamples < 2 || policy.RecoverySamples > admission.MaxRecoverySamples {
		return claim, errors.New("operator_machine_policy_missing_or_invalid")
	}
	// Global CPU count is legacy observation metadata. Only VM vCPUs above constrain geometry.
	if cpus, ok := policy.CPUBudget.(int64); ok && cpus >= 0 { claim.Policy.CPUs = uint64(cpus) }
	claim.Policy.MaxSwapoutBytesPerSecond = uint64(*policy.MaxSwapoutBytesPerSecond)
	claim.Policy.RecoverySamples = policy.RecoverySamples
	claim.Policy.RequireIOObservation = policy.RequireIOObservation
	if !filepath.IsAbs(vm.StateDir) || filepath.Clean(vm.StateDir) != vm.StateDir { return claim, errors.New("vm_disk_source_unknown") }
	// Each affected volume reserves the entire peak, conservatively covering
	// checkout, VM, returned archives and restore on different filesystems.
	claim.DiskPaths = append(append([]string(nil), diskRoots...), vm.StateDir)
	claim.Source = p.SourceSHA256 + "/" + p.sourceFiles[configPath]
	return claim, nil
}
