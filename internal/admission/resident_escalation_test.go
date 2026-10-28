//go:build !windows

package admission

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/testutil/residentchild"
)

func TestResidentUnresponsiveHelper(t *testing.T) {
	if os.Getenv("NOODLE_RESIDENT_TEST_CHILD") != "1" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	if waitLegacyRecoveryProbe(t) {
		return
	}
	if err := os.WriteFile("ready", []byte("ready"), 0600); err != nil {
		os.Exit(2)
	}
	if err := residentchild.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestResidentUnleasedForcePolicyEndsUnresponsiveChild(t *testing.T) {
	r, b, spec := residentFixture(t)
	// Decode the public contract so this also demonstrates an older Host's gap.
	if err := json.Unmarshal([]byte(`{"force_after_grace":true}`), &spec); err != nil {
		t.Fatal(err)
	}
	spec.Argv[1] = "-test.run=^TestResidentUnresponsiveHelper$"
	v, err := r.Acquire(spec, "a", b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(filepath.Join(spec.Directory, "exit"), []byte("exit"), 0600) })
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(spec.Directory, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = r.Acquire(spec, "b", b); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Release(v.ID, "a"); err != nil {
		t.Fatal(err)
	}
	rows, _ := r.Snapshot()
	if rows[0].State != "running" || len(rows[0].Contexts) != 1 {
		t.Fatal("shared consumer lost", rows)
	}
	if _, err = r.Release(v.ID, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Release(v.ID, "b"); err != nil {
		t.Fatal(err)
	}
	awaitResidentExit(t, r, v.ID)
	if b.Snapshot().Allocations != 0 {
		t.Fatal("allocation retained after original child and group exited")
	}
	rows, _ = r.Snapshot()
	if rows[0].StopRequested == "" || rows[0].ForceRequested == "" || len(rows[0].Errors) == 0 {
		t.Fatal("stop/force/exit history missing", rows)
	}
	select {
	case <-r.Released():
	default:
		t.Fatal("original Host was not notified of observed release")
	}
}

func TestResidentUnknownIdentityNeverSignalsAndHistorySurvives(t *testing.T) {
	r, b, spec := residentFixture(t)
	spec.Argv[1] = "-test.run=^TestResidentUnresponsiveHelper$"
	v, err := r.Acquire(spec, "a", b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(filepath.Join(spec.Directory, "exit"), nil, 0600) })
	r.mu.Lock()
	r.records[v.ID].Started = ""
	r.mu.Unlock()
	if _, err = r.Release(v.ID, "a"); err != nil {
		t.Fatal(err)
	}
	rows, _ := r.Snapshot()
	if rows[0].State != "needs_owner_readback" || b.Snapshot().Allocations != 1 || len(rows[0].Errors) == 0 {
		t.Fatal("unknown identity lost protection or failure history", rows)
	}
	if err := os.WriteFile(filepath.Join(spec.Directory, "exit"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	awaitResidentExit(t, r, v.ID)
	if err := os.Remove(filepath.Join(spec.Directory, "exit")); err != nil {
		t.Fatal(err)
	}
	next, err := r.Acquire(spec, "b", b)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.History) != 1 || len(next.History[0].Errors) == 0 || next.History[0].PID != v.PID {
		t.Fatal("next generation erased original failure", next)
	}
}

func TestResidentReapAndSignalShareOriginalChildLifetime(t *testing.T) {
	r, b, spec := residentFixture(t)
	spec.Argv[1] = "-test.run=^TestResidentUnresponsiveHelper$"
	v, err := r.Acquire(spec, "a", b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(filepath.Join(spec.Directory, "exit"), nil, 0600) })
	r.mu.Lock()
	child := r.children[v.ID]
	r.mu.Unlock()
	child.mu.Lock()
	if err := os.WriteFile(filepath.Join(spec.Directory, "exit"), nil, 0600); err != nil {
		child.mu.Unlock()
		t.Fatal(err)
	}
	// The child can exit while this owner is preparing a signal, but the sole
	// waiter cannot reap (and permit PID reuse) until the same lock is released.
	time.Sleep(100 * time.Millisecond)
	if child.reaped {
		child.mu.Unlock()
		t.Fatal("child reaped while signal ownership lock held")
	}
	child.mu.Unlock()
	awaitResidentExit(t, r, v.ID)
	if err := signalResident(&v, child, true); !errors.Is(err, os.ErrProcessDone) {
		t.Fatal("reaped child allowed a later group signal", err)
	}
}

func awaitResidentHelperReady(t *testing.T, directory string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(directory, "ready")); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("resident helper did not become ready")
}

func TestResidentForceDisabledRetainsUnresponsiveChild(t *testing.T) {
	r, b, spec := residentFixture(t)
	spec.Argv[1] = "-test.run=^TestResidentUnresponsiveHelper$"
	spec.ForceAfterGrace = false
	v, err := r.Acquire(spec, "a", b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(filepath.Join(spec.Directory, "exit"), nil, 0600) })
	awaitResidentHelperReady(t, spec.Directory)
	if _, err := r.Release(v.ID, "a"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(residentStopGrace + 100*time.Millisecond)
	rows, err := r.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].State != "stopping" || rows[0].ForceRequested != "" || b.Snapshot().Allocations != 1 {
		t.Fatal("force-disabled service lost its state or allocation", rows, b.Snapshot())
	}
	observed, err := ObserveResidents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(observed, func(row ProcessObservation) bool { return row.PID == v.PID && row.Started == v.Started }) {
		t.Fatal("force-disabled original child is no longer observed")
	}
	if err := os.WriteFile(filepath.Join(spec.Directory, "exit"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	awaitResidentExit(t, r, v.ID)
	if b.Snapshot().Allocations != 0 {
		t.Fatal("cooperative exit did not release the original allocation")
	}
}

func TestResidentReconcileWaitsForPublishedCompletionBeforeReplacement(t *testing.T) {
	r, b, spec := residentFixture(t)
	spec.Argv[1] = "-test.run=^TestResidentUnresponsiveHelper$"
	id, fingerprint, err := residentIdentity(spec, "a")
	if err != nil {
		t.Fatal(err)
	}
	permit, err := b.TryAcquireResident("a")
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.CreateTemp(t.TempDir(), "resident-log-")
	if err != nil {
		t.Fatal(err)
	}
	process, err := startResident(spec, log)
	log.Close()
	if err != nil {
		permit.Release()
		t.Fatal(err)
	}
	child := &residentChild{process: process, permit: permit, done: make(chan struct{})}
	waited := false
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(spec.Directory, "exit"), nil, 0600)
		if !waited {
			_ = waitResident(child)
		}
		permit.Release()
	})
	awaitResidentHelperReady(t, spec.Directory)
	observed, err := ObserveResidents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var started string
	for _, row := range observed {
		if row.PID == process.Pid {
			started = row.Started
		}
	}
	if started == "" {
		t.Fatal("original process start was not observed")
	}
	const failure = "original identity review requires owner readback"
	v := &Resident{ID: id, Name: spec.Name, Fingerprint: fingerprint, Sharing: spec.Sharing,
		PID: process.Pid, ProcessGroup: process.Pid, Started: started,
		State: "needs_owner_readback", Released: []string{"a"}, Error: failure}
	r.mu.Lock()
	r.records[id], r.children[id] = v, child
	err = r.persist()
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spec.Directory, "exit"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	// Hold completion publication independently of OS exit. This is the exact
	// boundary where the old Reconcile could release and replace a generation.
	err = waitResident(child)
	waited = true
	if err != nil {
		t.Fatal(err)
	}
	rows, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].State != "needs_owner_readback" || b.Snapshot().Allocations != 1 {
		t.Fatal("reconciliation overtook unpublished completion", rows, b.Snapshot())
	}
	if _, err := r.Acquire(spec, "b", b); err == nil {
		t.Fatal("replacement admitted before original completion publication")
	}
	close(child.done)
	rows, err = r.Reconcile(context.Background())
	if err != nil || len(rows) != 1 || rows[0].State != "exited" || b.Snapshot().Allocations != 0 {
		t.Fatal("completed original generation did not reconcile", rows, err)
	}
	for _, name := range []string{"exit", "ready"} {
		if err := os.Remove(filepath.Join(spec.Directory, name)); err != nil {
			t.Fatal(err)
		}
	}
	next, err := r.Acquire(spec, "b", b)
	if err != nil {
		t.Fatal(err)
	}
	awaitResidentHelperReady(t, spec.Directory)
	if len(next.History) != 1 || next.History[0].PID != v.PID || !slices.Contains(next.History[0].Errors, failure) {
		t.Fatal("replacement lost the original failure history", next)
	}
	r.mu.Lock()
	replacement := r.children[id]
	r.mu.Unlock()
	if replacement == nil || replacement == child || b.Snapshot().Allocations != 1 {
		t.Fatal("replacement lost its own child or allocation")
	}
	if err := os.WriteFile(filepath.Join(spec.Directory, "exit"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	awaitResidentExit(t, r, next.ID)
}
