package workspaceartifacts

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	maxArchivePayload int64 = 8 << 30
	maxArchiveEntries       = 100_000
	maxArchiveBytes         = maxArchivePayload + maxArchiveEntries*4096 + 1024
)

type archiveEntry struct {
	name   string
	kind   byte
	mode   int64
	size   int64
	link   string
	digest string
}

// Extract validates a complete uncompressed tar before creating destination.
// It never retries. A second-pass failure retains the partial destination for
// inspection; callers must not Apply it. The archive and destination are owned
// by the caller throughout this operation, not concurrent external writers.
func Extract(ctx context.Context, archiveFile, destination string) error {
	info, err := os.Lstat(archiveFile)
	if err != nil {
		return fmt.Errorf("inspect artifact archive: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("artifact archive is not a regular file")
	}
	if info.Size() > maxArchiveBytes {
		return fmt.Errorf("artifact archive exceeds %d byte limit", maxArchiveBytes)
	}
	file, err := os.Open(archiveFile)
	if err != nil {
		return fmt.Errorf("open artifact archive: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return fmt.Errorf("artifact archive identity changed while opening")
	}
	destination, err = newExtractionDirectory(destination)
	if err != nil {
		return err
	}
	entries, before, err := readArchive(ctx, file, nil, "")
	if err != nil {
		return err
	}
	if err := validateArchiveTree(entries); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return fmt.Errorf("create extraction directory: %w", err)
	}
	_, after, err := readArchive(ctx, file, entries, destination)
	if err != nil {
		return err
	}
	if before != after {
		return fmt.Errorf("artifact archive bytes changed during extraction")
	}
	finalInfo, err := os.Lstat(archiveFile)
	if err != nil {
		return err
	}
	if !finalInfo.Mode().IsRegular() || !os.SameFile(info, finalInfo) || finalInfo.Size() != info.Size() {
		return fmt.Errorf("artifact archive identity or size changed during extraction")
	}
	return nil
}

func newExtractionDirectory(destination string) (string, error) {
	destination, err := filepath.Abs(destination)
	if err != nil {
		return "", err
	}
	volume := filepath.VolumeName(destination)
	root := volume + string(filepath.Separator)
	rel, err := filepath.Rel(root, destination)
	if err != nil {
		return "", err
	}
	if err := safeAncestors(root, filepath.ToSlash(rel)); err != nil {
		return "", err
	}
	parent, err := os.Lstat(filepath.Dir(destination))
	if err != nil {
		return "", fmt.Errorf("inspect extraction parent: %w", err)
	}
	if !parent.IsDir() {
		return "", fmt.Errorf("extraction parent is not a directory")
	}
	if _, err := os.Lstat(destination); err == nil {
		return "", fmt.Errorf("extraction destination already exists")
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return destination, nil
}

type contextArchiveReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextArchiveReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

// readArchive hashes the entire file, including padding, and rejects data after
// the end marker. On the second pass each header/body must match the first pass.
func readArchive(ctx context.Context, file io.Reader, expected []archiveEntry, destination string) ([]archiveEntry, string, error) {
	hash := sha256.New()
	limited := &io.LimitedReader{R: file, N: maxArchiveBytes + 1}
	raw := io.TeeReader(contextArchiveReader{ctx: ctx, reader: limited}, hash)
	reader := tar.NewReader(raw)
	entries := make([]archiveEntry, 0)
	seen := make(map[string]bool)
	var total int64
	for {
		remaining := limited.N
		header, err := reader.Next()
		if err == io.EOF {
			if remaining-limited.N < 1024 {
				return nil, "", fmt.Errorf("artifact archive end marker is missing")
			}
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("read artifact archive header: %w", err)
		}
		if len(entries) >= maxArchiveEntries {
			return nil, "", fmt.Errorf("artifact archive exceeds %d entries", maxArchiveEntries)
		}
		entry, err := validateArchiveHeader(header)
		if err != nil {
			return nil, "", err
		}
		if seen[entry.name] {
			return nil, "", fmt.Errorf("duplicate artifact archive path %q", entry.name)
		}
		seen[entry.name] = true
		if entry.size > maxArchivePayload-total {
			return nil, "", fmt.Errorf("artifact archive exceeds %d total payload bytes", maxArchivePayload)
		}
		total += entry.size
		if destination != "" {
			if len(entries) >= len(expected) {
				return nil, "", fmt.Errorf("artifact archive gained entries during extraction")
			}
			want := expected[len(entries)]
			want.digest = ""
			if entry != want {
				return nil, "", fmt.Errorf("artifact archive header changed during extraction: %q", entry.name)
			}
		}
		bodyHash := sha256.New()
		body := io.TeeReader(reader, bodyHash)
		if destination == "" {
			_, err = io.Copy(io.Discard, body)
		} else {
			err = extractArchiveEntry(destination, entry, body)
		}
		if err != nil {
			return nil, "", fmt.Errorf("read artifact archive entry %q: %w", entry.name, err)
		}
		entry.digest = hex.EncodeToString(bodyHash.Sum(nil))
		if destination != "" && entry.digest != expected[len(entries)].digest {
			return nil, "", fmt.Errorf("artifact archive body changed during extraction: %q", entry.name)
		}
		entries = append(entries, entry)
	}
	if destination != "" && len(entries) != len(expected) {
		return nil, "", fmt.Errorf("artifact archive lost entries during extraction")
	}
	buffer := make([]byte, 32*1024)
	for {
		n, err := raw.Read(buffer)
		for _, b := range buffer[:n] {
			if b != 0 {
				return nil, "", fmt.Errorf("artifact archive has nonzero data after end marker")
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("read artifact archive padding: %w", err)
		}
	}
	if limited.N == 0 {
		return nil, "", fmt.Errorf("artifact archive exceeds %d byte limit", maxArchiveBytes)
	}
	return entries, hex.EncodeToString(hash.Sum(nil)), nil
}

func validateArchiveHeader(header *tar.Header) (archiveEntry, error) {
	name := header.Name
	if name == "" {
		return archiveEntry{}, fmt.Errorf("artifact archive path is empty")
	}
	for strings.HasPrefix(name, "./") {
		name = strings.TrimPrefix(name, "./")
	}
	if header.Typeflag == tar.TypeDir {
		name = strings.TrimSuffix(name, "/")
		if name == "" {
			name = "."
		}
	}
	if name != "." || header.Typeflag != tar.TypeDir {
		if err := validPath(name); err != nil {
			return archiveEntry{}, err
		}
	}
	// A leading slash cannot become the root directory after trimming.
	if path.IsAbs(header.Name) {
		return archiveEntry{}, fmt.Errorf("absolute artifact archive path %q", header.Name)
	}
	if header.Mode < 0 || header.Mode&^int64(0777) != 0 {
		return archiveEntry{}, fmt.Errorf("unsupported artifact archive mode for %q", name)
	}
	if header.Size < 0 || header.Size > maxArchivePayload {
		return archiveEntry{}, fmt.Errorf("artifact archive entry %q exceeds %d byte limit", name, maxArchivePayload)
	}
	if len(header.Xattrs) != 0 {
		return archiveEntry{}, fmt.Errorf("artifact archive extended attributes unsupported: %q", name)
	}
	for key := range header.PAXRecords {
		switch key {
		case "path", "linkpath", "size", "mtime", "atime", "ctime":
		default:
			return archiveEntry{}, fmt.Errorf("artifact archive metadata %q unsupported", key)
		}
	}
	kind := header.Typeflag
	if kind == tar.TypeRegA {
		kind = tar.TypeReg
	}
	switch kind {
	case tar.TypeReg:
	case tar.TypeDir, tar.TypeSymlink:
		if header.Size != 0 {
			return archiveEntry{}, fmt.Errorf("non-file artifact archive entry has payload: %q", name)
		}
	default:
		return archiveEntry{}, fmt.Errorf("unsupported artifact archive entry type for %q", name)
	}
	if kind == tar.TypeSymlink {
		if _, err := symlinkDestination(name, header.Linkname); err != nil {
			return archiveEntry{}, err
		}
	} else if header.Linkname != "" {
		return archiveEntry{}, fmt.Errorf("non-link artifact archive entry has link metadata: %q", name)
	}
	return archiveEntry{name: name, kind: kind, mode: header.Mode, size: header.Size, link: header.Linkname}, nil
}

func validateArchiveTree(entries []archiveEntry) error {
	byName := make(map[string]archiveEntry, len(entries))
	for _, entry := range entries {
		byName[entry.name] = entry
	}
	ancestors := func(name string) error {
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			if ancestor, exists := byName[dir]; exists && ancestor.kind != tar.TypeDir {
				return fmt.Errorf("artifact archive ancestor is not a directory: %q", dir)
			}
		}
		return nil
	}
	for _, entry := range entries {
		if err := ancestors(entry.name); err != nil {
			return err
		}
		if entry.kind != tar.TypeSymlink {
			continue
		}
		resolved, err := symlinkDestination(entry.name, entry.link)
		if err != nil {
			return err
		}
		if err := ancestors(resolved); err != nil {
			return err
		}
		if target, exists := byName[resolved]; exists && target.kind == tar.TypeSymlink {
			return fmt.Errorf("artifact archive symlink chain at %q", entry.name)
		}
	}
	return nil
}

func extractArchiveEntry(root string, entry archiveEntry, body io.Reader) error {
	if entry.name == "." {
		return nil
	}
	if err := safeAncestors(root, entry.name); err != nil {
		return err
	}
	full := filepath.Join(root, filepath.FromSlash(entry.name))
	if entry.kind == tar.TypeDir {
		return os.MkdirAll(full, 0700)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		return err
	}
	if entry.kind == tar.TypeSymlink {
		return os.Symlink(entry.link, full)
	}
	file, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, body)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	mode := os.FileMode(0644)
	if entry.mode&0111 != 0 {
		mode = 0755
	}
	return os.Chmod(full, mode)
}
