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

func TestRejectedReviewRoundReleaseUnknownOfferReadbackOnly(t *testing.T) {
	intent := loopruntime.SessionReadbackFileRef{Path: filepath.Join(t.TempDir(), "source-release.json"), SHA256: strings.Repeat("a", 64)}
	p := rejectedReviewReleasePlan{Request: loopruntime.SessionReadbackFileRef{Path: filepath.Join(filepath.Dir(intent.Path), "missing-native-request")}}
	offer := intent.Path + ".offer"
	if err := saveHostStorageBytes(offer, []byte(intent.SHA256)); err != nil {
		t.Fatal(err)
	}
	receipt := projectRejectedReviewRelease(p, intent.SHA256, &intent)
	observed, err := applyRejectedReviewRoundRelease(t.Context(), p, intent, receipt)
	if err == nil || observed.Status != "source_release_pending" || len(observed.Effects) != 0 ||
		observed.Next.Operation != "original_native_round_release_readback" {
		t.Fatalf("unknown offer attempted a second effect: %+v %v", observed, err)
	}
	if _, err := rollbackRejectedReviewRoundRelease(t.Context(), p, intent, receipt); err == nil {
		t.Fatal("withdrew pending original release after its effect offer")
	}
	saved, err := os.ReadFile(offer)
	if err != nil || string(saved) != intent.SHA256 {
		t.Fatalf("original release offer changed: %v", err)
	}
}

func TestRejectedReviewRoundReleaseRollbackRequiresRestoredOriginalOwner(t *testing.T) {
	root := t.TempDir()
	intent := loopruntime.SessionReadbackFileRef{Path: filepath.Join(root, "intent.json"), SHA256: strings.Repeat("b", 64)}
	request := hostReceipt{RequestID: "original-request", HostDir: root, ContextID: "original-context"}
	p := rejectedReviewReleasePlan{OwnerBefore: hostTaskOwner{Original: request, Current: request, State: "owned"}}
	preimage, err := json.Marshal(p.OwnerBefore)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(preimage, &p.OwnerBefore); err != nil {
		t.Fatal(err)
	}
	if err := saveHostStorageBytes(intent.Path+".rollback", []byte(intent.SHA256)); err != nil {
		t.Fatal(err)
	}
	for _, restored := range []bool{false, true} {
		owner := p.OwnerBefore
		if !restored {
			owner.ControllerOnly, owner.MaintenanceHold = true, rejectedReviewReleaseHold+intent.SHA256
		}
		pool := hostPool{Schema: 1, Generations: map[string]hostGeneration{}, Owners: map[string]hostTaskOwner{request.RequestID: owner}}
		raw, err := json.Marshal(pool)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "pool.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		receipt, err := readRejectedReviewRoundRelease(t.Context(), p, intent, projectRejectedReviewRelease(p, intent.SHA256, &intent))
		if err != nil || (receipt.Status == "source_release_withdrawn") != restored {
			t.Fatalf("fence restoration reported prematurely: %+v %v", receipt, err)
		}
	}
}

func TestRejectedReviewMaintenanceRetainsUnknownOriginalStart(t *testing.T) {
	intent := loopruntime.SessionReadbackFileRef{Path: filepath.Join(t.TempDir(), "intent"), SHA256: strings.Repeat("c", 64)}
	if err := saveHostStorageBytes(intent.Path+".maintenance-offer", []byte(intent.SHA256)); err != nil {
		t.Fatal(err)
	}
	_, err := runRejectedReviewMaintenance(t.Context(), rejectedReviewReleasePlan{}, intent, intent, nil)
	if err == nil || !strings.Contains(err.Error(), "no spawn replay") {
		t.Fatalf("unknown maintenance start recreated an original Gateway: %v", err)
	}
	if _, err := os.Lstat(intent.Path + ".maintenance"); !os.IsNotExist(err) {
		t.Fatalf("unknown source created new maintenance storage: %v", err)
	}
}

func TestRejectedReviewControllerRetirementRequiresSameHeldSourceWithoutSuccessor(t *testing.T) {
	const digest = "original"
	ref := admission.PreparationRef{ID: "original"}
	generation := hostGeneration{ID: "source", State: "active", Preparation: &ref}
	scope := hostReceipt{RequestID: "same-request", ContextID: "original-context", HostInstanceID: generation.ID}
	before := hostTaskOwner{State: "owned", Original: scope, Current: scope}
	owner := before
	owner.State, owner.ControllerOnly, owner.MaintenanceHold = "ready", true, rejectedReviewReleaseHold+digest
	owner.Current.ContextState, owner.Current.TerminalCause = "detached", loop.TerminalMigrationDetach
	owner.RoundState = &loop.HostContinuation{Mode: "supervised", DispatchState: loop.StatePaused}
	p := rejectedReviewReleasePlan{OwnerBefore: before, Generation: generation, LoopMode: "supervised"}
	pool := hostPool{Active: generation.ID, Owners: map[string]hostTaskOwner{scope.RequestID: owner}, Generations: map[string]hostGeneration{generation.ID: generation}}
	if _, err := rejectedReviewControllerScope(pool, p, digest); err != nil {
		t.Fatal(err)
	}
	pool.Active, generation.State = "", "retiring"
	pool.Generations[generation.ID] = generation
	if _, err := rejectedReviewControllerScope(pool, p, digest); err != nil {
		t.Fatal(err)
	}
	pool.Generations["new-before-zero"] = hostGeneration{ID: "new-before-zero", State: "starting"}
	if _, err := rejectedReviewControllerScope(pool, p, digest); err == nil {
		t.Fatal("source retirement admitted concurrent successor")
	}
}
