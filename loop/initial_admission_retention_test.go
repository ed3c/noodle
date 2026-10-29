package loop

import (
	"encoding/json"
	"testing"

	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/mise"
)

func TestFreshPromotionFreezesOriginalLocalTaskRetention(t *testing.T) {
	for _, tc := range []struct {
		name, runtime, provider, task string
		admitted, multi               bool
		want                          string
	}{
		{"codex", "openshell", "codex", "local-task", true, false, reducer.TaskDataAfterClosure},
		{"pi", "openshell", "pi", "local-task", true, false, reducer.TaskDataAfterClosure},
		{"process", "process", "codex", "local-task", true, false, taskDataRetention},
		{"other-task", "openshell", "codex", "execute", true, false, taskDataRetention},
		{"multi-stage", "openshell", "codex", "local-task", true, true, taskDataRetention},
		{"unadmitted", "openshell", "codex", "local-task", false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := initialLoop(t)
			l := fixture.loop
			id := "fresh-local-task"
			l.config.Agents.Codex.RequireTypedOutcome = false
			if tc.admitted {
				l.config.Runtime.AdmittedOrders = []string{id}
			}
			if _, exists := l.canonical.Orders[id]; exists {
				t.Fatal("fixture already owns promoted task")
			}
			stage := Stage{TaskKey: tc.task, Skill: tc.task, Provider: tc.provider, Model: "selected-model", Runtime: tc.runtime, Status: StageStatusPending, Prompt: `{"kind":"local_task","goal":"synthetic storage control","write_paths":["proof.txt"]}`}
			order := Order{ID: id, Status: OrderStatusActive, Stages: []Stage{stage}}
			if tc.multi {
				order.Stages = append(order.Stages, stage)
			}
			result := mergeResult{Promoted: true, InitialIDs: []string{id}, Orders: OrdersFile{Orders: []Order{order}}}
			if err := l.handlePromotionResult(result, mise.Brief{}, nil); err != nil {
				t.Fatal(err)
			}
			var payload map[string]string
			records := l.effectLedger.All()
			if len(records) != 1 || json.Unmarshal(records[0].Effect.Payload, &payload) != nil {
				t.Fatal("original admission receipt missing")
			}
			if payload["task_data_retention"] != tc.want {
				t.Fatalf("fresh admission retention = %q, want %q", payload["task_data_retention"], tc.want)
			}
			if reducer.ReclaimableTaskData(records, id) != (tc.want == reducer.TaskDataAfterClosure) {
				t.Fatal("original admission reclamation eligibility differs")
			}
			restarted := New(l.projectDir, "noodle", l.config, l.deps)
			if err := restarted.loadOrdersState(); err != nil {
				t.Fatal(err)
			}
			if err := restarted.loadOrBootstrapCanonical(); err != nil {
				t.Fatal(err)
			}
			if reducer.ReclaimableTaskData(restarted.effectLedger.All(), id) != (tc.want == reducer.TaskDataAfterClosure) {
				t.Fatal("restart changed original retention authority")
			}
		})
	}
}
