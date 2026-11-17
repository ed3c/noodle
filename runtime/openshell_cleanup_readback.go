package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/internal/workspaceartifacts"
)

type CleanupSessionReadback struct {
	OutcomeStatus SessionStatus            `json:"outcome_status"`
	PublishedHead string                   `json:"published_head"`
	Execution     CompletedSessionReadback `json:"execution"`
}

// ReadCleanupSession borrows only this runtime's original custody. Historical
// cancelled/failed progress remains that outcome; cleanup grants no retry or
// acceptance authority. Normal session readback still requires its live worktree.
func (r *OpenShellRuntime) ReadCleanupSession(ctx context.Context, b CompletedSessionBinding, repository string, expected SessionStatus) (CleanupSessionReadback, error) {
	if !filepath.IsAbs(repository) || (expected != StatusCompleted && expected != StatusCancelled && expected != StatusFailed) {
		return CleanupSessionReadback{}, fmt.Errorf("cleanup session outcome or repository missing")
	}
	progress := expected != StatusCompleted
	observe := func(ctx context.Context, id OpenShellIdentity) (OpenShellCompletedObservation, error) {
		if progress {
			return r.config.Gateway.ObserveSavedProgress(ctx, r.config.ContextID, id)
		}
		return r.config.Gateway.ObserveCompleted(ctx, r.config.ContextID, id)
	}
	retained := func(ctx context.Context, receipt openShellDispatchReceipt, head string) error {
		if err := verifyCleanupProgressArchives(ctx, filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID), receipt); err != nil {
			return err
		}
		return workspaceartifacts.VerifyRetainedPublication(ctx, repository, receipt.Baseline, receipt.Delivery, head)
	}
	result, err := r.readSettledSession(ctx, b, observe, progress, retained)
	if err != nil {
		return CleanupSessionReadback{}, err
	}
	if result.status != expected {
		return CleanupSessionReadback{}, fmt.Errorf("cleanup session outcome differs from canonical attempt")
	}
	head := result.completed.CandidateHead
	if progress {
		head = result.progressHead
	}
	return CleanupSessionReadback{OutcomeStatus: result.status, PublishedHead: head, Execution: result.completed}, nil
}

// The original archives remain recoverable even after the candidate worktree is
// removed. Check their exact recorded bytes, without extracting or changing them.
func verifyCleanupProgressArchives(ctx context.Context, dir string, receipt openShellDispatchReceipt) error {
	_, err := retainedProgressArchiveSources(ctx, dir, receipt)
	return err
}

func retainedProgressArchiveSources(ctx context.Context, dir string, receipt openShellDispatchReceipt) (map[string]SessionReadbackFileRef, error) {
	refs := map[string]SessionReadbackFileRef{}
	canonicalDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	if receipt.Progress == nil || len(receipt.Progress.Archives) != 2 {
		return nil, fmt.Errorf("cleanup retained progress archives missing")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{"noodle-result.tar": false, "noodle-session.tar": false}
	for _, archive := range receipt.Progress.Archives {
		seen, ok := names[archive.Name]
		if !ok || seen || archive.Bytes <= 0 {
			return nil, fmt.Errorf("cleanup retained archive identity differs")
		}
		names[archive.Name] = true
		count := 0
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), "progress-archive-") {
				continue
			}
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("cleanup archive directory is ambiguous")
			}
			path := filepath.Join(dir, entry.Name(), archive.Name)
			info, err := os.Lstat(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			count++
			if !info.Mode().IsRegular() || info.Size() != archive.Bytes {
				return nil, fmt.Errorf("cleanup retained archive type or size differs")
			}
			canonical, err := filepath.EvalSymlinks(path)
			if err != nil {
				return nil, err
			}
			if filepath.Dir(filepath.Dir(canonical)) != canonicalDir || filepath.Base(filepath.Dir(canonical)) != entry.Name() {
				return nil, fmt.Errorf("retained archive escaped original session")
			}
			file, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			opened, err := file.Stat()
			if err != nil || !os.SameFile(info, opened) {
				_ = file.Close()
				return nil, fmt.Errorf("cleanup retained archive changed before open")
			}
			hash := sha256.New()
			buffer := make([]byte, 64<<10)
			var total int64
			for {
				if err = ctx.Err(); err != nil {
					break
				}
				var n int
				n, err = file.Read(buffer)
				total += int64(n)
				if total > archive.Bytes {
					err = fmt.Errorf("retained archive grew beyond original receipt")
					break
				}
				if n > 0 {
					_, _ = hash.Write(buffer[:n])
				}
				if err == io.EOF {
					err = nil
					break
				}
				if err != nil {
					break
				}
			}
			closeErr := file.Close()
			if err != nil {
				return nil, err
			}
			if closeErr != nil {
				return nil, closeErr
			}
			after, err := os.Lstat(path)
			if err != nil {
				return nil, err
			}
			if !os.SameFile(info, after) || !info.ModTime().Equal(after.ModTime()) || total != archive.Bytes || !after.Mode().IsRegular() || after.Size() != archive.Bytes || hex.EncodeToString(hash.Sum(nil)) != archive.SHA256 {
				return nil, fmt.Errorf("cleanup retained archive changed")
			}
			afterPath, err := filepath.EvalSymlinks(path)
			if err != nil || afterPath != canonical {
				return nil, fmt.Errorf("retained archive path changed during readback")
			}
			refs["progress_archive_"+archive.Name] = SessionReadbackFileRef{Path: canonical, SHA256: archive.SHA256}
		}
		if count != 1 {
			return nil, fmt.Errorf("cleanup retained archive missing or ambiguous: %s", archive.Name)
		}
	}
	afterDir, err := filepath.EvalSymlinks(dir)
	if err != nil || afterDir != canonicalDir {
		return nil, fmt.Errorf("original archive session root changed")
	}
	return refs, nil
}
