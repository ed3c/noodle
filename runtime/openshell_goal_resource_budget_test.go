package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/poteto/noodle/internal/admission"
)

func TestGoalResourceBudgetIsOnceBoundAndCarriedToIndependentHosts(t *testing.T) {
	template := goalProfileFixture(t)
	template, canonicalErr := filepath.EvalSymlinks(template)
	if canonicalErr != nil {
		t.Fatal(canonicalErr)
	}
	original, err := LoadOpenShellProfile(template)
	if err != nil {
		t.Fatal(err)
	}
	beforeProfile, _ := os.ReadFile(template)
	beforeConfig, _ := os.ReadFile(original.Gateway.Binding.ConfigSource)
	hardware := admission.Sample{TotalMemoryBytes: 4 << 30, CPUs: 8}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	budgetRoot := filepath.Join(root, "policy")
	if err = os.Mkdir(budgetRoot, 0700); err != nil {
		t.Fatal(err)
	}
	issued, err := MaterializeBudgetedGoalOpenShellProfile(template, budgetRoot, ConservativeGoalResourceBudget, hardware, noMachinePolicy)
	if err != nil {
		t.Fatal(err)
	}
	hardware.Pressure = admission.Critical
	hardware.SwapUsedBytes = 100 << 30
	again, err := MaterializeBudgetedGoalOpenShellProfile(template, budgetRoot, ConservativeGoalResourceBudget, hardware, noMachinePolicy)
	if err != nil || again.Profile != issued.Profile {
		t.Fatal("policy reissued a new template", err)
	}
	different := hardware
	different.TotalMemoryBytes = 64 << 30
	again, err = MaterializeBudgetedGoalOpenShellProfile(template, budgetRoot, ConservativeGoalResourceBudget, different, noMachinePolicy)
	if err != nil || again.Profile != issued.Profile {
		t.Fatal("different RAM observation changed the bound publication", again, err)
	}
	var policy admission.CapacityPolicy
	for _, name := range []string{"a", "b"} {
		host := filepath.Join(root, name)
		if err = os.Mkdir(host, 0700); err != nil {
			t.Fatal(err)
		}
		receipt, err := MaterializeGoalOpenShellProfile(issued.Profile.Path, host, "")
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadOpenShellProfile(receipt.Profile.Path)
		if err != nil {
			t.Fatal(err)
		}
		worker, err := OpenShellPreparedWorkerResourceClaim(loaded, host)
		if err != nil {
			t.Fatal(err)
		}
		prep, err := OpenShellLifecycleResourceClaim(loaded, "task-preparation", host)
		if err != nil {
			t.Fatal(err)
		}
		resident, err := OpenShellLifecycleResourceClaim(loaded, "host-resident", host)
		if err != nil {
			t.Fatal(err)
		}
		if worker.MemoryBytes != 4096<<20 || worker.DiskBytes != (4096+17408+10240)<<20 || prep.MemoryBytes != 1024<<20 || resident.DiskBytes != 1024<<20 {
			t.Fatal("component facts and extra policy differ", worker, prep, resident)
		}
		if worker.MemoryBytes+resident.MemoryBytes <= hardware.TotalMemoryBytes {
			t.Fatal("fixture does not exceed physical RAM", worker, resident)
		}
		if name == "a" {
			policy = worker.Policy
		} else if policy != worker.Policy {
			t.Fatal("same hardware class split the machine policy")
		}
		if policy.MemoryBytes != 3<<30 || policy.CPUs != 6 || policy.MaxSwapUsedBytes != 1<<30 {
			t.Fatal("physical class used instantaneous telemetry", policy)
		}
	}
	after, _ := os.ReadFile(template)
	afterConfig, _ := os.ReadFile(original.Gateway.Binding.ConfigSource)
	if !bytes.Equal(beforeProfile, after) || !bytes.Equal(beforeConfig, afterConfig) {
		t.Fatal("old Frozen template was modified")
	}
}

func TestGoalResourceBudgetRejectsUnavailableSwapAndKeepsWorkerComponentOracle(t *testing.T) {
	template := goalProfileFixture(t)
	template, canonicalErr := filepath.EvalSymlinks(template)
	if canonicalErr != nil {
		t.Fatal(canonicalErr)
	}
	profile, err := LoadOpenShellProfile(template)
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range []admission.Sample{{Unsupported: true}, {SwapUsedBytes: 1 << 63}} {
		if _, err := conservativeGoalResourceBudget(profile, sample, ConservativeGoalResourceBudget); err == nil {
			t.Fatal("unavailable Swap sample accepted")
		}
	}
	budget, err := conservativeGoalResourceBudget(profile, admission.Sample{TotalMemoryBytes: 32 << 30, CPUs: 8}, ConservativeGoalResourceBudget)
	if err != nil {
		t.Fatal(err)
	}
	worker := budget["noodle_worker_resources"].(map[string]any)
	if worker["disk_peak_mib"].(int64) < worker["retention_peak_mib"].(int64)+4096 {
		t.Fatal("original component Oracle was weakened")
	}
}

func TestGoalResourceBudgetPublishesAndCarriesPolicyWhenRAMIsUnknown(t *testing.T) {
	for _, policySource := range []string{"first-use", "fixed"} {
		t.Run(policySource, func(t *testing.T) {
			template, err := filepath.EvalSymlinks(goalProfileFixture(t))
			if err != nil {
				t.Fatal(err)
			}
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			hardware := admission.Sample{SwapUsedBytes: (2 << 30) + 1, CPUs: 8}
			want := admission.CapacityPolicy{CPUs: 6, DiskSafetyBytes: 2 << 30, MaxSwapUsedBytes: (5 << 30) + (1 << 20), MaxSwapoutBytesPerSecond: 1 << 20, RecoverySamples: 2}
			var fixed *admission.MachineCapacityPolicyReadback
			if policySource == "fixed" {
				want.MaxSwapUsedBytes, want.MaxSwapoutBytesPerSecond, want.RecoverySamples = 7<<30, 512<<10, 3
				fixed = &admission.MachineCapacityPolicyReadback{Source: "/native/machine-capacity/capacity.json", Capacity: want}
			}
			receipt, err := MaterializeBudgetedGoalOpenShellProfile(template, root, ConservativeGoalResourceBudget, hardware, func() (*admission.MachineCapacityPolicyReadback, error) { return fixed, nil })
			if err != nil {
				t.Fatal(err)
			}
			hardware.TotalMemoryBytes, hardware.SwapUsedBytes = 16<<30, 100<<30
			again, err := MaterializeBudgetedGoalOpenShellProfile(template, root, ConservativeGoalResourceBudget, hardware, func() (*admission.MachineCapacityPolicyReadback, error) {
				t.Error("reentry read live policy")
				return nil, nil
			})
			if err != nil || again.Profile != receipt.Profile {
				t.Fatal("fresh RAM or Swap changed the published policy", again, err)
			}
			host, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			carried, err := MaterializeGoalOpenShellProfile(receipt.Profile.Path, host, "")
			if err != nil {
				t.Fatal(err)
			}
			profile, err := LoadOpenShellProfile(carried.Profile.Path)
			if err != nil {
				t.Fatal(err)
			}
			claim, err := OpenShellPreparedWorkerResourceClaim(profile, host)
			if err != nil || claim.Policy != want || claim.MemoryBytes != 4<<30 {
				t.Fatal("independent Host lost Swap policy or worker geometry", claim, err)
			}
		})
	}
}

func qualificationProfileFixture(t *testing.T) string {
	t.Helper()
	template := goalProfileFixture(t)
	template, err := filepath.EvalSymlinks(template)
	if err != nil {
		t.Fatal(err)
	}
	spec := filepath.Join(filepath.Dir(template), "spec.json")
	raw, err := os.ReadFile(spec)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err = json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	document["template"].(map[string]any)["image"] = qualificationImage
	raw, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(spec, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return template
}

func TestGoalResourceBudgetQualificationAccountsColdCacheAndKeepsStockBaseline(t *testing.T) {
	if goruntime.GOARCH != "arm64" {
		template := qualificationProfileFixture(t)
		p, err := LoadOpenShellProfile(template)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = conservativeGoalResourceBudget(p, admission.Sample{TotalMemoryBytes: 16 << 30, CPUs: 10}, QualificationGoalResourceBudget); err == nil {
			t.Fatal("unqualified image architecture accepted")
		}
		return
	}
	template := qualificationProfileFixture(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hardware := admission.Sample{TotalMemoryBytes: 16 << 30, CPUs: 10, SwapUsedBytes: (10 << 30) + 123}
	receipt, err := MaterializeBudgetedGoalOpenShellProfile(template, root, QualificationGoalResourceBudget, hardware, noMachinePolicy)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := LoadOpenShellProfile(receipt.Profile.Path)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := OpenShellPreparedWorkerResourceClaim(profile, root)
	if err != nil {
		t.Fatal(err)
	}
	if worker.DiskBytes != 5952<<20 || worker.MemoryBytes != 4096<<20 || worker.Policy.MaxSwapUsedBytes != 11265<<20 || worker.Policy.MaxSwapoutBytesPerSecond != 1<<20 || worker.Policy.RecoverySamples != 2 {
		t.Fatal("cold-cache/component/stock policy differs", worker)
	}
	hardware.SwapUsedBytes = 100 << 30
	again, err := MaterializeBudgetedGoalOpenShellProfile(template, root, QualificationGoalResourceBudget, hardware, noMachinePolicy)
	if err != nil || again.Profile != receipt.Profile {
		t.Fatal("retry ratcheted original stock baseline", err)
	}
	loaded, err := LoadOpenShellProfile(again.Profile.Path)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err = readOpenShellResourcePolicy(loaded, &config); err != nil {
		t.Fatal(err)
	}
	vm := config["openshell"].(map[string]any)["drivers"].(map[string]any)["vm"].(map[string]any)
	if vm["overlay_disk_mib"] != int64(1024) || vm["mem_mib"] != int64(2048) || vm["bootstrap_image"] != qualificationImage || vm["default_image"] != qualificationImage {
		t.Fatal("new qualification geometry/image was not fixed", vm)
	}
}

func TestGoalResourceBudgetQualificationRefusesForeignImageOrOversizedPayload(t *testing.T) {
	foreign := goalProfileFixture(t)
	foreign, err := filepath.EvalSymlinks(foreign)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := LoadOpenShellProfile(foreign)
	if err != nil {
		t.Fatal(err)
	}
	hardware := admission.Sample{TotalMemoryBytes: 16 << 30, CPUs: 10}
	if _, err = conservativeGoalResourceBudget(profile, hardware, QualificationGoalResourceBudget); err == nil {
		t.Fatal("foreign image received known-image cache allowance")
	}
	original := qualificationProfileFixture(t)
	profile, err = LoadOpenShellProfile(original)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(profile.PayloadDirectory, "oversized"))
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate((768 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = conservativeGoalResourceBudget(profile, hardware, QualificationGoalResourceBudget); err == nil {
		t.Fatal("payload could not fit qualification overlay but was accepted")
	}
}

func TestGoalResourceBudgetPreservesFixedMachinePolicyAcrossSwapSamples(t *testing.T) {
	template, err := filepath.EvalSymlinks(goalProfileFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := LoadOpenShellProfile(template)
	if err != nil {
		t.Fatal(err)
	}
	fixed := &admission.MachineCapacityPolicyReadback{
		Source:   "/native/machine-capacity/capacity.json",
		Capacity: admission.CapacityPolicy{MemoryBytes: 12 << 30, CPUs: 6, DiskSafetyBytes: 2 << 30, MaxSwapUsedBytes: 11080 << 20, MaxSwapoutBytesPerSecond: 1 << 20, RecoverySamples: 2},
	}
	for _, swap := range []uint64{10143 << 20, 12000 << 20} {
		budget, err := goalResourceBudgetWithMachinePolicy(profile, admission.Sample{TotalMemoryBytes: 16 << 30, CPUs: 8, SwapUsedBytes: swap}, ConservativeGoalResourceBudget, fixed)
		if err != nil {
			t.Fatal(err)
		}
		worker := budget["noodle_worker_resources"].(map[string]any)
		if worker["max_swap_used_mib"] != int64(11080) {
			t.Errorf("sample %d ratcheted fixed machine policy: %v", swap, worker["max_swap_used_mib"])
		}
		if worker["memory_budget_mib"] != int64(12288) || worker["cpu_budget"] != int64(6) || worker["disk_peak_mib"] != int64(4096+17408+10240) {
			t.Fatal("policy reuse changed hardware ceiling or class geometry", worker)
		}
	}
}

func noMachinePolicy() (*admission.MachineCapacityPolicyReadback, error) { return nil, nil }

func TestGoalResourceBudgetPublicationVerifiesSavedMachinePolicyAndReenters(t *testing.T) {
	template, err := filepath.EvalSymlinks(goalProfileFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	original, err := LoadOpenShellProfile(template)
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fixed := admission.MachineCapacityPolicyReadback{Source: "/native/machine-capacity/capacity.json", Capacity: admission.CapacityPolicy{MemoryBytes: 12 << 30, CPUs: 5, DiskSafetyBytes: 3 << 30, MaxSwapUsedBytes: 11080 << 20, MaxSwapoutBytesPerSecond: 512 << 10, RecoverySamples: 3, RequireIOObservation: true}}
	calls := 0
	read := func() (*admission.MachineCapacityPolicyReadback, error) { calls++; copy := fixed; return &copy, nil }
	hardware := admission.Sample{TotalMemoryBytes: 16 << 30, CPUs: 8, SwapUsedBytes: 10143 << 20}
	issued, err := MaterializeBudgetedGoalOpenShellProfile(template, root, ConservativeGoalResourceBudget, hardware, read)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := LoadOpenShellProfile(issued.Profile.Path)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := OpenShellPreparedWorkerResourceClaim(prior, root)
	if err != nil || claim.Policy != fixed.Capacity {
		t.Fatal("full fixed machine policy was not published", claim.Policy, err)
	}
	independent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	carried, err := MaterializeGoalOpenShellProfile(issued.Profile.Path, independent, "")
	if err != nil {
		t.Fatal(err)
	}
	carriedProfile, err := LoadOpenShellProfile(carried.Profile.Path)
	if err != nil {
		t.Fatal(err)
	}
	carriedClaim, err := OpenShellPreparedWorkerResourceClaim(carriedProfile, independent)
	if err != nil || carriedClaim.Policy != fixed.Capacity {
		t.Fatal("original reader lost new policy metadata", carriedClaim.Policy, err)
	}
	native, err := os.ReadFile(filepath.Join(root, "openshell", "gateway-native.toml"))
	if err != nil || bytes.Contains(native, []byte("machine_policy")) {
		t.Fatal("budget provenance leaked into native configuration", err)
	}
	var saved struct {
		Budget struct {
			Swap int64 `toml:"initial_swap_used_bytes"`
		} `toml:"noodle_resource_budget"`
	}
	if err = readOpenShellResourcePolicy(prior, &saved); err != nil || saved.Budget.Swap != int64(hardware.SwapUsedBytes) {
		t.Fatal("fixed ceiling replaced actual sampled stock", saved, err)
	}
	baseline, ok, err := GoalResourceBudgetBaseline(prior, original, hardware)
	if err != nil || !ok || baseline != hardware.SwapUsedBytes {
		t.Fatal("publication rejected its own saved policy", baseline, ok, err)
	}
	before, err := os.ReadFile(prior.Gateway.Binding.ConfigSource)
	if err != nil {
		t.Fatal(err)
	}
	hardware.SwapUsedBytes = 100 << 30
	again, err := MaterializeBudgetedGoalOpenShellProfile(template, root, ConservativeGoalResourceBudget, hardware, func() (*admission.MachineCapacityPolicyReadback, error) {
		t.Error("reentry reread current machine policy")
		return nil, errors.New("current policy unavailable")
	})
	if err != nil || again.Profile != issued.Profile || calls != 1 {
		t.Fatal("original publication drifted on reentry", again, calls, err)
	}
	after, err := os.ReadFile(prior.Gateway.Binding.ConfigSource)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("reentry rewrote original policy", err)
	}
	otherRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := MaterializeBudgetedGoalOpenShellProfile(template, otherRoot, ConservativeGoalResourceBudget, hardware, read)
	if err != nil {
		t.Fatal(err)
	}
	secondProfile, err := LoadOpenShellProfile(second.Profile.Path)
	if err != nil {
		t.Fatal(err)
	}
	secondClaim, err := OpenShellPreparedWorkerResourceClaim(secondProfile, otherRoot)
	if err != nil || secondClaim.Policy != fixed.Capacity {
		t.Fatal("later sample ratcheted new publication", secondClaim.Policy, err)
	}
	if baseline, ok, err = GoalResourceBudgetBaseline(secondProfile, original, hardware); err != nil || !ok || baseline != hardware.SwapUsedBytes {
		t.Fatal("later publication rejected truthful sample", baseline, ok, err)
	}
}

func TestGoalResourceBudgetPublicationRefusesUnknownPolicyWithoutMaterializing(t *testing.T) {
	template, err := filepath.EvalSymlinks(goalProfileFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, readers := range [][]func() (*admission.MachineCapacityPolicyReadback, error){
		{nil}, {noMachinePolicy, noMachinePolicy},
		{func() (*admission.MachineCapacityPolicyReadback, error) {
			return nil, errors.New("original machine capacity state corrupt")
		}},
	} {
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = MaterializeBudgetedGoalOpenShellProfile(template, root, ConservativeGoalResourceBudget, admission.Sample{TotalMemoryBytes: 16 << 30, CPUs: 8}, readers...); err == nil {
			t.Fatal("untrusted policy published")
		}
		if _, err = os.Lstat(filepath.Join(root, "openshell")); !os.IsNotExist(err) {
			t.Fatal("refusal materialized a profile", err)
		}
	}
}

func TestGoalResourceBudgetSmallOrUnknownCPUCountDoesNotRejectClass(t *testing.T) {
	template, err := filepath.EvalSymlinks(goalProfileFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	p, err := LoadOpenShellProfile(template)
	if err != nil {
		t.Fatal(err)
	}
	for _, cpus := range []int{0, 1, 2, 3} {
		if _, err := conservativeGoalResourceBudget(p, admission.Sample{TotalMemoryBytes: 16 << 30, CPUs: cpus}, ConservativeGoalResourceBudget); err != nil {
			t.Errorf("count %d rejected valid RAM/VM class: %v", cpus, err)
		}
	}
}

func TestGoalResourceBudgetCPUObservationMetadataAndLegacyReentry(t *testing.T) {
	template, err := filepath.EvalSymlinks(goalProfileFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	original, err := LoadOpenShellProfile(template)
	if err != nil {
		t.Fatal(err)
	}
	for _, cpus := range []int{0, 1} {
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		hardware := admission.Sample{TotalMemoryBytes: 16 << 30, CPUs: cpus}
		receipt, err := MaterializeBudgetedGoalOpenShellProfile(template, root, ConservativeGoalResourceBudget, hardware, noMachinePolicy)
		if err != nil {
			t.Fatal("unknown/small CPU count blocked publication", err)
		}
		p, err := LoadOpenShellProfile(receipt.Profile.Path)
		if err != nil {
			t.Fatal(err)
		}
		claim, err := OpenShellPreparedWorkerResourceClaim(p, root)
		if err != nil || claim.Policy.CPUs != 0 || claim.CPUs != 2 {
			t.Fatal("legacy metadata zero blocked valid VM", claim, err)
		}
		var saved struct {
			Budget struct {
				Mode string `toml:"cpu_policy"`
				Role string `toml:"cpu_count_role"`
			} `toml:"noodle_resource_budget"`
		}
		if err = readOpenShellResourcePolicy(p, &saved); err != nil || saved.Budget.Mode != "observation_only" || saved.Budget.Role == "" {
			t.Fatal("CPU count still appears to be available capacity", saved, err)
		}
		hardware.CPUs = 8
		if _, ok, err := GoalResourceBudgetBaseline(p, original, hardware); err != nil || !ok {
			t.Fatal("fresh CPU count changed saved source", ok, err)
		}
		again, err := MaterializeBudgetedGoalOpenShellProfile(template, root, ConservativeGoalResourceBudget, hardware, func() (*admission.MachineCapacityPolicyReadback, error) {
			t.Error("reentry read live policy")
			return nil, nil
		})
		if err != nil || again.Profile != receipt.Profile {
			t.Fatal("CPU observation changed publication", again, err)
		}
	}
	// Reconstruct only a fixture in the prior publication representation.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hardware := admission.Sample{TotalMemoryBytes: 16 << 30, CPUs: 8}
	legacyBudget, err := goalResourceBudgetWithMachinePolicy(original, hardware, ConservativeGoalResourceBudget, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := materializeGoalOpenShellProfile(template, root, "", legacyBudget)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "openshell", "gateway.toml"))
	if err != nil {
		t.Fatal(err)
	}
	hardware.CPUs = 0
	again, err := MaterializeBudgetedGoalOpenShellProfile(template, root, ConservativeGoalResourceBudget, hardware, noMachinePolicy)
	if err != nil || again.Profile != legacy.Profile {
		t.Fatal("CPUunknown invalidated prior publication", again, err)
	}
	after, err := os.ReadFile(filepath.Join(root, "openshell", "gateway.toml"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("legacy publication was migrated", err)
	}
}
