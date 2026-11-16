package runtime

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func resourceConfigFixture(state string) string {
	return fmt.Sprintf(`[openshell.drivers.vm]
mem_mib=4
vcpus=2
overlay_disk_mib=4
state_dir=%q
[noodle_worker_resources]
schema=1
memory_peak_mib=6
disk_peak_mib=7
checkout_peak_mib=1
dependency_peak_mib=0
return_tar_peak_mib=1
restore_peak_mib=1
memory_budget_mib=16
cpu_budget=8
disk_safety_mib=1
max_swap_used_mib=2
max_swapout_bytes_per_second=100
recovery_samples=2
require_io_observation=false
`, state)
}

func resourceProfileFixture(t *testing.T, transform func(string) string) OpenShellProfile {
	t.Helper()
	filename, _ := profileFixture(t)
	filename, err := filepath.EvalSymlinks(filename)
	if err != nil { t.Fatal(err) }
	configuration := resourceConfigFixture(filepath.Join(filepath.Dir(filename), "vm"))
	if transform != nil { configuration = transform(configuration) }
	if err := os.WriteFile(filepath.Join(filepath.Dir(filename), "gateway.toml"), []byte(configuration), 0600); err != nil { t.Fatal(err) }
	p, err := LoadOpenShellProfile(filename)
	if err != nil { t.Fatal(err) }
	return p
}

func TestOpenShellWorkerDemandUsesOnlyBoundOperatorConfig(t *testing.T) {
	p := resourceProfileFixture(t, nil)
	before, err := os.ReadFile(p.Gateway.Binding.ConfigSource)
	if err != nil { t.Fatal(err) }
	claim, err := OpenShellWorkerResourceClaim(p, filepath.Dir(p.SourcePath))
	if err != nil { t.Fatal(err) }
	if claim.MemoryBytes != 6<<20 || claim.CPUs != 2 || claim.DiskBytes != 7<<20 || claim.Policy.MemoryBytes != 16<<20 || claim.Policy.DiskSafetyBytes != 1<<20 || claim.Source != p.SourceSHA256+"/"+p.sourceFiles[p.Gateway.Binding.ConfigSource] { t.Fatalf("bound VM/operator resource source differs: %+v", claim) }
	after, err := os.ReadFile(p.Gateway.Binding.ConfigSource)
	if err != nil || !bytes.Equal(before, after) { t.Fatal("demand producer changed configuration", err) }
	// A loaded SDK route does not supply the missing operator peaks.
	p.SourcePath = ""
	if _, err = OpenShellWorkerResourceClaim(p, t.TempDir()); err == nil { t.Fatal("unbound route supplied trusted RAM/disk") }
}

func TestOpenShellWorkerDemandMissingPeaksNeverBackfills(t *testing.T) {
	for _, field := range []string{"mem_mib", "vcpus", "overlay_disk_mib", "memory_peak_mib", "disk_peak_mib", "checkout_peak_mib", "dependency_peak_mib", "return_tar_peak_mib", "restore_peak_mib", "memory_budget_mib", "cpu_budget", "disk_safety_mib", "max_swap_used_mib", "max_swapout_bytes_per_second", "recovery_samples", "schema"} {
		t.Run(field, func(t *testing.T) {
			p := resourceProfileFixture(t, func(config string) string {
				var lines []string
				for _, line := range strings.Split(config, "\n") { if !strings.HasPrefix(line, field+"=") { lines = append(lines, line) } }
				return strings.Join(lines, "\n")
			})
			path := p.Gateway.Binding.ConfigSource
			before, _ := os.ReadFile(path)
			if _, err := OpenShellWorkerResourceClaim(p, filepath.Dir(p.SourcePath)); err == nil { t.Fatal("missing operator/native requirement became ready") }
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) { t.Fatal("old config was backfilled") }
		})
	}
}

func TestOpenShellWorkerDemandRejectsChangedAndUnsupportedSources(t *testing.T) {
	for _, change := range []string{"config", "profile", "alias", "unknown", "driver", "below-vm", "negative", "overflow"} {
		t.Run(change, func(t *testing.T) {
			p := resourceProfileFixture(t, func(config string) string {
				switch change {
				case "below-vm": return strings.Replace(config, "memory_peak_mib=6", "memory_peak_mib=1", 1)
				case "negative": return strings.Replace(config, "checkout_peak_mib=1", "checkout_peak_mib=-1", 1)
				case "overflow": return strings.Replace(config, "checkout_peak_mib=1", "checkout_peak_mib=9223372036854775807", 1)
				}
				return config
			})
			switch change {
			case "config", "profile":
				path := p.SourcePath
				if change == "config" { path = p.Gateway.Binding.ConfigSource }
				data, _ := os.ReadFile(path)
				if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil { t.Fatal(err) }
			case "alias":
				path := p.Gateway.Binding.ConfigSource
				if err := os.Rename(path, path+".original"); err != nil { t.Fatal(err) }
				if err := os.Symlink(path+".original", path); err != nil { t.Fatal(err) }
			case "unknown": p.sourceFiles = nil
			case "driver": p.Gateway.ComputeDriver = "container"
			}
			if _, err := OpenShellWorkerResourceClaim(p, filepath.Dir(p.SourcePath)); err == nil { t.Fatal("untrusted or unsupported demand accepted") }
		})
	}
}
