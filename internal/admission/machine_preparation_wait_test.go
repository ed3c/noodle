//go:build darwin || linux

package admission

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/procx"
)

type waitingPreparationFixture struct {
	b      *Budget
	ref    PreparationRef
	source PreparationSource
	files  map[string]MachineSettlementFile
	claim  ResourceClaim
	order  string
}

func waitingPreparation(t *testing.T, root, owner string) waitingPreparationFixture {
	t.Helper()
	if !procx.NativeFixtureControlAvailable() {
		t.Skip("original native generation observer unavailable")
	}
	dir := filepath.Join(root, owner)
	if err := os.MkdirAll(filepath.Join(dir, "openshell"), 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]MachineSettlementFile{}
	paths := map[string]string{"task": filepath.Join(dir, "task.json"), "goal": filepath.Join(dir, "goal.json"),
		"execution_binary": filepath.Join(dir, "noodle"), "profile": filepath.Join(dir, "openshell", "profile.json"),
		"control": filepath.Join(dir, "control.json")}
	for role, path := range paths {
		data := []byte("original " + owner + " " + role)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		files[role] = MachineSettlementFile{Path: path, SHA256: hex.EncodeToString(digest[:])}
	}
	claim := capacityFixtureClaim(root)
	claim.Policy.CPUs, claim.Policy.MemoryBytes = 6, 10000
	claim.Source = "original-profile-" + owner
	b, now := capacityFixtureBudget(t, filepath.Join(root, "machine"), filepath.Join(dir, "quota"), owner, claim)
	b.BindResourceSource("original-native-preparation", "preparation", func(string) (ResourceClaim, error) { return claim, nil })
	for range 4 {
		*now = now.Add(PollInterval)
		b.Observe(Sample{At: *now, Pressure: Normal, TotalMemoryBytes: 10000, CPUs: 128}, nil)
	}
	source := PreparationSource{Purpose: "task-preparation", Bindings: map[string]string{
		"task_path": files["task"].Path, "task_sha256": files["task"].SHA256,
		"goal_path": files["goal"].Path, "goal_sha256": files["goal"].SHA256,
		"host_root": dir, "control_root": filepath.Join(dir, "checkout"),
		"profile": "bound-complete-profile", "execution_binary": files["execution_binary"].Path + "/" + files["execution_binary"].SHA256}}
	ref, err := b.reservePreparation(source, claim)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.StartPreparationEffects(ref); err != nil {
		t.Fatal(err)
	}
	order := "task-" + owner
	if err = b.BindPreparation(ref, ref.ID, order, "openshell"); err != nil {
		t.Fatal(err)
	}
	worker := claim
	worker.CPUs = 2
	b.Register(ref.ID)
	b.BindResourceSource(ref.ID, "openshell", func(string) (ResourceClaim, error) { return worker, nil })
	for range 4 {
		*now = now.Add(PollInterval)
		b.Observe(Sample{At: *now, Pressure: Normal, TotalMemoryBytes: 10000, CPUs: 128}, nil)
	}
	return waitingPreparationFixture{b, ref, source, files, claim, order}
}

func fixedPreparationBase(t *testing.T, f waitingPreparationFixture) {
	t.Helper()
	if err := f.b.machine.access(false, func(state *machineState) (bool, error) {
		for _, id := range []string{"old-resident", "A1-unknown", "new-A-resident", "new-B-resident"} {
			state.Leases[id] = machineLease{Owner: id, Source: id, MemoryBytes: 1, CPUs: 1, Disks: map[string]uint64{"fixture-volume": 1}, EffectsPossible: true}
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func beginWaiting(t *testing.T, f waitingPreparationFixture) io.Closer {
	t.Helper()
	closer, err := f.b.BeginPreparationWait(f.ref, f.ref.ID, f.order, f.files)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	return closer
}

func TestPreparationWaitingTwoHostsDoesNotBlockOnConfiguredCPUCount(t *testing.T) {
	root := capacityFixtureRoot(t)
	a, b := waitingPreparation(t, root, "A"), waitingPreparation(t, root, "B")
	fixedPreparationBase(t, a)
	before, err := os.ReadFile(filepath.Join(a.b.machine.dir, "capacity.json"))
	if err != nil { t.Fatal(err) }
	beginWaiting(t, a)
	beginWaiting(t, b)
	after, err := os.ReadFile(filepath.Join(a.b.machine.dir, "capacity.json"))
	if err != nil || !bytes.Equal(before, after) { t.Fatal("ready observation changed original row/source", err) }
	if snapshot := a.b.Snapshot().MachineCapacity; snapshot.CPUs != 4 || snapshot.Leases != 6 || snapshot.MemoryBytes != 24 || snapshot.DiskBytes != 44 {
		t.Fatal("waiting changed memory/disk or lost unknown reservations", snapshot)
	}
	for _, f := range []waitingPreparationFixture{a, b} {
		permit, err := f.b.TryAcquireTask(f.ref.ID, f.order, "openshell")
		if err != nil { t.Fatal("legacy CPU6/base4/two prep1 blocked original worker", err) }
		if permit.allocation.machineID != f.ref.ID { t.Fatal("promotion replaced original nonce") }
		if err = permit.StartResourceEffects(); err != nil { t.Fatal(err) }
		permit.Release()
	}
	if snapshot := a.b.Snapshot().MachineCapacity; snapshot.Leases != 6 || snapshot.MemoryBytes != 24 || snapshot.DiskBytes != 44 || snapshot.Capacity.CPUs != 6 {
		t.Fatal("CPU correction reclaimed unknown resources or rewrote historical policy", snapshot)
	}
}

func TestPreparationWaitingUnknownStaleAndForeignEvidenceRetainsOriginalCPU(t *testing.T) {
	for _, condition := range []string{"missing", "actor-exit", "source-drift", "foreign-nonce", "foreign-generation", "future", "unknown-schema", "receipt-tamper", "released-lock", "promoted", "session-bound", "foreign-lock-holder"} {
		t.Run(condition, func(t *testing.T) {
			f := waitingPreparation(t, capacityFixtureRoot(t), "A")
			var closer io.Closer
			if condition != "missing" {
				closer = beginWaiting(t, f)
			}
			path, lockPath := f.b.machine.preparationWaitPaths(f.ref.ID)
			if condition == "actor-exit" || condition == "released-lock" {
				_ = closer.Close()
			}
			if condition == "source-drift" {
				if err := os.WriteFile(f.files["task"].Path, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if condition == "foreign-lock-holder" {
				if err := os.WriteFile(lockPath, []byte("1\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if condition == "foreign-nonce" || condition == "foreign-generation" || condition == "future" || condition == "unknown-schema" || condition == "receipt-tamper" {
				raw, _ := os.ReadFile(path)
				var proof preparationWait
				if err := json.Unmarshal(raw, &proof); err != nil {
					t.Fatal(err)
				}
				switch condition {
				case "foreign-nonce":
					proof.Ref.ID = "another"
				case "foreign-generation":
					proof.Process.StartUsec++
				case "future":
					proof.ReadyAt = time.Now().Add(time.Hour)
				case "unknown-schema":
					proof.Schema = 2
				case "receipt-tamper":
					proof.ReadyAt = proof.ReadyAt.Add(-time.Second)
				}
				raw, _ = json.Marshal(proof)
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if condition != "receipt-tamper" {
					if err := bindPreparationWaitLock(lockPath, os.Getpid(), raw); err != nil {
						t.Fatal(err)
					}
				}
			}
			err := f.b.machine.access(false, func(state *machineState) (bool, error) {
				lease := state.Leases[f.ref.ID]
				if condition == "promoted" {
					lease.PreparationPromoted = true
					lease.CPUs = 2
				}
				if condition == "session-bound" {
					lease.Binding = &MachineSessionBinding{}
				}
				if cpus := f.b.machine.preparationCPUs(f.ref.ID, lease); cpus != lease.CPUs {
					t.Fatalf("%s discounted unresolved original CPU: %d", condition, cpus)
				}
				return false, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPreparationWaitingLostACKReentersSameSourceWithoutChangingGlobalRow(t *testing.T) {
	f := waitingPreparation(t, capacityFixtureRoot(t), "A")
	before, _ := os.ReadFile(filepath.Join(f.b.machine.dir, "capacity.json"))
	f.b.machine.write = func(path string, data []byte) error {
		if err := filex.WriteFileAtomicDurable(path, data); err != nil {
			return err
		}
		return errors.New("original ready ACK lost")
	}
	if closer, err := f.b.BeginPreparationWait(f.ref, f.ref.ID, f.order, f.files); err == nil || closer != nil {
		t.Fatal("lost ACK was accepted")
	}
	if f.b.Snapshot().MachineCapacity.CPUs != 1 {
		t.Fatal("failed ready kept a live discount")
	}
	f.b.machine.write = filex.WriteFileAtomicDurable
	beginWaiting(t, f)
	if f.b.Snapshot().MachineCapacity.CPUs != 0 {
		t.Fatal("same original actor cannot read back ready")
	}
	after, _ := os.ReadFile(filepath.Join(f.b.machine.dir, "capacity.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("ready retry changed original global state")
	}
	var a, b machineState
	if err := json.Unmarshal(before, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("preparation/source changed")
	}
}

// Also run with the exact old machine_capacity.go overlaid: it must be able to
// deserialize/write the unchanged row while leaving this actor's sidecar alone.
func TestPreparationWaitingOriginalJournalRoundTripPreservesSourceAndSidecar(t *testing.T) {
	f := waitingPreparation(t, capacityFixtureRoot(t), "A")
	beginWaiting(t, f)
	path, _ := f.b.machine.preparationWaitPaths(f.ref.ID)
	proofBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(f.b.machine.dir, "capacity.json")
	before, err := os.ReadFile(global)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.b.machine.access(false, func(s *machineState) (bool, error) {
		row := s.Leases[f.ref.ID]
		if row.CPUs != 1 || row.PreparationPromoted || row.Binding != nil || row.Preparation.SHA256 != f.ref.SHA256 || !reflect.DeepEqual(row.Preparation.Source, f.source) {
			t.Fatal("original preparation source or raw CPU changed")
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(global)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("journal roundtrip dropped original fields", err)
	}
	proofAfter, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(proofBefore, proofAfter) {
		t.Fatal("journal writer changed ready evidence", err)
	}
	t.Logf("reader held CPUs=%d; original serialized preparation CPUs=1", f.b.Snapshot().MachineCapacity.CPUs)
}

func TestPreparationWaitingDoesNotCertifyUnknownResourceMutation(t *testing.T) {
	for _, condition := range []string{"machine-fault", "acquire-ack", "machine-return", "quota-return"} {
		t.Run(condition, func(t *testing.T) {
			f := waitingPreparation(t, capacityFixtureRoot(t), "A")
			switch condition {
			case "machine-fault":
				f.b.machineFault = "original effect ACK unknown"
			case "acquire-ack":
				f.b.pendingMachineAcquires = map[string]machinePending{"original": {id: f.ref.ID}}
			case "machine-return":
				f.b.pendingMachineReturns = map[string]bool{"original": true}
			case "quota-return":
				f.b.pendingSharedReturns = map[string]bool{"original": true}
			}
			if closer, err := f.b.BeginPreparationWait(f.ref, f.ref.ID, f.order, f.files); err == nil || closer != nil {
				t.Fatal("unknown resource mutation became ready")
			}
			if f.b.Snapshot().MachineCapacity.CPUs != 1 {
				t.Fatal("unknown resource mutation discounted original preparation")
			}
		})
	}
}

func TestPreparationFrozenCPUClaimIdentityIsRetained(t *testing.T) {
	root := capacityFixtureRoot(t)
	f := waitingPreparation(t, root, "A")
	claim := f.claim
	claim.CPUs = 2
	claim.Policy.CPUs++
	f.b.resourceSources[f.ref.ID].read = func(string) (ResourceClaim, error) { return claim, nil }
	before, err := os.ReadFile(filepath.Join(f.b.machine.dir, "capacity.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.b.TryAcquireTask(f.ref.ID, f.order, "openshell"); !errors.Is(err, ErrDeferred) || !strings.Contains(err.Error(), "original preparation operator policy changed") {
		t.Fatal("CPU availability correction loosened original frozen identity", err)
	}
	after, err := os.ReadFile(filepath.Join(f.b.machine.dir, "capacity.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("frozen claim refusal changed journal", err)
	}
}
