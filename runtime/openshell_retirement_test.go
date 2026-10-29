package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc"
)

type retiringClient struct {
	pb.OpenShellClient
	delete func(context.Context, *pb.DeleteSandboxRequest) (*pb.DeleteSandboxResponse, error)
}

func (c retiringClient) DeleteSandbox(ctx context.Context, r *pb.DeleteSandboxRequest, _ ...grpc.CallOption) (*pb.DeleteSandboxResponse, error) {
	return c.delete(ctx, r)
}

func closureFixture(t *testing.T, id OpenShellIdentity) SessionReadbackFileRef {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "diagnostics.json")
	if err := os.WriteFile(p, []byte(`{"retained":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	evidence, err := ReadSessionReadbackFile(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(OpenShellClosure{Identity: id, Outcome: StatusCompleted, Reason: "original owner accepted artifact", Evidence: map[string]SessionReadbackFileRef{"diagnostics": evidence}})
	if err != nil {
		t.Fatal(err)
	}
	p = filepath.Join(dir, "closure.json")
	if err := os.WriteFile(p, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := ReadSessionReadbackFile(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestOpenShellRetirementPreservesHistoryAndAllowsNewWork(t *testing.T) {
	g, c, _, id := completedObservationFixture(t)
	closure := closureFixture(t, id)
	calls := 0
	c.client = retiringClient{OpenShellClient: c.client, delete: func(_ context.Context, r *pb.DeleteSandboxRequest) (*pb.DeleteSandboxResponse, error) {
		calls++
		if r.RequestId == "" || r.AllowMissing || r.Name != id.SandboxName {
			t.Fatal("unbound delete")
		}
		// The durable intent must precede the only effect.
		var disk OpenShellCustodyState
		data, err := os.ReadFile(filepath.Join(c.state.Binding.StateRoot, "owner.json"))
		if err != nil {
			t.Fatal(err)
		}
		if json.Unmarshal(data, &disk) != nil || disk.Records[0].Retirement.RequestID != r.RequestId {
			t.Fatal("intent missing")
		}
		g.mutate(func() { delete(g.sandboxes, r.Name) })
		return &pb.DeleteSandboxResponse{SandboxId: id.Sandbox.SandboxID, Outcome: pb.DeletionOutcome_DELETION_OUTCOME_COMPLETED}, nil
	}}
	if err := c.Retire(t.Context(), id, closure); err != nil {
		t.Fatal(err)
	}
	state := c.Snapshot()
	if len(state.Records) != 1 || !state.Records[0].Retirement.Completed || state.Records[0].Exec == nil {
		t.Fatal("history lost")
	}
	if _, err := c.ObserveCompleted(t.Context(), id); err == nil {
		t.Fatal("retired evidence represented as fresh completion")
	}
	observed, err := c.ObserveRetired(t.Context(), id, closure)
	if err != nil || observed.Identity != id {
		t.Fatal(err)
	}
	if err := c.Retire(t.Context(), id, closure); err == nil || calls != 1 {
		t.Fatal("delete replayed")
	}
	if _, err := c.Create(t.Context(), OpenShellTask{OrderID: "next", AttemptID: "next", SessionID: "next"}, &pb.SandboxSpec{Command: []string{"hold"}}); err != nil {
		t.Fatal(err)
	}
	if len(c.Snapshot().Records) != 2 {
		t.Fatal("historical identity recycled")
	}
}

func TestOpenShellRetirementUnknownAckCannotBeInferredOrReplayed(t *testing.T) {
	g, c, _, id := completedObservationFixture(t)
	closure := closureFixture(t, id)
	calls := 0
	c.client = retiringClient{OpenShellClient: c.client, delete: func(_ context.Context, r *pb.DeleteSandboxRequest) (*pb.DeleteSandboxResponse, error) {
		calls++
		g.mutate(func() { delete(g.sandboxes, r.Name) })
		return nil, errors.New("response lost")
	}}
	if err := c.Retire(t.Context(), id, closure); err == nil {
		t.Fatal("unknown acknowledged")
	}
	if _, err := c.ObserveRetired(t.Context(), id, closure); err == nil {
		t.Fatal("absence substituted for ACK")
	}
	if err := c.Retire(t.Context(), id, closure); err == nil || calls != 1 {
		t.Fatal("unknown delete replayed")
	}
	if err := c.Quiesce(t.Context()); err == nil {
		t.Fatal("unresolved retirement released")
	}
}

func TestOpenShellRetirementRejectsIncompleteOrChangedEvidence(t *testing.T) {
	for _, variant := range []string{"retained", "stop-pending", "no-stopped", "changed-closure", "changed-diagnostic", "wrong-ack", "pending"} {
		t.Run(variant, func(t *testing.T) {
			g, c, _, id := completedObservationFixture(t)
			closure := closureFixture(t, id)
			calls := 0
			switch variant {
			case "retained":
				c.state.Records[0].Retained = true
			case "stop-pending":
				c.state.Records[0].Worker.StopPending = true
			case "no-stopped":
				c.state.Records[0].Stopped = nil
			case "changed-closure":
				if err := os.WriteFile(closure.Path, []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "changed-diagnostic":
				if err := os.WriteFile(filepath.Join(filepath.Dir(closure.Path), "diagnostics.json"), []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			c.client = retiringClient{OpenShellClient: c.client, delete: func(_ context.Context, r *pb.DeleteSandboxRequest) (*pb.DeleteSandboxResponse, error) {
				calls++
				if variant == "wrong-ack" {
					return &pb.DeleteSandboxResponse{SandboxId: "other", Outcome: pb.DeletionOutcome_DELETION_OUTCOME_COMPLETED}, nil
				}
				return &pb.DeleteSandboxResponse{SandboxId: id.Sandbox.SandboxID, Outcome: pb.DeletionOutcome_DELETION_OUTCOME_ACCEPTED}, nil
			}}
			err := c.Retire(t.Context(), id, closure)
			if err == nil {
				t.Fatal("ineligible retirement passed")
			}
			if variant != "pending" && variant != "wrong-ack" && calls != 0 {
				t.Fatal("rejected proof caused delete")
			}
			if variant == "pending" {
				if !strings.Contains(err.Error(), "pending") {
					t.Fatal(err)
				}
				g.mutate(func() { delete(g.sandboxes, id.SandboxName) })
				if _, err := c.ObserveRetired(t.Context(), id, closure); err != nil {
					t.Fatal(err)
				}
				if calls != 1 {
					t.Fatal("asynchronous delete replayed")
				}
			}
		})
	}
}
