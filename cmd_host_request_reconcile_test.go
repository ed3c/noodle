package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestHostLostAttachReplyReconcilesOnlyProvenActivePendingRequest(t *testing.T) {
	for _, outcome := range []string{"active", "busy", "not_found", "unknown_effect", "detached", "foreign-context", "foreign-binary", "missing-owner"} {
		t.Run(outcome, func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "nrr-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(root) })
			root, err = filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("NOODLE_HOST_DIR", root)
			identity := strings.Repeat("a", 32)
			directory := filepath.Join(root, "g", identity)
			project := filepath.Join(root, "project")
			for _, path := range []string{directory, project} {
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			req := hostTestRequest(t, project)
			req.RequiredHostInstanceID = identity
			req.RequestFile = filepath.Join(root, "request.json")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			binary, err := hostReadbackBinaryRef(executable)
			if err != nil {
				t.Fatal(err)
			}
			if err := updateHostPool(root, func(pool *hostPool) error {
				pool.Active = identity
				pool.Generations[identity] = hostGeneration{ID: identity, Directory: directory, Binary: binary.Path, SHA: binary.SHA256, State: "active"}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			h := &noodleHost{dir: directory, instanceID: identity, binaryPath: binary.Path, binarySHA: binary.SHA256,
				budget: admission.NewBudget(admission.DefaultWorkerLimit), rolling: &hostRolling{root: root},
				newRuntime: func(string, string, config.Config, loop.Dependencies) hostRuntime {
					return &fakeHostRuntime{ready: make(chan struct{}), released: make(chan struct{})}
				}}
			listener, err := net.Listen("unix", filepath.Join(directory, "host.sock"))
			if err != nil {
				t.Fatal(err)
			}
			operations := make(chan string, 3)
			finished := make(chan struct{})
			serverError := make(chan error, 1)
			go func() {
				defer close(finished)
				for index := 0; index < 3; index++ {
					connection, err := listener.Accept()
					if err != nil {
						return
					}
					var request hostRequest
					if err := json.NewDecoder(connection).Decode(&request); err != nil {
						connection.Close()
						serverError <- err
						return
					}
					operations <- request.Operation
					response := h.handle(context.Background(), request)
					if index == 0 {
						if response.Status != "ok" || response.ContextState != "active" {
							connection.Close()
							serverError <- fmt.Errorf("fixture native admission failed: %+v", response)
							return
						}
						connection.Close() // The real frontend has sent its request; only the ACK is lost.
						continue
					}
					switch outcome {
					case "busy", "not_found", "unknown_effect":
						response.Status = outcome
					case "detached":
						response.ContextState = "detached"
					case "foreign-context":
						response.ContextID = "foreign"
					case "foreign-binary":
						response.BinarySHA256 = "foreign"
					case "missing-owner":
						if err := updateHostPool(root, func(pool *hostPool) error { delete(pool.Owners, request.RequestID); return nil }); err != nil {
							connection.Close()
							serverError <- err
							return
						}
					}
					if err := json.NewEncoder(connection).Encode(response); err != nil {
						connection.Close()
						serverError <- err
						return
					}
					connection.Close()
				}
			}()
			t.Cleanup(func() {
				listener.Close()
				select {
				case <-finished:
				case <-time.After(time.Second):
					t.Error("native fixture server did not stop")
				}
				for _, c := range h.contexts {
					c.cancel()
					if c.done != nil {
						<-c.done
					}
					h.finish(c, nil)
				}
			})
			first := callHost(context.Background(), req)
			if first.Status != "unknown_effect" {
				t.Fatalf("lost ACK became known outcome: %+v", first)
			}
			var pending hostRequestBinding
			pendingRef, err := loopruntime.ReadSessionReadbackFile(req.RequestFile, &pending)
			if err != nil || pending.Status != "pending" {
				t.Fatalf("lost ACK did not retain pending binding: %+v, %v", pending, err)
			}
			for retry := 0; retry < 2; retry++ {
				response := callHost(context.Background(), req)
				var current hostRequestBinding
				currentRef, err := loopruntime.ReadSessionReadbackFile(req.RequestFile, &current)
				if err != nil {
					t.Fatal(err)
				}
				want := "pending"
				if outcome == "active" {
					want = "accepted"
				}
				if current.Status != want || current.EnvironmentSHA256 != pending.EnvironmentSHA256 || current.RequestID != pending.RequestID {
					t.Fatalf("status (%s) did not reconcile exact original binding: %+v", outcome, current)
				}
				if outcome == "active" {
					if response.Status != "ok" || response.OwnerBinding == nil || response.OwnerBinding.Request != currentRef ||
						response.OwnerBinding.Sources["original_request"] != currentRef || currentRef == pendingRef {
						t.Fatalf("status returned stale acceptance source: %+v", response)
					}
				} else if currentRef != pendingRef {
					t.Fatalf("unproven outcome (%s) rewrote pending request", outcome)
				}
			}
			<-finished
			close(operations)
			var actual []string
			for operation := range operations {
				actual = append(actual, operation)
			}
			if strings.Join(actual, ",") != "attach,status,status" {
				t.Fatalf("unknown admission was reattached: %v", actual)
			}
			select {
			case err := <-serverError:
				t.Fatal(err)
			default:
			}
		})
	}
}
