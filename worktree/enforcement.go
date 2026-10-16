package worktree

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/poteto/noodle/internal/execution"
)

// ValidateLinkedCheckout resolves and validates a checkout path.
// It enforces that the path exists, is a git checkout, and is NOT a primary
// checkout (".git"). This structurally requires linked worktrees for agent work.
func ValidateLinkedCheckout(path string) (string, error) {
	return ValidateLinkedCheckoutWithExecution(path, execution.Capture("."))
}

func ValidateLinkedCheckoutWithExecution(path string, e *execution.Context) (string, error) {
	raw := strings.TrimSpace(path)
	if raw == "" {
		return "", fmt.Errorf("linked worktree path is required")
	}

	absPath, err := e.ResolvePath(raw)
	if err != nil {
		return "", fmt.Errorf("resolve worktree path: %w", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return "", fmt.Errorf("stat worktree path %q: %w", absPath, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("worktree path is not a directory: %s", absPath)
	}

	gitDir, err := absoluteGitDirWithExecution(absPath, e)
	if err != nil {
		return "", fmt.Errorf("path is not a git checkout: %s: %w", absPath, err)
	}
	if IsPrimaryCheckout(gitDir) {
		return "", fmt.Errorf("primary checkout is forbidden for agent work: %s", absPath)
	}

	return absPath, nil
}

func absoluteGitDir(dir string) (string, error) {
	return absoluteGitDirWithExecution(dir, execution.Capture(dir))
}

func absoluteGitDirWithExecution(dir string, e *execution.Context) (string, error) {
	out, err := e.Command(context.Background(), dir, "git", "rev-parse", "--absolute-git-dir").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --absolute-git-dir: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}
