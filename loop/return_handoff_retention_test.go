package loop

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/ingest"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

func TestReturnHandoffApproveMergeFailureRetainsFirstIntent(t *testing.T) {
	for _, mode := range []string{"acknowledged", "unknown", "lost-ack", "second-attempt", "legacy-checkpoint"} {
		t.Run(mode, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "nrh-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			listener, err := net.Listen("unix", filepath.Join(dir, "r.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			packets := make(chan returnPacket, 2)
			go func() {
				for {
					connection, err := listener.Accept()
					if err != nil {
						return
					}
					var packet returnPacket
					if json.NewDecoder(connection).Decode(&packet) == nil {
						packets <- packet
					}
					if mode != "lost-ack" {
						_ = json.NewEncoder(connection).Encode(map[string]string{"status": func() string {
							if mode == "second-attempt" {
								return "acknowledged"
							}
							return mode
						}()})
					}
					_ = connection.Close()
				}
			}()
			l, cook := returnHandoffTestLoop(t, filepath.Join(dir, "r.sock"))
			if mode == "second-attempt" {
				cook.attempt = 1
				cook.session = &adoptedSession{id: "second-worker", status: "completed"}
				if err := l.emitEventChecked(ingest.EventDispatchCompleted, map[string]any{"order_id": cook.orderID, "stage_index": 0, "attempt_id": dispatchAttemptID(cook.orderID, 0, 1), "session_id": cook.session.ID(), "worktree_name": cook.worktreeName}); err != nil {
					t.Fatal(err)
				}
			}
			l.eventCounter.Store(5)
			if err := l.parkPendingReview(cook, "original candidate"); err != nil {
				t.Fatal(err)
			}
			var original returnPacket
			select {
			case original = <-packets:
			case <-time.After(3 * time.Second):
				t.Fatal("first callback missing")
			}
			if original.EventID != "6" {
				t.Fatal("fixture did not preserve original event6")
			}
			if mode == "legacy-checkpoint" {
				order := l.canonical.Orders[cook.orderID]
				order.Stages[0].Attempts[0].ReturnHandoff = nil
				order.Stages[0].Attempts[0].ReturnHandoffUnattempted = false
				l.canonical.Orders[cook.orderID] = order
				if err := l.persistCanonicalCheckpoint(); err != nil {
					t.Fatal(err)
				}
				l = New(l.projectDir, "noodle", l.config, l.deps)
				if ok, err := l.loadCanonicalSnapshot(); err != nil || !ok {
					t.Fatal("legacy checkpoint", err)
				}
				if err := l.syncPendingReviewProjection(); err != nil {
					t.Fatal(err)
				}
			}
			l.deps.Worktree.(*fakeWorktree).mergeErr = errors.New("root checkout has uncommitted changes")
			if err := l.controlMerge(cook.orderID); err != nil {
				t.Fatal(err)
			}
			if len(l.deps.Worktree.(*fakeWorktree).merged) != 1 {
				t.Fatal("original approve did not reach merge")
			}
			saved, err := reducer.ReadSnapshot(l.canonicalSnapshotPath())
			if err != nil {
				t.Fatal(err)
			}
			pending := saved.State.PendingReviews[cook.orderID]
			if pending.ReturnHandoff == nil || pending.ReturnHandoff.EventID != original.EventID || pending.ReturnHandoff.Status != "unknown" {
				t.Fatalf("merge failure replaced first unknown intent: %+v", pending.ReturnHandoff)
			}
			if saved.State.LastEventID != "10" || saved.State.Orders[cook.orderID].Stages[0].Status != state.StageReview {
				t.Fatal("native merge failure did not repark")
			}
			restarted := New(l.projectDir, "noodle", l.config, l.deps)
			if ok, err := restarted.loadCanonicalSnapshot(); err != nil || !ok {
				t.Fatal("restart original checkpoint", err)
			}
			if err := restarted.parkPendingReview(cook, "same original merge failure after restart"); err != nil {
				t.Fatal(err)
			}
			if got := restarted.canonical.PendingReviews[cook.orderID].ReturnHandoff; got == nil || got.EventID != "6" || got.Status != "unknown" {
				t.Fatal("restart replaced unknown first intent", got)
			}
			select {
			case packet := <-packets:
				t.Fatalf("second callback effect: %+v", packet)
			case <-time.After(100 * time.Millisecond):
			}

		})
	}
}
