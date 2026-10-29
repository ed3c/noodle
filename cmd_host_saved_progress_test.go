package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/poteto/noodle/loop"
)

type savedProgressHostRuntime struct {
	fakeHostRuntime
	inspections, queues atomic.Int32
	inspect             func() (loop.SavedProgressInspection, error)
	queue               func(string) (loop.SavedProgressQueued, error)
}

func (r *savedProgressHostRuntime) InspectSavedProgress(_ context.Context, _, _ string) (loop.SavedProgressInspection, error) {
	r.inspections.Add(1)
	return r.inspect()
}
func (r *savedProgressHostRuntime) QueueSavedProgressAttempt(_ context.Context, _, _, version string) (loop.SavedProgressQueued, error) {
	r.queues.Add(1)
	return r.queue(version)
}

func hostSavedProgressFixture(t *testing.T) (*noodleHost, hostRequest, *savedProgressHostRuntime, string) {
	t.Helper()
	h, req, _, path := hostSessionReadbackFixture(t)
	req.Operation = "task_interruption_inspect"
	req.ExpectedVersion = strings.Repeat("a", 64)
	rt := &savedProgressHostRuntime{}
	rt.inspect = func() (loop.SavedProgressInspection, error) {
		return loop.SavedProgressInspection{SchemaVersion: 1, Status: "retryable", Version: req.ExpectedVersion, OrderID: req.ReadbackOrderID, SessionID: req.ReadbackSessionID, AttemptID: "cancelled-original", AttemptStatus: "cancelled", Runtime: "openshell", Effects: []string{}}, nil
	}
	rt.queue = func(v string) (loop.SavedProgressQueued, error) {
		if v != req.ExpectedVersion {
			return loop.SavedProgressQueued{}, errors.New("saved progress consent version changed")
		}
		return loop.SavedProgressQueued{Status: "queued", CommandID: "saved-progress-" + v, Version: v, Effects: []string{"original_control_queued"}}, nil
	}
	h.contexts[req.ContextID].runtime = rt
	return h, req, rt, path
}

func TestHostSavedProgressOriginalOwnerAndConsent(t *testing.T) {
	for _, variant := range []string{"inspect", "queue", "stale", "invalid-version", "instance", "binary", "context", "request", "config", "request-source", "environment", "detached", "failed", "blocked", "done", "unavailable"} {
		t.Run(variant, func(t *testing.T) {
			h, req, rt, path := hostSavedProgressFixture(t)
			before, _ := os.ReadFile(path)
			owner := h.contexts[req.ContextID]
			owner.done <- nil
			switch variant {
			case "queue":
				req.Operation = "task_new_attempt"
			case "stale":
				req.Operation = "task_new_attempt"
				req.ExpectedVersion = strings.Repeat("b", 64)
			case "invalid-version":
				req.Operation = "task_new_attempt"
				req.ExpectedVersion = ""
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
				req.ReadbackRequest.SHA256 = "foreign"
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
			case "unavailable":
				owner.runtime = &fakeHostRuntime{}
			}
			got := h.savedProgress(t.Context(), req)
			if variant == "inspect" {
				if got.Status != "observed" || rt.inspections.Load() != 1 || got.Inspection.NativeResumeSupported {
					t.Fatalf("inspection: %+v", got)
				}
			} else if variant == "queue" {
				if got.Status != "queued" || rt.queues.Load() != 1 || got.Queued.Status != "queued" || len(got.Effects) != 1 {
					t.Fatalf("queue: %+v", got)
				}
			} else {
				if got.Status != "refused" || len(got.Effects) != 0 {
					t.Fatalf("invalid owner accepted: %+v", got)
				}
				if variant != "stale" && (rt.inspections.Load() != 0 || rt.queues.Load() != 0) {
					t.Fatal("invalid owner reached Loop")
				}
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) || rt.shutdowns != 0 || (owner.done != nil && len(owner.done) != 1) {
				t.Fatal("request rewrite, cleanup or completion drain")
			}
		})
	}
}

func TestHostSavedProgressRetainsMutationUncertainty(t *testing.T) {
	for _, variant := range []string{"source-drift", "append-unknown", "inspect-drift", "shutdown"} {
		t.Run(variant, func(t *testing.T) {
			h, req, rt, _ := hostSavedProgressFixture(t)
			req.Operation = "task_new_attempt"
			drift := func() {
				p := filepath.Join(req.ProjectDir, ".noodle.toml")
				raw, _ := os.ReadFile(p)
				_ = os.WriteFile(p, append(raw, ' '), 0600)
			}
			switch variant {
			case "source-drift":
				old := rt.queue
				rt.queue = func(v string) (loop.SavedProgressQueued, error) { r, e := old(v); drift(); return r, e }
			case "append-unknown":
				rt.queue = func(v string) (loop.SavedProgressQueued, error) {
					return loop.SavedProgressQueued{Status: "unknown", CommandID: "saved-progress-" + v, Version: v, Effects: []string{"control_append_attempted"}}, errors.New("sync failed")
				}
			case "inspect-drift":
				req.Operation = "task_interruption_inspect"
				old := rt.inspect
				rt.inspect = func() (loop.SavedProgressInspection, error) { r, e := old(); drift(); return r, e }
			case "shutdown":
				reply := make(chan any, 1)
				h.shutdown = &hostShutdown{}
				if !h.handleShutdownCall(req, reply) {
					t.Fatal("shutdown operation escaped")
				}
				got := (<-reply).(hostSavedProgress)
				if got.Status != "refused" || rt.queues.Load() != 0 || len(got.Effects) != 0 {
					t.Fatalf("shutdown: %+v", got)
				}
				return
			}
			got := h.savedProgress(t.Context(), req)
			if variant == "inspect-drift" {
				if got.Status != "refused" || len(got.Effects) != 0 {
					t.Fatalf("read drift: %+v", got)
				}
			} else if got.Status != "unknown" || got.Queued == nil || got.Queued.CommandID == "" || len(got.Effects) != 1 {
				t.Fatalf("mutation evidence lost: %+v", got)
			}
		})
	}
}

func serveSavedProgressOnce(t *testing.T, h *noodleHost, variant string) {
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
		if err = json.NewDecoder(conn).Decode(&req); err != nil {
			done <- err
			return
		}
		result := h.savedProgress(context.Background(), req)
		switch variant {
		case "lost-ack":
			done <- nil
			return
		case "foreign":
			result.HostInstanceID = "foreign"
		case "receipts-drift":
			p := filepath.Join(h.dir, "receipts.json")
			raw, _ := os.ReadFile(p)
			err = os.WriteFile(p, append(raw, ' '), 0600)
			if err != nil {
				done <- err
				return
			}
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

func TestHostSavedProgressClientNoLaunchAndUnknownEffects(t *testing.T) {
	for _, variant := range []string{"missing", "inspect", "queue", "lost-ack", "foreign", "receipts-drift"} {
		t.Run(variant, func(t *testing.T) {
			h, req, rt, path := hostSavedProgressFixture(t)
			before, _ := os.ReadFile(path)
			if variant != "missing" {
				serveSavedProgressOnce(t, h, variant)
			}
			queue := variant != "inspect"
			got, err := callHostSavedProgress(t.Context(), req.ProjectDir, path, req.ReadbackOrderID, req.ReadbackSessionID, req.ExpectedVersion, queue)
			switch variant {
			case "inspect":
				if err != nil || got.Status != "observed" || rt.inspections.Load() != 1 {
					t.Fatalf("inspect: %+v %v", got, err)
				}
			case "queue":
				if err != nil || got.Status != "queued" || rt.queues.Load() != 1 {
					t.Fatalf("queue: %+v %v", got, err)
				}
			case "missing":
				if err == nil || rt.queues.Load() != 0 {
					t.Fatal("missing Host accepted")
				}
			default:
				if err == nil || got.Status != "unknown" || len(got.Effects) == 0 || rt.queues.Load() != 1 {
					t.Fatalf("effect ambiguity lost: %+v %v", got, err)
				}
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("request changed")
			}
			if _, err := os.Stat(filepath.Join(h.dir, "launch.lock")); !os.IsNotExist(err) {
				t.Fatal("client attempted launch")
			}
		})
	}
}

func TestHostSavedProgressCLIRequiresExplicitConsent(t *testing.T) {
	for _, queue := range []bool{false, true} {
		h, req, rt, path := hostSavedProgressFixture(t)
		cmd := newHostSavedProgressCmd(&App{projectDir: req.ProjectDir}, queue)
		args := []string{"--request-file", path, "--order-id", req.ReadbackOrderID, "--session-id", req.ReadbackSessionID}
		if queue {
			cmd.SetArgs(args)
			if err := cmd.Execute(); err == nil || rt.queues.Load() != 0 {
				t.Fatal("new attempt missing consent accepted")
			}
			cmd = newHostSavedProgressCmd(&App{projectDir: req.ProjectDir}, queue)
			args = append(args, "--expected-version", req.ExpectedVersion)
		}
		serveSavedProgressOnce(t, h, "")
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		var got hostSavedProgress
		if err := json.Unmarshal(output.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if queue && got.Status != "queued" || !queue && got.Status != "observed" {
			t.Fatalf("CLI: %+v", got)
		}
	}
}
