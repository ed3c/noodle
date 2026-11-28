package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
)

func rejectedReviewAdoptionFixtureFile(t *testing.T, path string, value any) loopruntime.SessionReadbackFileRef {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return loopruntime.SessionReadbackFileRef{Path: path, SHA256: hostDigest(raw)}
}

func TestRejectedReviewSuccessorPreparationReadbackUsesSameOriginalZero(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := rejectedReviewReleasePlan{Successor: loopruntime.SessionReadbackFileRef{Path: "/selected/noodle", SHA256: strings.Repeat("a", 64)},
		Generation: hostGeneration{Directory: root}}
	intent := rejectedReviewAdoptionFixtureFile(t, filepath.Join(root, "original-intent"), p)
	exit := rejectedReviewAdoptionFixtureFile(t, filepath.Join(root, "resource-resident-exit.json"), "original normal exit")
	maintenance := rejectedReviewAdoptionFixtureFile(t, intent.Path+".maintenance-exit", "original maintenance proof")
	zero := rejectedReviewAdoptionFixtureFile(t, intent.Path+".storage-zero", rejectedReviewStorageReceipt{
		Schema: 1, Intent: intent, Maintenance: maintenance, SourceExit: exit, SourceOnly: true,
	})
	before := rejectedReviewSuccessorPreparationIntent{1, intent, zero, p.Successor}
	rejectedReviewAdoptionFixtureFile(t, intent.Path+".successor-prepare", before)
	saved, err := readRejectedReviewSuccessorPreparationIntent(p, intent)
	if err != nil || saved != before {
		t.Fatalf("original ZERO did not bind the same pre-reservation intent: %+v %v", saved, err)
	}
	p.Successor.SHA256 = strings.Repeat("b", 64)
	if _, err := readRejectedReviewSuccessorPreparationIntent(p, intent); err == nil {
		t.Fatal("unknown reservation was rebound to a different executable")
	}
}

func TestRejectedReviewSuccessorSelectionRefusesCompetingGeneration(t *testing.T) {
	p := rejectedReviewReleasePlan{Generation: hostGeneration{ID: "old"},
		Successor: loopruntime.SessionReadbackFileRef{Path: "/selected/noodle", SHA256: strings.Repeat("a", 64)}}
	start := rejectedReviewSuccessorStart{Preparation: admission.PreparationRef{ID: "expected"}}
	successor := hostGeneration{ID: "new", State: "starting", Binary: p.Successor.Path, SHA: p.Successor.SHA256, Preparation: &start.Preparation}
	pool := hostPool{Generations: map[string]hostGeneration{"old": {ID: "old", State: "exited"}, "new": successor}}
	got, err := findRejectedReviewSuccessor(pool, p, start)
	if err != nil || got.ID != successor.ID {
		t.Fatalf("original starting generation was not observable: %v", err)
	}
	pool.Generations["extra"] = hostGeneration{ID: "extra", State: "starting"}
	if _, err := findRejectedReviewSuccessor(pool, p, start); err == nil {
		t.Fatal("admitted a competing successor generation")
	}
}

func TestRejectedReviewSuccessorKeepsHoldUntilExactPausedOwner(t *testing.T) {
	root := t.TempDir()
	successorID := strings.Repeat("a", 32)
	request := hostReceipt{RequestID: "original-request", ContextID: "old-context", HostDir: root}
	intent := loopruntime.SessionReadbackFileRef{SHA256: strings.Repeat("b", 64)}
	p := rejectedReviewReleasePlan{OwnerBefore: hostTaskOwner{Original: request, Current: request}, LoopMode: "supervised",
		Successor: loopruntime.SessionReadbackFileRef{Path: "/selected/noodle", SHA256: strings.Repeat("c", 64)}}
	owner := hostTaskOwner{State: "owned", Original: request, Current: request, MaintenanceHold: rejectedReviewReleaseHold + intent.SHA256,
		RoundState: &loop.HostContinuation{Mode: "supervised", DispatchState: loop.StatePaused}}
	owner.Current.HostInstanceID, owner.Current.ContextID, owner.Current.ContextState = successorID, "same-new-context", "active"
	pool := hostPool{Schema: 1, Active: successorID, Generations: map[string]hostGeneration{successorID: {ID: successorID,
		State: "active", Directory: filepath.Join(root, "g", successorID), Binary: p.Successor.Path, SHA: p.Successor.SHA256}},
		Owners: map[string]hostTaskOwner{request.RequestID: owner}}
	path := filepath.Join(root, "pool.json")
	rejectedReviewAdoptionFixtureFile(t, path, pool)
	receipt := rejectedReviewRoundRelease{Status: "successor_admission_pending", SuccessorHost: successorID, SuccessorContext: "other-context"}
	if _, err := acceptRejectedReviewSuccessor(p, intent, receipt); err == nil {
		t.Fatal("different context cleared original hold")
	}
	after, err := readHostPool(root)
	if err != nil || after.Owners[request.RequestID].MaintenanceHold != owner.MaintenanceHold {
		t.Fatalf("unknown admission changed original authority: %v", err)
	}
	receipt.SuccessorContext = owner.Current.ContextID
	observed, err := acceptRejectedReviewSuccessor(p, intent, receipt)
	if err != nil || observed.Status != "successor_round_adopted" || observed.AuthorizesDispatch {
		t.Fatalf("exact paused successor could not release original hold: %+v %v", observed, err)
	}
}
