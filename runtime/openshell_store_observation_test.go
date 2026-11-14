package runtime

import (
	"context"
	"testing"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestSourceStoreObservationReadsAllPagesWithoutMutations(t *testing.T) {
	g, binding, security, _ := custodyFixture(t)
	gateway := OpenShellGatewaySpec{Binding: binding, TLS: security}
	count, err := ObserveOpenShellStoreCount(t.Context(), gateway)
	if err != nil || count != 0 {
		t.Fatal("empty store observation failed", count, err)
	}
	g.mutate(func() {
		for _, name := range []string{"first", "second", "third"} {
			g.sandboxes[name] = custodySandbox(name, name+"-id", pb.SandboxPhase_SANDBOX_PHASE_READY)
		}
	})
	count, err = ObserveOpenShellStoreCount(t.Context(), gateway)
	if err != nil || count != 3 {
		t.Fatal("store observation missed pages", count, err)
	}
	creates, stops, execs := g.counts()
	if creates != 0 || stops != 0 || execs != 0 {
		t.Fatal("read-only store observation sent a mutation")
	}
}

func TestSourceStoreObservationRetainsRefusalAndRejectsPaginationCycle(t *testing.T) {
	for _, fault := range []string{"permission", "pagination"} {
		t.Run(fault, func(t *testing.T) {
			_, server := custodyServer(t)
			server.list = func(_ context.Context, request *pb.ListSandboxesRequest) (*pb.ListSandboxesResponse, error) {
				if request.GetWorkspaceScope().GetAllWorkspaces() == nil || request.GetLabelSelector() != "" {
					t.Error("Source inventory was filtered")
				}
				if fault == "permission" {
					return nil, status.Error(codes.PermissionDenied, "fixture refusal")
				}
				return &pb.ListSandboxesResponse{NextPageToken: "same"}, nil
			}
			target, security := openShellTestGateway(t, server)
			_, err := ObserveOpenShellStoreCount(t.Context(), OpenShellGatewaySpec{Binding: custodyBinding(t, target.Sandbox.Gateway), TLS: security})
			if err == nil || (fault == "permission" && status.Code(err) != codes.PermissionDenied) {
				t.Fatal("unknown/refused inventory was accepted", err)
			}
		})
	}
}
