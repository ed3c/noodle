package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/guestprogress"
)

func retainMaintenanceArchives(ctx context.Context, p ProgressMaintenance, saved guestprogress.Receipt) error {
	directory, err := os.MkdirTemp(p.SessionDirectory, "progress-archive-maintenance-")
	if err != nil {
		return err
	}
	for _, archive := range saved.Archives {
		if err := ctx.Err(); err != nil {
			return err
		}
		ref := p.Archives[archive.Name]
		if err := maintenanceVerify(ref, nil); err != nil {
			return err
		}
		data, err := os.ReadFile(ref.Path)
		if err != nil {
			return err
		}
		if err := maintenanceWriteBytes(filepath.Join(directory, archive.Name), data); err != nil {
			return err
		}
		copied, err := ReadSessionReadbackFile(filepath.Join(directory, archive.Name), nil)
		if err != nil {
			return err
		}
		if copied.SHA256 != archive.SHA256 || int64(len(data)) != archive.Bytes {
			return fmt.Errorf("maintenance retained archive differs: %s", archive.Name)
		}
	}
	return verifyCleanupProgressArchives(ctx, p.SessionDirectory, openShellDispatchReceipt{Progress: &saved})
}
