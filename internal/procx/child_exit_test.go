//go:build darwin || linux

package procx

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestWaitChildExitPreservesOriginalChildUntilWait(t *testing.T) {
	for i := 0; i < 20; i++ {
		cmd := exec.Command("sh", "-c", "exit 42")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			time.Sleep(10 * time.Millisecond)
		}
		if err := WaitChildExit(cmd.Process.Pid); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal(err)
		}
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("exit observation reaped original PID: %v", err)
		}
		if err := cmd.Wait(); err == nil || cmd.ProcessState.ExitCode() != 42 {
			t.Fatalf("original wait lost exit status: %v, %v", err, cmd.ProcessState)
		}
	}
}
