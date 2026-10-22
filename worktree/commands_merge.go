package worktree

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// MergeOpts holds optional parameters for Merge.
type MergeOpts struct {
	// Into overrides the integration branch for this merge.
	Into           string
	ExpectedHead   string
	ExpectedTree   string
	RetainWorktree bool
}

// Merge rebases the worktree branch onto a target branch, merges, and cleans up.
func (a *App) Merge(name string, opts ...MergeOpts) error {
	var o MergeOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	base := a.integrationBranch()
	if o.Into != "" {
		base = o.Into
	}
	wtPath := WorktreePath(a.Root, name)
	mergeBranch := a.resolveBranchName(name)
	if err := a.acquireMergeLock(); err != nil {
		return err
	}
	defer a.releaseMergeLock()

	if _, err := os.Stat(wtPath); os.IsNotExist(err) {
		return fmt.Errorf("worktree '%s' does not exist at %s", name, wtPath)
	}
	if err := a.assertCWDSafe(name); err != nil {
		return err
	}

	branch, _ := a.gitOutput("branch", "--show-current")
	if branch != base {
		return fmt.Errorf("not on %s branch (on '%s'). Run: git checkout %s", base, branch, base)
	}

	if err := a.assertRootClean(); err != nil {
		return err
	}

	if o.ExpectedHead != "" || o.ExpectedTree != "" {
		head, err := a.gitOutput("-C", wtPath, "rev-parse", "HEAD")
		if err != nil || head != o.ExpectedHead {
			return fmt.Errorf("reviewed candidate HEAD changed")
		}
		tree, err := a.gitOutput("-C", wtPath, "rev-parse", "HEAD^{tree}")
		if err != nil || tree != o.ExpectedTree {
			return fmt.Errorf("reviewed candidate tree changed")
		}
		dirty, err := a.gitOutput("-C", wtPath, "status", "--porcelain", "--untracked-files=all")
		if err != nil || dirty != "" {
			return fmt.Errorf("reviewed candidate worktree changed")
		}
		if err := a.git("merge-base", "--is-ancestor", base, o.ExpectedHead).Run(); err != nil {
			return fmt.Errorf("reviewed candidate integration branch advanced")
		}
		mergeBranch, err = a.gitOutput("-C", wtPath, "symbolic-ref", "--short", "HEAD")
		if err != nil || mergeBranch == "" {
			return fmt.Errorf("reviewed candidate branch unavailable")
		}
	}

	commits := a.countUnmergedCommits(name)
	if o.ExpectedHead != "" {
		count, err := a.gitOutput("rev-list", "--count", base+".."+o.ExpectedHead)
		if err != nil {
			return fmt.Errorf("count reviewed commits: %w", err)
		}
		commits, err = strconv.Atoi(count)
		if err != nil {
			return fmt.Errorf("parse reviewed commit count: %w", err)
		}
	}
	if commits == 0 {
		a.info(fmt.Sprintf("No commits on '%s' — nothing to merge", name))
		a.info(fmt.Sprintf("Use '%s cleanup %s' to remove without merging", a.cmdName(), name))
		return nil
	}
	a.info(fmt.Sprintf("%d commit(s) to merge", commits))

	if o.ExpectedHead == "" {
		stashed := false
		unstaged := a.git("-C", wtPath, "diff", "--quiet").Run() != nil
		staged := a.git("-C", wtPath, "diff", "--cached", "--quiet").Run() != nil
		if unstaged || staged {
			a.info("Stashing worktree changes before rebase...")
			if a.git("-C", wtPath, "stash", "--include-untracked", "--quiet").Run() == nil {
				stashed = true
			}
		}

		a.info(fmt.Sprintf("Rebasing %s onto %s...", name, base))
		if err := a.git("-C", wtPath, "rebase", base).Run(); err != nil {
			if stashed {
				_ = a.git("-C", wtPath, "rebase", "--abort").Run()
				_ = a.git("-C", wtPath, "stash", "pop", "--quiet").Run()
			}
			if a.hasMergeConflicts(wtPath) {
				return &MergeConflictError{Branch: mergeBranch, Err: err}
			}
			return fmt.Errorf("rebase failed — resolve conflicts manually in %s", wtPath)
		}

		if stashed {
			_ = a.git("-C", wtPath, "stash", "pop", "--quiet").Run()
		}
	}

	a.info(fmt.Sprintf("Merging %s into %s...", mergeBranch, base))
	mergeArgs := []string{"merge", mergeBranch}
	if o.ExpectedHead != "" {
		mergeArgs = []string{"merge", "--ff-only", o.ExpectedHead}
	}
	if err := a.gitRun(mergeArgs...); err != nil {
		if a.hasMergeConflicts(a.Root) {
			return &MergeConflictError{Branch: mergeBranch, Err: err}
		}
		return fmt.Errorf("merge failed: %w", err)
	}

	var warnings []string
	if o.RetainWorktree {
		a.info("Retaining task worktree and branch until explicit deletion")
	} else if o.ExpectedHead == "" {
		warnings = a.cleanupWorktreeAndBranch(wtPath, mergeBranch, false)
	} else {
		// A concurrent Git writer does not take our merge lock. Preserve its work.
		head, headErr := a.gitOutput("-C", wtPath, "rev-parse", "HEAD")
		currentBranch, branchErr := a.gitOutput("-C", wtPath, "symbolic-ref", "--short", "HEAD")
		if headErr != nil || branchErr != nil || head != o.ExpectedHead || currentBranch != mergeBranch {
			warnings = append(warnings, "candidate merged; worktree changed and was preserved")
		} else if err := a.git("worktree", "remove", wtPath).Run(); err != nil {
			warnings = append(warnings, "candidate merged; worktree cleanup refused and was preserved")
		} else if err := a.git("branch", "-d", mergeBranch).Run(); err != nil {
			warnings = append(warnings, "candidate merged; branch cleanup refused and was preserved")
		}
	}

	a.installDeps(a.Root)
	for _, warning := range warnings {
		a.warnf("WARNING: %s\n", warning)
	}

	if o.RetainWorktree {
		a.printf("\nDone. %s merged into %s; task worktree retained.\n", name, base)
	} else {
		a.printf("\nDone. %s merged into %s and cleaned up.\n", name, base)
	}
	return nil
}

func (a *App) MergeRemoteBranch(branch string) error {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return fmt.Errorf("remote branch not set")
	}

	base := a.integrationBranch()
	if err := a.acquireMergeLock(); err != nil {
		return err
	}
	defer a.releaseMergeLock()

	current, _ := a.gitOutput("branch", "--show-current")
	if current != base {
		return fmt.Errorf("not on %s branch (on '%s'). Run: git checkout %s", base, current, base)
	}
	if err := a.assertRootClean(); err != nil {
		return err
	}

	a.info(fmt.Sprintf("Fetching remote branch %s...", branch))
	if err := a.gitRun("fetch", "origin", branch); err != nil {
		return fmt.Errorf("fetch remote branch %s: %w", branch, err)
	}

	mergeRef := "origin/" + branch
	a.info(fmt.Sprintf("Merging %s into %s...", mergeRef, base))
	if err := a.gitRun("merge", mergeRef); err != nil {
		if a.hasMergeConflicts(a.Root) {
			return &MergeConflictError{Branch: mergeRef, Err: err}
		}
		return fmt.Errorf("merge remote branch %s: %w", branch, err)
	}

	a.cleanRemote(branch)

	a.installDeps(a.Root)
	a.printf("\nDone. %s merged into %s.\n", mergeRef, base)
	return nil
}
