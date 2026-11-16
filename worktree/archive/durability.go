package archive

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// A local Git clone owns hardlinks to immutable objects, independently of the
// source's directory entries. Persist them before publishing a deletable copy.
func syncTree(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("archive Git pool contains a nonregular object: %s", path)
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		return errors.Join(f.Sync(), f.Close())
	})
	if err != nil {
		return err
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dir := range dirs {
		if err = syncDir(dir); err != nil {
			return err
		}
	}
	return syncDir(filepath.Dir(root))
}
