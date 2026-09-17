//go:build !windows

package loop

import (
	"os/exec"
	"syscall"
	"testing"

	"github.com/poteto/noodle/internal/procx"
)

func TestAdmissionRecoveryDeadRootLiveGroup(t *testing.T) {
	tc, _ := recoveryFixture(t)
	// The root exits and is reaped while its child stays in the original group.
	cmd := exec.Command("sh", "-c", "sleep 120 >/dev/null 2>&1 &")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if procx.IsPIDAlive(pid) {
		t.Fatal("control root is still alive")
	}
	absent, err := procx.ProcessGroupAbsent(pid)
	if err != nil || absent {
		t.Fatalf("orphan group considered absent: %v %v", absent, err)
	}
	recoverySession(t, tc, "exited", pid)
	r := InspectAdmission(tc.projectDir, "noodle")
	if r.Status != "refused" {
		t.Fatalf("orphan group allowed retirement: %+v", r)
	}
	t.Logf("reaped root PID %d; live group refused", pid)
}
