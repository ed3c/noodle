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

var _ hostTaskAcceptanceOwner = (*loop.Loop)(nil)

type taskAcceptanceHostRuntime struct {
	fakeHostRuntime
	calls  atomic.Int32
	accept func(context.Context, string, string, loopruntime.SessionReadbackFileRef) error
}

func (r *taskAcceptanceHostRuntime) AcceptCompletedTask(ctx context.Context, order, session string, acceptance loopruntime.SessionReadbackFileRef) error {
	r.calls.Add(1)
	return r.accept(ctx, order, session, acceptance)
}

func hostTaskAcceptanceFixture(t *testing.T) (*noodleHost, hostRequest, *taskAcceptanceHostRuntime, string, string) {
	t.Helper()
	h, req, _, requestFile := hostSessionReadbackFixture(t)
	req.Operation = "task_delivery_accept"
	evidencePath := filepath.Join(req.ProjectDir, "integrated-evidence.txt")
	if err := os.WriteFile(evidencePath, []byte("outer integrated acceptance passed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	evidenceRef, err := loopruntime.ReadSessionReadbackFile(evidencePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	acceptance := map[string]any{
		"schema": 1, "order_id": req.ReadbackOrderID, "session_id": req.ReadbackSessionID,
		"candidate_head": strings.Repeat("a", 40), "status": "PASS",
		"conditions": []any{map[string]any{"condition": "integrated delivery", "status": "PASS", "evidence": []loopruntime.SessionReadbackFileRef{evidenceRef}}},
	}
	acceptancePath := filepath.Join(req.ProjectDir, "acceptance.json")
	raw, err := json.Marshal(acceptance)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(acceptancePath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	req.TaskAcceptance, err = loopruntime.ReadSessionReadbackFile(acceptancePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	rt := &taskAcceptanceHostRuntime{}
	rt.accept = func(ctx context.Context, order, session string, ref loopruntime.SessionReadbackFileRef) error {
		currentRef, sourceErr := loopruntime.ReadSessionReadbackFile(req.TaskAcceptance.Path, nil)
		if ctx.Err() != nil || sourceErr != nil || order != req.ReadbackOrderID || session != req.ReadbackSessionID || ref != currentRef {
			return fmt.Errorf("original acceptance arguments differ")
		}
		closure := map[string]any{
			"order":  state.OrderNode{OrderID: order, Status: state.OrderCompleted, Stages: []state.StageNode{{Status: state.StageCompleted, Runtime: "openshell", Attempts: []state.AttemptNode{{SessionID: session, Status: state.AttemptCompleted, WorktreeName: "worker"}}}}},
			"reason": "original task owner accepted completed delivery", "diagnostics": map[string]loopruntime.SessionReadbackFileRef{"owner_acceptance": ref},
		}
		path := filepath.Join(req.ProjectDir, ".noodle", "execution-closures", hostDigest([]byte(order))+".json")
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return fmt.Errorf("original delivery already accepted")
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
	return h, req, rt, requestFile, evidencePath
}

func rewriteHostAcceptance(t *testing.T, req *hostRequest, alter func(map[string]any)) {
	t.Helper()
	var value map[string]any
	if _, err := loopruntime.ReadSessionReadbackFile(req.TaskAcceptance.Path, &value); err != nil {
		t.Fatal(err)
	}
	alter(value)
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(req.TaskAcceptance.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	req.TaskAcceptance, err = loopruntime.ReadSessionReadbackFile(req.TaskAcceptance.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestHostTaskAcceptanceExactBindingAndEvidence(t *testing.T) {
	for _, variant := range []string{"accepted", "operation", "instance", "binary", "context", "request", "config", "request-source", "environment", "detached", "blocked", "unsupported", "shutdown", "acceptance-hash", "acceptance-drift", "evidence-drift", "schema", "order", "session", "candidate", "status", "no-conditions", "condition-status", "no-evidence"} {
		t.Run(variant, func(t *testing.T) {
			h, req, rt, path, evidence := hostTaskAcceptanceFixture(t)
			requestRef := req.ReadbackRequest
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
			case "config":
				req.ConfigSHA256 = "foreign"
			case "request-source":
				req.ReadbackRequest.SHA256 = strings.Repeat("a", 64)
			case "environment":
				owner.environmentSHA = "foreign"
			case "detached":
				owner.receipt.ContextState = "detached"
			case "blocked":
				h.blocked = true
			case "unsupported":
				owner.runtime = &fakeHostRuntime{}
			case "shutdown":
				h.shutdown = &hostShutdown{}
			case "acceptance-hash":
				req.TaskAcceptance.SHA256 = strings.Repeat("b", 64)
			case "acceptance-drift":
				closureTestDrift(t, req.TaskAcceptance.Path)
			case "evidence-drift":
				closureTestDrift(t, evidence)
			default:
				if variant != "accepted" {
					rewriteHostAcceptance(t, &req, func(v map[string]any) {
						switch variant {
						case "schema":
							v["schema"] = 2
						case "order":
							v["order_id"] = "foreign"
						case "session":
							v["session_id"] = "foreign"
						case "candidate":
							v["candidate_head"] = ""
						case "status":
							v["status"] = "FAIL"
						case "no-conditions":
							v["conditions"] = []any{}
						case "condition-status":
							v["conditions"].([]any)[0].(map[string]any)["status"] = "FAIL"
						case "no-evidence":
							v["conditions"].([]any)[0].(map[string]any)["evidence"] = []any{}
						}
					})
				}
			}
			got := h.taskAcceptance(t.Context(), req)
			if variant == "accepted" {
				if got.Status != "accepted" || got.ClosureReceipt == nil || rt.calls.Load() != 1 || len(got.Effects) != 1 || got.Effects[0] != "original_delivery_acceptance_persisted" {
					t.Fatalf("acceptance: %+v", got)
				}
			} else if got.Status != "refused" || got.Error == "" || rt.calls.Load() != 0 || got.ClosureReceipt != nil || len(got.Effects) != 0 {
				t.Fatalf("invalid acceptance reached owner: %+v calls=%d", got, rt.calls.Load())
			}
			if err := loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"original_request": requestRef}); err != nil {
				t.Fatal(err)
			}
			if path != requestRef.Path || rt.shutdowns != 0 || len(owner.done) != 1 || len(h.contexts) != 1 {
				t.Fatal("acceptance attached, cleaned up or drained completion")
			}
		})
	}
}

func serveTaskAcceptanceOnce(t *testing.T, h *noodleHost, alter func(*hostTaskAcceptance) bool) {
	t.Helper()
	listener, err := net.Listen("unix", filepath.Join(h.dir, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var serveErr error
		defer func() { done <- serveErr }()
		conn, err := listener.Accept()
		if err != nil {
			serveErr = err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		var req hostRequest
		if serveErr = json.NewDecoder(conn).Decode(&req); serveErr != nil {
			return
		}
		result, _ := h.taskAcceptanceOperation(context.Background(), req)
		if alter != nil && !alter(&result) {
			return
		}
		serveErr = json.NewEncoder(conn).Encode(result)
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		if err := <-done; err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
}

func TestHostTaskAcceptanceClientUnknownResponse(t *testing.T) {
	for _, variant := range []string{"accepted", "missing-host", "lost-ack", "instance", "context", "request", "order", "session", "status", "effects", "source-hash", "receipt-hash", "receipt-path", "receipts-drift", "completed-detach", "failed-detach", "migration-detach", "receipt-owner", "unrelated-receipt", "request-drift", "acceptance-drift", "evidence-drift", "refused", "owner-error"} {
		t.Run(variant, func(t *testing.T) {
			h, req, rt, path, evidence := hostTaskAcceptanceFixture(t)
			if variant == "refused" {
				h.blocked = true
			}
			if variant == "owner-error" {
				rt.accept = func(context.Context, string, string, loopruntime.SessionReadbackFileRef) error {
					return errors.New("original candidate differs")
				}
			}
			if variant != "missing-host" {
				serveTaskAcceptanceOnce(t, h, func(got *hostTaskAcceptance) bool {
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
					case "status":
						got.Status = "completed"
					case "effects":
						got.Effects = []string{}
					case "source-hash":
						got.Sources["acceptance"] = req.ReadbackRequest
					case "receipt-hash":
						got.ClosureReceipt.SHA256 = strings.Repeat("b", 64)
					case "receipt-path":
						got.ClosureReceipt = &req.TaskAcceptance
					case "receipts-drift":
						closureTestDrift(t, filepath.Join(h.dir, "receipts.json"))
					case "completed-detach", "failed-detach", "migration-detach", "receipt-owner", "unrelated-receipt":
						switch variant {
						case "completed-detach":
							h.receipts[0].ContextState = "detached"
							h.receipts[0].TerminalCause = loop.TerminalExplicitCleanup
							delete(h.contexts, req.ContextID)
						case "failed-detach":
							h.receipts[0].ContextState = "failed"
						case "migration-detach":
							h.receipts[0].ContextState = "detached"
							h.receipts[0].TerminalCause = loop.TerminalMigrationDetach
						case "receipt-owner":
							h.receipts[0].HostInstanceID = "replacement"
						case "unrelated-receipt":
							h.receipts = append(h.receipts, hostReceipt{RequestID: "unrelated"})
						}
						if err := h.save(); err != nil {
							t.Error(err)
							return false
						}
					case "request-drift":
						closureTestDrift(t, path)
					case "acceptance-drift":
						closureTestDrift(t, req.TaskAcceptance.Path)
					case "evidence-drift":
						closureTestDrift(t, evidence)
					}
					return true
				})
			}
			got, err := callHostTaskAcceptance(t.Context(), req.ProjectDir, path, req.ReadbackOrderID, req.ReadbackSessionID, req.TaskAcceptance.Path, req.TaskAcceptance.SHA256)
			switch variant {
			case "accepted", "receipts-drift", "completed-detach", "unrelated-receipt":
				if err != nil || got.Status != "accepted" || got.ClosureReceipt == nil || rt.calls.Load() != 1 || got.Sources["client_binary"].SHA256 == "" {
					t.Fatalf("accepted: %+v %v", got, err)
				}
			case "missing-host", "refused":
				if err == nil || rt.calls.Load() != 0 || len(got.Effects) != 0 {
					t.Fatalf("refused: %+v %v", got, err)
				}
			default:
				if err == nil || got.Status != "unknown" || rt.calls.Load() != 1 || len(got.Effects) == 0 {
					t.Fatalf("uncertainty lost: %+v %v", got, err)
				}
			}
		})
	}
}

func TestHostTaskAcceptanceOwnerUncertaintyAndRetainedHistory(t *testing.T) {
	for _, variant := range []string{"persisted-error", "missing-receipt", "config-drift", "evidence-drift", "wrong-reason", "wrong-diagnostics", "failed-order", "failed-attempt", "foreign-session"} {
		t.Run(variant, func(t *testing.T) {
			h, req, rt, _, evidence := hostTaskAcceptanceFixture(t)
			retained := make(chan loopruntime.SessionReadbackFileRef, 1)
			original := rt.accept
			rt.accept = func(ctx context.Context, order, session string, ref loopruntime.SessionReadbackFileRef) error {
				if variant == "missing-receipt" {
					return nil
				}
				if err := original(ctx, order, session, ref); err != nil {
					return err
				}
				path := filepath.Join(req.ProjectDir, ".noodle", "execution-closures", hostDigest([]byte(order))+".json")
				var closure struct {
					Order       state.OrderNode                               `json:"order"`
					Reason      string                                        `json:"reason"`
					Diagnostics map[string]loopruntime.SessionReadbackFileRef `json:"diagnostics"`
				}
				closureRef, err := loopruntime.ReadSessionReadbackFile(path, &closure)
				if err != nil {
					return err
				}
				switch variant {
				case "persisted-error":
					retained <- closureRef
					return errors.New("original checkpoint failed after receipt persistence")
				case "config-drift":
					closureTestDrift(t, filepath.Join(req.ProjectDir, ".noodle.toml"))
					return nil
				case "evidence-drift":
					closureTestDrift(t, evidence)
					return nil
				case "wrong-reason":
					closure.Reason = "Loop completed without outer acceptance"
				case "wrong-diagnostics":
					closure.Diagnostics = map[string]loopruntime.SessionReadbackFileRef{}
				case "failed-order":
					closure.Order.Status = state.OrderFailed
				case "failed-attempt":
					closure.Order.Stages[0].Attempts[0].Status = state.AttemptFailed
				case "foreign-session":
					closure.Order.Stages[0].Attempts[0].SessionID = "foreign"
				}
				raw, err := json.Marshal(closure)
				if err != nil {
					return err
				}
				return os.WriteFile(path, raw, 0600)
			}
			got := h.taskAcceptance(t.Context(), req)
			if got.Status != "unknown" || got.Error == "" || rt.calls.Load() != 1 || rt.shutdowns != 0 || len(got.Effects) != 1 || got.Effects[0] != "original_owner_call_attempted" {
				t.Fatalf("owner uncertainty: %+v", got)
			}
			if variant == "persisted-error" {
				select {
				case ref := <-retained:
					if err := loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"closure": ref, "request": req.ReadbackRequest, "acceptance": req.TaskAcceptance}); err != nil {
						t.Fatal("history changed:", err)
					}
				default:
					t.Fatal("original receipt was not retained")
				}
			}
		})
	}
}

func TestHostTaskAcceptanceCLI(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint("lost-ack=", lost), func(t *testing.T) {
			h, req, rt, path, _ := hostTaskAcceptanceFixture(t)
			serveTaskAcceptanceOnce(t, h, func(*hostTaskAcceptance) bool { return !lost })
			cmd := newHostCmd(&App{projectDir: req.ProjectDir})
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetArgs([]string{"task", "accept-delivery", "--request-file", path, "--order-id", req.ReadbackOrderID, "--session-id", req.ReadbackSessionID, "--acceptance", req.TaskAcceptance.Path, "--expected-acceptance-sha256", req.TaskAcceptance.SHA256})
			err := cmd.Execute()
			var got hostTaskAcceptance
			if decodeErr := json.Unmarshal(output.Bytes(), &got); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if lost {
				if err == nil || got.Status != "unknown" || got.ClosureReceipt != nil {
					t.Fatalf("lost acknowledgement: %+v %v", got, err)
				}
			} else if err != nil || got.Status != "accepted" || got.ClosureReceipt == nil {
				t.Fatalf("accept-delivery: %+v %v", got, err)
			}
			if rt.calls.Load() != 1 || rt.shutdowns != 0 {
				t.Fatal("accept-delivery repeated owner call or cleaned up")
			}
		})
	}
}

func TestHostTaskAcceptanceClientInputRefusedBeforeExchange(t *testing.T) {
	for _, variant := range []string{"relative", "hash", "empty-order", "empty-session", "evidence-drift", "binary-binding", "ambiguous-receipt"} {
		t.Run(variant, func(t *testing.T) {
			h, req, rt, path, evidence := hostTaskAcceptanceFixture(t)
			switch variant {
			case "relative":
				req.TaskAcceptance.Path = "acceptance.json"
			case "hash":
				req.TaskAcceptance.SHA256 = strings.Repeat("b", 64)
			case "empty-order":
				req.ReadbackOrderID = ""
			case "empty-session":
				req.ReadbackSessionID = ""
			case "evidence-drift":
				closureTestDrift(t, evidence)
			case "binary-binding":
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, bytes.ReplaceAll(raw, []byte(req.BinarySHA256), []byte(strings.Repeat("b", 64))), 0600); err != nil {
					t.Fatal(err)
				}
			case "ambiguous-receipt":
				h.receipts = append(h.receipts, h.receipts[0])
				if err := h.save(); err != nil {
					t.Fatal(err)
				}
			}
			got, err := callHostTaskAcceptance(t.Context(), req.ProjectDir, path, req.ReadbackOrderID, req.ReadbackSessionID, req.TaskAcceptance.Path, req.TaskAcceptance.SHA256)
			if err == nil || strings.Contains(err.Error(), "original Host is unavailable") || rt.calls.Load() != 0 || got.Status == "accepted" {
				t.Fatalf("invalid input reached exchange: %+v %v", got, err)
			}
		})
	}
}

func TestHostTaskAcceptanceHostActorRoute(t *testing.T) {
	h, req, _, _, _ := hostTaskAcceptanceFixture(t)
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
	var got hostTaskAcceptance
	hostSocketTestCall(t, h.dir, req, &got)
	if got.SchemaVersion != 1 || got.Status != "refused" || got.OrderID != req.ReadbackOrderID || got.SessionID != req.ReadbackSessionID || got.Error == "" || len(got.Effects) != 0 {
		t.Fatalf("actor route did not return bound no-effect refusal: %+v", got)
	}
}
