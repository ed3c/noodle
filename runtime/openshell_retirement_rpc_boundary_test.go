package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
)

func TestOpenShellRetirementRPCBudgetStartsAfterLocalDurability(t *testing.T) {
	g, c, _, id := completedObservationFixture(t)
	closure := closureFixture(t, id)
	phases := make(chan string)
	advance := make(chan struct{})
	verified, persisted := time.Time{}, time.Time{}
	pause := func(ctx context.Context, phase string) {
		if _, ok := ctx.Deadline(); ok {
			t.Error("local retirement phase inherited RPC deadline")
		}
		phases <- phase
		<-advance
	}
	verifies, saves, deletes, absence := 0, 0, 0, 0
	verify := func(ctx context.Context, ref SessionReadbackFileRef, id OpenShellIdentity) (OpenShellClosure, error) {
		verifies++
		if verifies == 2 {
			pause(ctx, "verify")
			verified = time.Now()
		}
		return readOpenShellClosure(ref, id)
	}
	persist := func(ctx context.Context) error {
		saves++
		if saves == 1 {
			pause(ctx, "intent-fsync")
		}
		if err := c.save(); err != nil {
			return err
		}
		if saves == 1 {
			persisted = time.Now()
		}
		return nil
	}
	client := c.client
	c.client = retiringClient{OpenShellClient: retirementDeadlineClient{OpenShellClient: client, get: func(ctx context.Context, req *pb.GetSandboxRequest) (*pb.SandboxResponse, error) {
		if deletes > 0 {
			absence++
			deadline, ok := ctx.Deadline()
			if !ok || !deadline.After(persisted.Add(30*time.Second)) {
				t.Errorf("absence budget started before local durability: %v %v", deadline, ok)
			}
		}
		return client.GetSandbox(ctx, req)
	}}, delete: func(ctx context.Context, req *pb.DeleteSandboxRequest) (*pb.DeleteSandboxResponse, error) {
		deletes++
		deadline, ok := ctx.Deadline()
		if !ok || !deadline.After(verified.Add(30*time.Second)) || !deadline.After(persisted.Add(30*time.Second)) {
			t.Errorf("delete did not receive fresh post-fsync budget: %v %v", deadline, ok)
		}
		g.mutate(func() { delete(g.sandboxes, req.Name) })
		return &pb.DeleteSandboxResponse{SandboxId: id.Sandbox.SandboxID, Outcome: pb.DeletionOutcome_DELETION_OUTCOME_COMPLETED}, nil
	}}
	done := make(chan error, 1)
	go func() { done <- c.retire(t.Context(), id, closure, verify, persist) }()
	for _, want := range []string{"verify", "intent-fsync"} {
		select {
		case phase := <-phases:
			if phase != want {
				t.Errorf("phase=%s want=%s", phase, want)
			}
		case err := <-done:
			t.Fatalf("retirement ended before %s: %v", want, err)
		case <-time.After(5 * time.Second):
			t.Fatal("local retirement phase not reached")
		}
		if deletes != 0 {
			t.Fatal("delete preceded local durability")
		}
		advance <- struct{}{}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if deletes != 1 || absence != 1 || !c.Snapshot().Records[0].Retirement.Completed {
		t.Fatalf("retirement incomplete: deletes=%d absence=%d", deletes, absence)
	}
}

func TestOpenShellRetirementCancellationAtIntentBoundary(t *testing.T) {
	for _, afterIntent := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-intent", true: "after-intent-before-send"}[afterIntent], func(t *testing.T) {
			_, c, _, id := completedObservationFixture(t)
			closure := closureFixture(t, id)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			verifies, deletes := 0, 0
			verify := func(_ context.Context, ref SessionReadbackFileRef, id OpenShellIdentity) (OpenShellClosure, error) {
				verifies++
				value, err := readOpenShellClosure(ref, id)
				if verifies == 2 && !afterIntent {
					cancel()
				}
				return value, err
			}
			persist := func(context.Context) error { err := c.save(); cancel(); return err }
			c.client = retiringClient{OpenShellClient: c.client, delete: func(context.Context, *pb.DeleteSandboxRequest) (*pb.DeleteSandboxResponse, error) {
				deletes++
				return nil, errors.New("cancelled owner reached native Delete")
			}}
			if err := c.retire(ctx, id, closure, verify, persist); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			state := c.Snapshot().Records[0]
			if deletes != 0 || (state.Retirement != nil) != afterIntent {
				t.Fatalf("unsafe intent boundary: deletes=%d retirement=%+v", deletes, state.Retirement)
			}
			if afterIntent {
				disk := custodyDisk(t, c.state.Binding).Records[0]
				if disk.Retirement == nil || disk.Retirement.RequestID != state.Retirement.RequestID || disk.Retirement.Acknowledged || disk.Retirement.Completed {
					t.Fatal("cancelled original intent was not durably retained as unknown")
				}
				if state.Retirement.Acknowledged || state.Retirement.Completed {
					t.Fatal("unsent intent acknowledged")
				}
				if err := c.Retire(t.Context(), id, closure); err == nil || deletes != 0 {
					t.Fatal("unsent original intent replayed")
				}
				if _, err := c.ObserveRetired(t.Context(), id, closure); err == nil {
					t.Fatal("unsent intent inferred from readback")
				}
			}
		})
	}
}
