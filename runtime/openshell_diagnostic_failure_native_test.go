package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
)

func TestOpenShellDiagnosticNativeObservationNeverCertifiesExec(t *testing.T) {
	g, c, b, id := completedObservationFixture(t)
	c.state.Records[0].Exec = nil
	c.state.Records[0].Worker.ExecResult = OpenShellExecResult{}
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
	before := beforeObservation(t, g, c, b)
	got, err := c.observeStopped(t.Context(), id, validateNotStartedRecord)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExecutionBasis != openShellNotStarted || got.ExecExitCode != -1 || got.ExecRequestID != "" || got.ExecAttemptID != "" || got.CommandSHA256 != "" {
		t.Fatalf("invented execution: %+v", got)
	}
	assertObservationReadOnly(t, g, c, b, before, 1)
	if _, err := c.ObserveCompleted(t.Context(), id); err == nil {
		t.Fatal("no exec treated as completion")
	}
	if _, err := c.ObserveSavedProgress(t.Context(), id); err == nil {
		t.Fatal("no exec treated as saved progress")
	}
	g.mutate(func() { g.sandboxes[id.SandboxName].Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_READY })
	if _, err := c.observeStopped(t.Context(), id, validateNotStartedRecord); err == nil {
		t.Fatal("old stopped receipt replaced fresh read")
	}
}

func TestOpenShellDiagnosticFailureUnknownDeleteAckRetainsTombstone(t *testing.T) {
	r, b, _ := diagnosticFailureFixture(t)
	c := r.config.Gateway.custody
	native := &reclamationClient{OpenShellClient: c.client, id: c.Snapshot().Records[0].Worker.Identity.Sandbox.SandboxID}
	calls := 0
	c.client = retiringClient{OpenShellClient: native, delete: func(_ context.Context, _ *pb.DeleteSandboxRequest) (*pb.DeleteSandboxResponse, error) {
		calls++
		native.deleted = true
		return nil, errors.New("response lost")
	}}
	authority := failedDiagnosticAuthority(t, b.OrderID)
	for i := 0; i < 2; i++ {
		if _, err := r.ReclaimCleanupSession(t.Context(), b, b.WorktreePath, StatusFailed, "original owner closed failure", authority); err == nil {
			t.Fatal("absence substituted for original delete ACK")
		}
	}
	if calls != 1 {
		t.Fatal("unknown delete replayed")
	}
	record := c.Snapshot().Records[0]
	if record.Exec != nil || record.Retirement == nil || record.Retirement.Acknowledged || record.Retirement.Completed || record.Retirement.Observation.ExecutionBasis != openShellNotStarted {
		t.Fatal("unknown retirement lost original uncertainty")
	}
	if _, err := ReadSessionReadbackFile(filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID, "execution-closure.json"), nil); err != nil {
		t.Fatal(err)
	}
}

func TestOpenShellDiagnosticNativeRejectsFreshIdentityDrift(t *testing.T) {
	for _, kind := range []string{"uuid", "main", "missing", "spec"} {
		t.Run(kind, func(t *testing.T) {
			g, c, b, id := completedObservationFixture(t)
			c.state.Records[0].Exec = nil
			c.state.Records[0].Worker.ExecResult = OpenShellExecResult{}
			if err := c.save(); err != nil {
				t.Fatal(err)
			}
			g.mutate(func() {
				s := g.sandboxes[id.SandboxName]
				switch kind {
				case "uuid":
					s.Metadata.Id = "replacement"
				case "main":
					s.Status.MainProcessInstanceId = "replacement"
				case "missing":
					delete(g.sandboxes, id.SandboxName)
				case "spec":
					s.Spec.Command = []string{"replacement"}
				}
			})
			before := beforeObservation(t, g, c, b)
			if _, err := c.observeStopped(t.Context(), id, validateNotStartedRecord); err == nil {
				t.Fatal("fresh drift accepted")
			}
			assertObservationReadOnly(t, g, c, b, before, 1)
		})
	}
}
