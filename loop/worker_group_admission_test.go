//go:build !windows

package loop

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/poteto/noodle/dispatcher"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestWorkerGroupRetainsAdmissionAfterLauncherExit(t *testing.T) {
	for _, stop := range []string{"terminate", "force-kill", "cancel"} {
		t.Run(stop, func(t *testing.T) { checkWorkerGroupAdmission(t, stop) })
	}
}

func checkWorkerGroupAdmission(t *testing.T, stop string) {
	t.Helper()
	root := t.TempDir()
	runtimeDir := filepath.Join(root, ".noodle")
	d := dispatcher.NewProcessDispatcher(dispatcher.ProcessDispatcherConfig{
		ProjectDir: root, RuntimeDir: runtimeDir, RuntimeKind: "process",
		RuntimeDefault: `cat >/dev/null; sleep 30 </dev/null >/dev/null 2>&1 & echo $! > child.pid; printf '%s\n' '{"type":"thread.started","thread_id":"fixture"}'`,
	})
	rt := loopruntime.NewProcessRuntime(d, runtimeDir, 1)
	capacity := rt.(interface{ CapacityAvailable() bool })
	l := &Loop{projectDir: root, runtimeDir: runtimeDir, deps: Dependencies{Runtimes: map[string]loopruntime.Runtime{"process": rt}}}
	b := resourceBudget(l, 1)
	b.Observe(admission.Sample{At: time.Now(), Unsupported: true}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var session loopruntime.SessionHandle
	err := l.withAdmission(ctx, "process", func(ctx context.Context) error {
		var err error
		session, _, err = l.dispatchSession(ctx, loopruntime.DispatchRequest{
			Name: "group-fixture", Prompt: "fixture", Provider: "codex", Model: "fixture",
			Runtime: "process", WorktreePath: root, AllowPrimaryCheckout: true,
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	child := 0
	t.Cleanup(func() {
		if child > 0 {
			_ = syscall.Kill(child, syscall.SIGKILL)
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(filepath.Join(root, "child.pid"))
		child, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		pid, _ := procx.ReadPIDFile(filepath.Join(runtimeDir, "sessions", session.ID(), "process.json"))
		if child > 0 && pid > 0 && !procx.IsPIDAlive(pid) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if child <= 0 {
		t.Fatal("fixture descendant missing")
	}
	pid, err := procx.ReadPIDFile(filepath.Join(runtimeDir, "sessions", session.ID(), "process.json"))
	if err != nil || procx.IsPIDAlive(pid) {
		t.Fatalf("launcher did not exit: %v", err)
	}
	if err := syscall.Kill(child, 0); err != nil {
		t.Fatalf("fixture descendant exited early: %v", err)
	}
	select {
	case <-session.Done():
		t.Fatal("worker Done released while its owned process group is alive")
	case <-time.After(200 * time.Millisecond):
	}
	if b.Snapshot().Allocations != 1 || capacity.CapacityAvailable() {
		t.Fatal("live descendant lost resource admission")
	}
	if _, err := b.TryAcquire(root); err == nil {
		t.Fatal("another worker admitted over the live descendant")
	}
	// The original Session must still stop its group after the launcher exits.
	if stop == "terminate" {
		err = session.Terminate()
	} else if stop == "force-kill" {
		err = session.ForceKill()
	} else {
		cancel()
	}
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("confirmed group exit did not complete the worker")
	}
	if b.Snapshot().Allocations != 0 || !capacity.CapacityAvailable() {
		t.Fatal("confirmed group exit did not return admission")
	}
}
