package loop

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/worktree"
)

const localTaskMergeKey = "local_task_merge"
const localTaskMergeRecoveryKey = state.LocalTaskMergeRecoveryKey

type localTaskFileRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type localTaskMergeBinding struct {
	Candidate localTaskFileRef `json:"candidate"`
	Head      string           `json:"head"`
	Tree      string           `json:"tree"`
}

func readLocalTaskCandidate(ref localTaskFileRef) (localTaskCandidate, error) {
	var c localTaskCandidate
	if !filepath.IsAbs(ref.Path) || ref.SHA256 == "" {
		return c, fmt.Errorf("local task candidate reference missing")
	}
	data, err := os.ReadFile(ref.Path)
	if err != nil {
		return c, err
	}
	if recoveryDigest(data) != ref.SHA256 {
		return c, fmt.Errorf("local task candidate source changed")
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	if c.Schema != 1 || c.OrderID == "" || c.SessionID == "" || c.AttemptID == "" || !filepath.IsAbs(c.Worktree) || !reconciliationSHA.MatchString(c.Head) || len(c.Files) == 0 {
		return c, fmt.Errorf("local task candidate shape invalid")
	}
	for name := range c.Files {
		if !localTaskSafePath(name) {
			return c, fmt.Errorf("local task candidate path invalid")
		}
	}
	return c, nil
}

func (l *Loop) controlMergeLocalTask(cmd ControlCommand) error {
	ref := localTaskFileRef{Path: cmd.Value, SHA256: cmd.Target}
	c, err := readLocalTaskCandidate(ref)
	if err != nil {
		return err
	}
	order, ok := l.canonical.Orders[cmd.OrderID]
	pending := l.cooks.pendingReview[cmd.OrderID]
	review, hasReview := l.canonical.PendingReviews[cmd.OrderID]
	if !ok || len(order.Stages) != 1 || !hasReview || pending == nil || order.Status != state.OrderActive || order.Stages[0].Status != state.StageReview || !l.isAdmittedLocalTask(order, order.Stages[0]) {
		return fmt.Errorf("local task candidate review missing")
	}
	stage := order.Stages[0]
	if _, exists := stage.Extra[localTaskMergeKey]; exists {
		return fmt.Errorf("local task merge already prepared; original merge readback required")
	}
	b, err := l.captureRequestChanges(pending)
	if err != nil {
		return err
	}
	if err := l.validateRecovery(order, review, b); err != nil {
		return err
	}
	if err := l.validateLocalTaskCandidate(c, order, b); err != nil {
		return err
	}
	var prompt struct {
		WritePaths []string `json:"write_paths"`
	}
	if err := json.Unmarshal([]byte(stage.Prompt), &prompt); err != nil {
		return err
	}
	for name := range c.Files {
		if !slices.Contains(prompt.WritePaths, name) {
			return fmt.Errorf("local task candidate exceeds admitted write paths")
		}
	}
	// The native worktree owner pins the control branch before any candidate commit.
	app, ok := l.deps.Worktree.(*worktree.App)
	if !ok || app.IntegrationBranch == "" {
		return fmt.Errorf("local task integration owner is unbound")
	}
	branch, err := recoveryGitWithExecution(l.deps.Execution, l.projectDir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return err
	}
	if branch != app.IntegrationBranch {
		return fmt.Errorf("local task integration branch changed")
	}
	committed, err := boundGitCommand(l.deps.Execution, b.WorktreePath, "-C", b.WorktreePath, "diff", "--name-only", "--no-renames", "-z", app.IntegrationBranch+"...HEAD", "--").Output()
	if err != nil {
		return err
	}
	for _, name := range strings.Split(string(committed), "\x00") {
		if name != "" && !slices.Contains(prompt.WritePaths, name) {
			return fmt.Errorf("local task committed path exceeds admission: %s", name)
		}
	}
	args := []string{"add", "-A", "--"}
	for _, name := range prompt.WritePaths {
		args = append(args, ":(literal)"+name)
	}
	if _, err := recoveryGitWithExecution(l.deps.Execution, b.WorktreePath, args...); err != nil {
		return err
	}
	// Staging may change the index; inspect the exact staged content before commit.
	for name, expected := range c.Files {
		if expected == nil {
			if _, err := boundGitCommand(l.deps.Execution, b.WorktreePath, "-C", b.WorktreePath, "show", ":"+filepath.ToSlash(name)).Output(); err == nil {
				return fmt.Errorf("local task staged deletion differs: %s", name)
			}
			continue
		}
		data, err := boundGitCommand(l.deps.Execution, b.WorktreePath, "-C", b.WorktreePath, "show", ":"+filepath.ToSlash(name)).Output()
		if err != nil || recoveryDigest(data) != expected.SHA256 {
			return fmt.Errorf("local task staged bytes differ: %s", name)
		}
	}
	if err := l.validateLocalTaskFiles(c, b.WorktreePath); err != nil {
		return err
	}
	staged, err := boundGitCommand(l.deps.Execution, b.WorktreePath, "-C", b.WorktreePath, "ls-files", "--stage", "-z").Output()
	if err != nil {
		return err
	}
	for _, entry := range strings.Split(string(staged), "\x00") {
		fields, name, found := strings.Cut(entry, "\t")
		if !found {
			continue
		}
		expected, reviewed := c.Files[name]
		if !reviewed {
			continue
		}
		mode := "100644"
		if expected != nil && expected.Executable {
			mode = "100755"
		}
		if expected == nil || !strings.HasPrefix(fields, mode+" ") || !strings.HasSuffix(fields, " 0") {
			return fmt.Errorf("local task staged mode differs: %s", name)
		}
	}
	changed, err := recoveryGitWithExecution(l.deps.Execution, b.WorktreePath, "diff", "--cached", "--name-only")
	if err != nil {
		return err
	}
	if changed != "" {
		if _, err := recoveryGitWithExecution(l.deps.Execution, b.WorktreePath, "-c", "user.name=Noodle", "-c", "user.email=noodle@localhost", "commit", "-m", "Apply reviewed local task "+cmd.Target); err != nil {
			return err
		}
	}
	head, err := recoveryGitWithExecution(l.deps.Execution, b.WorktreePath, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	tree, err := recoveryGitWithExecution(l.deps.Execution, b.WorktreePath, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return err
	}
	dirty, err := recoveryGitWithExecution(l.deps.Execution, b.WorktreePath, "status", "--porcelain")
	if err != nil {
		return err
	}
	if err := l.validateLocalTaskFiles(c, b.WorktreePath); err != nil {
		return err
	}
	if dirty != "" {
		return fmt.Errorf("local task candidate changed during commit")
	}
	changedPaths, err := boundGitCommand(l.deps.Execution, b.WorktreePath, "-C", b.WorktreePath, "diff", "--name-only", "--no-renames", "-z", c.Head, head, "--").Output()
	if err != nil {
		return err
	}
	for _, name := range strings.Split(string(changedPaths), "\x00") {
		if name != "" && !slices.Contains(prompt.WritePaths, name) {
			return fmt.Errorf("local task commit includes unreviewed path: %s", name)
		}
	}
	raw, err := json.Marshal(localTaskMergeBinding{Candidate: ref, Head: head, Tree: tree})
	if err != nil {
		return err
	}
	if stage.Extra == nil {
		stage.Extra = map[string]json.RawMessage{}
	}
	stage.Extra[localTaskMergeKey] = raw
	order.Stages[0] = stage
	l.canonical.Orders[cmd.OrderID] = order
	if err := l.persistCanonicalCheckpoint(); err != nil {
		return err
	}
	if err := l.syncPendingReviewProjection(); err != nil {
		return err
	}
	return l.controlMerge(cmd.OrderID)
}
