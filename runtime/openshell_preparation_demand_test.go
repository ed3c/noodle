package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func lifecycleResourceConfig(config string) string {
	config = strings.Replace(config, "disk_peak_mib=7", "disk_peak_mib=10", 1)
	config += `retention_peak_mib=3
[noodle_preparation_resources]
schema=1
memory_peak_mib=2
cpus=1
disk_peak_mib=3
[noodle_resident_resources]
schema=1
memory_peak_mib=3
cpus=1
disk_peak_mib=2
`
	return config
}

func TestOpenShellLifecycleDemandUsesSeparateTrustedLifetimes(t *testing.T) {
	p := resourceProfileFixture(t, lifecycleResourceConfig)
	root := filepath.Dir(p.SourcePath)
	before, err := os.ReadFile(p.Gateway.Binding.ConfigSource)
	if err != nil {
		t.Fatal(err)
	}
	prep, err := OpenShellLifecycleResourceClaim(p, "task-preparation", root)
	if err != nil {
		t.Fatal(err)
	}
	resident, err := OpenShellLifecycleResourceClaim(p, "host-resident", root)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := OpenShellPreparedWorkerResourceClaim(p, root)
	if err != nil {
		t.Fatal(err)
	}
	if prep.MemoryBytes != 2<<20 || prep.DiskBytes != 3<<20 || resident.MemoryBytes != 3<<20 || resident.DiskBytes != 2<<20 || worker.MemoryBytes != 6<<20 || worker.DiskBytes != 10<<20 || prep.Policy != worker.Policy || resident.Policy != worker.Policy || prep.Source == worker.Source || resident.Source == worker.Source {
		t.Fatal("lifetimes reused worker peaks or changed operator policy")
	}
	after, err := os.ReadFile(p.Gateway.Binding.ConfigSource)
	if err != nil || string(before) != string(after) {
		t.Fatal("policy producer backfilled the original source", err)
	}
}

func TestOpenShellLifecycleDemandMissingSourceNeverRenamesWorkerPeak(t *testing.T) {
	p := resourceProfileFixture(t, nil)
	for _, purpose := range []string{"task-preparation", "host-resident", "foreign"} {
		if _, err := OpenShellLifecycleResourceClaim(p, purpose, filepath.Dir(p.SourcePath)); err == nil {
			t.Fatal("worker peak substituted for missing lifecycle peak", purpose)
		}
	}
	if _, err := OpenShellPreparedWorkerResourceClaim(p, filepath.Dir(p.SourcePath)); err == nil {
		t.Fatal("archive/CAS/Git pool future peaks became zero")
	}
	p = resourceProfileFixture(t, func(config string) string {
		return strings.Replace(lifecycleResourceConfig(config), "disk_peak_mib=10", "disk_peak_mib=7", 1)
	})
	if _, err := OpenShellPreparedWorkerResourceClaim(p, filepath.Dir(p.SourcePath)); err == nil {
		t.Fatal("archive/CAS/Git pool omitted from the whole peak")
	}
	p = resourceProfileFixture(t, lifecycleResourceConfig)
	path := p.Gateway.Binding.ConfigSource
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenShellLifecycleResourceClaim(p, "task-preparation", filepath.Dir(p.SourcePath)); err == nil {
		t.Fatal("changed original lifecycle policy accepted")
	}
}
