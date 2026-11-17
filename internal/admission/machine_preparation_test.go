//go:build darwin || linux

package admission

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/filex"
)

func preparationFixture(t *testing.T, machine string, claim ResourceClaim) *Budget {
	t.Helper()
	b, now := capacityFixtureBudget(t, machine, filepath.Join(capacityFixtureRoot(t), "quota"), "original-native-owner", claim)
	b.BindResourceSource("original-native-preparation", "preparation", func(string) (ResourceClaim, error) { return claim, nil })
	for range 4 {
		*now = now.Add(PollInterval)
		b.Observe(Sample{At: *now, Pressure: Normal, TotalMemoryBytes: 10000, CPUs: 128}, nil)
	}
	return b
}

func preparationFixtureSource(root string) PreparationSource {
	return PreparationSource{Purpose: "task-preparation", Bindings: map[string]string{"task": "original-bytes", "repository": root, "head": "original-base", "profile": "original-profile", "binary": "original-executable"}}
}

func TestMachinePreparationProcessFixtureChild(t *testing.T) {
	root := os.Getenv("NOODLE_PREPARATION_FIXTURE_ROOT")
	if root == "" {
		return
	}
	claim := capacityFixtureClaim(root)
	b := preparationFixture(t, filepath.Join(root, "machine"), claim)
	ref, err := b.reservePreparation(preparationFixtureSource(root), claim)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(os.Getenv("NOODLE_PREPARATION_FIXTURE_OUTPUT"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestMachinePreparationAcrossProcessesUsesOriginalSourceNonce(t *testing.T) {
	root := capacityFixtureRoot(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var children []*exec.Cmd
	for _, name := range []string{"first", "second"} {
		child := exec.Command(executable, "-test.run=^TestMachinePreparationProcessFixtureChild$")
		child.Env = append(os.Environ(), "NOODLE_PREPARATION_FIXTURE_ROOT="+root, "NOODLE_PREPARATION_FIXTURE_OUTPUT="+filepath.Join(root, name))
		if err = child.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, child)
	}
	for _, child := range children {
		if err = child.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	var refs []PreparationRef
	for _, name := range []string{"first", "second"} {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		var ref PreparationRef
		if err = json.Unmarshal(raw, &ref); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	if refs[0].ID == "" || refs[0] != refs[1] {
		t.Fatal("two native processes minted different preparation nonces", refs)
	}
}

func TestMachinePreparationConcurrentSourceIsOneNonceWithoutWorkerQuota(t *testing.T) {
	root := capacityFixtureRoot(t)
	claim, source := capacityFixtureClaim(root), preparationFixtureSource(root)
	var budgets []*Budget
	for range 2 {
		budgets = append(budgets, preparationFixture(t, filepath.Join(root, "machine"), claim))
	}
	var group sync.WaitGroup
	results := make(chan PreparationRef, 2)
	for _, budget := range budgets {
		group.Add(1)
		go func(b *Budget) {
			defer group.Done()
			ref, err := b.reservePreparation(source, claim)
			if err != nil {
				t.Error(err)
			}
			results <- ref
		}(budget)
	}
	group.Wait()
	first, second := <-results, <-results
	if first.ID == "" || first != second {
		t.Fatal("same original source minted multiple nonces", first, second)
	}
	for _, b := range budgets {
		if b.active != 0 || b.shared.snapshot().Allocations != 0 {
			t.Fatal("preparation consumed Host worker quota")
		}
		if err := b.machine.access(false, func(state *machineState) (bool, error) {
			if len(state.Leases) != 1 || state.Leases[first.ID].EffectsPossible || state.Leases[first.ID].Binding != nil {
				t.Fatal("preparation fabricated a session or effects")
			}
			return false, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMachinePreparationLostACKRestartAndEffectFenceKeepOriginalNonce(t *testing.T) {
	root := capacityFixtureRoot(t)
	claim, source := capacityFixtureClaim(root), preparationFixtureSource(root)
	b := preparationFixture(t, filepath.Join(root, "machine"), claim)
	b.machine.write = func(path string, data []byte) error {
		if err := filex.WriteFileAtomicDurable(path, data); err != nil {
			return err
		}
		return errors.New("original write ACK lost")
	}
	first, err := b.reservePreparation(source, claim)
	if err == nil || first.ID == "" {
		t.Fatal("lost original write ACK was reported as success")
	}
	b.machine.write = filex.WriteFileAtomicDurable
	restarted := preparationFixture(t, filepath.Join(root, "machine"), claim)
	second, exists, err := restarted.preparationRead(source, claim)
	if err != nil || !exists || first != second {
		t.Fatal("restart failed to read original nonce", err)
	}
	if err := restarted.StartPreparationEffects(second); err != nil {
		t.Fatal(err)
	}
	if err := restarted.machine.cancelUnstarted(second.ID); err == nil {
		t.Fatal("effect-possible preparation was returned as unstarted")
	}
	if err := restarted.StartPreparationEffects(second); err != nil {
		t.Fatal("same effect fence cannot be read back", err)
	}
	third, err := restarted.reservePreparation(source, claim)
	if err != nil || third != second {
		t.Fatal("same preparation was reminted", err)
	}
}

func TestMachinePreparationCrossFilesystemPeakAndBootstrapWindow(t *testing.T) {
	root := capacityFixtureRoot(t)
	claim, source := capacityFixtureClaim(root), preparationFixtureSource(root)
	claim.DiskPaths = append(claim.DiskPaths, filepath.Join(root, "archive", "not-created"))
	b := preparationFixture(t, filepath.Join(root, "machine"), claim)
	b.machine.probe = func(path string) (diskCapacity, error) {
		if path == claim.DiskPaths[1] {
			return diskCapacity{Volume: "archive-volume", Available: claim.DiskBytes + claim.Policy.DiskSafetyBytes - 1}, nil
		}
		return diskCapacity{Volume: "project-volume", Available: 10000}, nil
	}
	if _, err := b.reservePreparation(source, claim); !errors.Is(err, ErrDeferred) {
		t.Fatal("separate archive filesystem was omitted", err)
	}
	if _, err := os.Stat(filepath.Join(root, "archive")); !os.IsNotExist(err) {
		t.Fatal("insufficient capacity created planned storage")
	}
	if err := b.machine.access(false, func(state *machineState) (bool, error) {
		if len(state.Leases) != 0 {
			t.Fatal("failed reservation retained a false successful claim")
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	claim.Policy.RecoverySamples = 12
	window, err := PreparationWindow(claim.Policy)
	if err != nil || window != 74*time.Second || window <= 45*time.Second {
		t.Fatal("bootstrap bound cannot cover the original sampler recovery", window, err)
	}
}

func TestMachinePreparationPromotesSameLeaseToRealWorkerAndRetainsUnknown(t *testing.T) {
	root := capacityFixtureRoot(t)
	prepClaim := capacityFixtureClaim(root)
	producer := preparationFixture(t, filepath.Join(root, "machine"), prepClaim)
	ref, err := producer.reservePreparation(preparationFixtureSource(root), prepClaim)
	if err != nil {
		t.Fatal(err)
	}
	if err = producer.StartPreparationEffects(ref); err != nil {
		t.Fatal(err)
	}
	workerClaim := prepClaim
	workerClaim.Source = "original-frozen-worker-profile"
	workerClaim.DiskBytes *= 2
	worker := preparationFixture(t, filepath.Join(root, "machine"), workerClaim)
	worker.machine.owner = "actual-native-host-generation"
	if err = worker.BindPreparation(ref, "original-native-owner", "order", "openshell"); err != nil {
		t.Fatal(err)
	}
	if err = worker.BindPreparation(ref, "foreign-context", "order", "openshell"); err == nil {
		t.Fatal("original preparation was rebound to another context")
	}
	write := worker.machine.write
	worker.machine.write = func(path string, raw []byte) error {
		if err := write(path, raw); err != nil {
			return err
		}
		return errors.New("worker promotion write ACK lost")
	}
	if _, err = worker.TryAcquireTask("original-native-owner", "order", "openshell"); !errors.Is(err, ErrDeferred) {
		t.Fatal("promotion with unknown write ACK was admitted", err)
	}
	worker.machine.write = write
	permit, err := worker.TryAcquireTask("original-native-owner", "order", "openshell")
	if err != nil {
		t.Fatal(err)
	}
	if permit.allocation.machineID != ref.ID || !permit.allocation.effectsPossible {
		t.Fatal("worker acquired a new lease or reset original effects")
	}
	binding := MachineSessionBinding{ContextID: "original-native-owner", ProjectDir: root, OrderID: "order", StageIndex: 0, RetryCount: 0, AttemptID: "real-attempt", SessionID: "real-session", Runtime: "openshell", WorktreePath: filepath.Join(root, "worker")}
	if err = permit.BindSession(binding); err != nil {
		t.Fatal("same preparation could not bind the real session", err)
	}
	permit.Release()
	if err = worker.machine.access(false, func(state *machineState) (bool, error) {
		lease := state.Leases[ref.ID]
		if len(state.Leases) != 1 || lease.Owner != worker.machine.owner || lease.Source != workerClaim.Source || lease.Binding == nil || *lease.Binding != binding || lease.Settlement != nil || lease.Preparation.Claim.Source != prepClaim.Source {
			t.Fatal("handoff lost original provenance or prematurely returned resources")
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = producer.StartPreparationEffects(ref); err == nil {
		t.Fatal("old preparation producer reused an assigned lifetime")
	}
	if worker.shared.snapshot().Allocations != 0 {
		t.Fatal("worker Done failed to return only Host quota")
	}
}
