package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/taskreg"
	"github.com/poteto/noodle/loop"
	"github.com/poteto/noodle/mise"
	"github.com/poteto/noodle/monitor"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/skill"
)

type diagnosticLoopMise struct{}

func (diagnosticLoopMise) Build(context.Context, mise.ActiveSummary, []mise.HistoryItem) (mise.Brief, []string, bool, error) {
	return mise.Brief{}, nil, false, nil
}

func diagnosticLoopJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestOpenShellDiagnosticNotStartedOriginalLoopClosure(t *testing.T) {
	rt, b, project, assertNoExecutionOrDelete := loopruntime.DiagnosticLoopFixtureForTest(t)
	dir := filepath.Join(project, ".noodle")
	prompt := `{"kind":"local_task","goal":"diagnose original failure","write_paths":["result.txt"]}`
	stage := state.StageNode{StageIndex: 0, TaskKey: "local-task", Skill: "local-task", Provider: b.Provider, Model: b.Model, Runtime: "openshell", Prompt: prompt, Status: state.StageFailed, Attempts: []state.AttemptNode{{AttemptID: "original-canonical-attempt", SessionID: b.SessionID, Status: state.AttemptFailed, WorktreeName: filepath.Base(b.WorktreePath)}}}
	canonical := state.State{SchemaVersion: 1, Mode: state.RunModeManual, LastEventID: "1", Orders: map[string]state.OrderNode{
		b.OrderID:  {OrderID: b.OrderID, Status: state.OrderFailed, Stages: []state.StageNode{stage}},
		"schedule": {OrderID: "schedule", Status: state.OrderActive, Stages: []state.StageNode{{StageIndex: 0, TaskKey: "schedule", Skill: "schedule", Status: state.StagePending}}},
	}}
	ledger := reducer.NewEffectLedger()
	id := reducer.InitialAdmissionEffectID(b.OrderID)
	payload, _ := json.Marshal(map[string]string{"order_id": b.OrderID, "task_data_retention": reducer.TaskDataAfterClosure, "initial_revision": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	ledger.Record(reducer.Effect{EffectID: id, Type: reducer.EffectInitialAdmission, Payload: payload})
	if err := ledger.MarkRunning(id); err != nil {
		t.Fatal(err)
	}
	if err := ledger.MarkDone(id, reducer.EffectResult{EffectID: id, Status: reducer.EffectResultCompleted}); err != nil {
		t.Fatal(err)
	}
	if err := reducer.WriteSnapshotAtomic(filepath.Join(dir, "state.snapshot.json"), reducer.BuildSnapshot(canonical, ledger, time.Now())); err != nil {
		t.Fatal(err)
	}
	diagnosticLoopJSON(t, filepath.Join(dir, "orders.json"), loop.OrdersFile{Orders: []loop.Order{{ID: b.OrderID, Status: loop.OrderStatusFailed, Stages: []loop.Stage{{TaskKey: "local-task", Skill: "local-task", Provider: b.Provider, Model: b.Model, Runtime: "openshell", Prompt: prompt, Status: loop.StageStatusFailed}}}}})
	cfg := config.DefaultConfig()
	cfg.Mode = "manual"
	cfg.Skills.Paths = []string{t.TempDir()}
	cfg.Runtime.AdmittedOrders = []string{b.OrderID}
	cfg.Agents.Codex.RequireTypedOutcome = false
	registry := taskreg.NewFromSkills([]skill.SkillMeta{{Name: "schedule", Frontmatter: skill.Frontmatter{Schedule: "schedule"}}, {Name: "local-task", Frontmatter: skill.Frontmatter{Schedule: "local task"}}})
	owner := loop.New(project, "noodle", cfg, loop.Dependencies{Runtimes: map[string]loopruntime.Runtime{"openshell": rt}, Mise: diagnosticLoopMise{}, Registry: registry, ModeOverride: state.RunModeManual})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- owner.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("original Loop did not finish")
		}
	}()
	select {
	case <-owner.Ready():
	case err := <-done:
		t.Fatalf("Loop startup: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var monitored monitor.SessionMeta
	metaPath := filepath.Join(dir, "sessions", b.SessionID, "meta.json")
	monitoredRef, err := loopruntime.ReadSessionReadbackFile(metaPath, &monitored)
	if err != nil {
		t.Fatal(err)
	}
	if monitored.Status != monitor.SessionStatusFailed || monitored.Alive || monitored.ExitPending {
		t.Fatalf("formal Monitor did not produce terminal failed metadata: %+v", monitored)
	}
	if monitored.SessionID != b.SessionID || monitored.Provider != b.Provider || monitored.Model != b.Model || monitored.Runtime != "openshell" {
		t.Fatalf("formal Monitor lost original dispatch identity: %+v", monitored)
	}
	original, err := rt.ReadCleanupSession(t.Context(), b, project, loopruntime.StatusFailed)
	if err != nil {
		t.Fatal(err)
	}
	if original.Diagnostic == nil || original.Diagnostic.ExecutionBasis != "not_started" || original.Execution.NativeExecExitCode != -1 {
		t.Fatalf("original runtime did not preserve unsubmitted failure: %+v", original)
	}
	if err := owner.Cleanup(ctx, false); !errors.Is(err, loop.ErrReleaseBusy) {
		t.Fatalf("unclosed failure became eligible: %v", err)
	}
	diagnostics := original.Execution.Sources["diagnostic_session/openshell-dispatch.json"]
	if err := owner.CloseFailedTask(ctx, b.OrderID, b.SessionID, "original owner abandons diagnosed pre-Exec failure", diagnostics); err != nil {
		t.Fatal(err)
	}
	if err := owner.Cleanup(ctx, false); err != nil {
		t.Fatalf("original -1 diagnostic rejected after closure: %v", err)
	}
	after, err := rt.ReadCleanupSession(ctx, b, project, loopruntime.StatusFailed)
	if err != nil {
		t.Fatal(err)
	}
	if after.Execution.Sources["diagnostic_session/meta.json"] != monitoredRef {
		t.Fatal("cleanup changed or failed to retain the formal Monitor metadata")
	}
	if after.Execution.NativeExecExitCode != -1 || after.Diagnostic.Outcome.ExitCode != -1 || after.OutcomeStatus != loopruntime.StatusFailed {
		t.Fatal("Loop forged execution success")
	}
	var closure struct {
		Order state.OrderNode `json:"order"`
	}
	// The closure path comes from the original owner's immutable canonical backlink.
	snapshot, err := reducer.ReadSnapshot(filepath.Join(dir, "state.snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	var closureRef loopruntime.SessionReadbackFileRef
	if err := json.Unmarshal(snapshot.State.Orders[b.OrderID].Stages[0].Extra["failed_task_closure"], &closureRef); err != nil {
		t.Fatal(err)
	}
	if _, err := loopruntime.ReadSessionReadbackFile(closureRef.Path, &closure); err != nil {
		t.Fatal(err)
	}
	if closure.Order.Status != state.OrderFailed || closure.Order.Stages[0].Attempts[0].Status != state.AttemptFailed {
		t.Fatal("explicit diagnostic closure promoted failure")
	}
	if _, err := os.Stat(b.WorktreePath); err != nil {
		t.Fatalf("inspection removed original worktree: %v", err)
	}
	select {
	case <-owner.Done():
		t.Fatal("cleanup inspect detached original actor")
	default:
	}
	assertNoExecutionOrDelete()
}
