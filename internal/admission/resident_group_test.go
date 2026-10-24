//go:build !windows

package admission

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestResidentLauncherExitRetainsAllocationUntilGroupAbsent(t *testing.T) {
	r, b, spec := residentFixture(t)
	spec.Argv = []string{"/bin/sh", "-c", "sleep 60 & echo $! > child.pid"}
	spec.Environment = []string{"PATH=/usr/bin:/bin"}
	v, err := r.Acquire(spec, "a", b)
	if err != nil {
		t.Fatal(err)
	}
	var child int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(filepath.Join(spec.Directory, "child.pid"))
		child, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		rows, _ := r.Snapshot()
		if child > 0 && rows[0].State == "needs_process_readback" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if child <= 0 {
		t.Fatal("fixture child missing")
	}
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	rows, err := r.Reconcile(context.Background())
	if err != nil || rows[0].State != "needs_process_readback" || b.Snapshot().Allocations != 1 {
		t.Fatalf("live descendant lost its quota: %+v %v", rows, err)
	}
	if _, err := r.Acquire(spec, "b", b); err == nil {
		t.Fatal("unknown resident was reused")
	}
	reopened, err := OpenResidents(filepath.Dir(r.path))
	if err != nil {
		t.Fatal(err)
	}
	newBudget := NewBudget(4)
	newBudget.Register("new-context")
	newBudget.Observe(Sample{At: time.Now(), Unsupported: true}, nil)
	reopened.BindBudget(newBudget)
	if _, err := newBudget.TryAcquire("new-context"); err == nil {
		t.Fatal("Host restart forgot unresolved resident")
	}
	if s := newBudget.Snapshot(); s.UnresolvedResidents != 1 || s.Limit != 0 {
		t.Fatalf("recovery not exposed: %+v", s)
	}
	if err := syscall.Kill(child, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, err = r.Reconcile(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if rows[0].State == "exited" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	awaitResidentExit(t, r, v.ID)
	if b.Snapshot().Allocations != 0 {
		t.Fatal("quota retained after observed group exit")
	}
	if _, err := reopened.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, err := newBudget.TryAcquire("new-context")
	if err != nil {
		t.Fatal("confirmed group exit did not restore admission", err)
	}
	p.Release()
}

func TestResidentStopsOwnedProcessGroup(t *testing.T) {
	r, b, spec := residentFixture(t)
	spec.Argv = []string{"/bin/sh", "-c", "sleep 60 & echo $! > child.pid; wait"}
	spec.Environment = []string{"PATH=/usr/bin:/bin"}
	v, err := r.Acquire(spec, "a", b)
	if err != nil {
		t.Fatal(err)
	}
	var child int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(filepath.Join(spec.Directory, "child.pid"))
		child, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		if child > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if child <= 0 {
		t.Fatal("fixture child missing")
	}
	// This PID comes only from our isolated fixture, never from production inventory.
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	if _, err = r.Release(v.ID, "a"); err != nil {
		t.Fatal(err)
	}
	awaitResidentExit(t, r, v.ID)
	if err := syscall.Kill(child, 0); err != syscall.ESRCH {
		t.Fatalf("child still exists after resident exited: %v", err)
	}
	if b.Snapshot().Allocations != 0 {
		t.Fatal("allocation retained after entire group exited")
	}
}
