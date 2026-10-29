package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/loopview"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/server"
)

func TestHostViewsBindingAndAutomaticNativeReadback(t *testing.T) {
	dir := t.TempDir()
	root := t.TempDir()
	t.Setenv("NOODLE_HOST_DIR", dir)
	view := loopview.Empty()
	next := "original owner continuation"
	view.Runs = append(view.Runs, loopview.Run{ID: "run", Repository: root, Mode: "inner-only", Status: "source_observed", Stages: []loopview.Stage{}, Cards: []loopview.Card{}, Notes: []loopview.Note{}, Breakpoints: []loopview.Breakpoint{}, Next: &next})
	if _, _, err := loopview.Import(dir, view); err != nil {
		t.Fatal(err)
	}
	request := hostRequestBinding{Schema: 1, RequestID: "request", Operation: "attach", ProjectDir: root, ConfigSHA256: "config", BinarySHA256: "binary", HostDir: dir, Mode: "manual"}
	requestPath := filepath.Join(t.TempDir(), "request.json")
	if err := saveHostRequest(requestPath, request); err != nil {
		t.Fatal(err)
	}
	bind := func(order string) error {
		cmd := NewRootCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs([]string{"host", "views", "bind", "--run-id", "run", "--request-file", requestPath, "--order-id", order})
		return cmd.Execute()
	}
	if err := bind("order"); err != nil {
		t.Fatal(err)
	}
	if err := bind("order"); err != nil {
		t.Fatal(err)
	}
	if err := bind("different"); err == nil {
		t.Fatal("binding changed")
	}
	sessionDir := filepath.Join(root, ".noodle", "sessions", "session-1")
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		t.Fatal(err)
	}
	snapshot := reducer.DurableSnapshot{State: state.State{Mode: "supervised", Orders: map[string]state.OrderNode{"order": {OrderID: "order", Status: state.OrderActive, Stages: []state.StageNode{{StageIndex: 0, Status: state.StageRunning, Prompt: "original stage", Attempts: []state.AttemptNode{{SessionID: "session-1", Status: state.AttemptRunning}}}}}}}}
	snapshotPath := filepath.Join(root, ".noodle", "state.snapshot.json")
	if err := reducer.WriteSnapshotAtomic(snapshotPath, snapshot); err != nil {
		t.Fatal(err)
	}
	receipt := hostReceipt{RequestID: "request", ProjectDir: root, ConfigSHA256: "config", BinarySHA256: "binary", Mode: "manual", ContextID: "context", ContextState: "active"}
	observation := hostViewObservation{Receipts: []hostReceipt{receipt}}
	handler := server.LoopViewsHandler(dir, nil, func(_ context.Context, v *loopview.View) error { return enrichHostView(dir, v, observation) })
	read := func(etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/loops/runs/run", nil)
		req.Header.Set("If-None-Match", etag)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	first := read("")
	if first.Code != 200 || !strings.Contains(first.Body.String(), "original stage") {
		t.Fatal(first.Body.String())
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "input.txt"), []byte("actual changed input <script>"), 0600); err != nil {
		t.Fatal(err)
	}
	second := read(first.Header().Get("ETag"))
	if second.Code != 200 || second.Header().Get("ETag") == first.Header().Get("ETag") {
		t.Fatal("native input did not automatically refresh")
	}
	var got loopview.View
	if err := json.Unmarshal(second.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Runs[0].Status != "source_observed" || *got.Runs[0].Next != next || len(got.Runs[0].Stages) != 2 || got.Runs[0].Stages[1].ConsumerStatus != "input_recorded" {
		t.Fatalf("view: %+v", got)
	}
	decoded, _ := json.Marshal(got)
	if _, err := loopview.Decode(decoded); err != nil {
		t.Fatal(err)
	}
	order := snapshot.State.Orders["order"]
	order.Stages[0].ExtraPrompt = "additional source constraint"
	order.Stages[0].Attempts[0].Status = state.AttemptFailed
	order.Stages[0].Attempts[0].Error = "changes requested: retain the counterexample"
	snapshot.State.Orders["order"] = order
	if err := reducer.WriteSnapshotAtomic(snapshotPath, snapshot); err != nil {
		t.Fatal(err)
	}
	third := read(second.Header().Get("ETag"))
	if third.Code != 200 || third.Header().Get("ETag") == second.Header().Get("ETag") || !strings.Contains(third.Body.String(), "additional source constraint") || !strings.Contains(third.Body.String(), "changes requested: retain the counterexample") {
		t.Fatal("canonical prompt delta/failure did not automatically refresh", third.Body.String())
	}
	observation.Receipts = nil
	unknown := read("")
	if !strings.Contains(unknown.Body.String(), "native_readback") || !strings.Contains(unknown.Body.String(), "unknown") {
		t.Fatal("missing context was not unknown")
	}
	for _, name := range []string{"host.sock", "host.lock", "receipts.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatal("offline binding changed Host lifecycle")
		}
	}
}

func TestHostViewsNativeFeedbackHistoryAndControlReadback(t *testing.T) {
	root := t.TempDir()
	runtimeDir := filepath.Join(root, ".noodle")
	for _, session := range []string{"first", "revision"} {
		dir := filepath.Join(runtimeDir, "sessions", session)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"prompt", "input"} {
			if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte(session+" "+name), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	feedback := "changes requested: preserve the missing source <quote>"
	snapshot := reducer.DurableSnapshot{State: state.State{Mode: "supervised", Orders: map[string]state.OrderNode{
		"order": {OrderID: "order", Status: state.OrderCompleted, Stages: []state.StageNode{{
			Status: state.StageCompleted, Prompt: "current stage", ExtraPrompt: "current additional requirement",
			Attempts: []state.AttemptNode{
				{SessionID: "first", Status: state.AttemptFailed, Error: feedback},
				{SessionID: "revision", Status: state.AttemptCompleted},
			},
		}}},
	}}}
	snapshotPath := filepath.Join(runtimeDir, "state.snapshot.json")
	if err := reducer.WriteSnapshotAtomic(snapshotPath, snapshot); err != nil {
		t.Fatal(err)
	}
	ackPath := filepath.Join(runtimeDir, "control-ack.ndjson")
	acks := "{\"id\":\"opaque-one\",\"action\":\"request-changes\",\"status\":\"ok\",\"at\":\"2026-10-15T08:00:00Z\"}\n" +
		"{\"id\":\"opaque-two\",\"action\":\"requeue\",\"status\":\"error\",\"message\":\"worktree custody missing\",\"at\":\"2026-10-15T08:01:00Z\"}\n" +
		"{\"id\":\"partial\""
	if err := os.WriteFile(ackPath, []byte(acks), 0600); err != nil {
		t.Fatal(err)
	}
	next := "original owner next"
	run := loopview.Run{ID: "run", Repository: root, Mode: "inner-only", Status: "source_observed", Next: &next}
	binding := hostViewBinding{RunID: "run", OrderID: "order", Request: hostRequestBinding{RequestID: "request", ProjectDir: root}}
	receipts := []hostReceipt{{RequestID: "request", ProjectDir: root, ContextState: "active"}}
	if err := enrichNativeRun(&run, binding, receipts); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, stage := range run.Stages {
		kinds = append(kinds, stage.Kind)
	}
	wantKinds := []string{"native_stage", "native_extra_prompt", "native_prompt", "native_input", "native_attempt_feedback", "native_prompt", "native_input"}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("feedback/input order = %v", kinds)
	}
	if run.Stages[4].Prompt != feedback || run.Stages[4].Status != "failed" || run.Stages[4].ConsumerStatus != "UNKNOWN" || run.Stages[6].Status != "completed" {
		t.Fatalf("history or consumer boundary lost: %+v", run.Stages)
	}
	if run.Status != "source_observed" || *run.Next != next || run.Stages[1].Status != "current_checkpoint" || run.Stages[1].ConsumerStatus != "UNKNOWN" {
		t.Fatal("display inferred execution authority or ExtraPrompt consumption")
	}
	if len(run.Breakpoints) != 3 || run.Breakpoints[0].Message != feedback || run.Breakpoints[0].StageID != run.Stages[4].ID || run.Breakpoints[1].Status != "error" || run.Breakpoints[2].Status != "unknown" {
		t.Fatalf("earliest failure/ACK unknown lost: %+v", run.Breakpoints)
	}
	if len(run.Notes) != 3 || !strings.Contains(run.Notes[1].Body, "order 綁定 UNKNOWN") || !strings.Contains(run.Notes[1].Body, `"status":"ok"`) || !strings.Contains(run.Notes[2].Body, "worktree custody missing") {
		t.Fatalf("context ACK provenance lost: %+v", run.Notes)
	}
	if err := enrichNativeRun(&run, binding, receipts); err != nil {
		t.Fatal(err)
	}
	if len(run.Stages) != len(wantKinds) || len(run.Notes) != 3 || len(run.Breakpoints) != 3 {
		t.Fatal("readback duplicated native history")
	}
	raw, err := json.Marshal(loopview.View{SchemaVersion: 1, Runs: []loopview.Run{run}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loopview.Decode(raw); err != nil {
		t.Fatal(err)
	}
}

func TestHostViewsFailedAttemptsWithoutInputsAndPendingReview(t *testing.T) {
	for _, session := range []string{"", "missing-session", "../invalid-session"} {
		t.Run(session, func(t *testing.T) {
			root := t.TempDir()
			runtimeDir := filepath.Join(root, ".noodle")
			if err := os.MkdirAll(runtimeDir, 0755); err != nil {
				t.Fatal(err)
			}
			snapshot := reducer.DurableSnapshot{State: state.State{Orders: map[string]state.OrderNode{
				"order": {OrderID: "order", Status: state.OrderFailed, Stages: []state.StageNode{{Status: state.StageFailed, Attempts: []state.AttemptNode{{SessionID: session, Status: state.AttemptFailed}}}}},
			}, PendingReviews: map[string]state.PendingReviewNode{"order": {StageIndex: 0, Reason: "native review stopped: missing outcome"}}}}
			if err := reducer.WriteSnapshotAtomic(filepath.Join(runtimeDir, "state.snapshot.json"), snapshot); err != nil {
				t.Fatal(err)
			}
			run := loopview.Run{ID: "run", Repository: root, Mode: "single", Status: "source_observed"}
			binding := hostViewBinding{OrderID: "order", Request: hostRequestBinding{RequestID: "request", ProjectDir: root}}
			if err := enrichNativeRun(&run, binding, []hostReceipt{{RequestID: "request", ProjectDir: root, ContextState: "active"}}); err != nil {
				t.Fatal(err)
			}
			if len(run.Stages) != 3 || run.Stages[1].Prompt != "" || run.Stages[1].ConsumerStatus != "UNKNOWN" || run.Stages[2].Prompt != "native review stopped: missing outcome" {
				t.Fatalf("missing capture hid failure/review: %+v", run.Stages)
			}
			last := run.Breakpoints[len(run.Breakpoints)-1]
			if last.Kind != "native_review" || last.Status != "pending" || last.StageID != run.Stages[2].ID {
				t.Fatalf("pending review lost: %+v", last)
			}
			if !strings.Contains(run.Breakpoints[len(run.Breakpoints)-2].Message, "UNKNOWN") {
				t.Fatal("missing failure cause invented")
			}
		})
	}
}
func TestHostTaskCapabilitiesOffline(t *testing.T) {
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"host", "task", "capabilities"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result) != 6 || result["schema_version"] != float64(2) || result["runtime_admitted_orders"] != true || result["config_key"] != "runtime.admitted_orders" || result["scope"] != "runtime_dispatch" || result["empty_unrestricted"] != true || result["native_execution_reclamation"] != float64(1) {
		t.Fatal(result)
	}
}
