package admission

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/testutil/residentchild"
)

func TestResidentChildHelper(t *testing.T) {
	if os.Getenv("NOODLE_RESIDENT_TEST_CHILD") != "1" {
		return
	}
	if waitLegacyRecoveryProbe(t) {
		return
	}
	if err := residentchild.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestResidentChildHelperCooperativeCleanup(t *testing.T) {
	t.Setenv("NOODLE_RESIDENT_TEST_CHILD", "1")
	t.Chdir(t.TempDir())
	done := make(chan struct{})
	go func() { TestResidentChildHelper(t); close(done) }()
	if err := os.WriteFile("exit", nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fixture helper ignored its cooperative exit marker")
	}
}

func TestResidentFixtureCleanupPreservesUnknownIdentityGuard(t *testing.T) {
	var child *residentChild
	t.Run("unknown-original-identity", func(t *testing.T) {
		r, budget, spec := residentFixture(t)
		v, err := r.Acquire(spec, "a", budget)
		if err != nil {
			t.Fatal(err)
		}
		r.mu.Lock()
		child = r.children[v.ID]
		r.records[v.ID].State = "needs_owner_readback"
		r.records[v.ID].Started = ""
		r.records[v.ID].Error = "planted unavailable start identity"
		r.mu.Unlock()
		if _, err := r.Release(v.ID, "a"); err == nil {
			t.Fatal("unknown original identity authorized a stop")
		}
		select {
		case <-child.done:
			t.Fatal("unknown identity stop ended the helper")
		default:
		}
	})
	select {
	case <-child.done:
	case <-time.After(3 * time.Second):
		t.Fatal("cooperative fixture cleanup left its original child alive")
	}
}

func TestResidentRestartPreservesLegacyFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "residents.json")
	before := []byte(`{"preview":{"id":"preview","state":"needs_owner_readback","error":"original stop outcome unknown"}}`)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := OpenResidents(dir)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := r.Snapshot()
	if err != nil || len(rows) != 1 || len(rows[0].Errors) != 1 || rows[0].Errors[0] != "original stop outcome unknown" || rows[0].State != "needs_owner_readback" {
		t.Fatalf("legacy failure was lost: %+v %v", rows, err)
	}
	if err := r.persist(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenResidents(dir)
	if err != nil {
		t.Fatal(err)
	}
	rows, err = reopened.Snapshot()
	if err != nil || len(rows[0].Errors) != 2 || rows[0].Errors[0] != "original stop outcome unknown" {
		t.Fatalf("restart replaced prior errors: %+v %v", rows, err)
	}
}
func residentFixture(t *testing.T) (*Residents, *Budget, ResidentSpec) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("graceful shutdown unsupported")
	}
	r, err := OpenResidents(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := NewBudget(3)
	b.Register("a")
	b.Register("b")
	now := time.Now()
	for i := 0; i < 4; i++ {
		b.Observe(Sample{At: now.Add(time.Duration(i-3) * time.Second), Pressure: Normal, SwapUsedBytes: 0, CPUs: 4, TotalMemoryBytes: 16 << 30}, nil)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentSpec{Name: "preview", Argv: []string{exe, "-test.run=^TestResidentChildHelper$"}, Directory: t.TempDir(), Environment: []string{"NOODLE_RESIDENT_TEST_CHILD=1"}, Sharing: "shared", Transport: "network"}
	t.Cleanup(func() {
		r.mu.Lock()
		children := make([]*residentChild, 0, len(r.children))
		for _, child := range r.children {
			children = append(children, child)
		}
		r.mu.Unlock()
		if err := os.WriteFile(filepath.Join(spec.Directory, "exit"), nil, 0600); err != nil {
			t.Error(err)
		}
		for _, child := range children {
			select {
			case <-child.done:
			case <-time.After(3 * time.Second):
				t.Error("fixture helper completion not observed")
			}
		}
	})
	return r, b, spec
}

// A released allocation does not create a start credit. Advance one real
// sampling event before a test admits a distinct replacement generation.
func observeResidentFixtureSample(b *Budget) {
	at := time.Now()
	if b.previous != nil && !at.After(b.previous.At) {
		at = b.previous.At.Add(time.Nanosecond)
	}
	b.Observe(Sample{At: at, Pressure: Normal, CPUs: 4, TotalMemoryBytes: 16 << 30}, nil)
}

func awaitResidentExit(t *testing.T, r *Residents, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := r.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range rows {
			if v.ID == id && v.State == "exited" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("resident exit not observed")
}
func TestResidentsShareLeaseAndReleaseOnlyLastConsumer(t *testing.T) {
	r, b, spec := residentFixture(t)
	first, err := r.Acquire(spec, "a", b)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Acquire(spec, "b", b)
	if err != nil || second.PID != first.PID || len(second.Contexts) != 2 {
		t.Fatalf("shared acquire: %+v %v", second, err)
	}
	again, err := r.Acquire(spec, "a", b)
	if err != nil || len(again.Contexts) != 2 {
		t.Fatalf("idempotence: %+v %v", again, err)
	}
	snapshot := b.Snapshot()
	if snapshot.Residents != 1 || snapshot.Active != 0 || snapshot.Allocations != 1 {
		t.Fatalf("accounting: %+v", snapshot)
	}
	foreign, err := r.Release(first.ID, "foreign")
	if err != nil || len(foreign.Contexts) != 2 {
		t.Fatalf("foreign release: %+v %v", foreign, err)
	}
	one, err := r.Release(first.ID, "a")
	if err != nil || one.State != "running" || len(one.Contexts) != 1 {
		t.Fatalf("live consumer lost: %+v %v", one, err)
	}
	if _, err = r.Acquire(spec, "a", b); err == nil {
		t.Fatal("released lease replay admitted")
	}
	if _, err = r.Release(first.ID, "b"); err != nil {
		t.Fatal(err)
	}
	awaitResidentExit(t, r, first.ID)
	if s := b.Snapshot(); s.Allocations != 0 || s.Residents != 0 {
		t.Fatalf("permit leaked: %+v", s)
	}
	if _, err = r.Acquire(spec, "b", b); err == nil {
		t.Fatal("completed request replay spawned")
	}
	b.Register("c")
	observeResidentFixtureSample(b)
	fresh, err := r.Acquire(spec, "c", b)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.PID == first.PID {
		t.Fatal("new owner did not start")
	}
	if _, err = r.Release(fresh.ID, "c"); err != nil {
		t.Fatal(err)
	}
	awaitResidentExit(t, r, fresh.ID)
}
func TestResidentsRefuseConflictStdioAndPressureButReuse(t *testing.T) {
	r, b, spec := residentFixture(t)
	first, err := r.Acquire(spec, "a", b)
	if err != nil {
		t.Fatal(err)
	}
	changed := spec
	changed.Environment = []string{"NOODLE_RESIDENT_TEST_CHILD=1", "DIFFERENT=yes"}
	if _, err = r.Acquire(changed, "b", b); err == nil {
		t.Fatal("conflict spawned")
	}
	stdio := spec
	stdio.Name = "stdio"
	stdio.Transport = "stdio"
	if _, err = r.Acquire(stdio, "b", b); err == nil {
		t.Fatal("stateful stdio shared")
	}
	b.Observe(Sample{At: time.Now(), Pressure: Critical, TotalMemoryBytes: 16 << 30, CPUs: 8}, nil)
	reused, err := r.Acquire(spec, "b", b)
	if err != nil || reused.PID != first.PID {
		t.Fatalf("reuse under pressure: %+v %v", reused, err)
	}
	distinct := spec
	distinct.Name = "another"
	if _, err = r.Acquire(distinct, "b", b); err == nil {
		t.Fatal("new resident bypassed pressure")
	}
}
func TestResidentsPriorHostNeedsReadbackNotPIDAdoption(t *testing.T) {
	r, b, spec := residentFixture(t)
	first, err := r.Acquire(spec, "a", b)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := OpenResidents(filepath.Dir(r.path))
	if err != nil {
		t.Fatal(err)
	}
	v, err := recovered.Acquire(spec, "b", b)
	if err == nil || v.State != "needs_owner_readback" || v.PID != first.PID {
		t.Fatalf("unsafe prior owner adoption: %+v %v", v, err)
	}
	if _, err = recovered.Release(first.ID, "a"); err == nil {
		t.Fatal("unowned process signalled")
	}
	rows, _ := r.Snapshot()
	if rows[0].State != "running" {
		t.Fatal("original child changed")
	}
}
func TestResidentInventoryIsDiagnosticNotKillAuthority(t *testing.T) {
	data := "101 10 101 Thu Oct 15 12:00:00 2026 1200 /some path/server\n102 10 101 Thu Oct 15 12:00:01 2026 1400 /some path/server\n103 11 103 Thu Oct 15 12:00:02 2026 500 /some path/server\n"
	rows, err := parseResidents(data)
	if err != nil || len(rows) != 3 {
		t.Fatal(rows, err)
	}
	if rows[0].DuplicateCandidates != 2 || rows[2].DuplicateCandidates != 1 || rows[0].Ownership != "needs_owner_readback" {
		t.Fatal(rows)
	}
	if _, err = parseResidents("bad row"); err == nil {
		t.Fatal("malformed inventory accepted")
	}
	if runtime.GOOS != "windows" {
		live, err := ObserveResidents(context.Background())
		if err != nil || len(live) == 0 {
			t.Fatal(live, err)
		}
		found := false
		for _, row := range live {
			if row.PID == os.Getpid() {
				found = true
				if row.ParentPID != os.Getppid() || row.ProcessGroup <= 0 || len(row.AncestorPIDs) == 0 || row.AncestorPIDs[0] != row.ParentPID {
					t.Fatalf("live self lineage differs from runtime: %+v", row)
				}
			}
		}
		if !found {
			t.Fatal("live inventory omitted current process")
		}
	}
}
func TestResidentsUnknownStartAndCorruptJournalRefuse(t *testing.T) {
	r, b, spec := residentFixture(t)
	key, digest, err := residentIdentity(spec, "a")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]Resident{key: {ID: key, Fingerprint: digest, State: "starting", Contexts: []string{"a"}}})
	if err = os.WriteFile(r.path, data, 0600); err != nil {
		t.Fatal(err)
	}
	prior, err := OpenResidents(filepath.Dir(r.path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = prior.Acquire(spec, "a", b); err == nil {
		t.Fatal("unknown start replayed")
	}
	if err = os.WriteFile(r.path, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenResidents(filepath.Dir(r.path)); err == nil {
		t.Fatal("corrupt journal ignored")
	}
}

func TestResidentsReconcileOnlyObservedExit(t *testing.T) {
	r, b, spec := residentFixture(t)
	v, err := r.Acquire(spec, "a", b)
	if err != nil {
		t.Fatal(err)
	}
	if v.Started == "" {
		t.Fatal("native process start identity missing")
	}
	recovered, err := OpenResidents(filepath.Dir(r.path))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := recovered.Reconcile(context.Background())
	if err != nil || rows[0].State != "needs_owner_readback" {
		t.Fatalf("live owner reclassified: %+v %v", rows, err)
	}
	if _, err = r.Release(v.ID, "a"); err != nil {
		t.Fatal(err)
	}
	awaitResidentExit(t, r, v.ID)
	rows, err = recovered.Reconcile(context.Background())
	if err != nil || rows[0].State != "exited" {
		t.Fatalf("absence readback: %+v %v", rows, err)
	}
	if _, err = recovered.Acquire(spec, "a", b); err == nil {
		t.Fatal("old owner replayed after reconciliation")
	}
}
