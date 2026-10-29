package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// A terminal display claim alone cannot move a session out of process custody.
// Only the explicit native handoff delegates this history to OpenShell Recover,
// which must still obtain fresh original native readback before admission.
func maintainedRecoveryBinding(directory string) (*CompletedSessionBinding, error) {
	var d openShellDispatchReceipt
	_, err := ReadSessionReadbackFile(filepath.Join(directory, "openshell-dispatch.json"), &d)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if d.Maintenance == nil {
		return nil, nil
	}
	c, err := maintenanceCompletion(d)
	if err != nil {
		return nil, err
	}
	var p ProgressMaintenance
	if err := maintenanceVerify(c.Plan, &p); err != nil {
		return nil, err
	}
	if p.SessionDirectory != directory || p.Sources["dispatch"].Path != filepath.Join(directory, "openshell-dispatch.json") || d.Version != 1 || d.SessionID != filepath.Base(directory) || d.Phase != "settled" || d.Outcome.Status != StatusCancelled || d.ProgressHead == "" || d.CandidateHead != "" {
		return nil, fmt.Errorf("maintenance recovery belongs to a different native session")
	}
	if err := VerifyProgressMaintenanceHandoff(c.Plan); err != nil {
		return nil, err
	}
	// Spawn uses snake_case public fields; retain the exact original source ref
	// rather than accepting a newly reconstructed identity.
	var spawn struct {
		SessionID    string `json:"session_id"`
		OrderID      string `json:"order_id"`
		Runtime      string `json:"runtime"`
		Provider     string `json:"provider"`
		Model        string `json:"model"`
		WorktreePath string `json:"worktree_path"`
		StageIndex   string `json:"stage_index"`
		RetryCount   int    `json:"retry_count"`
	}
	ref, err := ReadSessionReadbackFile(filepath.Join(directory, "spawn.json"), &spawn)
	if err != nil {
		return nil, err
	}
	if ref.SHA256 != p.Originals["spawn"].SHA256 || spawn.SessionID != d.SessionID || spawn.OrderID != d.OrderID || spawn.Runtime != "openshell" || spawn.WorktreePath != d.Target || spawn.Provider == "" || spawn.Model == "" || spawn.RetryCount < 0 {
		return nil, fmt.Errorf("maintenance original spawn identity changed")
	}
	index, err := strconv.Atoi(spawn.StageIndex)
	if err != nil || index != 0 {
		return nil, fmt.Errorf("maintenance original stage identity differs")
	}
	return &CompletedSessionBinding{OrderID: d.OrderID, SessionID: d.SessionID, WorktreePath: d.Target, Provider: spawn.Provider, Model: spawn.Model, StageIndex: index, RetryCount: spawn.RetryCount}, nil
}
