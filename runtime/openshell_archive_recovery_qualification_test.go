package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func qualifiedArchiveInventoryRow(t *testing.T, target OpenShellSandboxRef, sandbox *pb.Sandbox) OpenShellCustodyRecord {
	t.Helper()
	name := sandbox.GetMetadata().GetName()
	target.SandboxID = sandbox.GetMetadata().GetId()
	spec, err := openShellBytes(sandbox.GetSpec())
	if err != nil {
		t.Fatal(err)
	}
	requestID := "create-" + name
	create, err := openShellBytes(&pb.CreateSandboxRequest{Name: name, RequestId: requestID,
		WorkspaceScope: &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: target.Workspace}}, Spec: sandbox.GetSpec()})
	if err != nil {
		t.Fatal(err)
	}
	return OpenShellCustodyRecord{Name: name, Spec: spec, CreateAcknowledged: true, CreateRequestID: requestID, CreateIntent: create,
		MainProcessInstanceID: sandbox.GetStatus().GetMainProcessInstanceId(),
		Worker:                OpenShellWorkerRecord{Identity: OpenShellIdentity{Sandbox: target, SandboxName: name}}}
}

func TestIndependentArchiveCustodyRefusesUnresolvedEffects(t *testing.T) {
	root := t.TempDir()
	const gateway, workspace = "https://127.0.0.1:9000", "owned-workspace"
	const credentialBinding = "1111111111111111111111111111111111111111111111111111111111111111"
	profile := OpenShellGatewaySpec{Binding: OpenShellCustodyBinding{Gateway: gateway, Workspace: workspace},
		CredentialSource: &OpenShellCredentialSource{BindingSHA256: credentialBinding}}
	fixture := func(t *testing.T) OpenShellCustodyState {
		sandbox := custodySandbox("original", "native-original", pb.SandboxPhase_SANDBOX_PHASE_STOPPED)
		sandbox.Status.MainProcessInstanceId = "exact-original-main"
		ref := OpenShellSandboxRef{Gateway: gateway, Workspace: workspace}
		return OpenShellCustodyState{Version: openShellCredentialCustodyVersion, Binding: profile.Binding,
			Records: []OpenShellCustodyRecord{qualifiedArchiveInventoryRow(t, ref, sandbox)},
			Credentials: []OpenShellCredentialDelivery{{Provider: "fixture-provider", SourceBinding: credentialBinding,
				SourceProviderID: "fixture-source", ProfileID: "fixture-profile", MutationID: "fixture-installed",
				InstallationObserved: true}}}
	}
	if err := qualifyIndependentArchiveCustody(fixture(t), profile, root); err != nil {
		t.Fatal("fully settled original custody refused", err)
	}
	for _, tc := range []struct {
		name, want string
		change     func(*OpenShellCustodyState)
	}{
		{"create", "sandbox effect", func(s *OpenShellCustodyState) { s.Records[0].CreateAcknowledged = false }},
		{"exec", "sandbox effect", func(s *OpenShellCustodyState) { s.Records[0].Exec = &OpenShellCustodyExec{} }},
		{"progress stop", "sandbox effect", func(s *OpenShellCustodyState) { s.Records[0].ProgressStop = &OpenShellCustodyExec{} }},
		{"stop", "sandbox effect", func(s *OpenShellCustodyState) { s.Records[0].StopRequestID = "offered-original-stop" }},
		{"retirement", "sandbox effect", func(s *OpenShellCustodyState) { s.Records[0].Retirement = &openShellVMRetirement{} }},
		{"credential source", "credential effect", func(s *OpenShellCustodyState) { s.Credentials[0].SourceReadPending = true }},
		{"credential native mutation", "credential effect", func(s *OpenShellCustodyState) {
			s.Credentials[0].Pending = &OpenShellCredentialIntent{Kind: "update", RequestID: "fixture-update",
				StartedAt: time.Now(), SealedPayload: make([]byte, 32)}
		}},
		{"credential install", "credential effect", func(s *OpenShellCustodyState) { s.Credentials[0].InstallationObserved = false }},
		{"initialization", "unresolved initialization", func(s *OpenShellCustodyState) {
			s.PendingRetained = []OpenShellRetainedSandbox{{Name: "pending"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := fixture(t)
			tc.change(&state)
			if err := qualifyIndependentArchiveCustody(state, profile, root); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unresolved %s qualified independent cleanup: %v", tc.name, err)
			}
		})
	}
	if err := os.WriteFile(filepath.Join(root, "maintenance-stop-intent.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := qualifyIndependentArchiveCustody(fixture(t), profile, root); err == nil || !strings.Contains(err.Error(), "conflicting Stop intent") {
		t.Fatal("another maintenance Stop producer was ignored", err)
	}
}

func TestIndependentArchiveInventoryIsFreshAndReadOnly(t *testing.T) {
	const stopped = pb.SandboxPhase_SANDBOX_PHASE_STOPPED
	makeSandbox := func(name, id string, phase pb.SandboxPhase) *pb.Sandbox {
		s := custodySandbox(name, id, phase)
		s.Status.MainProcessInstanceId = "instance-" + name
		return s
	}
	original := makeSandbox("original", "native-original", stopped)
	historical := makeSandbox("historical", "native-historical", stopped)
	var mode, writes atomic.Int32
	server := &openShellNativeServer{
		list: func(_ context.Context, request *pb.ListSandboxesRequest) (*pb.ListSandboxesResponse, error) {
			if request.GetWorkspaceScope().GetAllWorkspaces() == nil {
				return nil, status.Error(codes.InvalidArgument, "missing all-workspace scope")
			}
			if request.GetPageToken() == "" {
				return &pb.ListSandboxesResponse{Sandboxes: []*pb.Sandbox{original}, NextPageToken: "second"}, nil
			}
			if request.GetPageToken() != "second" {
				return nil, status.Error(codes.InvalidArgument, "wrong page")
			}
			response := &pb.ListSandboxesResponse{Sandboxes: []*pb.Sandbox{historical}}
			switch mode.Load() {
			case 1:
				response.Sandboxes = append(response.Sandboxes, makeSandbox("foreign", "native-foreign", stopped))
			case 2:
				response.Sandboxes = append(response.Sandboxes, original)
			case 3:
				response.Sandboxes = nil
			case 4:
				response.Sandboxes, response.NextPageToken = nil, "second"
			case 5:
				response.Sandboxes = []*pb.Sandbox{makeSandbox("historical", "replacement", stopped)}
			case 6:
				response.Sandboxes = []*pb.Sandbox{makeSandbox("historical", "native-historical", pb.SandboxPhase_SANDBOX_PHASE_READY)}
			}
			return response, nil
		},
		create: func(context.Context, *pb.CreateSandboxRequest) (*pb.SandboxResponse, error) {
			writes.Add(1)
			return nil, status.Error(codes.Internal, "unexpected Create")
		},
		stop: func(context.Context, *pb.StopSandboxRequest) (*pb.SandboxResponse, error) {
			writes.Add(1)
			return nil, status.Error(codes.Internal, "unexpected Stop")
		},
		deleteMatches: func(context.Context, *pb.DeleteStoppedSandboxIfMatchesRequest) (*pb.DeleteStoppedSandboxIfMatchesResponse, error) {
			writes.Add(1)
			return nil, status.Error(codes.Internal, "unexpected retirement")
		},
	}
	target, tls := openShellTestGateway(t, server)
	conn, err := newOpenShellConnection(target.Sandbox.Gateway, tls)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	state := OpenShellCustodyState{Records: []OpenShellCustodyRecord{
		qualifiedArchiveInventoryRow(t, target.Sandbox, original), qualifiedArchiveInventoryRow(t, target.Sandbox, historical),
	}}
	client := pb.NewOpenShellClient(conn)
	if err = observeIndependentArchiveInventory(t.Context(), state, client); err != nil {
		t.Fatal("original two-page stopped inventory refused", err)
	}
	for i, name := range []string{"foreign VM", "duplicate VM", "missing VM", "cyclic page", "wrong VM identity", "active VM"} {
		t.Run(name, func(t *testing.T) {
			mode.Store(int32(i + 1))
			if err := observeIndependentArchiveInventory(t.Context(), state, client); err == nil {
				t.Fatal("changed current inventory qualified original custody")
			}
		})
	}
	if writes.Load() != 0 || server.execs.Load() != 0 {
		t.Fatal("read-only native qualification emitted an effect", writes.Load(), server.execs.Load())
	}
}
