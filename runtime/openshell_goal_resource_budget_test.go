package runtime

import (
	"bytes"
	"encoding/json"
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
	hardware := admission.Sample{TotalMemoryBytes: 32 << 30, CPUs: 8}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	budgetRoot := filepath.Join(root, "policy")
	if err = os.Mkdir(budgetRoot, 0700); err != nil {
		t.Fatal(err)
	}
	issued, err := MaterializeBudgetedGoalOpenShellProfile(template, budgetRoot, ConservativeGoalResourceBudget, hardware)
	if err != nil {
		t.Fatal(err)
	}
	hardware.Pressure = admission.Critical
	hardware.SwapUsedBytes = 100 << 30
	again, err := MaterializeBudgetedGoalOpenShellProfile(template, budgetRoot, ConservativeGoalResourceBudget, hardware)
	if err != nil || again.Profile != issued.Profile {
		t.Fatal("policy reissued a new template", err)
	}
	different := hardware
	different.TotalMemoryBytes = 64 << 30
	if _, err = MaterializeBudgetedGoalOpenShellProfile(template, budgetRoot, ConservativeGoalResourceBudget, different); err == nil {
		t.Fatal("same template identity silently changed class")
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
		if name == "a" {
			policy = worker.Policy
		} else if policy != worker.Policy {
			t.Fatal("same hardware class split the machine policy")
		}
		if policy.MemoryBytes != 24<<30 || policy.CPUs != 6 {
			t.Fatal("physical class used instantaneous telemetry", policy)
		}
	}
	after, _ := os.ReadFile(template)
	afterConfig, _ := os.ReadFile(original.Gateway.Binding.ConfigSource)
	if !bytes.Equal(beforeProfile, after) || !bytes.Equal(beforeConfig, afterConfig) {
		t.Fatal("old Frozen template was modified")
	}
}

func TestGoalResourceBudgetRejectsUnknownHardwareAndKeepsWorkerComponentOracle(t *testing.T) {
	template := goalProfileFixture(t)
	template, canonicalErr := filepath.EvalSymlinks(template)
	if canonicalErr != nil {
		t.Fatal(canonicalErr)
	}
	profile, err := LoadOpenShellProfile(template)
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range []admission.Sample{{}, {TotalMemoryBytes: 32 << 30, CPUs: 8, Unsupported: true}, {TotalMemoryBytes: 4 << 30, CPUs: 4}} {
		if _, err := conservativeGoalResourceBudget(profile, sample, ConservativeGoalResourceBudget); err == nil {
			t.Fatal("missing or unfit hardware accepted")
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
	receipt, err := MaterializeBudgetedGoalOpenShellProfile(template, root, QualificationGoalResourceBudget, hardware)
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
	again, err := MaterializeBudgetedGoalOpenShellProfile(template, root, QualificationGoalResourceBudget, hardware)
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
