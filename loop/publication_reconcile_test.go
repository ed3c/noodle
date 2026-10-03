package loop

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

func mergedPublicationFixture(t *testing.T, failed bool) (string, string, string, string) {
	t.Helper()
	project, _ := stoppedReviewFixture(t)
	c, err := InspectPublicationClaim(project, "order-1", "example/project#7")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "claim.json")
	recoveryWrite(t, path, raw)
	// Actual detached control HEAD contains the published candidate.
	publicationRun(t, project, "git", "checkout", "--detach", c.Head)
	if failed {
		p := filepath.Join(project, ".noodle/state.snapshot.json")
		s, err := reducer.ReadSnapshot(p)
		if err != nil {
			t.Fatal(err)
		}
		o := s.State.Orders[c.OrderID]
		o.Status, o.Stages[0].Status = state.OrderFailed, state.StageFailed
		o.Stages[0].Attempts[0].Status = state.AttemptFailed
		o.Stages[0].Attempts[0].Error = "merge failed: detached control checkout"
		s.State.Orders[c.OrderID] = o
		delete(s.State.PendingReviews, c.OrderID)
		payload, _ := json.Marshal(map[string]any{"order_id": c.OrderID, "stage_index": c.StageIndex, "worktree_name": c.WorktreeName})
		s.EffectLedger = append(s.EffectLedger, reducer.EffectLedgerRecord{
			EffectID: "original-merge", Status: reducer.EffectLedgerPending,
			Effect: reducer.Effect{EffectID: "original-merge", Type: reducer.EffectMerge, Payload: payload, CreatedAt: s.GeneratedAt},
		})
		if err := reducer.WriteSnapshotAtomic(p, s); err != nil {
			t.Fatal(err)
		}
	}
	return project, path, publicationDigest(raw), c.Head
}

func TestPublicationReconciliationPreservesFailedAttemptAndResumes(t *testing.T) {
	for _, failed := range []bool{false, true} {
		for _, cut := range []string{"after_intent", "after_canonical"} {
			project, path, digest, head := mergedPublicationFixture(t, failed)
			func() {
				defer func() {
					if recover() != cut {
						t.Error("interruption not reached")
					}
				}()
				_, err := reconcilePublication(project, path, digest, head, func(point string) {
					if point == cut {
						panic(cut)
					}
				})
				if err != nil {
					t.Fatal(err)
				}
			}()
			r, err := ReconcilePublication(project, path, digest, head)
			if err != nil || r.Status != "completed" {
				t.Fatalf("reconcile: %+v %v", r, err)
			}
			s, err := reducer.ReadSnapshot(filepath.Join(project, ".noodle/state.snapshot.json"))
			if err != nil {
				t.Fatal(err)
			}
			o := s.State.Orders["order-1"]
			if o.Status != state.OrderCompleted || len(o.Stages[0].Attempts) != 1 {
				t.Fatal("original order/attempt changed")
			}
			if failed && (o.Stages[0].Attempts[0].Status != state.AttemptFailed || o.Stages[0].Attempts[0].Error == "") {
				t.Fatal("historical failure erased")
			}
			if failed {
				last := s.EffectLedger[len(s.EffectLedger)-1]
				if last.Status != reducer.EffectLedgerDone || last.Result == nil || last.Result.Status != reducer.EffectResultCancelled || last.Attempts != 0 {
					t.Fatal("obsolete merge was not cancelled as a separate fact")
				}
			}
			before, _ := os.ReadFile(filepath.Join(project, ".noodle/state.snapshot.json"))
			if _, err := ReconcilePublication(project, path, digest, head); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(filepath.Join(project, ".noodle/state.snapshot.json"))
			if !bytes.Equal(before, after) {
				t.Fatal("readback repeated completion")
			}
		}
	}
}

func TestPublicationReconciliationRefusesChangedEvidence(t *testing.T) {
	for _, defect := range []string{"digest", "unmerged", "dirty", "events"} {
		project, path, digest, head := mergedPublicationFixture(t, true)
		switch defect {
		case "digest":
			digest = "invalid"
		case "unmerged":
			publicationRun(t, project, "git", "checkout", "--detach", "main")
		case "dirty":
			recoveryWrite(t, filepath.Join(project, "dirty.txt"), []byte("preserve"))
		case "events":
			recoveryWrite(t, filepath.Join(project, ".noodle/sessions/session-1/events.ndjson"), []byte("changed"))
		}
		before, _ := os.ReadFile(filepath.Join(project, ".noodle/state.snapshot.json"))
		if _, err := ReconcilePublication(project, path, digest, head); err == nil {
			t.Fatalf("accepted %s", defect)
		}
		after, _ := os.ReadFile(filepath.Join(project, ".noodle/state.snapshot.json"))
		if !bytes.Equal(before, after) {
			t.Fatalf("%s wrote state", defect)
		}
	}
}
