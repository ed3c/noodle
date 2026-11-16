//go:build darwin || linux

package admission

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/filex"
)

func capacityFixtureRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil { t.Fatal(err) }
	return root
}

func TestMachineCapacityNativeIdentitySurvivesHostEnvironmentChanges(t *testing.T) {
	before, err := nativeMachineIdentity()
	if err != nil { t.Fatal(err) }
	t.Setenv("HOME", capacityFixtureRoot(t))
	t.Setenv("XDG_CACHE_HOME", capacityFixtureRoot(t))
	t.Setenv("PATH", capacityFixtureRoot(t))
	after, err := nativeMachineIdentity()
	if err != nil || before == "" || before != after { t.Fatal("native physical machine/user identity changed with Host environment", err) }
}

func TestMachineCapacityNativeDiskAndFileIdentity(t *testing.T) {
	root := capacityFixtureRoot(t)
	if err := capacityFileIdentity(root, true); err != nil { t.Fatal(err) }
	disk, err := nativeDiskCapacity(root)
	if err != nil || disk.Volume == "" || disk.Available == 0 { t.Fatal("native disk capacity unavailable", disk, err) }
	path := filepath.Join(root, "owned")
	if err = os.WriteFile(path, []byte("fixture"), 0600); err != nil { t.Fatal(err) }
	if err = capacityFileIdentity(path, false); err != nil { t.Fatal(err) }
	if err = os.Link(path, filepath.Join(root, "alias")); err != nil { t.Fatal(err) }
	if err = capacityFileIdentity(path, false); err == nil { t.Fatal("native file identity accepted hardlink") }
}

func TestMachineCapacityDarwinPlatformUUIDObservation(t *testing.T) {
	const id = "12345678-9ABC-5DEF-8123-123456789ABC"
	property := `"IOPlatformUUID" = "` + id + `"`
	valid := "+-o fixture <class IOPlatformExpertDevice>\n  {\n    " + property + "\n  }\n"
	got, err := parseDarwinPlatformUUID([]byte(valid))
	if err != nil || got != strings.ToLower(id) { t.Fatal("hardware UUID unavailable from ioreg property", got, err) }
	for _, data := range []string{
		"", `"kern.bootsessionuuid" = "` + id + `"`,
		strings.Replace(valid, "IOPlatformUUID", "IOPlatformUUIDOther", 1),
		valid + property + "\n", strings.Replace(valid, id, "00000000-0000-0000-0000-000000000000", 1),
		strings.Replace(valid, id, "123456789ABC5DEF8123123456789ABC", 1),
		strings.Replace(valid, id, "12345678-9ABC-5DEF-8123-123456789ABZ", 1),
		strings.Replace(valid, id, "12345678-9ABC-5DEF-8123-123456789AB", 1),
		strings.Replace(valid, `"`+id+`"`, id, 1),
		strings.Repeat(" ", platformUUIDOutputLimit) + valid,
	} {
		if value, err := parseDarwinPlatformUUID([]byte(data)); err == nil || value != "" { t.Fatalf("invalid hardware observation admitted: %q %v", value, err) }
	}
}

func TestMachineCapacityConcurrentFirstUse(t *testing.T) {
	root := capacityFixtureRoot(t)
	machine := filepath.Join(root, "machine")
	start := make(chan struct{})
	results := make(chan error, 16)
	for i := range cap(results) {
		go func() {
			<-start
			results <- NewBudget(9).BindMachineCapacity(machine, fmt.Sprintf("host-%d", i))
		}()
	}
	close(start)
	for range cap(results) {
		if err := <-results; err != nil { t.Error("concurrent cold pool bind failed", err) }
	}
	before, err := os.ReadFile(filepath.Join(machine, "capacity.json"))
	if err != nil { t.Fatal(err) }
	if err = NewBudget(9).BindMachineCapacity(machine, "later-generation"); err != nil { t.Fatal(err) }
	after, err := os.ReadFile(filepath.Join(machine, "capacity.json"))
	if err != nil || !bytes.Equal(before, after) { t.Fatal("readback rewrote original pool", err) }
}

func TestMachineCapacityMissingInitializationStateIsNotRecreated(t *testing.T) {
	for _, condition := range []string{"initialized-state-deleted", "creator-write-failed", "orphan-lock"} {
		t.Run(condition, func(t *testing.T) {
			root := capacityFixtureRoot(t)
			machine := filepath.Join(root, "machine")
			path := filepath.Join(machine, "capacity.json")
			if err := os.Mkdir(machine, 0700); err != nil { t.Fatal(err) }
			switch condition {
			case "initialized-state-deleted":
				b, _ := capacityFixtureBudget(t, machine, filepath.Join(root, "quota"), "original", capacityFixtureClaim(root))
				if _, err := b.TryAcquireTask("original", "held", "openshell"); err != nil { t.Fatal(err) }
				if err := os.Remove(path); err != nil { t.Fatal(err) }
			case "creator-write-failed":
				identity, err := nativeMachineIdentity()
				if err != nil { t.Fatal(err) }
				m := &machineCapacity{dir: machine, identity: identity, owner: "original", write: func(string, []byte) error { return errors.New("fixture initial write failed") }}
				if err = m.access(true, nil); err == nil { t.Fatal("failed initialization reported success") }
			case "orphan-lock":
				if err := os.WriteFile(filepath.Join(machine, "capacity.lock"), []byte("original creator"), 0600); err != nil { t.Fatal(err) }
			}
			if err := NewBudget(9).BindMachineCapacity(machine, "later-generation"); err == nil || !strings.Contains(err.Error(), "machine capacity state missing") { t.Fatal("unknown original pool was recreated", err) }
			if _, err := os.Lstat(path); !os.IsNotExist(err) { t.Fatal("missing state was written", err) }
		})
	}
}

func capacityFixtureClaim(root string) ResourceClaim {
	return ResourceClaim{Source: "frozen-operator-profile", MemoryBytes: 10, CPUs: 1, DiskBytes: 20, DiskPaths: []string{root}, Policy: CapacityPolicy{MemoryBytes: 30, CPUs: 100, DiskSafetyBytes: 10, MaxSwapUsedBytes: 10, MaxSwapoutBytesPerSecond: 10, RecoverySamples: 2}}
}

func capacityFixtureBudget(t *testing.T, machine, quota, owner string, claim ResourceClaim) (*Budget, *time.Time) {
	t.Helper()
	b := NewBudget(9)
	if err := b.BindSharedQuota(quota, owner); err != nil { t.Fatal(err) }
	if err := b.BindMachineCapacity(machine, owner); err != nil { t.Fatal(err) }
	b.machine.probe = func(string) (diskCapacity, error) { return diskCapacity{Volume: "fixture-volume", Available: 10000}, nil }
	b.Register(owner)
	b.BindResourceSource(owner, "openshell", func(string) (ResourceClaim, error) { return claim, nil })
	now := time.Unix(4000, 0)
	b.now = func() time.Time { return now }
	for range 4 {
		now = now.Add(PollInterval)
		b.Observe(Sample{At: now, Pressure: Normal, TotalMemoryBytes: 10000, CPUs: 128}, nil)
	}
	return b, &now
}

func TestMachineCapacityTwoHostsGenerationsAtomic(t *testing.T) {
	root := capacityFixtureRoot(t)
	machine := filepath.Join(root, "machine")
	claim := capacityFixtureClaim(root)
	var budgets []*Budget
	for _, host := range []string{"host-a", "host-b"} {
		for _, generation := range []string{"old", "current"} {
			b, _ := capacityFixtureBudget(t, machine, filepath.Join(root, host), host+"/"+generation, claim)
			budgets = append(budgets, b)
		}
	}
	var mu sync.Mutex
	var permits []*Permit
	var wg sync.WaitGroup
	for index, b := range budgets {
		for n := range 9 {
			wg.Add(1)
			go func(b *Budget, owner, order string) {
				defer wg.Done()
				permit, err := b.TryAcquireTask(owner, order, "openshell")
				if err != nil && !errors.Is(err, ErrDeferred) { t.Error(err) }
				if err == nil { mu.Lock(); permits = append(permits, permit); mu.Unlock() }
			}(b, b.machine.owner, fmt.Sprintf("%d-%d", index, n))
		}
	}
	wg.Wait()
	if len(permits) != 3 { t.Fatalf("cross-Host capacity admitted %d, want 3; independent free-space checks would admit more", len(permits)) }
	for _, b := range budgets {
		s := b.Snapshot()
		if s.Maximum != 9 || s.Limit != 9 || s.SharedQuota.Maximum != 9 || s.MachineCapacity.Leases != 3 || s.MachineCapacity.MemoryBytes != 30 { t.Fatalf("quota or machine discriminator lost: %+v", s) }
	}
	for _, permit := range permits { permit.Release(); permit.Release() }
	b := budgets[0]
	p, err := b.TryAcquireTask(b.machine.owner, "after-safe-unstarted-return", "openshell")
	if err != nil { t.Fatal("safe capacity return did not unblock original admission", err) }
	p.Release()
}

func TestMachineCapacityKeepsNinePerHostAcrossGenerations(t *testing.T) {
	root := capacityFixtureRoot(t)
	claim := capacityFixtureClaim(root)
	claim.Policy.MemoryBytes = 10000
	var all []*Permit
	for _, host := range []string{"a", "b"} {
		old, _ := capacityFixtureBudget(t, filepath.Join(root, "machine"), filepath.Join(root, host), host+"-old", claim)
		current, _ := capacityFixtureBudget(t, filepath.Join(root, "machine"), filepath.Join(root, host), host+"-current", claim)
		for i := range 9 {
			b := old
			if i%2 == 0 { b = current }
			p, err := b.TryAcquireTask(b.machine.owner, fmt.Sprint(i), "openshell")
			if err != nil { t.Fatal(err) }
			all = append(all, p)
		}
		if _, err := current.TryAcquireTask(current.machine.owner, "tenth", "openshell"); !errors.Is(err, ErrDeferred) { t.Fatal("same Host generations exceeded nine") }
	}
	if len(all) != 18 { t.Fatal("machine gate restored a shared nine-worker ceiling") }
	for _, p := range all { p.Release() }
}

func TestMachineCapacityMissingStaleUnsupportedAndPressureRecovery(t *testing.T) {
	for _, condition := range []string{"missing", "stale", "future", "unsupported", "failed", "unknown", "critical", "ram", "cpu", "swap-used", "swap-rate", "io", "peak"} {
		t.Run(condition, func(t *testing.T) {
			root := capacityFixtureRoot(t)
			claim := capacityFixtureClaim(root)
			if condition == "io" { claim.Policy.RequireIOObservation = true }
			if condition == "peak" { claim.MemoryBytes = 0 }
			b, now := capacityFixtureBudget(t, filepath.Join(root, "machine"), filepath.Join(root, "quota"), "owner", claim)
			*now = now.Add(PollInterval)
			sample := Sample{At: *now, Pressure: Normal, TotalMemoryBytes: 10000, CPUs: 128}
			switch condition {
			case "missing": b.Observe(Sample{}, nil)
			case "failed": b.Observe(Sample{}, errors.New("fixture sampler error"))
			case "stale": *now = now.Add(MaxSampleAge+time.Second)
			case "future": sample.At = now.Add(time.Second); b.Observe(sample, nil)
			case "unsupported": sample.Unsupported = true; b.Observe(sample, nil)
			case "unknown": sample.Pressure = Unknown; b.Observe(sample, nil)
			case "critical": sample.Pressure = Critical; b.Observe(sample, nil)
			case "ram": sample.TotalMemoryBytes = 0; b.Observe(sample, nil)
			case "cpu": sample.CPUs = 0; b.Observe(sample, nil)
			case "swap-used": sample.SwapUsedBytes = 11; b.Observe(sample, nil)
			case "swap-rate": *now = now.Add(time.Second); sample.At = *now; sample.Swapouts = 100; b.Observe(sample, nil)
			}
			if _, err := b.TryAcquireTask("owner", "task", "openshell"); !errors.Is(err, ErrDeferred) { t.Fatal("unobserved or pressured worker admitted", err) }
			if s := b.Snapshot(); s.MachineCapacity.Reason == "" || s.MachineCapacity.Reason == "admitted" || s.Active != 0 || s.MachineCapacity.Leases != 0 { t.Fatalf("unnamed queue or leaked reservation: %+v", s) }
		})
	}
	root := capacityFixtureRoot(t)
	b, now := capacityFixtureBudget(t, filepath.Join(root, "machine"), filepath.Join(root, "quota"), "owner", capacityFixtureClaim(root))
	b.Observe(Sample{At: *now, Pressure: Critical, CPUs: 128, TotalMemoryBytes: 10000}, nil)
	for i := range 3 {
		*now = now.Add(PollInterval)
		b.Observe(Sample{At: *now, Pressure: Normal, CPUs: 128, TotalMemoryBytes: 10000}, nil)
		p, err := b.TryAcquireTask("owner", "task", "openshell")
		if i < 2 && !errors.Is(err, ErrDeferred) { t.Fatal("pressure hysteresis recovered too early") }
		if i == 2 { if err != nil { t.Fatal("pressure hysteresis never recovered", err) }; p.Release() }
	}
}

func TestMachineCapacityDiskMarginAndSharedVolumes(t *testing.T) {
	root := capacityFixtureRoot(t)
	claim := capacityFixtureClaim(root)
	claim.Policy.MemoryBytes = 1000
	b, _ := capacityFixtureBudget(t, filepath.Join(root, "machine"), filepath.Join(root, "quota"), "owner", claim)
	free := uint64(29)
	b.machine.probe = func(string) (diskCapacity, error) { return diskCapacity{Volume: "first", Available: free}, nil }
	if _, err := b.TryAcquireTask("owner", "first", "openshell"); !errors.Is(err, ErrDeferred) { t.Fatal("disk safety margin ignored") }
	free = 40
	p, err := b.TryAcquireTask("owner", "first", "openshell")
	if err != nil { t.Fatal(err) }
	// A different volume can report the same shared APFS-container free bytes.
	b.machine.probe = func(string) (diskCapacity, error) { return diskCapacity{Volume: "second", Available: free}, nil }
	if _, err = b.TryAcquireTask("owner", "second", "openshell"); !errors.Is(err, ErrDeferred) { t.Fatal("different volume invented disjoint free storage") }
	p.Release()
	q, err := b.TryAcquireTask("owner", "second", "openshell")
	if err != nil { t.Fatal(err) }
	q.Release()
}

func TestMachineCapacityLostAcquireACKReadsOriginalNonce(t *testing.T) {
	for _, landed := range []bool{false, true} {
		t.Run(fmt.Sprint(landed), func(t *testing.T) {
			root := capacityFixtureRoot(t)
			b, _ := capacityFixtureBudget(t, filepath.Join(root, "machine"), filepath.Join(root, "quota"), "owner", capacityFixtureClaim(root))
			b.machine.write = func(path string, data []byte) error {
				if landed { if err := filex.WriteFileAtomicDurable(path, data); err != nil { return err } }
				return errors.New("fixture lost durable ACK")
			}
			if _, err := b.TryAcquireTask("owner", "task", "openshell"); !errors.Is(err, ErrDeferred) { t.Fatal("lost ACK launched worker") }
			pending := b.pendingMachineAcquires["owner\x00task"]
			if pending.id == "" || b.Snapshot().SharedQuota.Allocations != 1 { t.Fatal("uncertain acquire lost original nonce/quota") }
			if _, err := b.TryAcquireTask("owner", "other", "openshell"); !errors.Is(err, ErrDeferred) { t.Fatal("uncertain acquire allowed another claim") }
			b.machine.write = filex.WriteFileAtomicDurable
			p, err := b.TryAcquireTask("owner", "task", "openshell")
			if err != nil || p.allocation.machineID != pending.id || b.Snapshot().MachineCapacity.Leases != 1 || b.Snapshot().SharedQuota.Allocations != 1 { t.Fatal("readback acquired a second lease", err) }
			if _, err = b.TryAcquireTask("owner", "task", "openshell"); !errors.Is(err, ErrDeferred) { t.Fatal("active reentry launched twice") }
			p.Release()
		})
	}
}

func TestMachineCapacityReplacementAndJoinedWorkerRetainResources(t *testing.T) {
	root := capacityFixtureRoot(t)
	b, _ := capacityFixtureBudget(t, filepath.Join(root, "machine"), filepath.Join(root, "quota"), "owner", capacityFixtureClaim(root))
	p, err := b.TryAcquireTask("owner", "task", "openshell")
	if err != nil { t.Fatal(err) }
	if err = p.StartResourceEffects(); err != nil { t.Fatal(err) }
	b.machine.probe = func(string) (diskCapacity, error) { return diskCapacity{Volume: "fixture-volume", Available: 31}, nil }
	path := filepath.Join(b.machine.dir, "capacity.json")
	before, err := os.ReadFile(path)
	if err != nil { t.Fatal(err) }
	for range 3 {
		replacement, err := p.ReserveReplacement("owner")
		if replacement != nil || !errors.Is(err, ErrDeferred) || !strings.Contains(err.Error(), "replacement_resource_retirement_required") { t.Fatal("replacement reused finite peak without retirement", err) }
		if s := b.Snapshot(); s.Active != 1 || s.SharedQuota.Allocations != 1 || s.MachineCapacity.Leases != 1 { t.Fatal("deferred replacement changed original quota or resource custody") }
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) { t.Fatal("replacement changed original held resource lease", err) }
	p.Release()
	s := b.Snapshot()
	if s.Active != 0 || s.SharedQuota.Allocations != 0 || s.MachineCapacity.Leases != 1 { t.Fatal("worker Done claimed VM or disk retirement") }
	if err = b.machine.cancelUnstarted(p.allocation.machineID); err == nil { t.Fatal("effects-possible lease returned without GC/VM owner proof") }
}

func TestMachineCapacitySwapRecoveryWithoutAcquireDuringPressure(t *testing.T) {
	for _, pressure := range []string{"used", "rate"} {
		t.Run(pressure, func(t *testing.T) {
			root := capacityFixtureRoot(t)
			claim := capacityFixtureClaim(root)
			b, now := capacityFixtureBudget(t, filepath.Join(root, "machine"), filepath.Join(root, "quota"), "owner", claim)
			var swapouts uint64
			for range 4 {
				*now = now.Add(PollInterval)
				sample := Sample{At: *now, Pressure: Normal, TotalMemoryBytes: 10000, CPUs: 128}
				if pressure == "used" { sample.SwapUsedBytes = claim.Policy.MaxSwapUsedBytes + 1
				} else { swapouts += (claim.Policy.MaxSwapoutBytesPerSecond + 1) * uint64(PollInterval/time.Second) }
				sample.Swapouts = swapouts
				b.Observe(sample, nil)
			}
			for i := 1; i <= claim.Policy.RecoverySamples; i++ {
				*now = now.Add(PollInterval)
				b.Observe(Sample{At: *now, Pressure: Normal, TotalMemoryBytes: 10000, CPUs: 128, Swapouts: swapouts}, nil)
				p, err := b.TryAcquireTask("owner", "after-pressure", "openshell")
				if i < claim.Policy.RecoverySamples {
					if p != nil || !errors.Is(err, ErrDeferred) || !strings.Contains(err.Error(), "machine_pressure_recovering") { t.Fatal("idle Swap pressure failed to reset recovery", err) }
					if s := b.Snapshot(); s.Active != 0 || s.MachineCapacity.Leases != 0 { t.Fatal("recovering pressure acquired resources") }
				} else {
					if err != nil { t.Fatal("healthy sequence did not resume admission", err) }
					p.Release()
				}
			}
		})
	}
}

func TestMachineCapacityRecoveryInvalidatesUnverifiableSequence(t *testing.T) {
	for _, condition := range []string{"missing", "stale", "future", "unsupported", "error", "counter-reset", "gap", "ram", "cpu", "policy-change", "source-change", "source-error"} {
		t.Run(condition, func(t *testing.T) {
			root := capacityFixtureRoot(t)
			claim := capacityFixtureClaim(root)
			b, now := capacityFixtureBudget(t, filepath.Join(root, "machine"), filepath.Join(root, "quota"), "owner", claim)
			var sourceError error
			b.resourceSources["owner"].read = func(string) (ResourceClaim, error) { return claim, sourceError }
			swapouts := uint64(100)
			for range 4 {
				*now = now.Add(PollInterval)
				b.Observe(Sample{At: *now, Pressure: Normal, TotalMemoryBytes: 10000, CPUs: 128, Swapouts: swapouts}, nil)
			}
			*now = now.Add(PollInterval)
			sample := Sample{At: *now, Pressure: Normal, TotalMemoryBytes: 10000, CPUs: 128, Swapouts: swapouts}
			var sampleError error
			switch condition {
			case "missing": sample.At = time.Time{}
			case "stale": sample.At = now.Add(-MaxSampleAge-time.Second)
			case "future": sample.At = now.Add(time.Second)
			case "unsupported": sample.Unsupported = true
			case "error": sampleError = errors.New("fixture sampler unavailable")
			case "counter-reset": swapouts = 0; sample.Swapouts = swapouts
			case "gap": *now = now.Add(MaxSampleAge); sample.At = *now
			case "ram": sample.TotalMemoryBytes = 0
			case "cpu": sample.CPUs = 0
			case "policy-change": claim.Policy.MaxSwapUsedBytes++
			case "source-change": claim.Source = "new-operator-source"
			case "source-error": sourceError = errors.New("original operator source unavailable")
			}
			b.Observe(sample, sampleError)
			changed := condition == "policy-change" || condition == "source-change"
			if changed {
				p, err := b.TryAcquireTask("owner", "new-policy-sequence", "openshell")
				if p != nil || !errors.Is(err, ErrDeferred) { t.Fatal("old policy health admitted a new source or policy", err) }
			}
			sourceError = nil
			for i := range 3 {
				*now = now.Add(PollInterval)
				b.Observe(Sample{At: *now, Pressure: Normal, TotalMemoryBytes: 10000, CPUs: 128, Swapouts: swapouts}, nil)
				p, err := b.TryAcquireTask("owner", "after-unverifiable-sequence", "openshell")
				if i == 0 && !changed && (p != nil || !errors.Is(err, ErrDeferred)) { t.Fatal("unverifiable health sequence was reused", err) }
				if i == 2 && err != nil { t.Fatal("fresh healthy observations never recovered", err) }
				if p != nil { p.Release() }
			}
		})
	}
}

func TestMachineCapacityLostEffectFenceAndReturnACKStayHeld(t *testing.T) {
	for _, operation := range []string{"start", "return"} {
		t.Run(operation, func(t *testing.T) {
			root := capacityFixtureRoot(t)
			b, now := capacityFixtureBudget(t, filepath.Join(root, "machine"), filepath.Join(root, "quota"), "owner", capacityFixtureClaim(root))
			p, err := b.TryAcquireTask("owner", "task", "openshell")
			if err != nil { t.Fatal(err) }
			b.machine.write = func(path string, data []byte) error { if err := filex.WriteFileAtomicDurable(path, data); err != nil { return err }; return errors.New("ACK lost") }
			if operation == "start" && !errors.Is(p.StartResourceEffects(), ErrDeferred) { t.Fatal("unknown effect fence accepted") }
			p.Release()
			if _, err = b.TryAcquireTask("owner", "next", "openshell"); !errors.Is(err, ErrDeferred) { t.Fatal("unknown resource mutation reentered") }
			b.machine.write = filex.WriteFileAtomicDurable
			*now = now.Add(PollInterval)
			b.Observe(Sample{At: *now, Pressure: Normal, CPUs: 128, TotalMemoryBytes: 10000}, nil)
			if operation == "return" { q, e := b.TryAcquireTask("owner", "next", "openshell"); if e != nil { t.Fatal(e) }; q.Release()
			} else if b.Snapshot().MachineCapacity.Leases != 1 { t.Fatal("unknown effect fence released original resources") }
		})
	}
}

func TestMachineCapacityRejectsForeignAliasCorruptAndLegacyWithoutRewrite(t *testing.T) {
	for _, condition := range []string{"foreign", "alias", "hardlink", "corrupt", "duplicate", "incomplete", "legacy", "missing"} {
		t.Run(condition, func(t *testing.T) {
			root := capacityFixtureRoot(t)
			b, _ := capacityFixtureBudget(t, filepath.Join(root, "machine"), filepath.Join(root, "quota"), "owner", capacityFixtureClaim(root))
			path := filepath.Join(b.machine.dir, "capacity.json")
			raw, err := os.ReadFile(path)
			if err != nil { t.Fatal(err) }
			switch condition {
			case "foreign": b.machine.identity = "other-machine-user"
			case "alias": alias := filepath.Join(root, "alias"); if err = os.Symlink(b.machine.dir, alias); err != nil { t.Fatal(err) }; b.machine.dir = alias
			case "hardlink": if err = os.Link(path, filepath.Join(root, "link")); err != nil { t.Fatal(err) }
			case "corrupt": raw = []byte("broken")
			case "duplicate": raw = bytes.Replace(raw, []byte(`"schema":1`), []byte(`"schema":1,"schema":1`), 1)
			case "incomplete": raw = []byte("{}")
			case "legacy": raw = bytes.Replace(raw, []byte(MachineAdmissionPolicy), []byte("legacy-machine-policy"), 1)
			case "missing": if err = os.Remove(path); err != nil { t.Fatal(err) }
			}
			if condition == "corrupt" || condition == "duplicate" || condition == "incomplete" || condition == "legacy" { if err = os.WriteFile(path, raw, 0600); err != nil { t.Fatal(err) } }
			if _, err = b.TryAcquireTask("owner", "task", "openshell"); !errors.Is(err, ErrDeferred) { t.Fatal("foreign or corrupt machine state admitted", err) }
			if condition == "missing" { if _, err = os.Stat(path); !os.IsNotExist(err) { t.Fatal("missing state was recreated") }; return }
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(raw, after) { t.Fatal("readback changed old state", err) }
			if b.Snapshot().MachineCapacity.Reason == "" { t.Fatal("missing refusal") }
		})
	}
}

func TestMachineCapacityProcessFixtureChild(t *testing.T) {
	root := os.Getenv("NOODLE_MACHINE_CAPACITY_FIXTURE")
	if root == "" { return }
	owner := os.Getenv("NOODLE_MACHINE_CAPACITY_OWNER")
	b, _ := capacityFixtureBudget(t, filepath.Join(root, "machine"), filepath.Join(root, owner), owner, capacityFixtureClaim(root))
	_, err := b.TryAcquireTask(owner, "fixture", "openshell")
	result := "admitted"
	if errors.Is(err, ErrDeferred) { result = "deferred" } else if err != nil { t.Fatal(err) }
	if err = os.WriteFile(filepath.Join(root, owner+".result"), []byte(result), 0600); err != nil { t.Fatal(err) }
	// A process exit is not native VM/GC proof, even for an unstarted lease.
}

func TestMachineCapacityAcrossProcessesRetainsUnknownOwner(t *testing.T) {
	root := capacityFixtureRoot(t)
	executable, err := os.Executable()
	if err != nil { t.Fatal(err) }
	var commands []*exec.Cmd
	var outputs []*bytes.Buffer
	for i := range 6 {
		owner := fmt.Sprintf("host-%d", i)
		cmd := exec.Command(executable, "-test.run=^TestMachineCapacityProcessFixtureChild$")
		cmd.Env = append(os.Environ(), "NOODLE_MACHINE_CAPACITY_FIXTURE="+root, "NOODLE_MACHINE_CAPACITY_OWNER="+owner)
		output := new(bytes.Buffer)
		cmd.Stdout, cmd.Stderr = output, output
		if err = cmd.Start(); err != nil { t.Fatal(err) }
		commands = append(commands, cmd)
		outputs = append(outputs, output)
	}
	admitted := 0
	for i, cmd := range commands {
		if err = cmd.Wait(); err != nil { t.Fatalf("fixture process: %v %s", err, outputs[i].String()) }
		raw, e := os.ReadFile(filepath.Join(root, fmt.Sprintf("host-%d.result", i)))
		if e != nil { t.Fatal(e) }
		if string(raw) == "admitted" { admitted++ } else if string(raw) != "deferred" { t.Fatal("unknown fixture result") }
	}
	if admitted != 3 { t.Fatalf("process-local capacity checks admitted %d instead of shared three", admitted) }
	path := filepath.Join(root, "machine", "capacity.json")
	before, err := os.ReadFile(path)
	if err != nil { t.Fatal(err) }
	b, _ := capacityFixtureBudget(t, filepath.Join(root, "machine"), filepath.Join(root, "after-exit"), "new-generation", capacityFixtureClaim(root))
	if _, err = b.TryAcquireTask("new-generation", "after-exit", "openshell"); !errors.Is(err, ErrDeferred) { t.Fatal("process exit expired old machine lease") }
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) { t.Fatal("new generation readback changed unknown resource leases", err) }
}
