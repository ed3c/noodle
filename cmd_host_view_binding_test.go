package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		req := httptest.NewRequest("GET", "/api/loops", nil)
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
	if _, err := loopview.Decode(second.Body.Bytes()); err != nil {
		t.Fatal(err)
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
	if len(result) != 5 || result["process_admitted_orders"] != true || result["config_key"] != "runtime.process.admitted_orders" {
		t.Fatal(result)
	}
}
