package loop

import (
	"context"
	"os/exec"

	"github.com/poteto/noodle/internal/execution"
)

func firstExecution(contexts []*execution.Context) *execution.Context {
	if len(contexts) > 0 {
		return contexts[0]
	}
	return nil
}

func boundGitCommand(e *execution.Context, dir string, args ...string) *exec.Cmd {
	if e == nil {
		e = execution.Capture(dir)
	}
	return e.Command(context.Background(), dir, "git", args...)
}
