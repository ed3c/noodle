package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRejectedReviewVMRetirementPreservesCheckoutAndDoesNotReplay(t *testing.T) {
	r, binding, fixture, _ := retirementRuntimeFixture(t, "completed")
	ctx := t.Context()
	stopped, err := r.ReadCompletedSession(ctx, binding)
	if err != nil {
		t.Fatal(err)
	}
	const maintenance = "rejected-review-maintenance"
	if err = fixture.owner.AcquireRejectedReviewMaintenance(ctx, maintenance); err == nil {
		t.Fatal("an initialized worker owner accepted maintenance takeover")
	}
	if _, err = fixture.owner.Acquire(ctx, maintenance); err != nil {
		t.Fatal(err)
	}
	// Keep the existing fixture native process so it retains the preloaded VM.
	fixture.owner.mu.Lock()
	fixture.owner.retirementOnly = true
	fixture.owner.mu.Unlock()
	if _, err = fixture.owner.Acquire(ctx, "unexpected-worker"); err == nil {
		t.Fatal("maintenance owner yielded worker custody")
	}
	t.Cleanup(func() {
		if _, err := fixture.owner.Release(maintenance); err != nil {
			t.Error(err)
		}
	})
	sources := map[string]SessionReadbackFileRef{}
	for _, role := range []string{"canonical_snapshot", "rejected_review_events", "rejected_review_release"} {
		path := filepath.Join(fixture.root, role+".json")
		writeReadbackJSON(t, path, map[string]string{"source": role})
		ref, err := ReadSessionReadbackFile(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		sources[role] = ref
	}
	attempt := RejectedReviewRetirementAttempt{binding, stopped.NativeAttemptID, stopped.CandidateHead}
	run := func(attempts ...RejectedReviewRetirementAttempt) ([]CleanupSessionRetirement, error) {
		return fixture.owner.RetireRejectedReviewSessions(ctx, r.config.Profile, maintenance, r.config.ContextID, r.config.RuntimeDir, binding.WorktreePath, attempts, sources)
	}
	if _, err := run(attempt, attempt); err == nil {
		t.Fatal("duplicate original attempt admitted")
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "vm-deletions")); !os.IsNotExist(err) {
		t.Fatal("unqualified review retirement attempted a deletion", err)
	}
	for range 2 {
		result, err := run(attempt)
		if err != nil || len(result) != 1 || result[0].State != CleanupSessionRetired || result[0].OutcomeStatus != StatusCompleted {
			t.Fatalf("original rejected VM retirement is unresolved: %+v %v", result, err)
		}
	}
	if current, err := sessionReadbackHead(ctx, binding.WorktreePath); err != nil || current != stopped.CandidateHead {
		t.Fatalf("rejected checkout changed: %s %v", current, err)
	}
	calls, err := os.ReadFile(filepath.Join(fixture.root, "vm-deletions"))
	if err != nil || len(strings.Fields(string(calls))) != 1 {
		t.Fatalf("original deletion replayed: %q %v", calls, err)
	}
	writeReadbackJSON(t, sources["rejected_review_release"].Path, map[string]string{"source": "foreign"})
	sources["rejected_review_release"], err = ReadSessionReadbackFile(sources["rejected_review_release"].Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(attempt); err == nil {
		t.Fatal("accepted a substituted original release")
	}
}
