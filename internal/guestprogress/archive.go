package guestprogress

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Called only after the owning waiter proves ECHILD. No child can still mutate
// these trees, including descendants that changed process groups or sessions.
func archiveTree(root, directory, name string, excluded map[string]bool) (result Archive, err error) {
	actual, err := filepath.EvalSymlinks(root)
	if err != nil {
		return result, err
	}
	if actual != filepath.Clean(root) {
		return result, fmt.Errorf("archive root has a symlink alias")
	}
	file, err := os.OpenFile(filepath.Join(directory, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}()
	hash := sha256.New()
	writer := tar.NewWriter(io.MultiWriter(file, hash))
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		relative = filepath.ToSlash(relative)
		if excluded[relative] {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, e := entry.Info()
		if e != nil {
			return e
		}
		if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("unsupported archive entry: %s", relative)
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, e = os.Readlink(path)
			if e != nil {
				return e
			}
		}
		header, e := tar.FileInfoHeader(info, link)
		if e != nil {
			return e
		}
		header.Name = relative
		if e = writer.WriteHeader(header); e != nil {
			return e
		}
		if info.Mode().IsRegular() {
			input, e := os.Open(path)
			if e != nil {
				return e
			}
			_, copyErr := io.Copy(writer, input)
			closeErr := input.Close()
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	if err = writer.Close(); err != nil {
		return result, err
	}
	if err = file.Sync(); err != nil {
		return result, err
	}
	info, err := file.Stat()
	if err != nil {
		return result, err
	}
	return Archive{Name: name, SHA256: hex.EncodeToString(hash.Sum(nil)), Bytes: info.Size()}, nil
}
