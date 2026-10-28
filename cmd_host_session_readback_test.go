package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
)

type sessionReadbackHostRuntime struct {
	fakeHostRuntime
	calls int
	read  func(string, string) (loop.SessionReadback, error)
}

func (r *sessionReadbackHostRuntime) SessionReadback(_ context.Context, order, session string) (loop.SessionReadback, error) {
	r.calls++
	return r.read(order, session)
}
func hostSessionReadbackFixture(t *testing.T) (*noodleHost, hostRequest, *sessionReadbackHostRuntime, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	req := hostTestRequest(t, root)
	hostDir, err := os.MkdirTemp("", "nh-rb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(hostDir) })
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := hostReadbackBinaryRef(executable)
	if err != nil {
		t.Fatal(err)
	}
	req.Operation = "task_session_readback"
	req.HostDir = hostDir
	req.BinarySHA256 = binary.SHA256
	req.InstanceID = "original-host"
	req.ContextID = "original-context"
	req.ReadbackOrderID = "order"
	req.ReadbackSessionID = "session"
	binding := hostRequestBinding{Schema: 1, RequestID: req.RequestID, Operation: "attach", ProjectDir: root, ConfigPath: req.ConfigPath, ConfigSHA256: req.ConfigSHA256, Mode: req.Mode, BinarySHA256: req.BinarySHA256, EnvironmentSHA256: "original-env", Status: "accepted", HostDir: hostDir}
	requestPath := filepath.Join(root, "request.json")
	raw, _ := json.Marshal(binding)
	if err = os.WriteFile(requestPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	req.ReadbackRequest, err = loopruntime.ReadSessionReadbackFile(requestPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	receipt := hostReceipt{Schema: 1, Protocol: hostProtocol, HostPID: os.Getpid(), HostInstanceID: req.InstanceID, ContextID: req.ContextID, ProjectDir: root, ContextState: "active", Mode: req.Mode, ConfigPath: req.ConfigPath, ConfigSHA256: req.ConfigSHA256, BinarySHA256: req.BinarySHA256, RequestID: req.RequestID, HostDir: hostDir}
	rt := &sessionReadbackHostRuntime{}
	rt.read = func(order, session string) (loop.SessionReadback, error) {
		if order != "order" || session != "session" {
			return loop.SessionReadback{}, fmt.Errorf("session binding differs")
		}
		return loop.SessionReadback{SchemaVersion: 1, Status: "observed", OrderID: order, SessionID: session, AttemptID: "canonical-attempt", AttemptStatus: "failed", Runtime: "openshell", Effects: []string{}}, nil
	}
	owner := &hostContext{receipt: receipt, runtime: rt, done: make(chan error, 1), openShell: true, environmentSHA: binding.EnvironmentSHA256}
	h := &noodleHost{dir: hostDir, instanceID: req.InstanceID, binarySHA: req.BinarySHA256, contexts: map[string]*hostContext{req.ContextID: owner}, receipts: []hostReceipt{receipt}}
	if err = h.save(); err != nil {
		t.Fatal(err)
	}
	return h, req, rt, requestPath
}
func TestHostSessionReadbackExactBindingWithoutCleanup(t *testing.T) {
	for _, variant := range []string{"observed", "instance", "binary", "request", "context", "config", "session", "request-source", "source-drift"} {
		t.Run(variant, func(t *testing.T) {
			h, req, rt, path := hostSessionReadbackFixture(t)
			original, _ := os.ReadFile(path)
			owner := h.contexts[req.ContextID]
			owner.done <- nil
			switch variant {
			case "instance":
				req.InstanceID = "foreign"
			case "binary":
				req.BinarySHA256 = "foreign"
			case "request":
				req.RequestID = "foreign"
			case "context":
				req.ContextID = "foreign"
			case "config":
				req.ConfigSHA256 = "foreign"
			case "session":
				req.ReadbackSessionID = "foreign"
			case "request-source":
				req.ReadbackRequest.SHA256 = "foreign"
			case "source-drift":
				before := rt.read
				rt.read = func(order, session string) (loop.SessionReadback, error) {
					result, err := before(order, session)
					data, _ := os.ReadFile(filepath.Join(req.ProjectDir, ".noodle.toml"))
					_ = os.WriteFile(filepath.Join(req.ProjectDir, ".noodle.toml"), append(data, ' '), 0600)
					return result, err
				}
			}
			result := h.sessionReadback(t.Context(), req)
			if variant == "observed" {
				if result.Status != "observed" || result.Observation.AttemptStatus != "failed" || rt.calls != 1 {
					t.Fatalf("observation: %+v", result)
				}
			} else if result.Status != "refused" {
				t.Fatalf("invalid binding accepted: %+v", result)
			}
			current, _ := os.ReadFile(path)
			if string(current) != string(original) || len(owner.done) != 1 || rt.shutdowns != 0 || owner.receipt.ContextState != "active" || len(result.Effects) != 0 {
				t.Fatal("readback wrote request, drained completion or cleaned context")
			}
		})
	}
}
func TestHostSessionReadbackClientDoesNotLaunchOrRewrite(t *testing.T) {
	for _, online := range []bool{false, true} {
		t.Run(fmt.Sprint(online), func(t *testing.T) {
			h, req, _, path := hostSessionReadbackFixture(t)
			before, _ := os.ReadFile(path)
			if online {
				listener, err := net.Listen("unix", filepath.Join(h.dir, "host.sock"))
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				done := make(chan error, 1)
				go func() {
					conn, err := listener.Accept()
					if err != nil {
						done <- err
						return
					}
					defer conn.Close()
					var call hostRequest
					if err = json.NewDecoder(conn).Decode(&call); err != nil {
						done <- err
						return
					}
					done <- json.NewEncoder(conn).Encode(h.sessionReadback(context.Background(), call))
				}()
				defer func() {
					if err := <-done; err != nil {
						t.Error(err)
					}
				}()
			}
			result, err := callHostSessionReadback(t.Context(), req.ProjectDir, path, req.ReadbackOrderID, req.ReadbackSessionID)
			if online {
				if err != nil || result.Status != "observed" {
					t.Fatalf("client: %+v %v", result, err)
				}
			} else {
				if err == nil {
					t.Fatal("missing Host reported observed")
				}
				if _, err = os.Stat(filepath.Join(h.dir, "launch.lock")); !os.IsNotExist(err) {
					t.Fatal("readback attempted Host launch")
				}
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Fatal("readback rewrote original request")
			}
		})
	}
}
