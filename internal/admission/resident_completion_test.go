package admission

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResidentNormalExitPreservesOriginalGuardFailure(t *testing.T) {
	r, budget, spec := residentFixture(t)
	guard, err := NewResidentGuard("original-source", time.Second, func(context.Context) error {
		return errors.New("original execution remains unknown")
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := r.AcquireGuarded(spec, "b", budget, guard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Release(started.ID, "b"); err == nil {
		t.Fatal("fixture did not refuse the original release")
	}
	if _, err = r.AcquireGuarded(spec, "a", budget, guard); err != nil {
		t.Fatal(err)
	}
	rows, err := r.Snapshot()
	if err != nil || len(rows) != 1 {
		t.Fatal("original parent baseline unavailable", err)
	}
	before := rows[0]
	if before.Error == "" || len(before.Errors) == 0 || before.State != "running" {
		t.Fatal("fixture did not preserve the real guard refusal", before)
	}
	if err := VerifyResidentNormalExit(before, before); err == nil {
		t.Fatal("running resident was reported as normally exited")
	}
	if err := os.WriteFile(filepath.Join(spec.Directory, "exit"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	awaitResidentExit(t, r, before.ID)
	rows, err = r.Snapshot()
	if err != nil || len(rows) != 1 {
		t.Fatal("original parent readback unavailable", err)
	}
	after := rows[0]
	if err := VerifyResidentNormalExit(before, after); err != nil {
		t.Fatal("original waiter's normal exit with historical guard refusal was lost", err)
	}
	for _, fault := range []string{"guard-cleared", "history-cleared", "signal", "nonzero", "absence", "force", "generation", "deployment", "leases", "already-terminal-error"} {
		t.Run(fault, func(t *testing.T) {
			left, right := cloneResident(&before), cloneResident(&after)
			switch fault {
			case "guard-cleared":
				right.Error = ""
			case "history-cleared":
				right.Errors = nil
			case "signal":
				right.Error = "resident terminated by killed"
				right.Errors = append(right.Errors, right.Error)
			case "nonzero":
				right.Error = "resident exited with status 2"
				right.Errors = append(right.Errors, right.Error)
			case "absence":
				right.Error = "original process and owned group absent; no adoption or signal"
			case "force":
				right.ForceRequested = time.Now().UTC().Format(time.RFC3339Nano)
			case "generation":
				right.Started = "another generation"
			case "deployment":
				right.Fingerprint = "another deployment"
			case "leases":
				right.Released = nil
			case "already-terminal-error":
				left.Error, right.Error = "resident exited with status 2", "resident exited with status 2"
				left.Errors, right.Errors = []string{left.Error}, []string{right.Error}
			}
			if err := VerifyResidentNormalExit(left, right); err == nil {
				t.Fatal("unproved normal exit accepted", fault)
			}
		})
	}
}

func TestResidentNormalExitRejectsRealSignaledWaiter(t *testing.T) {
	r, budget, spec := residentFixture(t)
	guard, err := NewResidentGuard("original-source", time.Second, func(context.Context) error {
		return errors.New("original execution remains unknown")
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := r.AcquireGuarded(spec, "a", budget, guard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Release(started.ID, "a"); err == nil {
		t.Fatal("fixture release was not refused")
	}
	rows, err := r.Snapshot()
	if err != nil || len(rows) != 1 {
		t.Fatal("original parent baseline unavailable", err)
	}
	before := rows[0]
	r.mu.Lock()
	child := r.children[started.ID]
	r.mu.Unlock()
	// This test owns the unreaped child handle; no reusable PID is adopted.
	child.mu.Lock()
	err = child.process.Kill()
	child.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	awaitResidentExit(t, r, before.ID)
	rows, err = r.Snapshot()
	if err != nil || len(rows) != 1 || rows[0].Error == before.Error {
		t.Fatal("real signaled wait result was not preserved", rows, err)
	}
	if err = VerifyResidentNormalExit(before, rows[0]); err == nil {
		t.Fatal("real signal termination was accepted as normal zero exit")
	}
}
