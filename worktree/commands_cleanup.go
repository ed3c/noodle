package worktree

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// CleanupOpts holds optional parameters for Cleanup.
type CleanupOpts struct {
	ExplicitDelete bool
	// Force permits discarding unmerged commits; uncommitted changes are retained.
	Force bool
}

// Cleanup removes a worktree without merging.
func (a *App) Cleanup(name string, opts ...CleanupOpts) error {
	var o CleanupOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	return a.cleanupAtPath(name, a.resolveWorktreePath(name), o)
}

func (a *App) cleanupAtPath(name, wtPath string, o CleanupOpts) error {
	// Do not re-resolve a discovered candidate to a different same-name worktree.
	if canonicalPath(a.resolveWorktreePath(name)) != canonicalPath(wtPath) {
		return fmt.Errorf("cleanup candidate path changed for %s: %s", name, wtPath)
	}
	retained, err := a.retainsTaskWorktree(name)
	if err != nil {
		return err
	}
	if retained && !o.ExplicitDelete {
		return fmt.Errorf("bound task worktree retained; only explicit cleanup --delete may remove %s", name)
	}
	_, statErr := os.Stat(wtPath)
	missing := os.IsNotExist(statErr)
	if statErr != nil && !missing {
		return fmt.Errorf("stat worktree %s: %w", wtPath, statErr)
	}
	branch := a.resolveBranchName(name)
	head := ""
	if !missing {
		branch, head, err = a.cleanupCandidate(wtPath)
		if err != nil {
			return err
		}
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		if IsCWDInsideWorktree(canonicalPath(cwd), canonicalPath(wtPath)) {
			return fmt.Errorf("shell CWD is inside the worktree (%s)", cwd)
		}
		dirty, err := a.gitOutput("-C", wtPath, "status", "--porcelain", "--untracked-files=all")
		if err != nil {
			return fmt.Errorf("read worktree status %s: %w", wtPath, err)
		}
		if dirty != "" {
			return fmt.Errorf("worktree %s has uncommitted changes", wtPath)
		}
	} else {
		if a.branchExists(name) && a.branchExists("noodle/"+name) {
			return fmt.Errorf("cleanup branch ambiguous for missing worktree %s", name)
		}
		head, err = a.gitOutput("rev-parse", "--verify", "refs/heads/"+branch)
		if err != nil {
			return fmt.Errorf("read cleanup branch %s: %w", branch, err)
		}
	}
	if !o.Force {
		if err := a.ensureNoUnmergedCommits(name, head); err != nil {
			return err
		}
	}
	if !missing {
		currentBranch, currentHead, err := a.cleanupCandidate(wtPath)
		if err != nil {
			return err
		}
		if currentBranch != branch || currentHead != head {
			return fmt.Errorf("cleanup candidate changed for %s", name)
		}
	} else {
		wtPath = ""
	}
	failures := a.cleanupWorktreeAndBranch(wtPath, branch, true, head)
	if len(failures) > 0 {
		return fmt.Errorf("cleanup %s: %s", name, strings.Join(failures, "; "))
	}
	a.printf("Done. %s removed.\n", name)
	return nil
}

func (a *App) cleanupCandidate(path string) (branch, head string, err error) {
	branch, err = a.gitOutput("-C", path, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("read cleanup candidate branch at %s: %w", path, err)
	}
	if branch == "" {
		return "", "", fmt.Errorf("cleanup candidate branch unavailable at %s", path)
	}
	head, err = a.gitOutput("-C", path, "rev-parse", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("read cleanup candidate HEAD at %s: %w", path, err)
	}
	return branch, head, nil
}

func (a *App) cleanupCherryStatus(revision string) (unmerged, equivalent int, err error) {
	out, err := a.gitOutput("cherry", a.integrationBranch(), revision)
	if err != nil {
		return 0, 0, fmt.Errorf("assess cleanup candidate %s: %w", revision, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "+") {
			unmerged++
		} else if strings.HasPrefix(line, "-") {
			equivalent++
		}
	}
	return unmerged, equivalent, nil
}

func (a *App) ensureNoUnmergedCommits(name, head string) error {
	commits, _, err := a.cleanupCherryStatus(head)
	if err != nil {
		return err
	}
	if commits == 0 {
		return nil
	}
	return fmt.Errorf(
		"%s has %d unmerged commit(s); use '%s cleanup %s --force' to discard, or '%s merge %s' to keep",
		name, commits, a.cmdName(), name, a.cmdName(), name,
	)
}

func (a *App) cleanupWorktreeAndBranch(wtPath, branch string, allowForceDelete bool, expectedHead ...string) []string {
	if allowForceDelete && (len(expectedHead) != 1 || expectedHead[0] == "") {
		return []string{"cleanup candidate HEAD is missing"}
	}
	if wtPath != "" {
		a.info("Removing worktree...")
		if err := a.gitRunWithDetails("worktree", "remove", wtPath); err != nil {
			return []string{fmt.Sprintf("failed to remove worktree %s: %v", wtPath, err)}
		}
	}
	// Release stale registrations before deleting refs for a missing directory.
	if err := a.gitRunWithDetails("worktree", "prune"); err != nil {
		return []string{fmt.Sprintf("failed to prune worktrees: %v", err)}
	}

	a.info(fmt.Sprintf("Deleting branch %s...", branch))
	if allowForceDelete {
		registrations, err := a.cleanupWorktreeRegistrations()
		if err != nil {
			return []string{err.Error()}
		}
		for _, r := range registrations {
			if r.branch == "refs/heads/"+branch {
				return []string{fmt.Sprintf("cleanup branch still registered at %s", r.path)}
			}
		}
		// Compare-and-delete: a concurrent writer may have advanced this ref
		// after the clean/safety checks, even after worktree removal.
		if err := a.gitRunWithDetails("update-ref", "--no-deref", "-d", "refs/heads/"+branch, expectedHead[0]); err != nil {
			return []string{fmt.Sprintf("failed to delete branch %s: %v", branch, err)}
		}
	} else if err := a.gitRunWithDetails("branch", "-d", "--", branch); err != nil {
		return []string{fmt.Sprintf("failed to delete branch %s: %v", branch, err)}
	}
	return nil
}

func (a *App) cleanRemote(branch string) {
	if err := a.git("push", "origin", "--delete", branch).Run(); err != nil {
		a.warnf("WARNING: failed to delete remote branch %s: %v\n", branch, err)
	}
}

// List shows all worktrees with merge status.
func (a *App) List() error {
	a.printf("Worktrees:\n")
	_ = a.gitRun("worktree", "list")

	base := a.integrationBranch()
	entries, err := a.allWorktreeEntries()
	if err != nil {
		return fmt.Errorf("list worktrees: %w", err)
	}
	if len(entries) == 0 {
		a.printf("\nNo managed worktrees found.\n")
		return nil
	}

	a.printf("\nBranch status:\n")
	lastSource := ""
	for _, e := range entries {
		if e.Source != lastSource {
			a.printf("  [%s]\n", e.Source)
			lastSource = e.Source
		}
		commits, equivalent, statusErr := a.cherryStatus(e.Name)
		if statusErr != nil {
			a.info(fmt.Sprintf("%s — status unknown (%v)", e.Name, statusErr))
			continue
		}
		if commits == 0 {
			if equivalent > 0 {
				a.info(fmt.Sprintf("%s — patch-equivalent to %s (safe to clean up)", e.Name, base))
			} else {
				a.info(fmt.Sprintf("%s — no commits ahead of %s (safe to clean up)", e.Name, base))
			}
		} else {
			a.info(fmt.Sprintf("%s — %d unmerged commit(s)", e.Name, commits))
		}
	}
	return nil
}

// Prune removes merged worktrees automatically. A worktree is prune-safe when:
// 1) all branch commits are already represented on the integration branch by
// patch equivalence (`git cherry` has no "+" lines), and
// 2) the worktree has no uncommitted changes.
func (a *App) Prune() error {
	base := a.integrationBranch()
	entries, err := a.allWorktreeEntries()
	if err != nil {
		return fmt.Errorf("list worktrees: %w", err)
	}

	removed := 0
	skipped := 0
	var failures []error
	for _, e := range entries {
		if !IsRealWorktree(e.Path) {
			skipped++
			failures = append(failures, fmt.Errorf("worktree path missing for %s: %s", e.Name, e.Path))
			continue
		}

		retained, retentionErr := a.retainsTaskWorktree(e.Name)
		if retentionErr != nil {
			skipped++
			failures = append(failures, fmt.Errorf("read retention for %s: %w", e.Name, retentionErr))
			continue
		}
		if retained {
			skipped++
			continue
		}
		_, head, candidateErr := a.cleanupCandidate(e.Path)
		if candidateErr != nil {
			skipped++
			failures = append(failures, candidateErr)
			continue
		}
		commits, equivalent, statusErr := a.cleanupCherryStatus(head)
		if statusErr != nil {
			skipped++
			failures = append(failures, fmt.Errorf("failed to assess %s against %s: %w", e.Name, base, statusErr))
			continue
		}
		if commits > 0 {
			a.info(fmt.Sprintf("%s — %d unmerged commit(s), skipping", e.Name, commits))
			skipped++
			continue
		}

		clean, cleanErr := a.isWorktreeClean(e.Path)
		if cleanErr != nil {
			skipped++
			failures = append(failures, fmt.Errorf("failed to read worktree status %s: %w", e.Name, cleanErr))
			continue
		}
		if !clean {
			a.info(fmt.Sprintf("%s — patch-equivalent but dirty, skipping", e.Name))
			skipped++
			continue
		}

		if equivalent > 0 {
			a.info(fmt.Sprintf("%s — patch-equivalent to %s, pruning", e.Name, base))
		} else {
			a.info(fmt.Sprintf("%s — no commits ahead of %s, pruning", e.Name, base))
		}
		if err := a.cleanupAtPath(e.Name, e.Path, CleanupOpts{}); err != nil {
			skipped++
			failures = append(failures, fmt.Errorf("failed to prune %s: %w", e.Name, err))
			continue
		}
		removed++
	}

	if err := a.gitRunWithDetails("worktree", "prune"); err != nil {
		failures = append(failures, fmt.Errorf("failed to prune worktree metadata: %w", err))
	}
	a.printf("\nPrune complete. removed=%d skipped=%d\n", removed, skipped)
	return errors.Join(failures...)
}
