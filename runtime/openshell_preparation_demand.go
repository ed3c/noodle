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

type openShellLifetimePeak struct {
	Schema    int    `toml:"schema"`
	MemoryMiB *int64 `toml:"memory_peak_mib"`
	CPUs      *int64 `toml:"cpus"`
	DiskMiB   *int64 `toml:"disk_peak_mib"`
}

// Whole preparation and resident lifetimes have their own operator peaks.
// Worker VM peaks cannot be renamed into preparation or resident estimates.
func OpenShellLifecycleResourceClaim(p OpenShellProfile, purpose string, roots ...string) (admission.ResourceClaim, error) {
	worker, err := OpenShellWorkerResourceClaim(p, roots...)
	if err != nil {
		return admission.ResourceClaim{}, err
	}
	var cfg struct {
		Preparation openShellLifetimePeak `toml:"noodle_preparation_resources"`
		Resident    openShellLifetimePeak `toml:"noodle_resident_resources"`
	}
	if err = readOpenShellResourcePolicy(p, &cfg); err != nil {
		return admission.ResourceClaim{}, err
	}
	var peak openShellLifetimePeak
	switch purpose {
	case "task-preparation":
		peak = cfg.Preparation
	case "host-resident":
		peak = cfg.Resident
	default:
		return admission.ResourceClaim{}, errors.New("original preparation purpose unsupported")
	}
	if peak.Schema != 1 || peak.CPUs == nil || *peak.CPUs <= 0 {
		return admission.ResourceClaim{}, fmt.Errorf("trusted_%s_peak_missing_or_unsupported", purpose)
	}
	claim := admission.ResourceClaim{Source: worker.Source + "/" + purpose, Policy: worker.Policy, CPUs: uint64(*peak.CPUs), DiskPaths: append([]string(nil), roots...)}
	claim.MemoryBytes, err = resourceMiB(peak.MemoryMiB, purpose+"_memory_peak_mib", false)
	if err != nil {
		return claim, err
	}
	claim.DiskBytes, err = resourceMiB(peak.DiskMiB, purpose+"_disk_peak_mib", false)
	if err != nil {
		return claim, err
	}
	return claim, nil
}

// Prepared workers additionally cover the original archive/CAS/Git pool and
// verification-copy writes. The old worker reader remains available for its
// historical profile; only newly admitted preparation uses this contract.
func OpenShellPreparedWorkerResourceClaim(p OpenShellProfile, roots ...string) (admission.ResourceClaim, error) {
	claim, err := OpenShellWorkerResourceClaim(p, roots...)
	if err != nil {
		return claim, err
	}
	var cfg struct {
		Worker struct {
			RetentionMiB *int64 `toml:"retention_peak_mib"`
		} `toml:"noodle_worker_resources"`
	}
	if err = readOpenShellResourcePolicy(p, &cfg); err != nil {
		return claim, err
	}
	// The original declared whole-worker peak must include these extra future
	// writes as well as every original native/checkout/return/restore component.
	var original openShellResourceConfig
	if err = readOpenShellResourcePolicy(p, &original); err != nil {
		return claim, err
	}
	var minimum uint64
	for index, component := range []*int64{original.OpenShell.Drivers.VM.OverlayMiB, original.Worker.CheckoutMiB, original.Worker.DependencyMiB, original.Worker.ReturnTarMiB, original.Worker.RestoreMiB, cfg.Worker.RetentionMiB} {
		size, err := resourceMiB(component, "prepared_worker_component_peak_mib", index == 2)
		if err != nil || size > math.MaxUint64-minimum {
			return claim, errors.New("prepared_worker_component_peak_missing_or_overflow")
		}
		minimum += size
	}
	if claim.DiskBytes < minimum {
		return claim, errors.New("prepared_worker_peak_below_original_and_retention_components")
	}
	return claim, nil
}

func readOpenShellResourcePolicy(p OpenShellProfile, result any) error {
	var config []byte
	for _, path := range []string{p.SourcePath, p.Gateway.Binding.ConfigSource} {
		expected := p.sourceFiles[path]
		resolved, err := filepath.EvalSymlinks(path)
		if expected == "" || err != nil || resolved != path {
			return errors.New("original lifecycle resource source unavailable or aliased")
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("original lifecycle resource source is not regular")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(raw)
		if hex.EncodeToString(digest[:]) != expected {
			return errors.New("original lifecycle resource source changed")
		}
		config = raw
	}
	if _, err := toml.Decode(string(config), result); err != nil {
		return fmt.Errorf("original lifecycle resource policy invalid: %w", err)
	}
	return nil
}
