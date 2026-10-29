package instructionbundle

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func noSymlinks(name string) error {
	if !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return fmt.Errorf("instruction local path is not canonical absolute")
	}
	volume := filepath.VolumeName(name)
	current := volume + string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(name, current), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect instruction path: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("instruction path contains symlink: %s", current)
		}
	}
	return nil
}

func readRegular(ctx context.Context, name string, limit int64, mode *uint32) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := noSymlinks(name); err != nil {
		return nil, err
	}
	before, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode() != before.Mode().Perm() || before.Size() > limit {
		return nil, fmt.Errorf("instruction file is not a bounded regular file")
	}
	if mode != nil && uint32(before.Mode().Perm()) != *mode {
		return nil, fmt.Errorf("instruction file mode differs")
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, opened) {
		return nil, fmt.Errorf("instruction file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	after, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(data) > int(limit) || !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() || before.Mode() != after.Mode() {
		return nil, fmt.Errorf("instruction file changed or exceeded limit while reading")
	}
	if err := noSymlinks(name); err != nil {
		return nil, err
	}
	return data, nil
}

func writeNew(name string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	closed := f.Close()
	if err != nil {
		return err
	}
	return closed
}
