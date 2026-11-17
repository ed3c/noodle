package admission

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSharedQuotaBindPreservesExistingLedgerBytes(t *testing.T) {
	for _, leases := range []string{`{}`, `{"original":{"owner":"old","context":"source-service","resident":true,"resident_key":"gateway","opaque":{"number":9007199254740993}}}`} {
		dir := t.TempDir()
		path := filepath.Join(dir, "quota.json")
		raw := []byte("{\n  \"leases\": " + leases + ",\n  \"opaque\": 9007199254740993, \"worker_admission_policy\": \"host-workers-v1\",\n  \"maximum\": 9, \"schema\": 3\n}\n")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		budget := NewBudget(DefaultWorkerLimit)
		if err = budget.BindSharedQuota(dir, "new"); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(raw, after) {
			t.Fatal("binding rewrote the original ledger", err)
		}
		afterInfo, err := os.Stat(path)
		if err != nil || !os.SameFile(before, afterInfo) {
			t.Fatal("binding replaced the original ledger", err)
		}
		got := budget.shared.snapshot()
		wantAllocations := 0
		if leases != `{}` {
			wantAllocations = 1
		}
		if got.Error != "" || got.Maximum != DefaultWorkerLimit || got.Allocations != wantAllocations || got.Residents != wantAllocations || got.Owned != 0 {
			t.Fatalf("bound ledger lost its allocation facts: %+v", got)
		}
	}
}

func sharedTestBudget(t *testing.T, dir, owner string, max int) (*Budget, *time.Time) {
	t.Helper()
	b := NewBudget(max)
	now := time.Unix(4000, 0)
	b.now = func() time.Time { return now }
	if err := b.BindSharedQuota(dir, owner); err != nil {
		t.Fatal(err)
	}
	b.Register(owner)
	for i := 0; i < 4; i++ {
		now = now.Add(PollInterval)
		b.Observe(Sample{At: now, Pressure: Normal, CPUs: 8, TotalMemoryBytes: 16 << 30}, nil)
	}
	return b, &now
}
func TestSharedQuotaTwoGenerationsUseOneHostCeiling(t *testing.T) {
	dir := t.TempDir()
	old, nowA := sharedTestBudget(t, dir, "old", 2)
	next, nowB := sharedTestBudget(t, dir, "new", 2)
	p, err := old.TryAcquire("old")
	if err != nil {
		t.Fatal(err)
	}
	q, err := next.TryAcquire("new")
	if err != nil {
		t.Fatal(err)
	}
	*nowA = nowB.Add(PollInterval)
	old.Observe(Sample{At: *nowA, Pressure: Normal}, nil)
	if _, err = old.TryAcquire("old"); !errors.Is(err, ErrDeferred) {
		t.Fatal("Host ceiling doubled")
	}
	if got := next.Snapshot(); got.Allocations != 2 || got.OwnedAllocations != 1 {
		t.Fatalf("global vs owned: %+v", got)
	}
	p.Release()
	p.Release()
	if next.Snapshot().Allocations != 1 {
		t.Fatal("original exit did not return exactly one shared slot")
	}
	q.Release()
	if old.Snapshot().Allocations != 0 {
		t.Fatal("leaked shared allocation")
	}
}

func TestSharedQuotaOldRoundRetainsServiceStartPriority(t *testing.T) {
	dir := t.TempDir()
	old, nowOld := sharedTestBudget(t, dir, "old", 4)
	next, nowNext := sharedTestBudget(t, dir, "new", 4)
	worker, err := old.TryAcquire("old")
	if err != nil {
		t.Fatal(err)
	}
	*nowNext = nowNext.Add(PollInterval)
	next.Observe(Sample{At: *nowNext, Pressure: Normal}, nil)
	if _, err = next.tryAcquire("new", true, "service", ""); !errors.Is(err, ErrDeferred) {
		t.Fatal("new service could strand the still-running old round")
	}
	old.HoldForSwitch()
	*nowOld = nowNext.Add(PollInterval)
	old.Observe(Sample{At: *nowOld, Pressure: Normal}, nil)
	service, err := old.tryAcquire("old", true, "service", "")
	if err != nil {
		t.Fatal("old round lost its remaining service preparation:", err)
	}
	if _, err = old.TryAcquire("old"); !errors.Is(err, ErrDeferred) {
		t.Fatal("old generation started another worker")
	}
	service.Release()
	worker.Release()
	*nowNext = nowOld.Add(PollInterval)
	next.Observe(Sample{At: *nowNext, Pressure: Normal}, nil)
	newService, err := next.tryAcquire("new", true, "service", "")
	if err != nil {
		t.Fatal("new version did not proceed after the old round exited:", err)
	}
	newService.Release()
	if got := old.Snapshot(); got.OwnedAllocations != 0 || got.SharedQuota.Error != "" {
		t.Fatalf("finished old round was falsely retained: %+v", got)
	}
}
func TestSharedQuotaMissingStateNeverInventsFreeMemory(t *testing.T) {
	dir := t.TempDir()
	b, _ := sharedTestBudget(t, dir, "old", 1)
	p, err := b.TryAcquire("old")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(dir, "quota.json")); err != nil {
		t.Fatal(err)
	}
	if got := b.Snapshot(); got.SharedQuota == nil || got.SharedQuota.Error == "" || got.Limit != 0 {
		t.Fatal("missing state claimed released")
	}
	if err = NewBudget(1).BindSharedQuota(dir, "new"); err == nil {
		t.Fatal("new Host silently recreated quota")
	}
	p.Release()
}

func TestSharedQuotaResidentKeepsOneNativeOwnerAcrossVersions(t *testing.T) {
	dir := t.TempDir()
	old, _ := sharedTestBudget(t, dir, "old", 4)
	next, now := sharedTestBudget(t, dir, "new", 4)
	p, err := old.tryAcquire("old", true, "shared-service", "")
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(PollInterval)
	next.Observe(Sample{At: *now, Pressure: Normal}, nil)
	if _, err = next.tryAcquire("new", true, "shared-service", ""); !errors.Is(err, ErrDeferred) {
		t.Fatal("two generations launched the same shared service")
	}
	other, err := next.tryAcquire("new", true, "independent-service", "")
	if err != nil {
		t.Fatal("unrelated service was blocked by a different owner:", err)
	}
	other.Release()
	p.Release()
	*now = now.Add(PollInterval)
	next.Observe(Sample{At: *now, Pressure: Normal}, nil)
	q, err := next.tryAcquire("new", true, "shared-service", "")
	if err != nil {
		t.Fatal("native exit did not permit the selected version's service:", err)
	}
	q.Release()
}
func TestSharedQuotaHelperProcess(t *testing.T) {
	dir := os.Getenv("NOODLE_TEST_SHARED_QUOTA")
	if dir == "" {
		return
	}
	b, _ := sharedTestBudget(t, dir, "child", 1)
	if _, err := b.TryAcquire("child"); !errors.Is(err, ErrDeferred) {
		t.Fatal("child bypassed another process allocation")
	}
}

func TestIndependentHostsEachKeepNineWorkersWhileGenerationsShareOneCeiling(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	old, _ := sharedTestBudget(t, rootA, "old-generation", 9)
	next, _ := sharedTestBudget(t, rootA, "next-generation", 9)
	other, _ := sharedTestBudget(t, rootB, "independent-host", 9)
	resident, err := old.TryAcquireScopedResident("old-generation", "gateway")
	if err != nil {
		t.Fatal(err)
	}
	defer resident.Release()
	var permits []*Permit
	defer func() {
		for _, permit := range permits {
			permit.Release()
		}
	}()
	for i := range 9 {
		b, id := old, "old-generation"
		if i%2 == 1 {
			b, id = next, "next-generation"
		}
		permit, err := b.TryAcquire(id)
		if err != nil {
			t.Fatalf("worker %d blocked: %v", i+1, err)
		}
		permits = append(permits, permit)
	}
	for _, selected := range []struct {
		b  *Budget
		id string
	}{{old, "old-generation"}, {next, "next-generation"}} {
		if _, err := selected.b.TryAcquire(selected.id); !errors.Is(err, ErrDeferred) {
			t.Fatal("generation exceeded the same Host's nine workers")
		}
	}
	for range 9 {
		permit, err := other.TryAcquire("independent-host")
		if err != nil {
			t.Fatal("full Host A blocked independent Host B:", err)
		}
		permits = append(permits, permit)
	}
	if got := next.Snapshot(); got.Active != 9 || got.Residents != 1 || got.Allocations != 10 {
		t.Fatalf("resident consumed a worker slot: %+v", got)
	}
	if got := other.Snapshot(); got.Active != 9 || got.Residents != 0 {
		t.Fatalf("independent Host ceiling changed: %+v", got)
	}
}

func TestSharedQuotaSurvivesOwnerProcessBoundary(t *testing.T) {
	dir := t.TempDir()
	b, _ := sharedTestBudget(t, dir, "parent", 1)
	p, err := b.TryAcquire("parent")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSharedQuotaHelperProcess$")
	cmd.Env = append(os.Environ(), "NOODLE_TEST_SHARED_QUOTA="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cross-process quota: %v %s", err, out)
	}
	p.Release()
}

func TestSharedQuotaReturnsAutomaticallyAfterStoreRecovery(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission fault requires an unprivileged owner")
	}
	dir := t.TempDir()
	b, now := sharedTestBudget(t, dir, "original-host", 2)
	permit, err := b.TryAcquire("original-host")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0700) })
	permit.Release()
	before, err := os.ReadFile(filepath.Join(dir, "quota.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Snapshot(); got.OwnedAllocations != 1 || got.SharedQuota.Error == "" {
		t.Fatalf("failed return was reported as released: %+v", got)
	}
	if _, err = b.TryAcquire("original-host"); !errors.Is(err, ErrDeferred) {
		t.Fatal("unknown quota return admitted another worker")
	}
	if b.AdmissionError() == "" {
		t.Fatal("unknown quota return was reported as healthy Host custody")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "quota.json"))
	if string(before) != string(after) {
		t.Fatal("unknown return generated another reservation")
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(PollInterval)
	b.Observe(Sample{}, errors.New("sampler unavailable during quota recovery"))
	if got := b.Snapshot(); got.OwnedAllocations != 0 || got.SharedQuota.Error != "" {
		t.Fatalf("original Host did not return its slot after storage recovery: %+v", got)
	}
	if got := b.Snapshot(); got.Pressure != Unknown || got.SampleError == nil || got.Limit != 2 || b.AdmissionError() != "" {
		t.Fatalf("sample failure prevented original quota recovery: %+v", got)
	}
	permit.Release()
	if b.Snapshot().Allocations != 0 {
		t.Fatal("return retry or duplicate Release changed the ceiling")
	}
}

func TestSharedQuotaReservationFailureWaitsForOriginalOwnerReadback(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission fault requires an unprivileged owner")
	}
	dir := t.TempDir()
	b, now := sharedTestBudget(t, dir, "original-host", 2)
	before, err := os.ReadFile(filepath.Join(dir, "quota.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0700) })
	if _, err = b.TryAcquire("original-host"); !errors.Is(err, ErrDeferred) {
		t.Fatal("failed reservation returned a launch permission")
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = b.TryAcquire("original-host"); !errors.Is(err, ErrDeferred) {
		t.Fatal("new intent started before original quota readback")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "quota.json"))
	if string(before) != string(after) {
		t.Fatal("unknown reservation was replayed")
	}
	*now = now.Add(PollInterval)
	b.Observe(Sample{At: *now, Pressure: Normal}, nil)
	permit, err := b.TryAcquire("original-host")
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Snapshot(); got.Allocations != 1 || got.SharedQuota.Error != "" {
		t.Fatalf("unreturned reservation was not reconciled: %+v", got)
	}
	permit.Release()
}

func TestSharedQuotaRejectsPriorWorkerPolicyWithoutRewritingLedger(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quota.json")
	original := []byte(`{"schema":1,"maximum":4,"swap_used_limit_bytes":15000000000,"last_start_window":-1,"leases":{}}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	b := NewBudget(9)
	if err := b.BindSharedQuota(dir, "candidate"); err == nil {
		t.Fatal("mixed worker admission policies shared a ledger")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("rejected admission rewrote the original quota ledger")
	}
}

func TestSharedQuotaRejectsIncompleteLedgerWithoutInventingCapacity(t *testing.T) {
	for _, original := range []string{
		`{}`,
		`{"schema":3,"maximum":9,"leases":{}}`,
		`{"schema":3,"maximum":9,"worker_admission_policy":"host-workers-v1"}`,
		`{"schema":3,"worker_admission_policy":"host-workers-v1","leases":{}}`,
		`{"schema":3,"maximum":0,"worker_admission_policy":"host-workers-v1","leases":{}}`,
		`{"schema":3,"maximum":10,"worker_admission_policy":"host-workers-v1","leases":{}}`,
		`{"schema":2,"maximum":9,"worker_admission_policy":"host-workers-v1","leases":{}}`,
	} {
		t.Run(original, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "quota.json")
			if err := os.WriteFile(path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			if err := NewBudget(0).BindSharedQuota(dir, "new-host"); err == nil {
				t.Fatal("incomplete custody ledger became new capacity")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != original {
				t.Fatalf("refusal changed original ledger: %s, %v", after, err)
			}
		})
	}
}

func TestAdmissionErrorSeparatesCapacityAndObservationsFromCustody(t *testing.T) {
	dir := t.TempDir()
	b, _ := sharedTestBudget(t, dir, "owner", 1)
	b.Observe(Sample{}, errors.New("sampler unavailable"))
	permit, err := b.TryAcquire("owner")
	if err != nil {
		t.Fatal(err)
	}
	defer permit.Release()
	if _, err = b.TryAcquire("owner"); !errors.Is(err, ErrDeferred) {
		t.Fatal("full Host admitted another worker")
	}
	if got := b.AdmissionError(); got != "" {
		t.Fatalf("capacity or observation became Host fault: %s", got)
	}
	b.SetUnresolvedResidents(1)
	if b.AdmissionError() == "" {
		t.Fatal("unknown resident owner was reported ready")
	}
	b.SetUnresolvedResidents(0)
	if err = os.Remove(filepath.Join(dir, "quota.json")); err != nil {
		t.Fatal(err)
	}
	if b.AdmissionError() == "" {
		t.Fatal("missing shared custody ledger was reported ready")
	}
}
