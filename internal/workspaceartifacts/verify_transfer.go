package workspaceartifacts

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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

// VerifyCapturedBaseline checks only the original transferable file manifest.
// It never captures a new checkout or opens unrelated cache/unlisted bytes.
func VerifyCapturedBaseline(ctx context.Context, baseline *Baseline) error {
	if baseline == nil || baseline.Head == "" || baseline.IndexSHA256 == "" || baseline.Entries == nil {
		return fmt.Errorf("captured baseline manifest missing")
	}
	root := baseline.Directory
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("captured baseline is not its original directory")
	}
	for name, want := range baseline.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		got, _, err := readEntry(root, name)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("captured baseline entry changed: %s", name)
		}
	}
	return filepath.WalkDir(root, func(full string, item fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if full == root {
			return nil
		}
		relative, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if item.IsDir() {
			return nil
		}
		if _, ok := baseline.Entries[name]; !ok {
			return fmt.Errorf("unexpected captured baseline entry: %s", name)
		}
		return nil
	})
}
