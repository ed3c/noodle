package filex

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomicDurable persists the renamed file and its directory before
// callers remove the input that the file records. Unlike WriteFileAtomic,
// success is a persistence boundary, not just an atomic visibility boundary.
func WriteFileAtomicDurable(path string, data []byte) error {
	if err := WriteFileAtomic(path, data); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return fmt.Errorf("sync file: %w", syncErr)
	}
	if closeErr != nil {
		return closeErr
	}
	return SyncDir(filepath.Dir(path))
}

// SyncDir durably records renames/removals. Unsupported persistence is an
// error, so a caller cannot silently claim a durable retirement.
func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return fmt.Errorf("sync directory %s: %w", path, syncErr)
	}
	return closeErr
}
