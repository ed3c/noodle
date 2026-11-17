//go:build darwin || linux

package admission

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/filex"
)

func managedSwitchFixture(t *testing.T) (*Budget, PreparationRef, PreparationRef, PreparationSource, ResourceClaim, ManagedResidentSwitchFence) {
	t.Helper()
	root := capacityFixtureRoot(t)
	claim := capacityFixtureClaim(root)
	b := preparationFixture(t, filepath.Join(root, "machine"), claim)
	source := PreparationSource{Purpose: "host-resident", Bindings: map[string]string{"host_root": root, "goal_path": filepath.Join(root, "goal"), "goal_sha256": strings.Repeat("e", 64), "binary_path": filepath.Join(root, "old"), "binary_sha256": strings.Repeat("a", 64)}}
	old, err := b.reservePreparation(source, claim)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.StartPreparationEffects(old); err != nil {
		t.Fatal(err)
	}
	targetSource := PreparationSource{Purpose: "host-resident", Bindings: map[string]string{"host_root": root, "binary_path": filepath.Join(root, "target"), "binary_sha256": strings.Repeat("b", 64), "managed_switch": strings.Repeat("c", 32), "managed_switch_source": old.ID}}
	target, err := b.reservePreparation(targetSource, claim)
	if err != nil {
		t.Fatal(err)
	}
	fence := ManagedResidentSwitchFence{HostRoot: root, SourceGeneration: strings.Repeat("d", 32), SourceBinary: MachineSettlementFile{Path: source.Bindings["binary_path"], SHA256: source.Bindings["binary_sha256"]}, Request: targetSource.Bindings["managed_switch"], TargetBinary: MachineSettlementFile{Path: targetSource.Bindings["binary_path"], SHA256: targetSource.Bindings["binary_sha256"]}, Preparation: target}
	return b, old, target, source, claim, fence
}

func TestManagedSwitchFenceRetainsExactNonceAcrossLostACK(t *testing.T) {
	b, old, target, source, claim, fence := managedSwitchFixture(t)
	write := b.machine.write
	b.machine.write = func(path string, raw []byte) error {
		if err := filex.WriteFileAtomicDurable(path, raw); err != nil {
			return err
		}
		return errors.New("fence ACK lost")
	}
	reads := 0
	reader := func(PreparationSource) error { reads++; return nil }
	if err := b.FenceManagedResidentSwitch(old, fence, reader); err == nil {
		t.Fatal("unknown fence ACK was success")
	}
	b.machine.write = write
	if err := b.FenceManagedResidentSwitch(old, fence, reader); err != nil || reads != 1 {
		t.Fatal("fence replayed effects", err, reads)
	}
	if err := b.CancelUnstartedPreparation(target); err == nil {
		t.Fatal("fenced successor nonce canceled")
	}
	if _, err := b.reservePreparation(source, claim); err == nil {
		t.Fatal("old producer minted/read a lifetime")
	}
	if err := b.StartPreparationEffects(old); err == nil {
		t.Fatal("old cached producer crossed fence")
	}
	if err := b.StartPreparationEffects(target); err == nil {
		t.Fatal("successor started before original exit closure")
	}
	if err := b.machine.access(false, func(state *machineState) (bool, error) {
		if len(state.Leases) != 2 || state.Leases[old.ID].ManagedSwitch.Preparation != target || state.Leases[target.ID].EffectsPossible {
			t.Fatal("original history or reservation changed")
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestManagedSwitchLatePreparationWinsMachineLockAndPreventsFence(t *testing.T) {
	b, old, _, source, claim, fence := managedSwitchFixture(t)
	late := PreparationSource{Purpose: "task-preparation", Bindings: map[string]string{"host_root": source.Bindings["host_root"], "execution_binary": source.Bindings["binary_path"] + "/" + source.Bindings["binary_sha256"]}}
	if _, err := b.reservePreparation(late, claim); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := b.FenceManagedResidentSwitch(old, fence, func(PreparationSource) error { called = true; return nil }); err == nil || called {
		t.Fatal("late original prep was ignored", err)
	}
	if err := b.CancelUnstartedPreparation(fence.Preparation); err != nil {
		t.Fatal("unfenced reservation incorrectly frozen", err)
	}
}

func TestManagedSwitchChecksForeignActiveGoalInsideMachineLock(t *testing.T) {
	for _, kind := range []string{"foreign-task", "unknown-task", "same-goal-unknown", "foreign-resident"} {
		t.Run(kind, func(t *testing.T) {
			b, old, _, source, claim, fence := managedSwitchFixture(t)
			foreign := PreparationSource{Purpose: "task-preparation", Bindings: map[string]string{
				"host_root": filepath.Join(fence.HostRoot, "other-host"),
				"goal_path": source.Bindings["goal_path"], "goal_sha256": source.Bindings["goal_sha256"],
			}}
			// A separate Host does not share this root's owner namespace.
			if kind == "foreign-task" {
				foreign.Bindings["goal_sha256"] = strings.Repeat("f", 64)
			}
			if kind == "unknown-task" {
				delete(foreign.Bindings, "goal_path")
			}
			if kind == "foreign-resident" {
				foreign.Purpose = "host-resident"
			}
			ref, err := b.reservePreparation(foreign, claim)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "same-goal-unknown" {
				if err = b.StartPreparationEffects(ref); err != nil {
					t.Fatal(err)
				}
			}
			err = b.FenceManagedResidentSwitch(old, fence, func(PreparationSource) error { return nil })
			allowed := kind == "same-goal-unknown" || kind == "foreign-resident"
			if (err == nil) != allowed {
				t.Fatal("machine-locked participant boundary differs", kind, err)
			}
			if err = b.machine.access(false, func(state *machineState) (bool, error) {
				retained, exists := state.Leases[ref.ID]
				if !exists || retained.PreparationClosed != nil || retained.Settlement != nil || retained.Preparation.Source.Bindings["goal_sha256"] != foreign.Bindings["goal_sha256"] {
					t.Fatal("original participant history changed")
				}
				return false, nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
