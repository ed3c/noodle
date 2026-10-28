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
	if receipt.Progress == nil || len(receipt.Progress.Archives) != 2 {
		return fmt.Errorf("cleanup retained progress archives missing")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, archive := range receipt.Progress.Archives {
		count := 0
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), "progress-archive-") {
				continue
			}
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("cleanup archive directory is ambiguous")
			}
			path := filepath.Join(dir, entry.Name(), archive.Name)
			info, err := os.Lstat(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			count++
			if !info.Mode().IsRegular() || info.Size() != archive.Bytes {
				return fmt.Errorf("cleanup retained archive type or size differs")
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			hash := sha256.New()
			buffer := make([]byte, 64<<10)
			for {
				if err = ctx.Err(); err != nil {
					break
				}
				var n int
				n, err = file.Read(buffer)
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
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			after, err := os.Lstat(path)
			if err != nil {
				return err
			}
			if !os.SameFile(info, after) || !after.Mode().IsRegular() || after.Size() != archive.Bytes || hex.EncodeToString(hash.Sum(nil)) != archive.SHA256 {
				return fmt.Errorf("cleanup retained archive changed")
			}
		}
		if count != 1 {
			return fmt.Errorf("cleanup retained archive missing or ambiguous: %s", archive.Name)
		}
	}
	return nil
}
