package runtime

import (
	"fmt"
	"reflect"
	"testing"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const progressStopRequestID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

func TestOpenShellCustodyProgressStopHasIndependentReceipt(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint("lost-ack-", lost), func(t *testing.T) {
			gateway, server := custodyServer(t)
			release := make(chan struct{})
			started := make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			var binding OpenShellCustodyBinding
			server.exec = func(stream pb.OpenShell_ExecSandboxInteractiveServer) error {
				request, err := openShellReceiveStart(stream)
				if err != nil {
					return err
				}
				gateway.mutate(func() { gateway.execs++ })
				saved := custodyDisk(t, binding).Records[0]
				switch request.RequestId {
				case openShellRequestID:
					if saved.Exec == nil || saved.Exec.Command.RequestID != request.RequestId {
						return status.Error(codes.Internal, "model intent missing")
					}
					return stream.Send(openShellExit(7))
				case progressStopRequestID:
					if saved.ProgressStop == nil || saved.ProgressStop.Command.RequestID != request.RequestId || saved.ProgressStop.Result.ExitObserved {
						return status.Error(codes.Internal, "stop purpose intent missing")
					}
					expected, err := progressStopCommand(saved.Exec.Command, progressStopRequestID)
					if err != nil || !reflect.DeepEqual(saved.ProgressStop.Command, expected) {
						return status.Error(codes.Internal, "stop authority differs")
					}
					close(started)
					select {
					case <-release:
					case <-stream.Context().Done():
						return stream.Context().Err()
					}
					if lost {
						return status.Error(codes.Unavailable, "stop acknowledgement lost")
					}
					return stream.Send(openShellExit(0))
				default:
					return status.Error(codes.InvalidArgument, "unexpected exec UUID")
				}
			}
			target, security := openShellTestGateway(t, server)
			binding = custodyBinding(t, target.Sandbox.Gateway)
			custody := custodyOpen(t, binding, security, nil, openTestCustody)
			record := custodyCreate(t, custody, "one")
			model, err := custody.StartOwned(t.Context(), record.Worker.Identity.Sandbox, "exec-one", progressTestCommand(t))
			if err != nil {
				t.Fatal(err)
			}
			for range model.Output() {
			}
			modelResult, err := model.Wait()
			if err != nil || !modelResult.ExitObserved {
				t.Fatalf("model wait: %v", err)
			}
			control, err := custody.StartProgressStop(t.Context(), record.Worker.Identity.Sandbox, progressStopRequestID)
			if err != nil {
				t.Fatal(err)
			}
			<-started
			detached := custody.Snapshot()
			detached.Records[0].ProgressStop.Command.Argv[0] = "changed"
			detached.Records[0].ProgressStop.Command.Env["NOODLE_PROGRESS_TOKEN"] = "changed"
			if custody.Snapshot().Records[0].ProgressStop.Command.Argv[0] == "changed" {
				t.Fatal("snapshot aliases stop authority")
			}
			if _, err := custody.StartProgressStop(t.Context(), record.Worker.Identity.Sandbox, progressStopRequestID); err == nil {
				t.Fatal("stop-purpose replay accepted")
			}
			worker, err := custody.RecoverWorker(t.Context(), record.Worker.Identity.Sandbox)
			if err != nil {
				t.Fatal(err)
			}
			if err := worker.Stop(t.Context(), custody, custody); err != nil {
				t.Fatal(err)
			}
			if err := custody.Quiesce(t.Context()); err == nil {
				t.Fatal("unresolved stop-purpose released gateway")
			}
			if _, err := custody.Create(t.Context(), OpenShellTask{OrderID: record.Task.OrderID, AttemptID: "new-attempt", SessionID: "new-session"}, &pb.SandboxSpec{Command: []string{"hold"}}); err == nil {
				t.Fatal("unresolved stop-purpose allowed new attempt")
			}
			close(release)
			for range control.Output() {
			}
			stopResult, stopErr := control.Wait()
			saved := custodyDisk(t, binding).Records[0]
			if !reflect.DeepEqual(saved.Exec.Result, modelResult) || !reflect.DeepEqual(saved.Worker.ExecResult, modelResult) {
				t.Fatal("control result replaced original model outcome")
			}
			if saved.ProgressStop.Result.RequestID != progressStopRequestID || saved.ProgressStop.Result.ExitObserved == lost {
				t.Fatal("control result missing or fabricated")
			}
			if lost {
				if stopErr == nil || stopResult.ExitObserved {
					t.Fatal("lost control acknowledgement reported settled")
				}
				if err := custody.Quiesce(t.Context()); err == nil {
					t.Fatal("lost control acknowledgement released gateway")
				}
			} else {
				if stopErr != nil || stopResult.ExitCode != 0 || !stopResult.ExitObserved {
					t.Fatalf("control wait: %+v %v", stopResult, stopErr)
				}
				if err := custody.Quiesce(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			_, _, execs := gateway.counts()
			if execs != 2 {
				t.Fatalf("executed %d native requests", execs)
			}
		})
	}
}

func TestOpenShellCustodyProgressStopRefusesUnboundOriginal(t *testing.T) {
	gateway, binding, security, open := custodyFixture(t)
	custody := custodyOpen(t, binding, security, nil, open)
	record := custodyCreate(t, custody, "one")
	if _, err := custody.StartProgressStop(t.Context(), record.Worker.Identity.Sandbox, progressStopRequestID); err == nil {
		t.Fatal("missing model allowed control")
	}
	model, err := custody.StartOwned(t.Context(), record.Worker.Identity.Sandbox, "exec-one", openShellTestCommand())
	if err != nil {
		t.Fatal(err)
	}
	for range model.Output() {
	}
	if _, err := model.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := custody.StartProgressStop(t.Context(), record.Worker.Identity.Sandbox, progressStopRequestID); err == nil {
		t.Fatal("unbound original command allowed control")
	}
	_, _, execs := gateway.counts()
	if execs != 1 || custody.Snapshot().Records[0].ProgressStop != nil {
		t.Fatal("refusal submitted a control exec")
	}
}

func TestOpenShellObserveCompletedDoesNotAcceptProgressStop(t *testing.T) {
	for _, observed := range []bool{false, true} {
		t.Run(fmt.Sprint(observed), func(t *testing.T) {
			gateway, custody, binding, identity := completedObservationFixture(t)
			custody.mu.Lock()
			custody.state.Records[0].ProgressStop = &OpenShellCustodyExec{AttemptID: "progress-stop", Command: OpenShellCommand{RequestID: progressStopRequestID}, Result: OpenShellExecResult{RequestID: progressStopRequestID, StartAttempted: true, ExitObserved: observed}}
			custody.mu.Unlock()
			before := beforeObservation(t, gateway, custody, binding)
			if _, err := custody.ObserveCompleted(t.Context(), identity); err == nil {
				t.Fatal("interruption promoted to successful model completion")
			}
			assertObservationReadOnly(t, gateway, custody, binding, before, 0)
		})
	}
}
