package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
)

func automaticTaskAcceptanceFixture(t *testing.T) (*noodleHost, hostRequest, *taskAcceptanceHostRuntime, string) {
	t.Helper()
	h, req, rt, path, _ := hostTaskAcceptanceFixture(t)
	req.Mode, req.CleanupCompleted = "auto", true
	var binding hostRequestBinding
	if _, err := loopruntime.ReadSessionReadbackFile(path, &binding); err != nil {
		t.Fatal(err)
	}
	binding.Mode, binding.CleanupCompleted = req.Mode, true
	raw, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	req.ReadbackRequest, err = loopruntime.ReadSessionReadbackFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.receipts[0].Mode, h.receipts[0].CleanupCompleted = req.Mode, true
	h.contexts[req.ContextID].receipt = h.receipts[0]
	if err := h.save(); err != nil {
		t.Fatal(err)
	}
	return h, req, rt, path
}

func detachAcceptedTaskFixture(h *noodleHost, req hostRequest) error {
	h.receipts[0].ContextState = "detached"
	h.receipts[0].TerminalCause = loop.TerminalAutoCompleted
	delete(h.contexts, req.ContextID)
	return h.save()
}

func TestHostTaskAcceptanceACKSurvivesAutomaticDetach(t *testing.T) {
	h, req, rt, path := automaticTaskAcceptanceFixture(t)
	serveTaskAcceptanceOnce(t, h, func(_ *hostTaskAcceptance) bool {
		if err := detachAcceptedTaskFixture(h, req); err != nil {
			t.Error(err)
			return false
		}
		return true
	})
	got, err := callHostTaskAcceptance(t.Context(), req.ProjectDir, path, req.ReadbackOrderID, req.ReadbackSessionID, req.TaskAcceptance.Path, req.TaskAcceptance.SHA256)
	if err != nil || got.Status != "accepted" || got.ClosureReceipt == nil || rt.calls.Load() != 1 {
		t.Fatalf("completed detachment lost original ACK: %+v %v", got, err)
	}
	if _, mutable := got.Sources["host_receipts"]; mutable {
		t.Fatal("mutable receipt index reported as immutable source")
	}
}

func TestHostTaskAcceptanceReceiptVerificationKeepsIdentityExact(t *testing.T) {
	for _, variant := range []string{"completed", "pid", "binary", "instance", "context", "request", "config", "cleanup-policy", "failed", "migration", "missing-cause", "duplicate"} {
		t.Run(variant, func(t *testing.T) {
			h, req, _, path := automaticTaskAcceptanceFixture(t)
			bound, err := originalHostTaskAcceptanceClient(req.ProjectDir, path, req.ReadbackOrderID, req.ReadbackSessionID, "task_delivery_accept")
			if err != nil {
				t.Fatal(err)
			}
			if err := detachAcceptedTaskFixture(h, req); err != nil {
				t.Fatal(err)
			}
			switch variant {
			case "pid":
				h.receipts[0].HostPID++
			case "binary":
				h.receipts[0].BinarySHA256 = "replacement"
			case "instance":
				h.receipts[0].HostInstanceID = "replacement"
			case "context":
				h.receipts[0].ContextID = "replacement"
			case "request":
				h.receipts[0].RequestID = "replacement"
			case "config":
				h.receipts[0].ConfigSHA256 = "replacement"
			case "cleanup-policy":
				h.receipts[0].CleanupCompleted = false
			case "failed":
				h.receipts[0].ContextState = "failed"
			case "migration":
				h.receipts[0].TerminalCause = loop.TerminalMigrationDetach
			case "missing-cause":
				h.receipts[0].TerminalCause = ""
			case "duplicate":
				h.receipts = append(h.receipts, h.receipts[0])
			}
			if err := h.save(); err != nil {
				t.Fatal(err)
			}
			err = bound.verifyAcceptance()
			if variant == "completed" {
				if err != nil {
					t.Fatal(err)
				}
				if err := bound.hostTaskClientBinding.verify(); err == nil {
					t.Fatal("other commands' receipt hash verification was relaxed")
				}
			} else if err == nil {
				t.Fatal("changed identity or unresolved detachment accepted")
			}
		})
	}
}

func TestHostTaskAcceptanceReadbackRequiresOriginalHost(t *testing.T) {
	for _, variant := range []string{"detached", "active", "missing-host", "replacement", "evidence-drift", "failed", "migration", "missing-receipt"} {
		t.Run(variant, func(t *testing.T) {
			h, req, rt, path := automaticTaskAcceptanceFixture(t)
			prepareTaskReclamationFixture(t, h, &req)
			accepted := h.taskAcceptance(t.Context(), req)
			if accepted.Status != "accepted" {
				t.Fatalf("prepare original acceptance: %+v", accepted)
			}
			writeTaskReclamationFixture(t, req)
			if variant != "active" {
				if err := detachAcceptedTaskFixture(h, req); err != nil {
					t.Fatal(err)
				}
			}
			switch variant {
			case "replacement":
				h.instanceID = "replacement"
			case "evidence-drift":
				closureTestDrift(t, filepath.Join(req.ProjectDir, "integrated-evidence.txt"))
			case "failed":
				h.receipts[0].ContextState = "failed"
				if err := h.save(); err != nil {
					t.Fatal(err)
				}
			case "migration":
				h.receipts[0].TerminalCause = loop.TerminalMigrationDetach
				if err := h.save(); err != nil {
					t.Fatal(err)
				}
			case "missing-receipt":
				if err := os.Remove(accepted.ClosureReceipt.Path); err != nil {
					t.Fatal(err)
				}
			}
			// Preflight failures need no listener; a missing Host never falls back to files.
			if variant != "missing-host" && variant != "evidence-drift" && variant != "failed" && variant != "migration" {
				serveTaskAcceptanceOnce(t, h, nil)
			}
			got, err := callHostTaskAcceptanceReadback(t.Context(), req.ProjectDir, path, req.ReadbackOrderID, req.ReadbackSessionID, req.TaskAcceptance.Path, req.TaskAcceptance.SHA256)
			if variant == "detached" || variant == "active" {
				if err != nil || got.Status != "reclaimed" || got.ClosureReceipt == nil || *got.ClosureReceipt != *accepted.ClosureReceipt || len(got.Effects) != 0 {
					t.Fatalf("readback: %+v %v", got, err)
				}
			} else if err == nil || got.Status == "accepted" || got.Status == "reclaimed" {
				t.Fatalf("unbound readback accepted: %+v %v", got, err)
			}
			if rt.calls.Load() != 1 || rt.shutdowns != 0 {
				t.Fatal("readback replayed acceptance or cleanup")
			}
		})
	}
}

func TestHostTaskAcceptanceLostACKThenDetachedReadback(t *testing.T) {
	h, req, rt, path := automaticTaskAcceptanceFixture(t)
	prepareTaskReclamationFixture(t, h, &req)
	listener, err := net.Listen("unix", filepath.Join(h.dir, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var serveErr error
		defer func() { done <- serveErr }()
		for i := 0; i < 2; i++ {
			conn, err := listener.Accept()
			if err != nil {
				serveErr = err
				return
			}
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			var request hostRequest
			if serveErr = json.NewDecoder(conn).Decode(&request); serveErr != nil {
				conn.Close()
				return
			}
			result, handled := h.taskAcceptanceOperation(context.Background(), request)
			if !handled {
				serveErr = errors.New("unexpected operation")
				conn.Close()
				return
			}
			if i == 0 {
				writeTaskReclamationFixture(t, req)
				serveErr = detachAcceptedTaskFixture(h, req)
			} else {
				serveErr = json.NewEncoder(conn).Encode(result)
			}
			conn.Close()
			if serveErr != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		if err := <-done; err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
	got, err := callHostTaskAcceptance(t.Context(), req.ProjectDir, path, req.ReadbackOrderID, req.ReadbackSessionID, req.TaskAcceptance.Path, req.TaskAcceptance.SHA256)
	if err == nil || got.Status != "unknown" || got.ClosureReceipt != nil {
		t.Fatalf("lost ACK claimed accepted: %+v %v", got, err)
	}
	observed, err := callHostTaskAcceptanceReadback(t.Context(), req.ProjectDir, path, req.ReadbackOrderID, req.ReadbackSessionID, req.TaskAcceptance.Path, req.TaskAcceptance.SHA256)
	if err != nil || observed.Status != "reclaimed" || observed.ClosureReceipt == nil || len(observed.Effects) != 0 || rt.calls.Load() != 1 {
		t.Fatalf("original readback replayed or lost retained acceptance: %+v %v", observed, err)
	}
}

func TestHostTaskAcceptanceReadbackCLI(t *testing.T) {
	h, req, rt, path := automaticTaskAcceptanceFixture(t)
	prepareTaskReclamationFixture(t, h, &req)
	if got := h.taskAcceptance(t.Context(), req); got.Status != "accepted" {
		t.Fatalf("prepare acceptance: %+v", got)
	}
	writeTaskReclamationFixture(t, req)
	if err := detachAcceptedTaskFixture(h, req); err != nil {
		t.Fatal(err)
	}
	serveTaskAcceptanceOnce(t, h, nil)
	cmd := newHostCmd(&App{projectDir: req.ProjectDir})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"task", "acceptance-readback", "--request-file", path, "--order-id", req.ReadbackOrderID, "--session-id", req.ReadbackSessionID, "--acceptance", req.TaskAcceptance.Path, "--expected-acceptance-sha256", req.TaskAcceptance.SHA256})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var got hostTaskAcceptance
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "reclaimed" || got.ClosureReceipt == nil || len(got.Effects) != 0 || rt.calls.Load() != 1 {
		t.Fatalf("readback CLI replayed or claimed original ACK: %+v", got)
	}
}
