package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
)

var _ hostFailedTaskClosureOwner = (*loop.Loop)(nil)

type failedTaskClosureHostRuntime struct {
	fakeHostRuntime
	calls atomic.Int32
	close func(context.Context, string, string, string, loopruntime.SessionReadbackFileRef) error
}

func (r *failedTaskClosureHostRuntime) CloseFailedTask(ctx context.Context, order, session, reason string, diagnostics loopruntime.SessionReadbackFileRef) error {
	r.calls.Add(1)
	return r.close(ctx, order, session, reason, diagnostics)
}

func hostFailedTaskClosureFixture(t *testing.T) (*noodleHost, hostRequest, *failedTaskClosureHostRuntime, string) {
	t.Helper()
	h, req, _, requestFile := hostSessionReadbackFixture(t)
	req.Operation = "task_failure_close"
	req.FailureReason = "Explicitly close the failed original attempt"
	diagnostics := filepath.Join(req.ProjectDir, "diagnostics.txt")
	if err := os.WriteFile(diagnostics, []byte("original failure evidence\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var err error
	req.FailureDiagnostics, err = loopruntime.ReadSessionReadbackFile(diagnostics, nil)
	if err != nil {
		t.Fatal(err)
	}
	rt := &failedTaskClosureHostRuntime{}
	rt.close = func(ctx context.Context, order, session, reason string, ref loopruntime.SessionReadbackFileRef) error {
		if ctx.Err() != nil || order != req.ReadbackOrderID || session != req.ReadbackSessionID || reason != req.FailureReason || ref != req.FailureDiagnostics {
			return fmt.Errorf("original closure arguments differ")
		}
		// Use the original Loop's on-disk format, independently of the Host response.
		closure := map[string]any{
			"order":  state.OrderNode{OrderID: order, Status: state.OrderFailed, Stages: []state.StageNode{{Status: state.StageFailed, Runtime: "openshell", Attempts: []state.AttemptNode{{SessionID: session, Status: state.AttemptFailed}}}}},
			"reason": reason, "diagnostics": map[string]loopruntime.SessionReadbackFileRef{"original_flow_diagnostics": ref},
		}
		path := filepath.Join(req.ProjectDir, ".noodle", "execution-closures", hostDigest([]byte(order))+".json")
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return fmt.Errorf("original failure already closed")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		raw, err := json.Marshal(closure)
		if err != nil {
			return err
		}
		return os.WriteFile(path, raw, 0600)
	}
	h.contexts[req.ContextID].runtime = rt
	return h, req, rt, requestFile
}

func TestHostFailedTaskClosureExactBinding(t *testing.T) {
	for _, variant := range []string{"closed", "operation", "instance", "binary", "context", "request", "project", "config", "config-bytes", "request-source", "request-bytes", "environment", "detached", "failed", "blocked", "done", "unsupported", "shutdown", "empty-order", "empty-session", "empty-reason", "long-reason", "relative-diagnostics", "diagnostics-hash", "diagnostics-bytes"} {
		t.Run(variant, func(t *testing.T) {
			h, req, rt, path := hostFailedTaskClosureFixture(t)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			owner := h.contexts[req.ContextID]
			owner.done <- nil
			switch variant {
			case "operation":
				req.Operation = "attach"
			case "instance":
				req.InstanceID = "foreign"
			case "binary":
				req.BinarySHA256 = "foreign"
			case "context":
				req.ContextID = "foreign"
			case "request":
				req.RequestID = "foreign"
			case "project":
				req.ProjectDir = t.TempDir()
			case "config":
				req.ConfigSHA256 = "foreign"
			case "config-bytes":
				closureTestDrift(t, filepath.Join(req.ProjectDir, ".noodle.toml"))
			case "request-source":
				req.ReadbackRequest.SHA256 = strings.Repeat("a", 64)
			case "request-bytes":
				closureTestDrift(t, path)
			case "environment":
				owner.environmentSHA = "foreign"
			case "detached":
				owner.receipt.ContextState = "detached"
			case "failed":
				owner.failed = true
			case "blocked":
				h.blocked = true
			case "done":
				owner.done = nil
			case "unsupported":
				owner.runtime = &fakeHostRuntime{}
			case "shutdown":
				h.shutdown = &hostShutdown{}
			case "empty-order":
				req.ReadbackOrderID = ""
			case "empty-session":
				req.ReadbackSessionID = ""
			case "empty-reason":
				req.FailureReason = " \n"
			case "long-reason":
				req.FailureReason = strings.Repeat("a", 4097)
			case "relative-diagnostics":
				req.FailureDiagnostics.Path = "diagnostics.txt"
			case "diagnostics-hash":
				req.FailureDiagnostics.SHA256 = strings.Repeat("a", 64)
			case "diagnostics-bytes":
				closureTestDrift(t, req.FailureDiagnostics.Path)
			}
			got := h.failedTaskClosure(t.Context(), req)
			if variant == "closed" {
				if got.Status != "closed_failed" || rt.calls.Load() != 1 || got.ClosureReceipt == nil || len(got.Effects) != 1 || got.Effects[0] != "original_failure_closure_persisted" {
					t.Fatalf("closure: %+v", got)
				}
				if err := loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"closure": *got.ClosureReceipt}); err != nil {
					t.Fatal(err)
				}
			} else if got.Status != "refused" || rt.calls.Load() != 0 || len(got.Effects) != 0 || got.ClosureReceipt != nil || got.Error == "" {
				t.Fatalf("invalid binding reached owner: %+v calls=%d", got, rt.calls.Load())
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if variant != "request-bytes" && !bytes.Equal(before, after) || rt.shutdowns != 0 || owner.done != nil && len(owner.done) != 1 || len(h.contexts) != 1 {
				t.Fatal("request rewrite, cleanup, attachment or completion drain")
			}
		})
	}
}

func closureTestDrift(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, ' '), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestHostFailedTaskClosureOwnerUncertainty(t *testing.T) {
	for _, variant := range []string{"owner-error", "persisted-error", "missing-receipt", "source-drift", "diagnostics-drift", "success-receipt", "foreign-order", "foreign-session"} {
		t.Run(variant, func(t *testing.T) {
			h, req, rt, _ := hostFailedTaskClosureFixture(t)
			original := rt.close
			rt.close = func(ctx context.Context, order, session, reason string, ref loopruntime.SessionReadbackFileRef) error {
				if variant == "owner-error" {
					return errors.New("original owner checkpoint failed")
				}
				if variant == "missing-receipt" {
					return nil
				}
				if err := original(ctx, order, session, reason, ref); err != nil {
					return err
				}
				switch variant {
				case "persisted-error":
					return errors.New("checkpoint sync failed after receipt write")
				case "source-drift":
					closureTestDrift(t, filepath.Join(req.ProjectDir, ".noodle.toml"))
				case "diagnostics-drift":
					closureTestDrift(t, ref.Path)
				case "success-receipt":
					path := filepath.Join(req.ProjectDir, ".noodle", "execution-closures", hostDigest([]byte(order))+".json")
					raw, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					return os.WriteFile(path, bytes.ReplaceAll(raw, []byte(`"failed"`), []byte(`"completed"`)), 0600)
				}
				return nil
			}
			if variant == "foreign-order" {
				req.ReadbackOrderID = "foreign"
			}
			if variant == "foreign-session" {
				req.ReadbackSessionID = "foreign"
			}
			got := h.failedTaskClosure(t.Context(), req)
			if got.Status != "unknown" || got.Error == "" || rt.calls.Load() != 1 || len(got.Effects) != 1 || got.Effects[0] != "original_owner_call_attempted" {
				t.Fatalf("mutation uncertainty lost: %+v", got)
			}
		})
	}
}

func serveFailedTaskClosureOnce(t *testing.T, h *noodleHost, alter func(*hostRequest, *hostFailedTaskClosure) bool) {
	t.Helper()
	listener, err := net.Listen("unix", filepath.Join(h.dir, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		var req hostRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			done <- err
			return
		}
		result := h.failedTaskClosure(context.Background(), req)
		if alter != nil && !alter(&req, &result) {
			done <- nil
			return
		}
		done <- json.NewEncoder(conn).Encode(result)
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		if err := <-done; err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
}

func TestHostFailedTaskClosureClientSourcesAndUnknownResponse(t *testing.T) {
	for _, variant := range []string{"closed", "missing-host", "lost-ack", "instance", "context", "request", "order", "session", "reason", "status", "effects", "nil-sources", "source-hash", "receipt-hash", "receipt-path", "receipt-drift", "receipts-drift", "request-drift", "diagnostics-drift", "refused", "owner-error"} {
		t.Run(variant, func(t *testing.T) {
			h, req, rt, path := hostFailedTaskClosureFixture(t)
			if variant == "refused" {
				h.blocked = true
			}
			if variant == "owner-error" {
				rt.close = func(context.Context, string, string, string, loopruntime.SessionReadbackFileRef) error {
					return errors.New("checkpoint failed")
				}
			}
			if variant != "missing-host" {
				serveFailedTaskClosureOnce(t, h, func(_ *hostRequest, got *hostFailedTaskClosure) bool {
					switch variant {
					case "lost-ack":
						return false
					case "instance":
						got.HostInstanceID = "foreign"
					case "context":
						got.ContextID = "foreign"
					case "request":
						got.RequestID = "foreign"
					case "order":
						got.OrderID = "foreign"
					case "session":
						got.SessionID = "foreign"
					case "reason":
						got.Reason = "foreign"
					case "status":
						got.Status = "success"
					case "effects":
						got.Effects = []string{}
					case "nil-sources":
						got.Sources = nil
					case "source-hash":
						got.Sources["config"] = got.Sources["diagnostics"]
					case "receipt-hash":
						got.ClosureReceipt.SHA256 = strings.Repeat("a", 64)
					case "receipt-path":
						got.ClosureReceipt = &req.FailureDiagnostics
					case "receipt-drift":
						closureTestDrift(t, got.ClosureReceipt.Path)
					case "receipts-drift":
						closureTestDrift(t, filepath.Join(h.dir, "receipts.json"))
					case "request-drift":
						closureTestDrift(t, path)
					case "diagnostics-drift":
						closureTestDrift(t, req.FailureDiagnostics.Path)
					}
					return true
				})
			}
			got, err := callHostFailedTaskClosure(t.Context(), req.ProjectDir, path, req.ReadbackOrderID, req.ReadbackSessionID, req.FailureReason, req.FailureDiagnostics.Path, req.FailureDiagnostics.SHA256)
			switch variant {
			case "closed":
				if err != nil || got.Status != "closed_failed" || got.ClosureReceipt == nil || rt.calls.Load() != 1 || got.Sources["host_receipts"].SHA256 == "" || got.Sources["client_binary"].SHA256 == "" {
					t.Fatalf("closure: %+v %v", got, err)
				}
			case "missing-host", "refused":
				if err == nil || rt.calls.Load() != 0 || got.Status == "closed_failed" || len(got.Effects) != 0 {
					t.Fatalf("refusal: %+v %v", got, err)
				}
			default:
				if err == nil || got.Status != "unknown" || rt.calls.Load() != 1 || len(got.Effects) == 0 {
					t.Fatalf("unknown response: %+v %v", got, err)
				}
			}
		})
	}
}

func TestHostFailedTaskClosureCLI(t *testing.T) {
	h, req, rt, path := hostFailedTaskClosureFixture(t)
	serveFailedTaskClosureOnce(t, h, nil)
	cmd := newHostCmd(&App{projectDir: req.ProjectDir})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"task", "close-failure", "--request-file", path, "--order-id", req.ReadbackOrderID, "--session-id", req.ReadbackSessionID, "--reason", req.FailureReason, "--diagnostics", req.FailureDiagnostics.Path, "--expected-diagnostics-sha256", req.FailureDiagnostics.SHA256})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var got hostFailedTaskClosure
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed_failed" || got.ClosureReceipt == nil || rt.calls.Load() != 1 || rt.shutdowns != 0 {
		t.Fatalf("CLI: %+v", got)
	}
}

func TestHostFailedTaskClosureCLIUnknownResponse(t *testing.T) {
	h, req, rt, path := hostFailedTaskClosureFixture(t)
	serveFailedTaskClosureOnce(t, h, func(_ *hostRequest, _ *hostFailedTaskClosure) bool { return false })
	cmd := newHostCmd(&App{projectDir: req.ProjectDir})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"task", "close-failure", "--request-file", path, "--order-id", req.ReadbackOrderID, "--session-id", req.ReadbackSessionID, "--reason", req.FailureReason, "--diagnostics", req.FailureDiagnostics.Path, "--expected-diagnostics-sha256", req.FailureDiagnostics.SHA256})
	if err := cmd.Execute(); err == nil {
		t.Fatal("lost acknowledgement reported success")
	}
	var got hostFailedTaskClosure
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "unknown" || len(got.Effects) != 1 || got.ClosureReceipt != nil || rt.calls.Load() != 1 {
		t.Fatalf("CLI uncertainty: %+v", got)
	}
}

func TestHostFailedTaskClosureFailedOwnerPreservesHistory(t *testing.T) {
	h, req, rt, requestFile := hostFailedTaskClosureFixture(t)
	receiptsRef, err := loopruntime.ReadSessionReadbackFile(filepath.Join(h.dir, "receipts.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	retained := make(chan loopruntime.SessionReadbackFileRef, 1)
	original := rt.close
	rt.close = func(ctx context.Context, order, session, reason string, diagnostics loopruntime.SessionReadbackFileRef) error {
		if err := original(ctx, order, session, reason, diagnostics); err != nil {
			return err
		}
		path := filepath.Join(req.ProjectDir, ".noodle", "execution-closures", hostDigest([]byte(order))+".json")
		ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
		if err != nil {
			return err
		}
		retained <- ref
		return errors.New("original Loop checkpoint failed after closure persistence")
	}
	serveFailedTaskClosureOnce(t, h, nil)
	got, err := callHostFailedTaskClosure(t.Context(), req.ProjectDir, requestFile, req.ReadbackOrderID, req.ReadbackSessionID, req.FailureReason, req.FailureDiagnostics.Path, req.FailureDiagnostics.SHA256)
	if err == nil || got.Status != "unknown" || got.ClosureReceipt != nil || rt.calls.Load() != 1 || rt.shutdowns != 0 || len(got.Effects) != 1 || got.Effects[0] != "original_owner_call_attempted" {
		t.Fatalf("failed owner reported closure or retried: %+v %v", got, err)
	}
	var retainedClosure loopruntime.SessionReadbackFileRef
	select {
	case retainedClosure = <-retained:
	default:
		t.Fatal("original Loop did not retain failure closure")
	}
	if err := loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{
		"original_request":        req.ReadbackRequest,
		"host_receipts":           receiptsRef,
		"diagnostics":             req.FailureDiagnostics,
		"retained_failed_closure": retainedClosure,
	}); err != nil {
		t.Fatal("original failure history changed:", err)
	}
	var closure struct {
		Order state.OrderNode `json:"order"`
	}
	if _, err := loopruntime.ReadSessionReadbackFile(retainedClosure.Path, &closure); err != nil {
		t.Fatal(err)
	}
	if closure.Order.Status != state.OrderFailed || closure.Order.Stages[0].Attempts[0].Status != state.AttemptFailed {
		t.Fatalf("original failed outcome changed: %+v", closure.Order)
	}
}

func TestHostFailedTaskClosureAbsoluteDiagnosticsDirectoryAlias(t *testing.T) {
	h, req, rt, path := hostFailedTaskClosureFixture(t)
	alias := filepath.Join(t.TempDir(), "original")
	if err := os.Symlink(req.ProjectDir, alias); err != nil {
		t.Fatal(err)
	}
	serveFailedTaskClosureOnce(t, h, nil)
	got, err := callHostFailedTaskClosure(t.Context(), req.ProjectDir, path, req.ReadbackOrderID, req.ReadbackSessionID, req.FailureReason, filepath.Join(alias, "diagnostics.txt"), req.FailureDiagnostics.SHA256)
	if err != nil || got.Status != "closed_failed" || rt.calls.Load() != 1 || got.Sources["diagnostics"] != req.FailureDiagnostics {
		t.Fatalf("absolute path alias: %+v %v", got, err)
	}
}

func TestHostFailedTaskClosureReceiptContent(t *testing.T) {
	for _, variant := range []string{"order", "session", "reason", "diagnostics", "attempt-success", "stage-success", "merge", "cancelled-attempt"} {
		t.Run(variant, func(t *testing.T) {
			h, req, rt, _ := hostFailedTaskClosureFixture(t)
			original := rt.close
			rt.close = func(ctx context.Context, order, session, reason string, ref loopruntime.SessionReadbackFileRef) error {
				if err := original(ctx, order, session, reason, ref); err != nil {
					return err
				}
				path := filepath.Join(req.ProjectDir, ".noodle", "execution-closures", hostDigest([]byte(order))+".json")
				var closure struct {
					Order       state.OrderNode                               `json:"order"`
					Reason      string                                        `json:"reason"`
					Diagnostics map[string]loopruntime.SessionReadbackFileRef `json:"diagnostics"`
				}
				if _, err := loopruntime.ReadSessionReadbackFile(path, &closure); err != nil {
					return err
				}
				switch variant {
				case "order":
					closure.Order.OrderID = "foreign"
				case "session":
					closure.Order.Stages[0].Attempts[0].SessionID = "foreign"
				case "reason":
					closure.Reason = "foreign"
				case "diagnostics":
					closure.Diagnostics["original_flow_diagnostics"] = req.ReadbackRequest
				case "attempt-success":
					closure.Order.Stages[0].Attempts[0].Status = state.AttemptCompleted
				case "stage-success":
					closure.Order.Stages[0].Status = state.StageCompleted
				case "merge":
					closure.Order.Stages[0].Merge = &state.MergeRecoveryNode{WorktreeName: "unresolved"}
				case "cancelled-attempt":
					closure.Order.Stages[0].Attempts[0].Status = state.AttemptCancelled
				}
				raw, err := json.Marshal(closure)
				if err != nil {
					return err
				}
				return os.WriteFile(path, raw, 0600)
			}
			got := h.failedTaskClosure(t.Context(), req)
			if variant == "cancelled-attempt" {
				if got.Status != "closed_failed" || got.ClosureReceipt == nil {
					t.Fatalf("cancelled attempt failure closure: %+v", got)
				}
			} else if got.Status != "unknown" || got.ClosureReceipt != nil || len(got.Effects) != 1 {
				t.Fatalf("receipt substitution accepted: %+v", got)
			}
		})
	}
}

func TestHostFailedTaskClosureHostActorRoute(t *testing.T) {
	// Exercise the real Host socket/actor without attaching any context or runtime.
	h, req, _, _ := hostFailedTaskClosureFixture(t)
	t.Setenv("NOODLE_HOST_DIR", h.dir)
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serveHostWithSampler(ctx, hostShutdownTestSampler{}) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("isolated Host did not stop")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", filepath.Join(h.dir, "host.sock"), 50*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("isolated Host socket unavailable", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	var got hostFailedTaskClosure
	hostSocketTestCall(t, h.dir, req, &got)
	if got.SchemaVersion != 1 || got.Status != "refused" || got.OrderID != req.ReadbackOrderID || got.SessionID != req.ReadbackSessionID || got.Reason != req.FailureReason || got.Error == "" || len(got.Effects) != 0 {
		t.Fatalf("actor route did not return the bound no-effect refusal: %+v", got)
	}
}

func TestHostFailedTaskClosureClientInputRefusedBeforeExchange(t *testing.T) {
	for _, variant := range []string{"relative", "hash", "uppercase-hash", "empty-reason", "missing-file", "binary-binding", "ambiguous-receipt"} {
		t.Run(variant, func(t *testing.T) {
			h, req, rt, path := hostFailedTaskClosureFixture(t)
			switch variant {
			case "relative":
				req.FailureDiagnostics.Path = "diagnostics.txt"
			case "hash":
				req.FailureDiagnostics.SHA256 = strings.Repeat("a", 64)
			case "uppercase-hash":
				req.FailureDiagnostics.SHA256 = strings.ToUpper(req.FailureDiagnostics.SHA256)
			case "empty-reason":
				req.FailureReason = ""
			case "missing-file":
				req.FailureDiagnostics.Path += ".missing"
			case "binary-binding":
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, bytes.ReplaceAll(raw, []byte(req.BinarySHA256), []byte(strings.Repeat("a", 64))), 0600); err != nil {
					t.Fatal(err)
				}
			case "ambiguous-receipt":
				h.receipts = append(h.receipts, h.receipts[0])
				if err := h.save(); err != nil {
					t.Fatal(err)
				}
			}
			got, err := callHostFailedTaskClosure(t.Context(), req.ProjectDir, path, req.ReadbackOrderID, req.ReadbackSessionID, req.FailureReason, req.FailureDiagnostics.Path, req.FailureDiagnostics.SHA256)
			if err == nil || strings.Contains(err.Error(), "original Host is unavailable") || got.Status == "closed_failed" || rt.calls.Load() != 0 {
				t.Fatalf("input reached exchange: %+v %v", got, err)
			}
		})
	}
}
