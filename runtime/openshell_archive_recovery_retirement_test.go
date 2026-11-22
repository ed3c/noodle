package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/admission"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestArchiveRecoveryVMRetirementUsesOriginalStopAndReadsBackDiskWithoutReplay(t *testing.T) {
	for _, mode := range []string{"confirmed", "lost-ack-and-held-disk", "wrong-original-hold"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const name, id, main = "owned-vm", "native-vm", "original-main"
			sessionDir, custodyRoot, diskRoot := filepath.Join(root, "original-session"), filepath.Join(root, "owner"), filepath.Join(root, "vm")
			disk := filepath.Join(diskRoot, "sandboxes", id)
			for _, path := range []string{sessionDir, custodyRoot, disk} {
				if err = os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			config := filepath.Join(root, "gateway.toml")
			if err = os.WriteFile(config, []byte(fmt.Sprintf("[openshell.gateway]\ncompute_driver='vm'\n[openshell.drivers.vm]\nstate_dir=%q\n", diskRoot)), 0600); err != nil {
				t.Fatal(err)
			}
			fileRef := func(path string) SessionReadbackFileRef {
				t.Helper()
				ref, err := ReadSessionReadbackFile(path, nil)
				if err != nil {
					t.Fatal(err)
				}
				return ref
			}
			write := func(name string) SessionReadbackFileRef {
				t.Helper()
				path := filepath.Join(root, name+".json")
				if err := maintenanceWrite(path, map[string]string{"original": name}); err != nil {
					t.Fatal(err)
				}
				return fileRef(path)
			}
			storage := openShellVMStorage{Config: fileRef(config), Root: diskRoot}
			var stopped, absent atomic.Bool
			var deleteCalls atomic.Int32
			sandbox := func() *pb.Sandbox {
				phase := pb.SandboxPhase_SANDBOX_PHASE_READY
				if stopped.Load() {
					phase = pb.SandboxPhase_SANDBOX_PHASE_STOPPED
				}
				s := custodySandbox(name, id, phase)
				s.Metadata.ResourceVersion, s.Status.MainProcessInstanceId = 11, main
				return s
			}
			server := &openShellNativeServer{
				list: func(context.Context, *pb.ListSandboxesRequest) (*pb.ListSandboxesResponse, error) {
					result := &pb.ListSandboxesResponse{}
					if !absent.Load() {
						result.Sandboxes = []*pb.Sandbox{sandbox()}
					}
					return result, nil
				},
				get: func(context.Context, *pb.GetSandboxRequest) (*pb.SandboxResponse, error) {
					if absent.Load() {
						return nil, status.Error(codes.NotFound, "fixture VM absent")
					}
					return &pb.SandboxResponse{Sandbox: sandbox()}, nil
				},
				stop: func(context.Context, *pb.StopSandboxRequest) (*pb.SandboxResponse, error) {
					stopped.Store(true)
					return &pb.SandboxResponse{Sandbox: sandbox()}, nil
				},
				deleteMatches: func(_ context.Context, req *pb.DeleteStoppedSandboxIfMatchesRequest) (*pb.DeleteStoppedSandboxIfMatchesResponse, error) {
					deleteCalls.Add(1)
					if req.GetName() != name || req.GetExpectedSandboxId() != id || req.GetExpectedResourceVersion() != 11 ||
						req.GetWorkspaceScope().GetWorkspace() != "owned-workspace" || req.GetRequestId() == "" {
						t.Error("deletion escaped exact original VM and version")
						return nil, status.Error(codes.Aborted, "wrong fixture identity")
					}
					absent.Store(true)
					if mode == "lost-ack-and-held-disk" {
						return nil, status.Error(codes.Unavailable, "fixture ACK lost; VM disk still exists")
					}
					if err := os.Remove(disk); err != nil {
						return nil, err
					}
					return &pb.DeleteStoppedSandboxIfMatchesResponse{SandboxId: id, Outcome: pb.DeletionOutcome_DELETION_OUTCOME_COMPLETED}, nil
				},
			}
			target, tls := openShellTestGateway(t, server)
			conn, err := newOpenShellConnection(target.Sandbox.Gateway, tls)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			spec, err := openShellBytes(sandbox().GetSpec())
			if err != nil {
				t.Fatal(err)
			}
			task := OpenShellTask{"order", "native-attempt", "original-session"}
			exec := OpenShellExecResult{RequestID: "original-exec", StartAttempted: true, ExitObserved: true, ExitCode: 74}
			create := &pb.CreateSandboxRequest{Name: name, RequestId: "original-create",
				WorkspaceScope: &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: target.Sandbox.Workspace}}, Spec: sandbox().GetSpec()}
			createBytes, err := openShellBytes(create)
			if err != nil {
				t.Fatal(err)
			}
			record := OpenShellCustodyRecord{Task: task, Name: name, Spec: spec, CreateAcknowledged: true, MainProcessInstanceID: main,
				CreateRequestID: create.GetRequestId(), CreateIntent: createBytes,
				Worker: OpenShellWorkerRecord{Identity: OpenShellIdentity{OrderID: task.OrderID, AttemptID: task.AttemptID, SessionID: task.SessionID,
					Sandbox: target.Sandbox, SandboxName: name, ConfigSource: config}, ExecResult: exec},
				Exec: &OpenShellCustodyExec{AttemptID: task.AttemptID, Command: OpenShellCommand{RequestID: exec.RequestID}, Result: exec}}
			state := OpenShellCustodyState{Version: openShellCustodyVersion, Binding: OpenShellCustodyBinding{
				StateRoot: custodyRoot, Gateway: target.Sandbox.Gateway, Workspace: target.Sandbox.Workspace, ConfigSource: config},
				Records: []OpenShellCustodyRecord{record}}
			owner := &OpenShellCustody{state: state, client: pb.NewOpenShellClient(conn), initialized: true, ctx: ctx}
			review, capture := write("review"), write("capture")
			p := ProgressMaintenance{SessionDirectory: sessionDir, Request: write("request")}
			hold := ArchiveRecoveryDispatchHold{PauseCommandID: "original-pause"}
			stop, err := stopOpenShellArchiveRecovery(ctx, owner.client, storageFailureInputs{state: state, record: record}, p,
				review, capture, hold, filepath.Join(sessionDir, "archive-recovery-stop"), func() error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			proof := archiveRecoveryVMProof{
				disposition: archiveRecoveryRetirementDisposition{Stop: stop, Hold: hold, SandboxID: id, MainProcessInstanceID: main,
					OrderID: task.OrderID, NativeAttemptID: task.AttemptID, SessionID: task.SessionID},
				request: p, candidate: archiveRecoveryCandidate{archiveRecoveryApplyRecord: archiveRecoveryApplyRecord{
					archiveRecoveryApplyIntent: archiveRecoveryApplyIntent{Review: review, Capture: capture}}},
				dispatch: openShellDispatchReceipt{Worker: &record.Worker},
				sources: map[string]SessionReadbackFileRef{"archive_recovery_disposition": write("disposition"),
					"archive_recovery_stop": stop, "checkout_packet": write("packet"),
					"checkout_removal": write("removed"), "checkout_verification_release": write("verification-released")},
			}
			if mode == "wrong-original-hold" {
				proof.disposition.Hold.PauseCommandID = "foreign-pause"
			}
			if mode == "confirmed" {
				runtime := &OpenShellRuntime{active: map[string]*openShellSession{}}
				session := newOpenShellSession(runtime, task.SessionID, sessionDir)
				session.receipt = openShellDispatchReceipt{ContextID: "fixture-context", OrderID: task.OrderID,
					AttemptID: task.AttemptID, SessionID: task.SessionID, Phase: "progress-unknown", Error: "native progress failed",
					Outcome: SessionOutcome{Status: StatusFailed}, Worker: &record.Worker}
				proof.dispatch = session.receipt
				if err := session.persist(); err != nil {
					t.Fatal(err)
				}
				dispatch := fileRef(filepath.Join(session.dir, "openshell-dispatch.json"))
				close(session.runExited)
				close(session.progressStopDone)
				runtime.active[session.id] = session
				release := func() error {
					runtime.mu.Lock()
					defer runtime.mu.Unlock()
					if _, err := runtime.inertArchiveRecoveryHandle(proof); err != nil {
						return err
					}
					return runtime.observeArchiveRecoveryStoppedVM(ctx, owner, proof, storage, true)
				}
				stopped.Store(false)
				if err := release(); err == nil || len(runtime.active) != 1 {
					t.Fatal("fresh native stopped mismatch released original runtime handle", err)
				}
				stopped.Store(true)
				for range 2 {
					if err := release(); err != nil {
						t.Fatalf("confirmed stopped VM did not release original runtime handle: %v", err)
					}
				}
				select {
				case <-session.Done():
				default:
					t.Fatal("original local handle stayed live after confirmed stop")
				}
				if len(runtime.active) != 0 || deleteCalls.Load() != 0 ||
					fileRef(dispatch.Path) != dispatch || session.Outcome().Status != StatusFailed {
					t.Fatal("resource-only release modified original execution history or offered native retirement")
				}
			}
			result, err := owner.retireCleanupSession(ctx, task, storage, proof.sources, &proof)
			if mode == "wrong-original-hold" {
				if err == nil || deleteCalls.Load() != 0 || owner.state.Records[0].Retirement != nil {
					t.Fatalf("foreign Stop intent affected VM: %+v %v", result, err)
				}
				return
			}
			if mode == "lost-ack-and-held-disk" {
				if err == nil || result.State == CleanupSessionRetired || deleteCalls.Load() != 1 {
					t.Fatalf("native absence alone settled held disk: %+v %v", result, err)
				}
				if err = os.Remove(disk); err != nil {
					t.Fatal(err)
				}
			} else if err != nil || result.State != CleanupSessionRetired {
				t.Fatalf("exact archived VM was not retired: %+v %v", result, err)
			}
			var reopened OpenShellCustodyState
			if _, err = ReadSessionReadbackFile(filepath.Join(custodyRoot, "owner.json"), &reopened); err != nil {
				t.Fatal(err)
			}
			if err = validateOpenShellCustodyLedger(reopened); err != nil {
				t.Fatalf("original retired custody cannot restart: %v", err)
			}
			owner.state = reopened
			result, err = owner.retireCleanupSession(ctx, task, storage, proof.sources, &proof)
			if err != nil || result.State != CleanupSessionRetired || deleteCalls.Load() != 1 {
				t.Fatalf("original native deletion replayed or readback failed: %+v %v", result, err)
			}
			row := owner.state.Records[0]
			if row.Exec.Result.ExitCode != 74 || row.StopAcknowledged || row.StopRequestID != "" {
				t.Fatalf("historical execution was promoted by retirement: %+v", row)
			}
			if mode == "lost-ack-and-held-disk" && (row.Retirement.Acknowledged || row.Retirement.Failure == "" || result.NativeDeletionOutcome != "unknown") {
				t.Fatalf("lost native ACK was rewritten: %+v", result)
			}
			if err = os.Mkdir(disk, 0700); err != nil {
				t.Fatal(err)
			}
			if _, err = owner.retireCleanupSession(ctx, task, storage, proof.sources, &proof); err == nil || deleteCalls.Load() != 1 {
				t.Fatal("reappeared disk was accepted or caused another deletion", err)
			}
		})
	}
}

func TestArchiveRecoveryInertHandleRefusesReconstructedOrForeignActiveWork(t *testing.T) {
	r := &OpenShellRuntime{active: map[string]*openShellSession{}}
	session := newOpenShellSession(r, "original", t.TempDir())
	session.receipt = openShellDispatchReceipt{ContextID: "context", OrderID: "order", AttemptID: "native", SessionID: "original",
		Phase: "progress-unknown", Error: "original failure", Outcome: SessionOutcome{Status: StatusFailed}}
	r.active[session.id] = session
	proof := archiveRecoveryVMProof{disposition: archiveRecoveryRetirementDisposition{SessionID: session.id},
		request: ProgressMaintenance{SessionDirectory: session.dir}, dispatch: session.receipt}
	if _, err := r.inertArchiveRecoveryHandle(proof); err == nil {
		t.Fatal("reconstructed handle supplied an execution exit without an original run")
	}
	close(session.runExited)
	if _, err := r.inertArchiveRecoveryHandle(proof); err == nil {
		t.Fatal("unresolved supervisor control accepted an execution exit")
	}
	close(session.progressStopDone)
	r.active["foreign"] = newOpenShellSession(r, "foreign", t.TempDir())
	if _, err := r.inertArchiveRecoveryHandle(proof); err == nil {
		t.Fatal("original disposition qualified foreign active work")
	}
	delete(r.active, "foreign")
	proof.dispatch.Error = "foreign result"
	if _, err := r.inertArchiveRecoveryHandle(proof); err == nil {
		t.Fatal("changed in-process history inherited frozen archive authority")
	}
	proof.dispatch = session.receipt
	if observed, err := r.inertArchiveRecoveryHandle(proof); err != nil || observed != session {
		t.Fatalf("exact inert original handle could not be observed: %v", err)
	}
}

func TestArchiveRecoveryRecoverPreservesOriginalFrozenDispatchWhenRetirementIsUnknown(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const session, contextID = "original-session", "original-context"
	dir := filepath.Join(root, "sessions", session)
	custody := filepath.Join(root, "custody")
	for _, name := range []string{dir, custody} {
		if err = os.MkdirAll(name, 0700); err != nil {
			t.Fatal(err)
		}
	}
	binding := OpenShellCustodyBinding{StateRoot: custody}
	dispatch := filepath.Join(dir, "openshell-dispatch.json")
	if err = maintenanceWrite(dispatch, openShellDispatchReceipt{Version: 1, ContextID: contextID, OrderID: "original-order",
		SessionID: session, AttemptID: "native", Phase: "progress-unknown", Error: "original archive failure"}); err != nil {
		t.Fatal(err)
	}
	original, err := ReadSessionReadbackFile(dispatch, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = maintenanceWrite(filepath.Join(dir, "archive-recovery-cleanup-disposition.json"), archiveRecoveryRetirementDisposition{
		Schema: 1, Kind: "archive_recovery_cleanup_disposition", ContextID: contextID, OrderID: "original-order",
		AttemptID: "original-attempt", SessionID: session, NativeAttemptID: "native", OriginalReturn: "UNKNOWN",
		Lease: admission.MachineSessionLease{Binding: &admission.MachineSessionBinding{ContextID: contextID, OrderID: "original-order",
			AttemptID: "original-attempt", SessionID: session}},
	}); err != nil {
		t.Fatal(err)
	}
	if err = maintenanceWrite(filepath.Join(custody, "owner.json"), OpenShellCustodyState{Binding: binding,
		Records: []OpenShellCustodyRecord{{Task: OpenShellTask{"original-order", "native", session}}}}); err != nil {
		t.Fatal(err)
	}
	// There is deliberately no native gateway owner to acquire. A hold must
	// return before starting or rewriting an inconclusive original session.
	r := &OpenShellRuntime{config: OpenShellRuntimeConfig{RuntimeDir: root, ContextID: contextID,
		Profile: OpenShellProfile{Gateway: OpenShellGatewaySpec{Binding: binding}}}, active: map[string]*openShellSession{}}
	recovered, err := r.Recover(t.Context())
	if err == nil || len(recovered) != 0 || len(r.active) != 0 {
		t.Fatalf("unsettled archive disposition was re-adopted: %+v %v", recovered, err)
	}
	observed, err := ReadSessionReadbackFile(dispatch, nil)
	if err != nil || observed != original {
		t.Fatalf("retained unknown dispatch was rewritten: %v", err)
	}
}
