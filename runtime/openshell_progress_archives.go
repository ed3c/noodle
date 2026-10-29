package runtime

import (
	"context"
	"fmt"
	"path/filepath"
)

func progressArchiveSources(ctx context.Context, dir string, receipt openShellDispatchReceipt) (map[string]SessionReadbackFileRef, error) {
	if err := verifyCleanupProgressArchives(ctx, dir, receipt); err != nil {
		return nil, err
	}
	refs := make(map[string]SessionReadbackFileRef)
	for _, archive := range receipt.Progress.Archives {
		paths, err := filepath.Glob(filepath.Join(dir, "progress-archive-*", archive.Name))
		if err != nil {
			return nil, err
		}
		if len(paths) != 1 {
			return nil, fmt.Errorf("retained progress archive is missing or ambiguous: %s", archive.Name)
		}
		ref, err := ReadSessionReadbackFile(paths[0], nil)
		if err != nil {
			return nil, err
		}
		if ref.SHA256 != archive.SHA256 {
			return nil, fmt.Errorf("retained progress archive changed: %s", archive.Name)
		}
		refs["progress_archive_"+archive.Name] = ref
	}
	return refs, nil
}
