package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree/archive"
)

func TestArchiveRecoveryCleanupDispositionIsExactAndRestartable(t *testing.T) {
	session, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref := func(base string) loopruntime.SessionReadbackFileRef {
		return loopruntime.SessionReadbackFileRef{Path: filepath.Join(session, base), SHA256: strings.Repeat("a", 64)}
	}
	plan := archiveRecoveryCleanupPlan{Candidate: ref("archive-recovery-candidate.json"),
		LandingProof: ref("archive-landing-proof.json"), Request: ref("host-request.json"),
		OrderID: "order", AttemptID: "attempt", SessionID: "session", NativeAttemptID: "native",
		MainProcessInstanceID: "main", SandboxID: "vm", Head: "head", Tree: "tree",
		Stop: ref("archive-recovery-stop.json"), Archive: ref("workspace.tar"), OriginalReturn: "UNKNOWN", Effects: []string{}}
	original := newArchiveRecoveryCleanupDisposition(plan, loopruntime.ProgressMaintenance{HostInstance: "host", ContextID: "context"},
		loopruntime.ArchiveRecoveryDispatchHold{PauseCommandID: "pause"}, archive.CheckoutIdentity{Incarnation: "original-checkout"},
		admission.MachineSessionLease{ID: "original-lease"})
	path := filepath.Join(session, archiveRecoveryCleanupDispositionFile)

	staged := plan
	if err = archiveRecoveryCleanupContinue(session, &staged, original, ""); err != nil {
		t.Fatal(err)
	}
	if staged.Next == nil || staged.Next.Argv[0] != staged.Next.Executable.Path ||
		!reflect.DeepEqual(staged.Next.Argv[3:], []string{archiveRecoveryCleanupReserveOperation,
			"--candidate-file", plan.Candidate.Path, "--expected-candidate-sha256", plan.Candidate.SHA256,
			"--landing-proof-file", plan.LandingProof.Path, "--expected-landing-proof-sha256", plan.LandingProof.SHA256,
			"--expected-disposition-sha256", staged.DispositionSHA256}) || staged.Disposition != nil || len(staged.Effects) != 0 {
		t.Fatalf("unexpected original planned reservation: %+v", staged)
	}
	if _, err = os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("inspection wrote an original disposition: %v", err)
	}

	changed := original
	changed.Lease.ID = "foreign-lease"
	refused := plan
	if err = archiveRecoveryCleanupContinue(session, &refused, changed, staged.DispositionSHA256); err == nil {
		t.Fatal("changed lease received the old planned disposition")
	}
	if _, err = os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("refusal wrote an original disposition: %v", err)
	}
	changed = original
	changed.Checkout.Incarnation = "foreign-checkout"
	if err = archiveRecoveryCleanupContinue(session, &refused, changed, staged.DispositionSHA256); err == nil {
		t.Fatal("changed checkout inherited the old planned disposition")
	}

	recorded := plan
	if err = archiveRecoveryCleanupContinue(session, &recorded, original, staged.DispositionSHA256); err != nil {
		t.Fatal(err)
	}
	if recorded.Next != nil || recorded.Disposition == nil || recorded.Disposition.Path != path ||
		recorded.Disposition.SHA256 != staged.DispositionSHA256 || recorded.WaitFor != "original_archive_recovery_reclamation_owner" ||
		!reflect.DeepEqual(recorded.Effects, []string{"archive_recovery_cleanup_disposition_recorded"}) {
		t.Fatalf("unexpected durable reservation %+v", recorded)
	}
	var saved archiveRecoveryCleanupDisposition
	if err = readArchiveCleanupRef(*recorded.Disposition, &saved); err != nil || !reflect.DeepEqual(saved, original) {
		t.Fatalf("wrong saved disposition: %+v, %v", saved, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("original disposition permissions differ: %v", err)
	}

	observed, again := plan, plan
	if err = archiveRecoveryCleanupContinue(session, &observed, original, ""); err != nil {
		t.Fatal(err)
	}
	if err = archiveRecoveryCleanupContinue(session, &again, original, staged.DispositionSHA256); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(info, after) {
		t.Fatalf("re-entry rewrote the original disposition: %v", err)
	}
	for _, result := range []archiveRecoveryCleanupPlan{observed, again} {
		if result.Next != nil || result.Disposition == nil || *result.Disposition != *recorded.Disposition || len(result.Effects) != 0 {
			t.Fatalf("original disposition readback offered a second effect: %+v", result)
		}
	}
	if err = archiveRecoveryCleanupContinue(session, &refused, changed, ""); err == nil {
		t.Fatal("a different disposition replaced the earlier immutable reservation")
	}
	if err = readArchiveCleanupRef(*recorded.Disposition, nil); err != nil {
		t.Fatalf("refused re-entry changed original disposition: %v", err)
	}
}
