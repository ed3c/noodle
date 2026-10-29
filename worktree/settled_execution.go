package worktree

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ObserveSettledExecutionAbsent only reads the original project's Git and
// filesystem. It never repairs registrations or deletes a recreated branch.
func (a *App) ObserveSettledExecutionAbsent(name, expectedHead string) error {
	path, err := a.settledExecutionPath(name, expectedHead)
	if err != nil {
		return err
	}
	if err := a.verifySettledExecutionAbsent(path, ""); err != nil {
		return err
	}
	refs, err := a.settledExecutionRefs(name)
	if err != nil {
		return err
	}
	if len(refs) != 0 {
		return fmt.Errorf("settled execution branch is present for %s", name)
	}
	return nil
}

// RemoveSettledExecutionCopy removes a named Noodle worker copy after its owner
// has settled the task and durably archived the original runtime evidence. The
// caller owns that authorization and excludes concurrent task execution. This
// operation bypasses task retention, but never forces worktree removal.
func (a *App) RemoveSettledExecutionCopy(name, expectedHead string) error {
	path, err := a.settledExecutionPath(name, expectedHead)
	if err != nil {
		return err
	}
	registrations, err := a.cleanupWorktreeRegistrations()
	if err != nil {
		return err
	}
	refs, err := a.settledExecutionRefs(name)
	if err != nil {
		return err
	}
	// Name-only retries cannot distinguish two historical branch conventions.
	// Refuse ambiguity before removal so a retry cannot select the other ref.
	if len(refs) > 1 {
		return fmt.Errorf("execution branch ambiguous for %s", name)
	}
	info, statErr := os.Lstat(path)
	if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("stat execution copy: %w", statErr)
	}
	var candidate *cleanupWorktreeRegistration
	for i := range registrations {
		r := &registrations[i]
		if r.path == path {
			candidate = r
		}
		if r.path != path && (r.branch == "refs/heads/"+name || r.branch == "refs/heads/noodle/"+name) {
			return fmt.Errorf("execution branch registered at another worktree: %s", r.path)
		}
	}
	branch := ""
	if os.IsNotExist(statErr) {
		if candidate != nil {
			return fmt.Errorf("missing execution copy still registered: %s", path)
		}
		for ref := range refs {
			branch = ref
		}
	} else {
		if !info.IsDir() || candidate == nil {
			return fmt.Errorf("execution copy is not a registered linked worktree: %s", path)
		}
		branch = candidate.branch
		if branch != "refs/heads/"+name && branch != "refs/heads/noodle/"+name {
			return fmt.Errorf("execution copy branch does not match %s: %s", name, branch)
		}
		if candidate.head != expectedHead || refs[branch] != expectedHead {
			return fmt.Errorf("execution copy HEAD changed for %s", name)
		}
		if err := a.verifySettledExecutionCopy(path, branch, expectedHead); err != nil {
			return err
		}
		if err := a.gitRunWithDetails("worktree", "remove", path); err != nil {
			return fmt.Errorf("remove settled execution copy %s: %w", name, err)
		}
	}
	// A missing directory alone is not proof that Git released the worktree.
	if err := a.verifySettledExecutionAbsent(path, branch); err != nil {
		return err
	}
	if branch != "" {
		if refs[branch] != expectedHead {
			return fmt.Errorf("execution branch tip changed for %s", name)
		}
		// Git checks the old object ID under the ref lock; no -D/--force fallback.
		if err := a.gitRunWithDetails("update-ref", "--no-deref", "-d", branch, expectedHead); err != nil {
			return fmt.Errorf("delete settled execution branch %s: %w", branch, err)
		}
	}
	if err := a.verifySettledExecutionAbsent(path, branch); err != nil {
		return err
	}
	remaining, err := a.settledExecutionRefs(name)
	if err != nil {
		return err
	}
	if branch != "" && remaining[branch] != "" {
		return fmt.Errorf("settled execution branch still present: %s", branch)
	}
	if branch == "" && len(remaining) != 0 {
		return fmt.Errorf("execution branch appeared during readback for %s", name)
	}
	return nil
}

func (a *App) settledExecutionPath(name, head string) (string, error) {
	if name == "" || name == "." || strings.Contains(name, `\`) || filepath.IsAbs(name) ||
		filepath.ToSlash(filepath.Clean(filepath.FromSlash(name))) != name || name == ".." || strings.HasPrefix(name, "../") {
		return "", fmt.Errorf("execution copy name is not a managed relative name: %q", name)
	}
	if len(head) != 40 && len(head) != 64 {
		return "", fmt.Errorf("expected execution HEAD is not a full object ID")
	}
	if _, err := hex.DecodeString(head); err != nil {
		return "", fmt.Errorf("invalid expected execution HEAD: %w", err)
	}
	root := canonicalPath(a.Root)
	top, err := a.gitOutput("rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("read execution project root: %w", err)
	}
	if canonicalPath(top) != root {
		return "", fmt.Errorf("execution project root changed: %s", top)
	}
	if err := a.gitRunWithDetails("check-ref-format", "refs/heads/"+name); err != nil {
		return "", fmt.Errorf("invalid execution branch name: %w", err)
	}
	resolved, err := a.gitOutput("rev-parse", "--verify", head+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("read expected execution commit %s: %w", head, err)
	}
	if resolved != head {
		return "", fmt.Errorf("expected execution commit differs: %s", head)
	}
	path := WorktreePath(root, name)
	for current := path; current != root; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("stat execution path %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("execution path is a symlink: %s", current)
		}
	}
	return path, nil
}

func (a *App) verifySettledExecutionCopy(path, branch, head string) error {
	info, err := os.Lstat(filepath.Join(path, ".git"))
	if err != nil {
		return fmt.Errorf("read linked execution metadata: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("execution copy has no regular linked .git file: %s", path)
	}
	common, err := a.gitOutput("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("read project Git directory: %w", err)
	}
	linkedCommon, err := a.gitOutput("-C", path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("read execution Git directory: %w", err)
	}
	if canonicalPath(common) != canonicalPath(linkedCommon) {
		return fmt.Errorf("execution copy belongs to a different project: %s", path)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if IsCWDInsideWorktree(canonicalPath(cwd), path) {
		return fmt.Errorf("shell CWD is inside the execution copy: %s", path)
	}
	dirty, err := a.gitOutput("-C", path, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return fmt.Errorf("read execution copy status: %w", err)
	}
	if dirty != "" {
		return fmt.Errorf("execution copy has uncommitted changes: %s", path)
	}
	actualBranch, err := a.gitOutput("-C", path, "symbolic-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("read execution branch: %w", err)
	}
	actualHead, err := a.gitOutput("-C", path, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read execution HEAD: %w", err)
	}
	if actualBranch != branch || actualHead != head {
		return fmt.Errorf("execution candidate changed: %s", path)
	}
	return nil
}

type cleanupWorktreeRegistration struct{ path, branch, head string }

func (a *App) cleanupWorktreeRegistrations() ([]cleanupWorktreeRegistration, error) {
	out, err := a.gitOutput("worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, fmt.Errorf("read worktree registrations: %w", err)
	}
	var entries []cleanupWorktreeRegistration
	var current cleanupWorktreeRegistration
	for _, field := range strings.Split(out, "\x00") {
		switch {
		case strings.HasPrefix(field, "worktree "):
			current = cleanupWorktreeRegistration{path: canonicalPath(strings.TrimPrefix(field, "worktree "))}
		case strings.HasPrefix(field, "HEAD "):
			current.head = strings.TrimPrefix(field, "HEAD ")
		case strings.HasPrefix(field, "branch "):
			current.branch = strings.TrimPrefix(field, "branch ")
		case field == "" && current.path != "":
			entries = append(entries, current)
			current = cleanupWorktreeRegistration{}
		}
	}
	return entries, nil
}

func (a *App) settledExecutionRefs(name string) (map[string]string, error) {
	bare, prefixed := "refs/heads/"+name, "refs/heads/noodle/"+name
	out, err := a.gitOutput("for-each-ref", "--format=%(refname) %(objectname)", bare, prefixed)
	if err != nil {
		return nil, fmt.Errorf("read execution branch refs: %w", err)
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		ref, head, _ := strings.Cut(line, " ")
		if ref == bare || ref == prefixed {
			refs[ref] = head
		}
	}
	return refs, nil
}

func (a *App) verifySettledExecutionAbsent(path, branch string) error {
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		if err != nil {
			return fmt.Errorf("read execution copy absence: %w", err)
		}
		return fmt.Errorf("settled execution copy still present: %s", path)
	}
	registrations, err := a.cleanupWorktreeRegistrations()
	if err != nil {
		return err
	}
	for _, r := range registrations {
		if r.path == path {
			return fmt.Errorf("settled execution copy still registered: %s", path)
		}
		if branch != "" && r.branch == branch {
			return fmt.Errorf("execution branch registered at another worktree: %s", r.path)
		}
	}
	return nil
}
