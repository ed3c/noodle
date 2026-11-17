package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/internal/lockfile"
)

func ensurePool(c Config, s Source, id string) (string, error) {
	dir := filepath.Join(c.ArchiveRoot, "git-pools")
	if err := durableMkdirAll(dir, 0700); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.ItemTimeoutSeconds)*time.Second)
	defer cancel()
	lock, err := lockfile.Acquire(ctx, filepath.Join(dir, rootKey(s.Common)+".lock"))
	if err != nil {
		return "", err
	}
	defer lock.Close()
	pool := filepath.Join(dir, rootKey(s.Common)+".git")
	if _, err := os.Lstat(pool); os.IsNotExist(err) {
		if _, err = git(dir, "init", "--bare", pool); err != nil {
			return "", err
		}
		if _, err = git(pool, "config", "gc.auto", "0"); err != nil {
			return "", err
		}
		if err = syncTree(pool); err != nil {
			return "", err
		}
		if err = syncDir(dir); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	if err := canonical(pool); err != nil {
		return "", err
	}
	if _, err := os.Lstat(filepath.Join(pool, "objects", "info", "alternates")); !os.IsNotExist(err) {
		return "", errors.New("archive pool has borrowed or uncertain objects")
	}
	if _, err := git(pool, "fetch", "--no-auto-maintenance", "--no-write-fetch-head", s.Common, s.Head+":refs/archive/"+id); err != nil {
		return "", err
	}
	if _, err := git(pool, "fsck", "--connectivity-only", "--no-reflogs", s.Head); err != nil {
		return "", err
	}
	if err := syncTree(pool); err != nil {
		return "", err
	}
	return pool, nil
}
