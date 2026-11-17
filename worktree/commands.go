package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/poteto/noodle/internal/execution"
)

// CreateOpts holds optional parameters for Create.
type CreateOpts struct {
	// From is the branch or commit to base the new worktree on. Empty means HEAD.
	From string
}

// Create creates a new worktree and branch, symlinks local settings, and installs deps.
func (a *App) Create(name string, opts ...CreateOpts) error {
	var o CreateOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	wtPath := WorktreePath(a.Root, name)

	e, err := execution.New(a.Root, a.execution().Environ())
	if err != nil {
		return err
	}
	managed, err := PrepareStorageExcludes(context.Background(), e)
	if err != nil {
		return fmt.Errorf("prepare owned runtime excludes: %w", err)
	}
	if !managed {
		out, err := a.git("check-ignore", "-q", ".worktrees/").CombinedOutput()
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				return fmt.Errorf("check worktree ignore: %w: %s", err, out)
			}
			f, err := os.OpenFile(filepath.Join(a.Root, ".gitignore"), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
			if err != nil {
				return fmt.Errorf("open worktree ignore: %w", err)
			}
			_, writeErr := f.WriteString(".worktrees/\n")
			closeErr := f.Close()
			if writeErr != nil {
				return fmt.Errorf("write worktree ignore: %w", writeErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close worktree ignore: %w", closeErr)
			}
		}
	}

	if _, err := os.Stat(wtPath); err == nil {
		return fmt.Errorf("worktree '%s' already exists at %s", name, wtPath)
	}

	a.info(fmt.Sprintf("Creating worktree at %s...", wtPath))
	addArgs := []string{"worktree", "add", wtPath, "-b", name}
	if o.From != "" {
		addArgs = append(addArgs, o.From)
	}
	if err := a.gitRunWithDetails(addArgs...); err != nil {
		// Reuse an existing local branch if it already exists but has no worktree path.
		if a.branchExists(name) {
			if retryErr := a.gitRunWithDetails("worktree", "add", wtPath, name); retryErr == nil {
				goto created
			}
		}
		return fmt.Errorf("failed to create worktree: %w", err)
	}

created:
	if err := a.EnsureCustody(name); err != nil {
		return fmt.Errorf("bind created worker custody: %w", err)
	}
	settingsPath := filepath.Join(a.Root, ".claude", "settings.local.json")
	if _, err := os.Stat(settingsPath); err == nil {
		wtClaudeDir := filepath.Join(wtPath, ".claude")
		_ = os.MkdirAll(wtClaudeDir, 0o755)
		_ = os.Symlink(settingsPath, filepath.Join(wtClaudeDir, "settings.local.json"))
		a.info("Symlinked .claude/settings.local.json")
	}

	a.installDeps(wtPath)

	a.printf("\nWorktree ready: %s (branch: %s)\n", wtPath, name)
	return nil
}

// Exec runs a command inside the named worktree.
func (a *App) Exec(name string, args []string) error {
	wtPath := WorktreePath(a.Root, name)
	if _, err := os.Stat(wtPath); os.IsNotExist(err) {
		return fmt.Errorf("worktree '%s' does not exist at %s", name, wtPath)
	}

	cmd := a.execution().Command(context.Background(), wtPath, args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = a.stdout()
	cmd.Stderr = a.stderr()

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return &ExitError{Code: exitErr.ExitCode()}
		}
		return err
	}
	return nil
}
