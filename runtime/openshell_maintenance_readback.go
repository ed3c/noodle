package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// ReadMaintainedProgress only recognizes an explicitly restored terminal
// attempt. Ordinary unknown sessions retain the normal recovery refusal.
func (r *OpenShellRuntime) ReadMaintainedProgress(ctx context.Context, b CompletedSessionBinding) (bool, error) {
	if b.SessionID == "" || filepath.Base(b.SessionID) != b.SessionID || b.SessionID == "." || b.SessionID == ".." {
		return false, fmt.Errorf("maintenance session identity invalid")
	}
	var d openShellDispatchReceipt
	_, err := ReadSessionReadbackFile(filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID, "openshell-dispatch.json"), &d)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if d.Maintenance == nil {
		return false, nil
	}
	if _, err := maintenanceCompletion(d); err != nil {
		return false, err
	}
	_, err = r.ReadSavedProgress(ctx, b)
	return err == nil, err
}
