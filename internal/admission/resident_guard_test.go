package admission

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGuardResidentChild(t *testing.T) {
	stop := os.Getenv("NOODLE_GUARD_CHILD_STOP")
	if stop == "" {
		return
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stop); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func guardResidentFixture(t *testing.T) (*Residents, *Budget, ResidentSpec) {
	t.Helper()
	r, b, spec := residentFixture(t)
	stop := filepath.Join(t.TempDir(), "stop-child")
	spec.Argv[1] = "-test.run=^TestGuardResidentChild$"
	spec.Environment = []string{"NOODLE_GUARD_CHILD_STOP=" + stop}
	t.Cleanup(func() {
		if err := os.WriteFile(stop, nil, 0600); err != nil {
			t.Error(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := r.WaitClosed(ctx); err != nil {
			t.Error(err)
		}
	})
	return r, b, spec
}

func TestResidentGuardLastLease(t *testing.T) {
	r, b, spec := guardResidentFixture(t)
	var busy atomic.Bool
	busy.Store(true)
	var checks atomic.Int32
	guard, err := NewResidentGuard("custody/original", time.Second, func(context.Context) error {
		checks.Add(1)
		// Readback and reentrant mutation cannot deadlock the native owner.
		rows, err := r.Snapshot()
		if err != nil || len(rows) != 1 || rows[0].State != "running" || len(rows[0].Contexts) != 1 || rows[0].StopRequested != "" {
			return errors.New("guard observed lease removal or stop intent")
		}
		if _, err := r.Release(rows[0].ID, "b"); err == nil {
			return errors.New("reentrant release admitted")
		}
		if busy.Load() {
			return errors.New("VM Ready")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { busy.Store(false) })
	first, err := r.AcquireGuarded(spec, "a", b, guard)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := r.AcquireGuarded(spec, "b", b, guard)
	if err != nil || shared.PID != first.PID {
		t.Fatalf("share: %+v %v", shared, err)
	}
	for _, ctx := range []string{"foreign", "a", "a"} {
		if _, err := r.Release(first.ID, ctx); err != nil {
			t.Fatal(err)
		}
	}
	if checks.Load() != 0 {
		t.Fatal("nonfinal or duplicate release checked")
	}
	if err := r.ReleaseContext("b"); err == nil || !strings.Contains(err.Error(), "VM Ready") {
		t.Fatalf("busy release: %v", err)
	}
	rows, err := r.Snapshot()
	if err != nil || rows[0].State != "running" || !reflect.DeepEqual(rows[0].Contexts, []string{"b"}) || rows[0].StopRequested != "" || rows[0].Error == "" || b.Snapshot().Allocations != 1 {
		t.Fatalf("refusal lost ownership: %+v %v", rows, err)
	}
	select {
	case <-r.children[first.ID].done:
		t.Fatal("guard refusal stopped native child")
	default:
	}
	if _, err := r.AcquireGuarded(spec, "a", b, guard); err == nil {
		t.Fatal("released context replayed")
	}
	if _, err := r.Acquire(spec, "b", b); err == nil {
		t.Fatal("ordinary acquire downgraded protected service")
	}
	contextSpec := spec
	contextSpec.Sharing = "context"
	if _, err := r.Acquire(contextSpec, "b", b); err == nil {
		t.Fatal("changed sharing bypassed persistent guard identity")
	}
	if _, err := r.AcquireGuarded(contextSpec, "b", b, guard); err == nil {
		t.Fatal("changed sharing split protected service identity")
	}
	other, _ := NewResidentGuard("custody/other", time.Second, func(context.Context) error { return nil })
	sameID, _ := NewResidentGuard("custody/original", time.Second, func(context.Context) error { return nil })
	for _, wrong := range []*ResidentGuard{nil, {}, other, sameID} {
		if _, err := r.AcquireGuarded(spec, "b", b, wrong); err == nil {
			t.Fatal("foreign callback accepted")
		}
	}
	busy.Store(false)
	if got, err := r.Release(first.ID, "b"); err != nil || got.State != "stopping" {
		t.Fatalf("native graceful stop: %+v %v", got, err)
	}
	awaitResidentExit(t, r, first.ID)
	if b.Snapshot().Allocations != 0 || checks.Load() != 2 {
		t.Fatal("last native exit did not return allocation")
	}
	// Even an exited generation keeps its guard identity across restart.
	reopened, err := OpenResidents(filepath.Dir(r.path))
	if err != nil {
		t.Fatal(err)
	}
	b.Register("c")
	if _, err := reopened.Acquire(spec, "c", b); err == nil {
		t.Fatal("restart removed persistent guard")
	}
	if _, err := reopened.AcquireGuarded(spec, "c", b, other); err == nil {
		t.Fatal("restart changed guard identity")
	}
}

func TestResidentGuardTimeoutRetainsChild(t *testing.T) {
	r, b, spec := guardResidentFixture(t)
	entered, unblock := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	guard, _ := NewResidentGuard("blocked", 50*time.Millisecond, func(context.Context) error {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-unblock // deliberately ignore cancellation
		return nil
	})
	first, err := r.AcquireGuarded(spec, "a", b, guard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(unblock)
	})
	start := time.Now()
	got, err := r.Release(first.ID, "a")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second || got.State != "running" || len(got.Contexts) != 1 || b.Snapshot().Allocations != 1 {
		t.Fatalf("timeout lost owner: %+v %v", got, err)
	}
	<-entered
	if _, err := r.Release(first.ID, "a"); err == nil || calls.Load() != 1 {
		t.Fatal("timeout retried a still-running callback")
	}
	if _, err := r.AcquireGuarded(spec, "b", b, guard); err == nil {
		t.Fatal("acquire raced guard")
	}
}

func TestResidentGuardRestartAndForceRefusal(t *testing.T) {
	r, b, spec := guardResidentFixture(t)
	guard, _ := NewResidentGuard("original", time.Second, func(context.Context) error { return nil })
	spec.ForceAfterGrace = true
	if _, err := r.AcquireGuarded(spec, "a", b, guard); err == nil || b.Snapshot().Allocations != 0 {
		t.Fatal("forceful guarded service admitted")
	}
	spec.ForceAfterGrace = false
	first, err := r.AcquireGuarded(spec, "a", b, guard)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenResidents(filepath.Dir(r.path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.AcquireGuarded(spec, "b", b, guard); err == nil {
		t.Fatal("restart adopted live child")
	}
	if _, err := reopened.Release(first.ID, "a"); err == nil {
		t.Fatal("restart without callback released live child")
	}
	if _, err := r.Release(first.ID, "a"); err != nil {
		t.Fatal(err)
	}
	awaitResidentExit(t, r, first.ID)
}

func TestResidentGuardRefusalPersistenceFailure(t *testing.T) {
	r, b, spec := guardResidentFixture(t)
	refused := errors.New("VM effects unknown")
	guard, _ := NewResidentGuard("original", time.Second, func(context.Context) error { return refused })
	first, err := r.AcquireGuarded(spec, "a", b, guard)
	if err != nil {
		t.Fatal(err)
	}
	backup := r.path + ".before-failure"
	if err := os.Rename(r.path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(r.path, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(r.path)
		_ = os.Rename(backup, r.path)
	})
	got, err := r.Release(first.ID, "a")
	if !errors.Is(err, refused) || got.State != "running" || len(got.Contexts) != 1 || got.StopRequested != "" || b.Snapshot().Allocations != 1 {
		t.Fatalf("refusal persistence lost original ownership: %+v %v", got, err)
	}
	if _, retry := r.Release(first.ID, "a"); retry == nil {
		t.Fatal("persistence failure disappeared")
	}
}
