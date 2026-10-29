package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	loopruntime "github.com/poteto/noodle/runtime"
)

func maintenanceOriginalAttempt(p loopruntime.ProgressMaintenance) error {
	var dispatch struct{ ContextID, OrderID, SessionID string }
	if _, err := loopruntime.ReadSessionReadbackFile(filepath.Join(p.SessionDirectory, "openshell-dispatch.json"), &dispatch); err != nil {
		return err
	}
	if dispatch.ContextID != p.ContextID || dispatch.SessionID != filepath.Base(p.SessionDirectory) {
		return fmt.Errorf("maintenance original dispatch identity differs")
	}
	type attempt struct {
		SessionID string `json:"session_id"`
		Status    string `json:"status"`
	}
	type stage struct {
		Runtime  string    `json:"runtime"`
		Attempts []attempt `json:"attempts"`
	}
	var snapshot struct {
		State struct {
			Orders map[string]struct {
				OrderID string  `json:"order_id"`
				Stages  []stage `json:"stages"`
			} `json:"orders"`
		} `json:"state"`
	}
	if _, err := loopruntime.ReadSessionReadbackFile(filepath.Join(filepath.Dir(filepath.Dir(p.SessionDirectory)), "state.snapshot.json"), &snapshot); err != nil {
		return err
	}
	active := 0
	for _, order := range snapshot.State.Orders {
		for _, stage := range order.Stages {
			for _, a := range stage.Attempts {
				if a.Status == "running" || a.Status == "launching" {
					active++
					if a.SessionID != dispatch.SessionID || order.OrderID != dispatch.OrderID || stage.Runtime != "openshell" {
						return fmt.Errorf("another original attempt owns the worker allocation")
					}
				}
			}
		}
	}
	if active != 1 {
		return fmt.Errorf("maintenance original active attempt is unavailable")
	}
	return nil
}

func verifyProgressMaintenanceTransactions(hostDir string) error {
	directory := filepath.Join(hostDir, "progress-maintenance")
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			return fmt.Errorf("maintenance transaction is not a regular file")
		}
		var ref loopruntime.SessionReadbackFileRef
		if _, err := loopruntime.ReadSessionReadbackFile(filepath.Join(directory, entry.Name()), &ref); err != nil {
			return err
		}
		var p loopruntime.ProgressMaintenance
		actual, err := loopruntime.ReadSessionReadbackFile(ref.Path, &p)
		if err != nil {
			return err
		}
		if actual != ref || p.HostDir != hostDir || entry.Name() != filepath.Base(p.SessionDirectory)+".json" {
			return fmt.Errorf("maintenance transaction source differs")
		}
		if err := loopruntime.VerifyProgressMaintenanceHandoff(ref); err != nil {
			return fmt.Errorf("original maintenance transaction is incomplete: %w", err)
		}
		var handoff loopruntime.ProgressMaintenanceHostHandoff
		if _, err := loopruntime.ReadSessionReadbackFile(filepath.Join(filepath.Dir(ref.Path), "host-handoff.json"), &handoff); err != nil {
			return err
		}
		var expected, live []hostReceipt
		if _, err := loopruntime.ReadSessionReadbackFile(handoff.HostReceiptsAfter.Path, &expected); err != nil {
			return err
		}
		if _, err := loopruntime.ReadSessionReadbackFile(filepath.Join(hostDir, "receipts.json"), &live); err != nil {
			return err
		}
		var request hostRequestBinding
		if _, err := loopruntime.ReadSessionReadbackFile(p.Request.Path, &request); err != nil {
			return err
		}
		matches := 0
		for _, original := range expected {
			if original.RequestID != request.RequestID {
				continue
			}
			for _, current := range live {
				if current.RequestID == request.RequestID {
					if !reflect.DeepEqual(current, original) {
						return fmt.Errorf("maintenance original Host history changed")
					}
					matches++
				}
			}
		}
		if matches != 1 {
			return fmt.Errorf("maintenance original Host release history is unavailable")
		}
	}
	return nil
}
