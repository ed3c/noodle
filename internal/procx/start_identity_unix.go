//go:build !windows

package procx

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func processStartedAfter(pid int, recorded time.Time) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	data, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("read process start identity: %w", err)
	}
	observed, err := time.ParseInLocation(time.ANSIC, strings.TrimSpace(string(data)), time.UTC)
	if err != nil {
		return false, fmt.Errorf("parse process start identity: %w", err)
	}
	// Dispatcher records after spawn. A strictly later OS start proves reuse.
	// POSIX does not reuse a PID while its original process group still exists.
	return observed.After(recorded), nil
}
