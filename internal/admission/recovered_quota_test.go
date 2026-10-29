package admission

import (
	"testing"
)

func TestRecoveredQuotaReturnsOnlyOriginalResident(t *testing.T) {
	dir := t.TempDir()
	old, _ := sharedTestBudget(t, dir, "old", 4)
	next, _ := sharedTestBudget(t, dir, "new", 4)
	permit, err := old.tryAcquire("old", true, "fixture-key", "")
	if err != nil {
		t.Fatal(err)
	}
	q := next.shared
	if err = q.update(0, func(s *quotaState) error {
		s.Leases["foreign"] = quotaLease{Owner: "other", Resident: true, ResidentKey: "fixture-key"}
		s.Leases["worker"] = quotaLease{Owner: "old", Resident: false, ResidentKey: "fixture-key"}
		s.Leases["other-service"] = quotaLease{Owner: "old", Resident: true, ResidentKey: "other-key"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = q.releaseRecoveredResident("old", "fixture-key"); err != nil {
		t.Fatal(err)
	}
	if got := q.snapshot().Allocations; got != 3 {
		t.Fatal("unrelated quota released", got)
	}
	if err = q.releaseRecoveredResident("old", "fixture-key"); err != nil {
		t.Fatal(err)
	}
	permit.Release()
	if got := q.snapshot().Allocations; got != 3 {
		t.Fatal("repeat return released another allocation", got)
	}
	if err = next.ReleaseRecoveredFixtureQuota("old", FixtureReclamation{State: "exited"}); err == nil {
		t.Fatal("missing native evidence authorized quota return")
	}
}
