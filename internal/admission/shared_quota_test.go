package admission

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

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
func TestSharedQuotaTwoGenerationsUseOneCeilingAndStartWindow(t *testing.T) {
	dir := t.TempDir()
	old, nowA := sharedTestBudget(t, dir, "old", 2)
	next, nowB := sharedTestBudget(t, dir, "new", 2)
	p, err := old.TryAcquire("old")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = next.TryAcquire("new"); !errors.Is(err, ErrDeferred) {
		t.Fatal("two generation startup credits became a burst")
	}
	if got := next.Snapshot(); got.SharedQuota.Error != "" {
		t.Fatalf("normal quota wait was reported as an unknown effect: %+v", got)
	}
	*nowB = nowB.Add(PollInterval)
	next.Observe(Sample{At: *nowB, Pressure: Normal}, nil)
	q, err := next.TryAcquire("new")
	if err != nil {
		t.Fatal(err)
	}
	*nowA = nowB.Add(PollInterval)
	old.Observe(Sample{At: *nowA, Pressure: Normal}, nil)
	if _, err = old.TryAcquire("old"); !errors.Is(err, ErrDeferred) {
		t.Fatal("machine ceiling doubled")
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

func TestSharedQuotaWarningGrantCannotBypassOtherGenerationOrSwapCeiling(t *testing.T) {
	dir := t.TempDir()
	old, nowOld := sharedTestBudget(t, dir, "old", 4)
	next, nowNext := sharedTestBudget(t, dir, "new", 4)
	for range 4 {
		*nowNext = nowNext.Add(PollInterval)
		next.Observe(Sample{At: *nowNext, Pressure: Warning}, nil)
	}
	for nowOld.Before(*nowNext) {
		*nowOld = nowOld.Add(PollInterval)
		old.Observe(Sample{At: *nowOld, Pressure: Normal}, nil)
	}
	worker, err := old.TryAcquire("old")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = next.GrantWarning("new", "task"); err == nil {
		t.Fatal("warning grant ignored another generation's live allocation")
	}
	worker.Release()
	if _, err = next.GrantWarning("new", "task"); err != nil {
		t.Fatal(err)
	}
	*nowNext = nowNext.Add(PollInterval)
	next.Observe(Sample{At: *nowNext, Pressure: Warning}, nil)
	permit, err := next.TryAcquireTask("new", "task")
	if err != nil {
		t.Fatal(err)
	}
	*nowOld = nowNext.Add(PollInterval)
	old.Observe(Sample{At: *nowOld, Pressure: Normal}, nil)
	if _, err = old.TryAcquire("old"); !errors.Is(err, ErrDeferred) {
		t.Fatal("other generation bypassed the exclusive warning allocation")
	}
	permit.Release()
	if next.WarningGrants()[0].Status != "consumed" {
		t.Fatal("shared permit did not consume original permission")
	}
	*nowNext = nowOld.Add(PollInterval)
	next.Observe(Sample{At: *nowNext, Pressure: Warning, SwapUsedBytes: DefaultSwapUsedLimit + 1}, nil)
	if _, err = next.GrantWarning("new", "another-task"); err == nil {
		t.Fatal("explicit warning grant exceeded the 15 GB capacity ceiling")
	}
	if next.Snapshot().Allocations != 0 {
		t.Fatal("native exit did not return the shared allocation")
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
	after, _ := os.ReadFile(filepath.Join(dir, "quota.json"))
	if string(before) != string(after) {
		t.Fatal("unknown return generated another reservation")
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(PollInterval)
	b.Observe(Sample{At: *now, Pressure: Warning}, nil)
	if got := b.Snapshot(); got.OwnedAllocations != 0 || got.SharedQuota.Error != "" {
		t.Fatalf("original Host did not return its slot after storage recovery: %+v", got)
	}
	if got := b.Snapshot(); got.Pressure != Warning || got.Limit < 1 {
		t.Fatal("quota return lost valid warning-below-ceiling admission")
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
	b := NewBudgetWithSwapUsedLimit(4, DefaultSwapUsedLimit)
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
