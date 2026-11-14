package worktree

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
)

// PrepareStorageExcludes keeps owned runtime aliases out of tracked source.
// Git's info/exclude is repository-local and shared by its linked worktrees.
func PrepareStorageExcludes(ctx context.Context, e *execution.Context) (bool, error) {
	var original []byte
	readSource := func() (bool, error) {
		raw, err := e.Command(ctx, e.Root(), "git", "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
		if err != nil {
			return false, fmt.Errorf("resolve storage Git owner: %w", err)
		}
		common := strings.TrimSpace(string(raw))
		sum := sha256.Sum256([]byte(e.Root()))
		owner := filepath.Join(common, "noodle-contexts", hex.EncodeToString(sum[:]), "source.json")
		before, err := os.ReadFile(owner)
		managed := !os.IsNotExist(err)
		for _, name := range []string{".noodle", ".worktrees"} {
			info, statErr := os.Lstat(filepath.Join(e.Root(), name))
			if statErr != nil && !os.IsNotExist(statErr) {
				return false, statErr
			}
			managed = managed || (statErr == nil && info.Mode()&os.ModeSymlink != 0)
		}
		if !managed {
			return false, nil
		}
		if err != nil {
			return true, fmt.Errorf("original storage owner unavailable: %w", err)
		}
		if original != nil && !bytes.Equal(original, before) {
			return true, fmt.Errorf("original storage owner changed during preparation")
		}
		var source StorageSource
		if json.Unmarshal(before, &source) != nil || source.Project != e.Root() || source.CommonDir != common || !filepath.IsAbs(source.StorageRoot) {
			return true, fmt.Errorf("original storage source binding changed")
		}
		storage, err := filepath.EvalSymlinks(source.StorageRoot)
		if err != nil || storage != source.StorageRoot || storage == e.Root() || strings.HasPrefix(storage, e.Root()+string(filepath.Separator)) || storage == common || strings.HasPrefix(storage, common+string(filepath.Separator)) {
			return true, fmt.Errorf("original storage root changed")
		}
		paired, err := os.ReadFile(filepath.Join(storage, "source.json"))
		if err != nil || !bytes.Equal(before, paired) {
			return true, fmt.Errorf("original storage paired receipt changed")
		}
		for _, name := range []string{".noodle", ".worktrees"} {
			path := filepath.Join(e.Root(), name)
			info, err := os.Lstat(path)
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				return true, fmt.Errorf("original storage alias %s changed", name)
			}
			target, err := filepath.EvalSymlinks(path)
			if err != nil || target != filepath.Join(storage, name) {
				return true, fmt.Errorf("original storage alias %s belongs to another owner", name)
			}
		}
		tracked, err := e.Command(ctx, e.Root(), "git", "ls-files", "--", ".noodle", ".worktrees").Output()
		if err != nil {
			return true, fmt.Errorf("read runtime tracked source: %w", err)
		}
		if len(tracked) != 0 {
			return true, fmt.Errorf("runtime aliases overlap tracked source")
		}
		original = before
		return true, nil
	}
	managed, err := readSource()
	if err != nil || !managed {
		return managed, err
	}
	raw, err := e.Command(ctx, e.Root(), "git", "rev-parse", "--path-format=absolute", "--git-path", "info/exclude").Output()
	if err != nil {
		return true, fmt.Errorf("resolve repository-local excludes: %w", err)
	}
	path := strings.TrimSpace(string(raw))
	if !filepath.IsAbs(path) {
		return true, fmt.Errorf("repository-local exclude path is not absolute")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil || parent != filepath.Dir(path) {
		return true, fmt.Errorf("repository-local exclude directory changed")
	}
	if info, err := os.Lstat(path + ".noodle.lock"); err != nil && !os.IsNotExist(err) {
		return true, err
	} else if err == nil && !info.Mode().IsRegular() {
		return true, fmt.Errorf("repository-local exclude lock is not a regular file")
	}
	lock, err := lockfile.Acquire(ctx, path+".noodle.lock")
	if err != nil {
		return true, err
	}
	defer lock.Close()
	info, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		return true, err
	}
	if err == nil && !info.Mode().IsRegular() {
		return true, fmt.Errorf("repository-local excludes is not a regular file")
	}
	before, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return true, err
	}
	var extra []byte
	for _, rule := range []string{"/.noodle", "/.worktrees"} {
		if !bytes.Contains(append([]byte{'\n'}, before...), []byte("\n"+rule+"\n")) {
			extra = append(extra, []byte(rule+"\n")...)
		}
	}
	if len(extra) != 0 {
		if len(before) != 0 && before[len(before)-1] != '\n' {
			extra = append([]byte{'\n'}, extra...)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return true, err
		}
		opened, statErr := f.Stat()
		if statErr != nil || !opened.Mode().IsRegular() || (info != nil && !os.SameFile(info, opened)) {
			f.Close()
			return true, fmt.Errorf("repository-local excludes changed before append")
		}
		_, writeErr := f.Write(extra)
		syncErr, closeErr := f.Sync(), f.Close()
		if writeErr != nil {
			return true, writeErr
		}
		if syncErr != nil {
			return true, syncErr
		}
		if closeErr != nil {
			return true, closeErr
		}
		if err = filex.SyncDir(filepath.Dir(path)); err != nil {
			return true, err
		}
	}
	if _, err = readSource(); err != nil {
		return true, err
	}
	for _, name := range []string{".noodle", ".worktrees"} {
		out, err := e.Command(ctx, e.Root(), "git", "check-ignore", "-q", name).CombinedOutput()
		if err != nil {
			return true, fmt.Errorf("owned runtime alias %s is not ignored: %w: %s", name, err, out)
		}
	}
	return true, nil
}
