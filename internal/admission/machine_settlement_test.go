package admission

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/filex"
)

func settlementFixture(t *testing.T) (*Budget, *Permit, MachineSessionLease, MachineReclamation) {
	t.Helper()
	root := capacityFixtureRoot(t)
	b, _ := capacityFixtureBudget(t, filepath.Join(root, "machine"), filepath.Join(root, "quota"), "original-generation", capacityFixtureClaim(root))
	p, err := b.TryAcquireTask("original-generation", "original-order", "openshell")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.StartResourceEffects(); err != nil {
		t.Fatal(err)
	}
	binding := MachineSessionBinding{ContextID: "original-generation", ProjectDir: root,
		OrderID: "original-order", AttemptID: "canonical-attempt", SessionID: "native-session", Runtime: "openshell",
		WorktreePath: filepath.Join(root, ".worktrees", "original-worker")}
	if err := p.BindSession(binding); err != nil {
		t.Fatal(err)
	}
	leases, err := b.MachineSessionLeases(binding.ContextID, root)
	if err != nil || len(leases) != 1 || leases[0].Binding == nil {
		t.Fatal(leases, err)
	}
	proof := MachineReclamation{Binding: binding, CheckoutOperation: "original-reclamation",
		Sources: map[string]MachineSettlementFile{}, RuntimeState: "retired", NativeAttemptID: "original-native-attempt",
		SandboxID: "original-native-uuid", NativeDeletionOutcome: "DELETION_OUTCOME_COMPLETED"}
	for _, name := range []string{"main_ack", "checkout_packet", "checkout_removal", "checkout_verification_release"} {
		path := filepath.Join(root, name+".json")
		raw := []byte("original verified " + name)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		proof.Sources[name] = MachineSettlementFile{Path: path, SHA256: hex.EncodeToString(sum[:])}
	}
	p.Release() // Native completion and cleanup proof are independent of quota.
	return b, p, leases[0], proof
}

func TestMachineSettlementSameOriginalLeaseReturnsExactlyOnce(t *testing.T) {
	b, _, lease, proof := settlementFixture(t)
	if s := b.Snapshot(); s.Active != 0 || s.MachineCapacity.Leases != 1 {
		t.Fatal("worker exit returned physical resources", s)
	}
	writes := 0
	b.machine.write = func(path string, data []byte) error {
		writes++
		return filex.WriteFileAtomicDurable(path, data)
	}
	for range 2 {
		if err := b.SettleMachineSession(lease, proof); err != nil {
			t.Fatal(err)
		}
	}
	if writes != 1 || b.Snapshot().MachineCapacity.Leases != 0 {
		t.Fatal("settlement replayed or kept capacity", writes)
	}
	if rows, err := b.MachineSessionLeases(proof.Binding.ContextID, proof.Binding.ProjectDir); err != nil || len(rows) != 0 {
		t.Fatal("settled original lease remained actionable", rows, err)
	}
}

func TestMachineSettlementPolicyChangeRequiresAllResourcesReturned(t *testing.T) {
	for _, kind := range []string{"unstarted-returned", "held", "settled"} {
		t.Run(kind, func(t *testing.T) {
			var b *Budget
			var root string
			var originalID string
			var tombstone machineLease
			if kind == "settled" {
				var lease MachineSessionLease
				var proof MachineReclamation
				b, _, lease, proof = settlementFixture(t)
				root, originalID = proof.Binding.ProjectDir, lease.ID
				if err := b.SettleMachineSession(lease, proof); err != nil {
					t.Fatal(err)
				}
				if err := b.machine.access(false, func(state *machineState) (bool, error) {
					tombstone = state.Leases[originalID]
					return false, nil
				}); err != nil || tombstone.Settlement == nil {
					t.Fatal("original settlement tombstone missing", err)
				}
			} else {
				root = capacityFixtureRoot(t)
				b, _ = capacityFixtureBudget(t, filepath.Join(root, "machine"), filepath.Join(root, "original-quota"), "original", capacityFixtureClaim(root))
				permit, err := b.TryAcquireTask("original", "old-task", "openshell")
				if err != nil {
					t.Fatal(err)
				}
				if kind == "held" {
					if err := permit.StartResourceEffects(); err != nil {
						t.Fatal(err)
					}
				}
				permit.Release()
			}
			claim := capacityFixtureClaim(root)
			claim.Source, claim.Policy.DiskSafetyBytes = "new-unfrozen-profile", 40
			next, _ := capacityFixtureBudget(t, b.machine.dir, filepath.Join(root, "next-quota"), "next-host", claim)
			permit, err := next.TryAcquireTask("next-host", "new-task", "openshell")
			if kind == "held" {
				if permit != nil || !errors.Is(err, ErrDeferred) || !strings.Contains(err.Error(), "machine_operator_policy_differs_with_held_leases") {
					t.Fatal("policy update bypassed original held resources", err)
				}
				return
			}
			if err != nil {
				t.Fatal("returned resources still block fresh policy", err)
			}
			permit.Release()
			if kind == "settled" {
				if err := b.machine.access(false, func(state *machineState) (bool, error) {
					if !reflect.DeepEqual(state.Leases[originalID], tombstone) || state.Capacity == nil || *state.Capacity != claim.Policy {
						t.Fatal("policy update changed settlement history or failed to adopt policy")
					}
					return false, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestMachineSettlementLostACKReadsSameNonceAndKeepsSealedProof(t *testing.T) {
	for _, persisted := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-write", true: "after-write"}[persisted], func(t *testing.T) {
			b, _, lease, proof := settlementFixture(t)
			original := b.machine.write
			b.machine.write = func(path string, data []byte) error {
				if persisted {
					if err := original(path, data); err != nil {
						return err
					}
				}
				return errors.New("lost original settlement write acknowledgement")
			}
			if err := b.SettleMachineSession(lease, proof); err == nil {
				t.Fatal("lost ACK claimed synchronous completion")
			}
			b.machine.write = original
			if err := b.SettleMachineSession(lease, proof); err != nil {
				t.Fatal("original nonce did not reconcile", err)
			}
			if b.Snapshot().MachineCapacity.Leases != 0 {
				t.Fatal("original settlement did not return capacity")
			}
		})
	}
}

func TestMachineSettlementStartupReadsOnlyOwnBoundIdentity(t *testing.T) {
	b, _, lease, proof := settlementFixture(t)
	recovered := NewBudget(9)
	if err := recovered.BindMachineCapacity(b.machine.dir, b.machine.owner); err != nil {
		t.Fatal(err)
	}
	recovered.BindResourceSource(proof.Binding.ContextID, proof.Binding.Runtime, b.resourceSources[proof.Binding.ContextID].read)
	rows, err := recovered.MachineSessionLeases(proof.Binding.ContextID, proof.Binding.ProjectDir)
	if err != nil || len(rows) != 1 || !reflect.DeepEqual(rows[0], lease) {
		t.Fatal("startup guessed a new nonce or identity", rows, err)
	}
	if err := recovered.SettleMachineSession(rows[0], proof); err != nil {
		t.Fatal(err)
	}
	foreign := NewBudget(9)
	if err := foreign.BindMachineCapacity(b.machine.dir, "foreign-generation"); err != nil {
		t.Fatal(err)
	}
	if err := foreign.SettleMachineSession(lease, proof); err == nil {
		t.Fatal("foreign generation released original resources")
	}
}

func TestMachineSettlementMissingBindingAndIncompleteProofKeepNamedHold(t *testing.T) {
	b, _, lease, proof := settlementFixture(t)
	for _, kind := range []string{"session", "attempt", "owner", "source", "runtime", "retirement", "native-id", "missing-restore", "changed-source"} {
		t.Run(kind, func(t *testing.T) {
			original, candidate := lease, proof
			candidate.Sources = make(map[string]MachineSettlementFile)
			for key, value := range proof.Sources {
				candidate.Sources[key] = value
			}
			switch kind {
			case "session":
				candidate.Binding.SessionID = "foreign-session"
			case "attempt":
				candidate.Binding.AttemptID = "foreign-attempt"
			case "owner":
				original.Owner = "foreign-generation"
			case "source":
				original.Source = "foreign-profile"
			case "runtime":
				candidate.Binding.Runtime = "process"
			case "retirement":
				candidate.RuntimeState = "pending"
			case "native-id":
				candidate.SandboxID = ""
			case "missing-restore":
				delete(candidate.Sources, "checkout_verification_release")
			case "changed-source":
				ref := candidate.Sources["checkout_packet"]
				ref.SHA256 = "00"
				candidate.Sources["checkout_packet"] = ref
			}
			if err := b.SettleMachineSession(original, candidate); err == nil {
				t.Fatal("incomplete or foreign proof returned original resources")
			}
			if b.Snapshot().MachineCapacity.Leases != 1 {
				t.Fatal("refusal changed original lease")
			}
		})
	}
	if err := b.machine.access(false, func(state *machineState) (bool, error) {
		row := state.Leases[lease.ID]
		row.Binding = nil
		state.Leases[lease.ID] = row
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := b.MachineSessionLeases(proof.Binding.ContextID, proof.Binding.ProjectDir)
	if err != nil || len(rows) != 1 || rows[0].Binding != nil || rows[0].HoldReason != "original_machine_session_binding_missing" {
		t.Fatal("old unbound lease was expired or adopted", rows, err)
	}
	if err := b.SettleMachineSession(lease, proof); err == nil {
		t.Fatal("missing original binding allowed settlement")
	}
}

func TestMachineSettlementChangedOriginalProfileKeepsResources(t *testing.T) {
	b, _, lease, proof := settlementFixture(t)
	original := b.resourceSources[proof.Binding.ContextID].read
	b.resourceSources[proof.Binding.ContextID].read = func(runtime string) (ResourceClaim, error) {
		claim, err := original(runtime)
		claim.Source = "changed-original-profile"
		return claim, err
	}
	if err := b.SettleMachineSession(lease, proof); err == nil || b.Snapshot().MachineCapacity.Leases != 1 {
		t.Fatal("changed original profile released resources", err)
	}
}

func TestMachineSettlementRetiredReadbackKeepsLostNativeACKHistory(t *testing.T) {
	b, _, lease, proof := settlementFixture(t)
	proof.NativeDeletionOutcome = "unknown"
	if err := b.SettleMachineSession(lease, proof); err != nil {
		t.Fatal("verified native absence was confused with missing deletion acknowledgement", err)
	}
	if err := b.machine.access(false, func(state *machineState) (bool, error) {
		if state.Leases[lease.ID].Settlement.NativeDeletionOutcome != "unknown" {
			t.Fatal("lost native ACK history changed")
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMachineSessionBindingLostACKReadsSameOriginalPermit(t *testing.T) {
	for _, persisted := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-write", true: "after-write"}[persisted], func(t *testing.T) {
			b, _, _, proof := settlementFixture(t)
			p, err := b.TryAcquireTask(proof.Binding.ContextID, "next-original-order", proof.Binding.Runtime)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Release()
			if err := p.StartResourceEffects(); err != nil {
				t.Fatal(err)
			}
			binding := proof.Binding
			binding.OrderID, binding.AttemptID, binding.SessionID = "next-original-order", "next-canonical-attempt", "next-native-session"
			nonce := p.allocation.machineID
			write := b.machine.write
			b.machine.write = func(path string, data []byte) error {
				if persisted {
					if err := write(path, data); err != nil {
						return err
					}
				}
				return errors.New("lost original session binding acknowledgement")
			}
			if err := p.BindSession(binding); err == nil {
				t.Fatal("lost binding ACK claimed completion")
			}
			b.machine.write = write
			if err := p.BindSession(binding); err != nil || p.allocation.machineID != nonce {
				t.Fatal("binding readback changed original permit", err)
			}
		})
	}
}
