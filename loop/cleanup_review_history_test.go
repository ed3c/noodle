package loop

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

func rejectedCleanupFixture(t *testing.T) (*Loop, state.OrderNode) {
	t.Helper()
	l, cook, _ := newRequestChangesRecoveryOutcome(t, event.StageOutcomeCompleted)
	if err := l.controlEditItem(ControlCommand{OrderID: cook.orderID, Prompt: "correct the rejected candidate"}); err != nil {
		t.Fatal(err)
	}
	if err := l.controlRequeue(cook.orderID); err != nil {
		t.Fatal(err)
	}
	order := l.canonical.Orders[cook.orderID]
	stage := &order.Stages[cook.stageIndex]
	stage.Runtime = "openshell"
	stage.TaskKey = "local-task"
	stage.Skill = "local-task"
	stage.Attempts = append(stage.Attempts, state.AttemptNode{AttemptID: "successor-attempt", SessionID: "successor-session", Status: state.AttemptCompleted, WorktreeName: stage.Attempts[0].WorktreeName})
	stage.Status = state.StageCompleted
	order.Status = state.OrderCompleted
	return l, order
}

func TestCleanupReviewRejectionUsesOriginalTypedRequeue(t *testing.T) {
	for _, variant := range []string{"bound", "completed-later", "wrong-session", "wrong-reason", "no-successor", "missing-requeue", "foreign-requeue", "untyped-failure"} {
		t.Run(variant, func(t *testing.T) {
			l, order := rejectedCleanupFixture(t)
			path := filepath.Join(l.runtimeDir, "loop-events.ndjson")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var changed bytes.Buffer
			for _, line := range bytes.Split(data, []byte{'\n'}) {
				if len(bytes.TrimSpace(line)) == 0 {
					continue
				}
				var e event.LoopEvent
				if err := json.Unmarshal(line, &e); err != nil {
					t.Fatal(err)
				}
				if e.Type == event.LoopEventOrderRequeued {
					if variant == "missing-requeue" {
						continue
					}
					if variant == "foreign-requeue" {
						e.Payload = json.RawMessage(`{"order_id":"foreign"}`)
					}
				}
				if variant == "untyped-failure" && e.Type == event.LoopEventStageFailed {
					var p map[string]any
					if err := json.Unmarshal(e.Payload, &p); err != nil {
						t.Fatal(err)
					}
					delete(p, "agent_mistake")
					e.Payload, _ = json.Marshal(p)
				}
				raw, _ := json.Marshal(e)
				changed.Write(raw)
				changed.WriteByte('\n')
			}
			if variant == "completed-later" {
				raw, _ := json.Marshal(event.LoopEvent{Type: event.LoopEventOrderCompleted, Payload: json.RawMessage(`{"order_id":"` + order.OrderID + `"}`)})
				changed.Write(raw)
				changed.WriteByte('\n')
			}
			if err := os.WriteFile(path, changed.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			switch variant {
			case "wrong-session":
				order.Stages[0].Attempts[0].SessionID = "foreign-session"
			case "wrong-reason":
				order.Stages[0].Attempts[0].Error = "changes requested: a different rejection"
			case "no-successor":
				order.Stages[0].Attempts = order.Stages[0].Attempts[:1]
			}
			original, _ := json.Marshal(order)
			ref, err := l.cleanupReviewHistory(order, order.Stages[0], 0)
			allowed := variant == "bound" || variant == "completed-later"
			if (err == nil) != allowed {
				t.Fatalf("variant %s: %v", variant, err)
			}
			after, _ := json.Marshal(order)
			if !bytes.Equal(original, after) {
				t.Fatal("canonical history changed")
			}
			if allowed {
				if err := os.WriteFile(path, append(changed.Bytes(), []byte("{}\n")...), 0600); err != nil {
					t.Fatal(err)
				}
				if loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"review_history": ref}) == nil {
					t.Fatal("changed review history accepted")
				}
			}
		})
	}
}

func TestCleanupReadsRejectedCandidateWithoutChangingFailedAttempt(t *testing.T) {
	l, order := rejectedCleanupFixture(t)
	l.canonical.Orders[order.OrderID] = order
	before, _ := json.Marshal(l.canonical)
	rt := &cleanupReadbackRuntime{mockRuntime: newMockRuntime()}
	calls := []string{}
	rt.read = func(b loopruntime.CompletedSessionBinding, project string, status loopruntime.SessionStatus) (loopruntime.CleanupSessionReadback, error) {
		calls = append(calls, b.SessionID)
		if status != loopruntime.StatusCompleted {
			t.Fatalf("completed rejected runtime read as %s", status)
		}
		path := filepath.Join(l.runtimeDir, "sessions", b.SessionID, "bound-evidence.json")
		recoveryWriteJSON(t, path, map[string]string{"session_id": b.SessionID})
		ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return loopruntime.CleanupSessionReadback{OutcomeStatus: status, PublishedHead: "retained-head", Execution: loopruntime.CompletedSessionReadback{NativeExecExitCode: 0, NativePhase: "Stopped", StopAcknowledged: true, Sources: map[string]loopruntime.SessionReadbackFileRef{"bound": ref}}}, nil
	}
	next := filepath.Join(l.runtimeDir, "sessions", "successor-session")
	if err := os.Mkdir(next, 0700); err != nil {
		t.Fatal(err)
	}
	l.deps.Runtimes = map[string]loopruntime.Runtime{"openshell": rt}
	if err := l.observeCleanupSessions(); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(l.canonical)
	if !bytes.Equal(before, after) || !reflect.DeepEqual(calls, []string{order.Stages[0].Attempts[0].SessionID, "successor-session"}) || len(rt.calls) != 0 {
		t.Fatalf("history or runtime effects changed: %v", calls)
	}
}
