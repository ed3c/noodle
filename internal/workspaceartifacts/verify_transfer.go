package workspaceartifacts

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
)

// VerifyBaseline proves that no delivery has changed the captured checkout.
func VerifyBaseline(ctx context.Context, target string, baseline *Baseline) error {
	if baseline == nil {
		return fmt.Errorf("artifact baseline is missing")
	}
	root, err := repositoryRoot(ctx, target)
	if err != nil {
		return err
	}
	return unchanged(ctx, root, baseline)
}

// VerifyExtracted compares the complete retained directory with the verified
// archive through the same safe extractor. It does not mutate retained evidence.
func VerifyExtracted(ctx context.Context, archive, retained string) (resultErr error) {
	info, err := os.Lstat(retained)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("retained extraction is not a directory")
	}
	temporary, err := os.MkdirTemp("", "noodle-transfer-verify-")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(temporary)) }()
	root, err := filepath.EvalSymlinks(temporary)
	if err != nil {
		return err
	}
	expected := filepath.Join(root, "expected")
	if err := Extract(ctx, archive, expected); err != nil {
		return err
	}
	want, _, err := scanDirectory(expected)
	if err != nil {
		return err
	}
	got, _, err := scanDirectory(retained)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("retained extraction differs from original archive")
	}
	return ctx.Err()
}
