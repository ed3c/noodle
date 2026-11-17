package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/guestprogress"
)

// Only the original receipt's two archives leave the small JSON evidence path.
func ReadProgressMaintenanceArchives(ctx context.Context, directory string) (map[string]SessionReadbackFileRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var saved guestprogress.Receipt
	result, err := ReadSessionReadbackFile(filepath.Join(directory, "result.json"), &saved)
	if err != nil {
		return nil, err
	}
	if err := validateMaintenanceArchiveReceipt(saved); err != nil {
		return nil, err
	}
	refs := map[string]SessionReadbackFileRef{"result.json": result}
	for _, archive := range saved.Archives {
		path := filepath.Join(directory, archive.Name)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("maintenance archive is not a regular file: %s", archive.Name)
		}
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, err
		}
		refs[archive.Name] = SessionReadbackFileRef{Path: canonical, SHA256: archive.SHA256}
	}
	if err := verifyMaintenanceArchives(ctx, refs, saved); err != nil {
		return nil, err
	}
	return refs, nil
}

func validateMaintenanceArchiveReceipt(saved guestprogress.Receipt) error {
	if len(saved.Archives) != 2 {
		return fmt.Errorf("maintenance archive set differs")
	}
	names := map[string]bool{"noodle-result.tar": false, "noodle-session.tar": false}
	for _, archive := range saved.Archives {
		seen, ok := names[archive.Name]
		_, err := hex.DecodeString(archive.SHA256)
		if !ok || seen || archive.Bytes <= 0 || len(archive.SHA256) != 64 || err != nil {
			return fmt.Errorf("maintenance archive receipt differs")
		}
		names[archive.Name] = true
	}
	return nil
}

func verifyMaintenanceArchives(ctx context.Context, refs map[string]SessionReadbackFileRef, saved guestprogress.Receipt) error {
	if err := validateMaintenanceArchiveReceipt(saved); err != nil {
		return err
	}
	for _, archive := range saved.Archives {
		if err := streamMaintenanceArchive(ctx, refs[archive.Name], archive, io.Discard); err != nil {
			return err
		}
	}
	return maintenanceVerify(refs["result.json"], nil)
}

func streamMaintenanceArchive(ctx context.Context, ref SessionReadbackFileRef, archive guestprogress.Archive, destination io.Writer) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ref.SHA256 != archive.SHA256 {
		return fmt.Errorf("maintenance archive reference differs: %s", archive.Name)
	}
	before, err := os.Lstat(ref.Path)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Size() != archive.Bytes {
		return fmt.Errorf("maintenance archive type or size differs: %s", archive.Name)
	}
	canonical, err := filepath.EvalSymlinks(ref.Path)
	if err != nil || canonical != ref.Path {
		return fmt.Errorf("maintenance archive source is unavailable or aliased: %s", archive.Name)
	}
	file, err := os.Open(ref.Path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	unchanged := func(info os.FileInfo) bool {
		return os.SameFile(before, info) && info.Mode() == before.Mode() && info.Size() == before.Size() && info.ModTime().Equal(before.ModTime())
	}
	opened, err := file.Stat()
	if err != nil || !unchanged(opened) {
		return fmt.Errorf("maintenance archive changed before open: %s", archive.Name)
	}
	hash := sha256.New()
	buffer := make([]byte, 64<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := file.Read(buffer)
		if int64(n) > archive.Bytes-total {
			return fmt.Errorf("maintenance archive grew beyond its receipt: %s", archive.Name)
		}
		if n > 0 {
			total += int64(n)
			_, _ = hash.Write(buffer[:n])
			written, writeErr := destination.Write(buffer[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	after, err := file.Stat()
	if err != nil || !unchanged(after) || total != archive.Bytes || hex.EncodeToString(hash.Sum(nil)) != ref.SHA256 {
		return fmt.Errorf("maintenance archive content or identity changed: %s", archive.Name)
	}
	current, err := os.Lstat(ref.Path)
	if err != nil || !unchanged(current) {
		return fmt.Errorf("maintenance archive source changed: %s", archive.Name)
	}
	currentPath, err := filepath.EvalSymlinks(ref.Path)
	if err != nil || currentPath != ref.Path {
		return fmt.Errorf("maintenance archive source path changed: %s", archive.Name)
	}
	return ctx.Err()
}

func retainMaintenanceArchives(ctx context.Context, p ProgressMaintenance, saved guestprogress.Receipt) error {
	if err := verifyMaintenanceArchives(ctx, p.Archives, saved); err != nil {
		return err
	}
	if err := verifyCleanupProgressArchives(ctx, p.SessionDirectory, openShellDispatchReceipt{Progress: &saved}); err == nil {
		return nil
	}
	entries, err := os.ReadDir(p.SessionDirectory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "progress-archive-") {
			return fmt.Errorf("maintenance archive retention has partial or ambiguous effects")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, err := os.MkdirTemp(p.SessionDirectory, "progress-archive-maintenance-")
	if err != nil {
		return err
	}
	if err := filex.SyncDir(p.SessionDirectory); err != nil {
		return err
	}
	for _, archive := range saved.Archives {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, err := os.OpenFile(filepath.Join(directory, archive.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		copyErr := streamMaintenanceArchive(ctx, p.Archives[archive.Name], archive, file)
		if err := errors.Join(copyErr, file.Sync(), file.Close(), filex.SyncDir(directory)); err != nil {
			return err
		}
	}
	return verifyCleanupProgressArchives(ctx, p.SessionDirectory, openShellDispatchReceipt{Progress: &saved})
}
