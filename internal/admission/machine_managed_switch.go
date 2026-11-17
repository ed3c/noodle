package admission

import (
	"encoding/hex"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
)

// ManagedResidentSwitchFence preserves the original resident and exact successor.
// Legacy strict readers HOLD before effects when they encounter this field.
type ManagedResidentSwitchFence struct {
	HostRoot         string                `json:"host_root"`
	SourceGeneration string                `json:"source_generation"`
	SourceBinary     MachineSettlementFile `json:"source_binary"`
	Request          string                `json:"version_request"`
	TargetBinary     MachineSettlementFile `json:"target_binary"`
	Preparation      PreparationRef        `json:"target_preparation"`
}

func managedSwitchHex(value string, size int) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == size
}

func validateManagedResidentSwitch(row machineLease) error {
	f := row.ManagedSwitch
	if f == nil {
		return nil
	}
	if row.Preparation == nil || row.Preparation.Source.Purpose != "host-resident" || !row.EffectsPossible || row.ContextID != "" || row.PreparationPromoted || row.EmptyRetirement != nil || f.HostRoot != row.Preparation.Source.Bindings["host_root"] || !filepath.IsAbs(f.HostRoot) || filepath.Clean(f.HostRoot) != f.HostRoot || !managedSwitchHex(f.SourceGeneration, 16) || !managedSwitchHex(f.Request, 16) || !managedSwitchHex(f.Preparation.ID, 16) || !managedSwitchHex(f.Preparation.SHA256, 32) || f.SourceBinary.Path != row.Preparation.Source.Bindings["binary_path"] || f.SourceBinary.SHA256 != row.Preparation.Source.Bindings["binary_sha256"] || !managedSwitchHex(f.SourceBinary.SHA256, 32) || f.TargetBinary == f.SourceBinary || !filepath.IsAbs(f.TargetBinary.Path) || filepath.Clean(f.TargetBinary.Path) != f.TargetBinary.Path || !managedSwitchHex(f.TargetBinary.SHA256, 32) {
		return errors.New("original managed switch resident source differs")
	}
	return nil
}

func managedSwitchTarget(state *machineState, f *ManagedResidentSwitchFence) (machineLease, error) {
	row, ok := state.Leases[f.Preparation.ID]
	if !ok || row.Preparation == nil || row.Preparation.SHA256 != f.Preparation.SHA256 || row.Preparation.Source.Purpose != "host-resident" || row.Preparation.Source.Bindings["host_root"] != f.HostRoot || row.Preparation.Source.Bindings["binary_path"] != f.TargetBinary.Path || row.Preparation.Source.Bindings["binary_sha256"] != f.TargetBinary.SHA256 || row.Preparation.Source.Bindings["managed_switch"] != f.Request {
		return row, errors.New("original managed switch successor missing or changed")
	}
	return row, nil
}

func refuseManagedSwitchSource(state *machineState, source PreparationSource) error {
	for _, row := range state.Leases {
		f := row.ManagedSwitch
		if f == nil || source.Bindings["host_root"] != f.HostRoot {
			continue
		}
		path, sha := source.Bindings["binary_path"], source.Bindings["binary_sha256"]
		if source.Purpose == "task-preparation" {
			joined := source.Bindings["execution_binary"]
			cut := strings.LastIndex(joined, "/")
			if cut >= 0 {
				path, sha = joined[:cut], joined[cut+1:]
			}
		}
		if path == "" || sha == "" || path == f.SourceBinary.Path || sha == f.SourceBinary.SHA256 {
			return errors.New("original execution generation fenced for managed switch")
		}
	}
	return nil
}

func refuseManagedSwitchCancellation(state *machineState, ref PreparationRef) error {
	for _, row := range state.Leases {
		if row.ManagedSwitch != nil && row.ManagedSwitch.Preparation == ref {
			return errors.New("original managed successor reservation cannot be canceled")
		}
	}
	return nil
}

// FenceManagedResidentSwitch uses the same machine lock as every preparation
// producer. The foreground pool owner supplies its fresh zero-consumer readback.
func (b *Budget) FenceManagedResidentSwitch(ref PreparationRef, fence ManagedResidentSwitchFence, readback func(PreparationSource) error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.machine == nil || readback == nil {
		return errors.New("original managed switch owner missing")
	}
	return b.machine.access(false, func(state *machineState) (bool, error) {
		row, ok := state.Leases[ref.ID]
		if !ok || row.Owner != b.machine.owner || row.Preparation == nil || row.Preparation.SHA256 != ref.SHA256 {
			return false, errors.New("original managed resident lifetime differs")
		}
		if row.ManagedSwitch != nil {
			if !reflect.DeepEqual(*row.ManagedSwitch, fence) {
				return false, errors.New("original managed switch choice changed")
			}
			_, err := managedSwitchTarget(state, row.ManagedSwitch)
			return false, err
		}
		if row.PreparationClosed != nil {
			return false, errors.New("original managed resident already closed without switch")
		}
		target, err := managedSwitchTarget(state, &fence)
		if err != nil {
			return false, err
		}
		if target.Owner != b.machine.owner || target.EffectsPossible || target.PreparationClosed != nil || target.Preparation.Source.Bindings["managed_switch_source"] != ref.ID || !reflect.DeepEqual(target.Preparation.Claim, row.Preparation.Claim) {
			return false, errors.New("original successor resident preparation differs")
		}
		for id, other := range state.Leases {
			if id == ref.ID || id == fence.Preparation.ID || other.Settlement != nil || other.PreparationClosed != nil {
				continue
			}
			if other.Preparation != nil && other.Preparation.Source.Bindings["host_root"] == fence.HostRoot || strings.HasPrefix(other.Owner, fence.HostRoot+"/") {
				return false, errors.New("original managed Host has unresolved machine consumers")
			}
		}
		row.ManagedSwitch = &fence
		if err := validateManagedResidentSwitch(row); err != nil {
			return false, err
		}
		if err := readback(row.Preparation.Source); err != nil {
			return false, err
		}
		state.Leases[ref.ID] = row
		return true, nil
	})
}
