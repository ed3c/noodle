package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestArchiveRecoveryStopUsesOriginalVMExactlyOnce(t *testing.T) {
	for _, test := range []struct {
		name, refusal string
		wrongIdentity bool
		lostAck       bool
	}{
		{name: "confirmed"},
		{name: "replaced", refusal: "native sandbox identity changed", wrongIdentity: true},
		{name: "lost acknowledgement", refusal: "acknowledgement unresolved", lostAck: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stopped atomic.Bool
			var stopCalls atomic.Int32
			const name, id, main = "owned-name", "native-vm", "original-main"
			sandbox := func() *pb.Sandbox {
				s := custodySandbox(name, id, pb.SandboxPhase_SANDBOX_PHASE_READY)
				s.Status.MainProcessInstanceId = main
				if stopped.Load() {
					s.Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_STOPPED
				}
				if test.wrongIdentity {
					s.Status.MainProcessInstanceId = "foreign-main"
				}
				return s
			}
			server := &openShellNativeServer{
				list: func(context.Context, *pb.ListSandboxesRequest) (*pb.ListSandboxesResponse, error) {
					return &pb.ListSandboxesResponse{Sandboxes: []*pb.Sandbox{sandbox()}}, nil
				},
				get: func(context.Context, *pb.GetSandboxRequest) (*pb.SandboxResponse, error) {
					return &pb.SandboxResponse{Sandbox: sandbox()}, nil
				},
				stop: func(_ context.Context, req *pb.StopSandboxRequest) (*pb.SandboxResponse, error) {
					stopCalls.Add(1)
					if req.GetName() != name || req.GetWorkspaceScope().GetWorkspace() != "owned-workspace" || req.GetRequestId() == "" {
						t.Error("native Stop was not bound to the original VM")
					}
					stopped.Store(true)
					if test.lostAck {
						return nil, status.Error(codes.Unavailable, "fixture lost Stop acknowledgement")
					}
					return &pb.SandboxResponse{Sandbox: sandbox()}, nil
				},
			}
			target, tls := openShellTestGateway(t, server)
			conn, err := newOpenShellConnection(target.Sandbox.Gateway, tls)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			spec, err := openShellBytes(sandbox().Spec)
			if err != nil {
				t.Fatal(err)
			}
			record := OpenShellCustodyRecord{Name: name, MainProcessInstanceID: main, Spec: spec, Worker: OpenShellWorkerRecord{Identity: OpenShellIdentity{Sandbox: target.Sandbox}}}
			state := OpenShellCustodyState{Binding: OpenShellCustodyBinding{Workspace: target.Sandbox.Workspace}, Records: []OpenShellCustodyRecord{record}}
			in := storageFailureInputs{state: state, record: record}
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			p := ProgressMaintenance{SessionDirectory: root}
			review, capture := SessionReadbackFileRef{Path: "/test/review"}, SessionReadbackFileRef{Path: "/test/capture"}
			hold := ArchiveRecoveryDispatchHold{PauseCommandID: "original-pause"}
			output := filepath.Join(root, "archive-recovery-stop")
			client := pb.NewOpenShellClient(conn)
			if err := observeOpenShellArchiveRecovery(t.Context(), client, in, p, review, capture, nil); (err != nil) != test.wrongIdentity || stopCalls.Load() != 0 {
				t.Fatalf("read-only Ready proof differed or sent Stop: %v", err)
			}
			receipt, err := stopOpenShellArchiveRecovery(t.Context(), client, in, p, review, capture, hold, output, func() error { return nil })
			if test.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), test.refusal) {
					t.Fatalf("expected %q, got %v", test.refusal, err)
				}
			} else if err != nil || receipt.Path == "" {
				t.Fatalf("expected confirmed stop receipt: %v", err)
			}
			if test.wrongIdentity {
				if stopCalls.Load() != 0 {
					t.Fatal("replaced VM was stopped")
				}
				return
			}
			in.record.Stopped = openShellStoppedObservation(sandbox())
			in.state.Records[0] = in.record
			if err := observeOpenShellArchiveRecovery(t.Context(), client, in, p, review, capture, &hold); (err != nil) != test.lostAck || stopCalls.Load() != 1 {
				t.Fatalf("read-only Stopped proof differed or sent Stop: %v", err)
			}
			again, err := stopOpenShellArchiveRecovery(t.Context(), client, in, p, review, capture, hold, output, func() error { return nil })
			if stopCalls.Load() != 1 {
				t.Fatal("native Stop was replayed")
			}
			if test.lostAck {
				if err == nil || !strings.Contains(err.Error(), "UNKNOWN") {
					t.Fatalf("already Stopped supplied a missing acknowledgement: %v", err)
				}
				if _, err = os.Stat(filepath.Join(output, "archive-recovery-stop.json")); !os.IsNotExist(err) {
					t.Fatal("missing acknowledgement produced a completion receipt")
				}
			} else if err != nil || again != receipt {
				t.Fatalf("confirmed Stop did not return the same receipt: %v", err)
			}
		})
	}
}

func TestArchiveRecoveryPostStopCustodyObservation(t *testing.T) {
	observed := OpenShellCustodyRecord{Stopped: &OpenShellStoppedObservation{}}
	if !archiveCaptureHasConflictingStop(observed, false) {
		t.Fatal("native Stopped observation still prohibits capture and a new Stop")
	}
	if archiveCaptureHasConflictingStop(observed, true) {
		t.Fatal("custody observation alone is admissible after a separately verified Stop")
	}
	for _, change := range []struct {
		name  string
		apply func(*OpenShellCustodyRecord)
	}{
		{"another Stop request", func(r *OpenShellCustodyRecord) { r.StopRequestID = "other" }},
		{"independent Stop ACK", func(r *OpenShellCustodyRecord) { r.StopAcknowledged = true }},
		{"pending worker Stop", func(r *OpenShellCustodyRecord) { r.Worker.StopPending = true }},
		{"progress Stop", func(r *OpenShellCustodyRecord) { r.ProgressStop = &OpenShellCustodyExec{} }},
	} {
		t.Run(change.name, func(t *testing.T) {
			record := observed
			change.apply(&record)
			if !archiveCaptureHasConflictingStop(record, true) {
				t.Fatal("post-Stop observation qualified another Stop effect")
			}
		})
	}
}
