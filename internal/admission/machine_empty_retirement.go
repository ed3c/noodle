package admission

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// EmptyResidentRetirementFence belongs to the existing resident row. Legacy
// strict readers refuse the new field before writing; they cannot erase it.
type EmptyResidentRetirementFence struct {
	HostRoot   string                `json:"host_root"`
	Generation string                `json:"generation"`
	Intent     MachineSettlementFile `json:"intent"`
	Inventory  MachineSettlementFile `json:"inventory"`
}

func validateEmptyResidentRetirement(lease machineLease) error {
	fence := lease.EmptyRetirement
	if fence == nil {
		return nil
	}
	if lease.Preparation == nil || lease.Preparation.Source.Purpose != "host-resident" || !lease.EffectsPossible || lease.ContextID != "" || lease.PreparationPromoted || fence.HostRoot != lease.Preparation.Source.Bindings["host_root"] || !filepath.IsAbs(fence.HostRoot) || filepath.Clean(fence.HostRoot) != fence.HostRoot {
		return errors.New("empty resident retirement subject differs")
	}
	if id, err := hex.DecodeString(fence.Generation); err != nil || len(id) != 16 || fence.Intent.Path != filepath.Join(fence.HostRoot, "empty-retirement-intent.json") || fence.Inventory.Path != filepath.Join(fence.HostRoot, "empty-retirement-machine-before.json") {
		return errors.New("empty resident retirement source differs")
	}
	return verifyMachineSettlementFiles(map[string]MachineSettlementFile{"intent": fence.Intent, "inventory": fence.Inventory})
}

func refuseRetiredPreparationHost(state *machineState, source PreparationSource) error {
	if err := refuseManagedSwitchSource(state, source); err != nil {
		return err
	}
	root := source.Bindings["host_root"]
	if root == "" {
		return nil
	}
	for _, row := range state.Leases {
		if row.EmptyRetirement != nil && row.EmptyRetirement.HostRoot == root {
			return errors.New("original Host preparation is fenced for empty retirement")
		}
	}
	return nil
}

func emptyResidentRows(state *machineState, root, residentID string) error {
	for id, row := range state.Leases {
		if id == residentID {
			continue
		}
		if row.Preparation != nil && row.Preparation.Source.Bindings["host_root"] == root || strings.HasPrefix(row.Owner, root+"/") {
			return errors.New("original Host has another machine lifetime; empty retirement refused")
		}
	}
	return nil
}

// FenceEmptyResidentPreparation serializes the empty check and fence with all
// preparation producers. The native Goal/pool owner supplies the readback; an
// arbitrary FileRef or a caller assertion does not establish that authority.
func (b *Budget) FenceEmptyResidentPreparation(ref PreparationRef, fence EmptyResidentRetirementFence, readback func(PreparationSource) error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.machine == nil || readback == nil {
		return errors.New("original empty resident owner unavailable")
	}
	return b.machine.access(false, func(state *machineState) (bool, error) {
		row, exists := state.Leases[ref.ID]
		if !exists || row.Owner != b.machine.owner || row.Preparation == nil || row.Preparation.SHA256 != ref.SHA256 || row.Preparation.Source.Purpose != "host-resident" || row.Preparation.Source.Bindings["host_root"] != fence.HostRoot || !row.EffectsPossible || row.ContextID != "" {
			return false, errors.New("original empty resident lifetime differs")
		}
		if err := emptyResidentRows(state, fence.HostRoot, ref.ID); err != nil {
			return false, err
		}
		if row.EmptyRetirement != nil {
			if !reflect.DeepEqual(*row.EmptyRetirement, fence) {
				return false, errors.New("original empty resident retirement fence changed")
			}
			return false, nil
		}
		if row.PreparationClosed != nil {
			return false, errors.New("original resident was closed outside empty retirement")
		}
		row.EmptyRetirement = &fence
		if err := validateEmptyResidentRetirement(row); err != nil {
			return false, err
		}
		before, err := os.ReadFile(fence.Inventory.Path)
		if err != nil {
			return false, err
		}
		current, err := os.ReadFile(filepath.Join(b.machine.dir, "capacity.json"))
		if err != nil || !bytes.Equal(before, current) {
			return false, errors.New("original empty retirement machine inventory changed")
		}
		if err := readback(row.Preparation.Source); err != nil {
			return false, err
		}
		state.Leases[ref.ID] = row
		return true, nil
	})
}

// ReadEmptyResidentRetirement returns the original closure after a lost ACK.
func (b *Budget) ReadEmptyResidentRetirement(ref PreparationRef, fence EmptyResidentRetirementFence) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.machine == nil {
		return false, errors.New("original empty resident owner unavailable")
	}
	closed := false
	err := b.machine.access(false, func(state *machineState) (bool, error) {
		row, exists := state.Leases[ref.ID]
		if !exists || row.Owner != b.machine.owner || row.Preparation == nil || row.Preparation.SHA256 != ref.SHA256 || row.EmptyRetirement == nil || !reflect.DeepEqual(*row.EmptyRetirement, fence) {
			return false, errors.New("original empty resident fence is missing or changed")
		}
		closed = row.PreparationClosed != nil
		return false, emptyResidentRows(state, fence.HostRoot, ref.ID)
	})
	return closed, err
}
