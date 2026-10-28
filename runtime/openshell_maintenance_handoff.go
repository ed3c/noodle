package runtime

import (
	"fmt"
	"reflect"
)

func validateMaintenanceHostHandoff(c ProgressMaintenanceCompletion, h ProgressMaintenanceHostHandoff) error {
	var p ProgressMaintenance
	if err := maintenanceVerify(c.Plan, &p); err != nil {
		return err
	}
	if h.HostInstance != p.HostInstance {
		return fmt.Errorf("maintenance Host handover generation differs")
	}
	var request struct {
		RequestID        string `json:"request_id"`
		ProjectDir       string `json:"project_dir"`
		BinarySHA256     string `json:"binary_sha256"`
		ConfigSHA256     string `json:"config_sha256"`
		ConfigPath       string `json:"config_path"`
		HostDir          string `json:"host_dir"`
		Mode             string `json:"mode"`
		CleanupCompleted bool   `json:"cleanup_completed"`
	}
	if err := maintenanceVerify(p.Request, &request); err != nil {
		return err
	}
	var before, after []map[string]any
	if err := maintenanceVerify(h.HostReceiptsBefore, &before); err != nil {
		return err
	}
	if err := maintenanceVerify(h.HostReceiptsAfter, &after); err != nil {
		return err
	}
	if len(before) != len(after) {
		return fmt.Errorf("maintenance Host history was replaced")
	}
	matches := 0
	for i, row := range before {
		if row["request_id"] != request.RequestID {
			if !reflect.DeepEqual(row, after[i]) {
				return fmt.Errorf("maintenance changed another Host receipt")
			}
			continue
		}
		matches++
		configPath, _ := row["config_path"].(string)
		hostDir, _ := row["host_dir"].(string)
		mode, _ := row["mode"].(string)
		cleanup, _ := row["cleanup_completed"].(bool)
		if configPath != request.ConfigPath || hostDir != request.HostDir || mode != request.Mode || cleanup != request.CleanupCompleted {
			return fmt.Errorf("maintenance original Host configuration binding differs")
		}
		if row["host_instance_id"] != p.HostInstance || row["host_pid"] != float64(p.HostPID) || row["context_id"] != p.ContextID || row["project_dir"] != request.ProjectDir || row["binary_sha256"] != request.BinarySHA256 || row["config_sha256"] != request.ConfigSHA256 || (row["context_state"] != "failed" && row["context_state"] != "active") {
			return fmt.Errorf("maintenance original failed Host receipt differs")
		}
		row["context_state"], row["terminal_cause"] = "detached", "maintenance-handoff"
		if !reflect.DeepEqual(row, after[i]) {
			return fmt.Errorf("maintenance Host release changed original task bindings")
		}
	}
	if matches != 1 {
		return fmt.Errorf("maintenance Host receipt is missing or ambiguous")
	}
	release, exit, err := ValidateProgressMaintenanceClosure(c.Plan, p)
	if err != nil {
		return err
	}
	if h.Release != release || h.HostExit != exit {
		return fmt.Errorf("maintenance Host closure sources differ")
	}
	return nil
}
