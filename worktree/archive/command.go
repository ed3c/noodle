package archive

import (
	"context"
	"os/exec"
	"time"
)

func timedCommand(timeout time.Duration, binary string, args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.WaitDelay = 2 * time.Second
	isolateCommand(cmd)
	return cmd, cancel
}
