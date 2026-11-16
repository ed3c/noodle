//go:build darwin || linux

package admission

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/filex"
)

func TestMachineResidentPreparationReturnsOnlyOriginalExitAndReadsLostACK(t *testing.T) {
	root := capacityFixtureRoot(t)
	claim := capacityFixtureClaim(root)
	b := preparationFixture(t, filepath.Join(root, "machine"), claim)
	source := preparationFixtureSource(root)
	source.Purpose = "host-resident"
	ref, err := b.reservePreparation(source, claim)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.StartPreparationEffects(ref); err != nil {
		t.Fatal(err)
	}
	if err = b.BindPreparation(ref, "context", "order", "openshell"); err == nil {
		t.Fatal("resident consumed worker identity")
	}
	if err = b.CloseResidentPreparation(ref, func(PreparationSource) (map[string]MachineSettlementFile, error) {
		return nil, errors.New("original group still running or unknown")
	}); err == nil {
		t.Fatal("unknown exit released resources")
	}
	path := filepath.Join(root, "native-resident-exit.json")
	if err = os.WriteFile(path, []byte("original native exit evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	exit := MachineSettlementFile{Path: path, SHA256: hex.EncodeToString(digest[:])}
	write := b.machine.write
	b.machine.write = func(path string, raw []byte) error {
		if err := filex.WriteFileAtomicDurable(path, raw); err != nil {
			return err
		}
		return errors.New("original return ACK lost")
	}
	reads := 0
	reader := func(observed PreparationSource) (map[string]MachineSettlementFile, error) {
		reads++
		if observed.Purpose != "host-resident" {
			t.Fatal("wrong original lifetime")
		}
		return map[string]MachineSettlementFile{"resident_exit": exit}, nil
	}
	if err = b.CloseResidentPreparation(ref, reader); err == nil {
		t.Fatal("unknown return ACK reported success")
	}
	b.machine.write = write
	if err = b.CloseResidentPreparation(ref, reader); err != nil || reads != 1 {
		t.Fatal("lost ACK reran original exit or minted another lifetime", reads, err)
	}
	if err = b.StartPreparationEffects(ref); err == nil {
		t.Fatal("closed resident lifetime was started again")
	}
	if err = b.machine.access(false, func(state *machineState) (bool, error) {
		if len(state.Leases) != 1 || state.Leases[ref.ID].PreparationClosed["resident_exit"] != exit || !state.Leases[ref.ID].EffectsPossible {
			t.Fatal("original resident history was erased")
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	if snapshot := b.Snapshot().MachineCapacity; snapshot == nil || snapshot.Leases != 0 {
		t.Fatal("closed resident still deducted future capacity", snapshot)
	}
}
