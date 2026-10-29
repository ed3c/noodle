package workspaceartifacts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// ResumePinnedRetainedPublication resumes only an existing, fully pinned
// publication compaction. It never creates/replaces the manifest, Git objects
// or refs, and never removes the execution worktree or a native resource.
// The caller verifies the original closure/retention authority and exclusively
// owns the quiescent session and repository through the final deletion.
// expectedManifestSHA binds the exact previously reviewed manifest bytes.
func ResumePinnedRetainedPublication(ctx context.Context, repository string, b *Baseline, r *Result, head, expectedManifestSHA string) error {
	return resumePinnedRetainedPublication(ctx, repository, b, r, head, expectedManifestSHA, os.Remove)
}

func resumePinnedRetainedPublication(ctx context.Context, repository string, b *Baseline, r *Result, head, expectedManifestSHA string, remove func(string) error) error {
	root, evidence, copies, err := compactionPaths(ctx, repository, b, r, head)
	if err != nil {
		return err
	}
	path := filepath.Join(evidence, compactionManifestName)
	raw, err := retentionReadFile(path)
	if err != nil {
		return fmt.Errorf("existing compaction manifest unavailable: %w", err)
	}
	if digest(raw) != expectedManifestSHA {
		return fmt.Errorf("existing compaction manifest digest differs")
	}
	m, err := readCompaction(raw, root, b, r, head)
	if err != nil {
		return err
	}
	if len(m.Copies) != len(copies) {
		return fmt.Errorf("existing compaction copy coverage differs")
	}
	for _, dir := range copies {
		if m.Copies[dir]["."].Kind != "directory" {
			return fmt.Errorf("existing compaction copy inventory missing: %s", dir)
		}
	}
	// This path deliberately never calls pinCompaction or the create-capable
	// compactor. Losing every ref cannot select their first-publication branches.
	if err := verifyCompaction(ctx, root, m, raw, true); err != nil {
		return err
	}
	pins, err := compactionRefs(ctx, root, m)
	if err != nil {
		return err
	}
	manifestObject := pins[compactionRefPrefix(b, r)+"/manifest"]
	if !matchesRawBlob(raw, manifestObject) {
		return fmt.Errorf("existing compaction manifest pin differs")
	}
	// Re-establish the existing persistence boundary after a lost sync ACK.
	// These operations only open/sync original files; neither can recreate a pin.
	if err := syncCompactionSources(ctx, root, m, manifestObject); err != nil {
		return err
	}
	if err := syncPinnedCompaction(ctx, root, m); err != nil {
		return err
	}
	current, err := retentionReadFile(path)
	if err != nil {
		return err
	}
	if digest(current) != expectedManifestSHA {
		return fmt.Errorf("existing compaction manifest changed before resume")
	}
	if err := verifyCompaction(ctx, root, m, raw, true); err != nil {
		return err
	}
	for _, dir := range copies {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := removeRetainedCopy(ctx, dir, m.Copies[dir], remove); err != nil {
			return fmt.Errorf("resume retained copy %q: %w", dir, err)
		}
	}
	return nil
}
