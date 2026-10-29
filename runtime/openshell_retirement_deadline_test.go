package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc"
)

type retirementDeadlineClient struct {
	pb.OpenShellClient
	get func(context.Context, *pb.GetSandboxRequest) (*pb.SandboxResponse, error)
}

func (c retirementDeadlineClient) GetSandbox(ctx context.Context, req *pb.GetSandboxRequest, _ ...grpc.CallOption) (*pb.SandboxResponse, error) {
	return c.get(ctx, req)
}

func TestOpenShellReclamationNativeOperationsHaveIndependentDeadlines(t *testing.T) {
	f := newManagedFixture(t, true)
	owner := f.open(t)
	if _, err := owner.Acquire(t.Context(), "a"); err != nil {
		t.Fatal(err)
	}
	g, c, _, id := completedObservationFixture(t)
	original := owner.custody
	owner.custody = c
	t.Cleanup(func() { owner.custody = original })
	closure := closureFixture(t, id)
	// Observe the last native validation before the local closure recheck and
	// durable intent. An outer gateway deadline would already be older than this
	// checkpoint and would clamp the later Delete budget.
	beforeDurability := time.Time{}
	originalClient := c.client
	c.client = retirementDeadlineClient{OpenShellClient: originalClient, get: func(ctx context.Context, req *pb.GetSandboxRequest) (*pb.SandboxResponse, error) {
		beforeDurability = time.Now()
		return originalClient.GetSandbox(ctx, req)
	}}
	var deleteDeadline time.Time
	deleteCalls := 0
	c.client = retiringClient{OpenShellClient: c.client, delete: func(ctx context.Context, req *pb.DeleteSandboxRequest) (*pb.DeleteSandboxResponse, error) {
		deleteCalls++
		var ok bool
		deleteDeadline, ok = ctx.Deadline()
		if !ok || time.Until(deleteDeadline) > 30*time.Second || time.Until(deleteDeadline) <= 0 {
			t.Errorf("native delete lacks independent 30s budget: %v %v", deleteDeadline, ok)
		}
		if !deleteDeadline.After(beforeDurability.Add(30 * time.Second)) {
			t.Error("gateway deadline consumed local closure verification or intent durability budget")
		}
		g.mutate(func() { delete(g.sandboxes, req.Name) })
		return &pb.DeleteSandboxResponse{SandboxId: id.Sandbox.SandboxID, Outcome: pb.DeletionOutcome_DELETION_OUTCOME_COMPLETED}, nil
	}}
	if err := owner.Retire(t.Context(), "a", id, closure); err != nil {
		t.Fatal(err)
	}
	// Retired observation uses a fresh native absence read, not a cached result.
	client := c.client
	calls := 0
	c.client = retirementDeadlineClient{OpenShellClient: client, get: func(ctx context.Context, req *pb.GetSandboxRequest) (*pb.SandboxResponse, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || !deadline.After(deleteDeadline) || time.Until(deadline) > 30*time.Second || time.Until(deadline) <= 0 {
			t.Errorf("native absence reused delete budget: %v %v", deadline, ok)
		}
		return client.GetSandbox(ctx, req)
	}}
	if _, err := owner.ObserveCleanup(t.Context(), "a", id, false); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("absence calls=%d", calls)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := owner.Retire(ctx, "a", id, closure); !errors.Is(err, context.Canceled) {
		t.Fatalf("native retirement ignored parent cancellation: %v", err)
	}
	if _, err := owner.ObserveCleanup(ctx, "a", id, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("native observation ignored parent cancellation: %v", err)
	}
	if deleteCalls != 1 || calls != 1 {
		t.Fatalf("cancelled native operation replayed: deletes=%d observations=%d", deleteCalls, calls)
	}
}
