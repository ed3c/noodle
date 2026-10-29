//go:build darwin || linux

package dispatcher

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOwnedWorkerReclaimsDescendantsAfterLauncherCompletes(t *testing.T) {
	unrelated := exec.Command("sleep", "30")
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() })
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	h, err := startProcess(exec.Command("sh", "-c", `sleep 30 </dev/null >/dev/null 2>&1 & echo $! > "$1"; printf retained-result`, "sh", pidFile), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.ForceKill(); _ = h.Stdout().Close(); _ = h.Stderr().Close() })
	select {
	case <-h.Done():
	case err := <-h.waitProblem:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("completed launcher left its owned group running")
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("owned descendant still present after Done: %v", err)
	}
	output, err := io.ReadAll(h.Stdout())
	if err != nil || string(output) != "retained-result" {
		t.Fatalf("original output changed: %q, %v", output, err)
	}
	if code, exited := h.ExitCode(); !exited || code != 0 {
		t.Fatalf("native launcher outcome changed: %d, %v", code, exited)
	}
	if err := h.ForceKill(); err != nil {
		t.Fatalf("repeat stop after actual exit: %v", err)
	}
	if err := unrelated.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("cleanup stopped an unrelated process: %v", err)
	}
}
