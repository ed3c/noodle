package admission

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/filex"
)

func emptyRetirementFile(t *testing.T, path string, raw []byte) MachineSettlementFile {
	t.Helper()
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	return MachineSettlementFile{Path: path, SHA256: hex.EncodeToString(digest[:])}
}

func emptyRetirementFixture(t *testing.T) (*Budget, PreparationRef, EmptyResidentRetirementFence, PreparationRef) {
	t.Helper()
	root := capacityFixtureRoot(t)
	claim := capacityFixtureClaim(root)
	b := preparationFixture(t, filepath.Join(root, "machine"), claim)
	unknown := preparationFixtureSource(root)
	unknown.Bindings["host_root"] = filepath.Join(root, "a")
	a, err := b.reservePreparation(unknown, claim)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.StartPreparationEffects(a); err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(root, "b")
	if err = os.Mkdir(host, 0700); err != nil {
		t.Fatal(err)
	}
	source := PreparationSource{Purpose: "host-resident", Bindings: map[string]string{"host_root": host, "binary_path": "/original/noodle", "binary_sha256": strings.Repeat("b", 64)}}
	ref, err := b.reservePreparation(source, claim)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.StartPreparationEffects(ref); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(b.machine.dir, "capacity.json"))
	if err != nil {
		t.Fatal(err)
	}
	fence := EmptyResidentRetirementFence{HostRoot: host, Generation: strings.Repeat("b", 32),
		Intent:    emptyRetirementFile(t, filepath.Join(host, "empty-retirement-intent.json"), []byte(`{"schema":1,"fixture":"original empty Host owner"}`)),
		Inventory: emptyRetirementFile(t, filepath.Join(host, "empty-retirement-machine-before.json"), raw)}
	return b, ref, fence, a
}

func TestEmptyResidentRetirementFenceSameRowAndLatePreparation(t *testing.T) {
	b, ref, fence, a := emptyRetirementFixture(t)
	path := filepath.Join(b.machine.dir, "capacity.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var original struct {
		Leases map[string]json.RawMessage `json:"leases"`
	}
	if err = json.Unmarshal(before, &original); err != nil {
		t.Fatal(err)
	}
	late := preparationFixtureSource(fence.HostRoot)
	late.Bindings["host_root"] = fence.HostRoot
	claim := capacityFixtureClaim(filepath.Dir(fence.HostRoot))
	// The source/claim have already been selected before the original Host
	// fence. Resume the producer only after retirement has returned its row.
	resume, lateResult := make(chan struct{}), make(chan error, 1)
	go func() { <-resume; _, err := b.reservePreparation(late, claim); lateResult <- err }()
	if err = b.FenceEmptyResidentPreparation(ref, fence, func(source PreparationSource) error {
		if source.Purpose != "host-resident" || source.Bindings["host_root"] != fence.HostRoot {
			return errors.New("original native source differs")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = b.ReadEmptyResidentRetirement(ref, fence); err != nil {
		t.Fatal(err)
	}
	if evidence := os.Getenv("NOODLE_EMPTY_RETIREMENT_TEST_EVIDENCE"); evidence != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(evidence, "fenced.capacity.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	proof := emptyRetirementFile(t, filepath.Join(fence.HostRoot, "original-exit.json"), []byte("original Actor exit fixture"))
	for range 2 {
		if err = b.CloseResidentPreparation(ref, func(PreparationSource) (map[string]MachineSettlementFile, error) {
			return map[string]MachineSettlementFile{"resident_exit": proof}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	close(resume)
	if err = <-lateResult; err == nil || !strings.Contains(err.Error(), "fenced") {
		t.Fatal("late selected control acquired resources after resident retirement", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var current struct {
		Leases map[string]json.RawMessage `json:"leases"`
	}
	if err = json.Unmarshal(after, &current); err != nil || !bytes.Equal(original.Leases[a.ID], current.Leases[a.ID]) || len(current.Leases) != 2 {
		t.Fatal("retirement rewrote unknown or minted another lease", err)
	}
	if err = b.machine.access(false, func(state *machineState) (bool, error) {
		row := state.Leases[ref.ID]
		if row.EmptyRetirement == nil || row.PreparationClosed == nil || row.CPUs == 0 || row.MemoryBytes == 0 {
			return false, errors.New("same original fenced row was deleted or relabelled")
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyResidentRetirementLostFenceACKReadsOnce(t *testing.T) {
	b, ref, fence, _ := emptyRetirementFixture(t)
	writes := 0
	b.machine.write = func(path string, raw []byte) error {
		writes++
		if err := filex.WriteFileAtomicDurable(path, raw); err != nil {
			return err
		}
		return errors.New("original fence ACK lost")
	}
	if err := b.FenceEmptyResidentPreparation(ref, fence, func(PreparationSource) error { return nil }); err == nil {
		t.Fatal("lost ACK became success")
	}
	if _, err := b.ReadEmptyResidentRetirement(ref, fence); err != nil {
		t.Fatal(err)
	}
	if err := b.FenceEmptyResidentPreparation(ref, fence, func(PreparationSource) error { return errors.New("effect replayed") }); err != nil || writes != 1 {
		t.Fatal("fence replayed instead of reading the original row", writes, err)
	}
}

func TestEmptyResidentRetirementRefusesTaskOrDriftingInventory(t *testing.T) {
	for _, fault := range []string{"task", "inventory", "foreign"} {
		t.Run(fault, func(t *testing.T) {
			b, ref, fence, _ := emptyRetirementFixture(t)
			if fault == "task" {
				source := preparationFixtureSource(fence.HostRoot)
				source.Bindings["host_root"] = fence.HostRoot
				if _, err := b.reservePreparation(source, capacityFixtureClaim(filepath.Dir(fence.HostRoot))); err != nil {
					t.Fatal(err)
				}
			} else if fault == "inventory" {
				if err := os.WriteFile(fence.Inventory.Path, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				fence.HostRoot = filepath.Dir(fence.HostRoot)
			}
			before, err := os.ReadFile(filepath.Join(b.machine.dir, "capacity.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err = b.FenceEmptyResidentPreparation(ref, fence, func(PreparationSource) error { return nil }); err == nil {
				t.Fatal("unsafe original lifetime was fenced")
			}
			after, err := os.ReadFile(filepath.Join(b.machine.dir, "capacity.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("refused fence changed the journal", err)
			}
		})
	}
}
