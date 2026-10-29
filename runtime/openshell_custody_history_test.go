package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func testOpenShellCustodyStoppedHistory(t *testing.T, fixture custodyFactory) {
	for _, kind := range []string{"phase", "main instance", "compute instance"} {
		t.Run(kind, func(t *testing.T) {
			g, b, security, open := fixture(t)
			c := custodyOpen(t, b, security, nil, open)
			r := custodyCreate(t, c, "original")
			g.mutate(func() {
				s := g.sandboxes[r.Name]
				s.Status.MainProcessInstanceId = "main-original"
				s.Status.AgentPod = "compute-original"
			})
			w, err := c.RecoverWorker(t.Context(), r.Worker.Identity.Sandbox)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Stop(t.Context(), c, c); err != nil {
				t.Fatal(err)
			}
			assertOpenShellBoundary(t, w, OpenShellBoundaryStopped)
			// Reopening while still Stopped must preserve the terminal history.
			_ = c.Close()
			c = custodyOpen(t, b, security, nil, open)
			g.mutate(func() {
				s := g.sandboxes[r.Name]
				switch kind {
				case "phase":
					s.Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_READY
				case "main instance":
					s.Status.MainProcessInstanceId = "main-replacement"
				case "compute instance":
					s.Status.AgentPod = "compute-replacement"
				}
			})
			if _, err := c.Create(t.Context(), OpenShellTask{"next", "next", "next"}, &pb.SandboxSpec{Command: []string{"hold"}}); err == nil {
				t.Fatal("stopped VM lifecycle drift allowed native create")
			}
			if creates, _, _ := g.counts(); creates != 1 {
				t.Fatalf("native creates=%d, want original only", creates)
			}
			if c.Snapshot().Blocked == "" || custodyDisk(t, b).Blocked == "" {
				t.Fatal("lifecycle block not persistent")
			}
			// Reversing the visible drift cannot clear the historical block.
			g.mutate(func() {
				s := g.sandboxes[r.Name]
				s.Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_STOPPED
				s.Status.MainProcessInstanceId = "main-original"
				s.Status.AgentPod = "compute-original"
			})
			_ = c.Close()
			c, err = open(t.Context(), b, security, nil)
			if c == nil || err == nil {
				t.Fatal("reopen cleared lifecycle block")
			}
			defer c.Close()
		})
	}
}

func testOpenShellCustodyExplicitNewAttempt(t *testing.T, fixture custodyFactory) {
	for _, runExec := range []bool{false, true} {
		t.Run(map[bool]string{false: "no exec", true: "known exec"}[runExec], func(t *testing.T) {
			g, b, security, open := fixture(t)
			c := custodyOpen(t, b, security, nil, open)
			original := custodyCreate(t, c, "original")
			if runExec {
				handle, err := c.StartOwned(t.Context(), original.Worker.Identity.Sandbox, "exec-original", openShellTestCommand())
				if err != nil {
					t.Fatal(err)
				}
				for range handle.Output() {
				}
				if result, err := handle.Wait(); err != nil || !result.ExitObserved {
					t.Fatalf("known exec lost: %+v %v", result, err)
				}
			}
			w, err := c.RecoverWorker(t.Context(), original.Worker.Identity.Sandbox)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Stop(t.Context(), c, c); err != nil {
				t.Fatal(err)
			}
			assertOpenShellBoundary(t, w, OpenShellBoundaryStopped)
			before := custodyDisk(t, b).Records[0]
			_ = c.Close()
			c = custodyOpen(t, b, security, nil, open)
			next := OpenShellTask{OrderID: original.Task.OrderID, AttemptID: "explicit-new-attempt", SessionID: "explicit-new-session"}
			name, err := c.Create(t.Context(), next, &pb.SandboxSpec{Command: []string{"hold"}})
			if err != nil {
				t.Fatalf("explicit new attempt lost original order: %v", err)
			}
			records := custodyDisk(t, b).Records
			if len(records) != 2 || name == original.Name || records[1].CreateRequestID == original.CreateRequestID || records[1].Task != next {
				t.Fatalf("new attempt identity/history incorrect: %+v", records)
			}
			if records[0].Task != before.Task || records[0].Name != before.Name || records[0].CreateRequestID != before.CreateRequestID || records[0].StopRequestID != before.StopRequestID {
				t.Fatal("new attempt rewrote original history")
			}
			for _, replay := range []OpenShellTask{original.Task, next, {OrderID: original.Task.OrderID, AttemptID: "third-attempt", SessionID: "third-session"}, {OrderID: "other-order", AttemptID: original.Task.AttemptID, SessionID: "other-session"}, {OrderID: "other-order", AttemptID: "other-attempt", SessionID: original.Task.SessionID}} {
				if _, err := c.Create(t.Context(), replay, &pb.SandboxSpec{}); err == nil {
					t.Fatal("reserved attempt/session reused")
				}
			}
			if creates, _, _ := g.counts(); creates != 2 {
				t.Fatalf("explicit admission/replay count=%d", creates)
			}
		})
	}
}

func testOpenShellCustodyNewAttemptRequiresSettledHistory(t *testing.T, fixture custodyFactory) {
	for _, kind := range []string{"active", "stop ACK only", "ACK without readback", "lost stop ACK", "unknown exec"} {
		t.Run(kind, func(t *testing.T) {
			g, b, security, open := fixture(t)
			c := custodyOpen(t, b, security, nil, open)
			r := custodyCreate(t, c, "original")
			if kind == "unknown exec" {
				g.mutate(func() { g.execErr = status.Error(codes.Unavailable, "exec ACK lost") })
				handle, err := c.StartOwned(t.Context(), r.Worker.Identity.Sandbox, "exec-original", openShellTestCommand())
				if handle == nil {
					t.Fatal(err)
				}
				for range handle.Output() {
				}
				if result, err := handle.Wait(); err == nil || result.ExitObserved {
					t.Fatal("unknown exec inferred complete")
				}
			}
			w, err := c.RecoverWorker(t.Context(), r.Worker.Identity.Sandbox)
			if err != nil {
				t.Fatal(err)
			}
			if kind != "active" {
				g.mutate(func() {
					switch kind {
					case "stop ACK only":
						g.stopPhase = pb.SandboxPhase_SANDBOX_PHASE_READY
					case "lost stop ACK":
						g.stopErr = status.Error(codes.Unavailable, "stop ACK lost")
					}
				})
				if kind == "ACK without readback" {
					err = c.StopSandbox(t.Context(), r.Worker.Identity.Sandbox)
				} else {
					err = w.Stop(t.Context(), c, c)
				}
				if (err != nil) != (kind == "lost stop ACK") {
					t.Fatalf("stop result=%v", err)
				}
			}
			_ = c.Close()
			c = custodyOpen(t, b, security, nil, open)
			if _, err := c.Create(t.Context(), OpenShellTask{OrderID: r.Task.OrderID, AttemptID: "explicit-new-attempt", SessionID: "explicit-new-session"}, &pb.SandboxSpec{Command: []string{"hold"}}); err == nil {
				t.Fatal("unsettled history allowed replacement VM")
			}
			if creates, _, _ := g.counts(); creates != 1 {
				t.Fatalf("unsettled history native creates=%d", creates)
			}
			if kind == "lost stop ACK" {
				recovered, err := c.RecoverWorker(t.Context(), r.Worker.Identity.Sandbox)
				if err != nil {
					t.Fatal(err)
				}
				if !recovered.Record().StopPending {
					t.Fatal("fresh Stopped cleared lost ACK")
				}
				assertOpenShellBoundary(t, recovered, OpenShellBoundaryUnknown)
			}
		})
	}
}

func testOpenShellCustodyInitialProvisioningIsNotRestart(t *testing.T, fixture custodyFactory) {
	g, b, security, open := fixture(t)
	c := custodyOpen(t, b, security, nil, open)
	g.mutate(func() {
		g.afterCreate = func() {
			for _, s := range g.sandboxes {
				s.Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_PROVISIONING
			}
		}
	})
	r := custodyCreate(t, c, "original")
	g.mutate(func() {
		g.afterCreate = nil
		s := g.sandboxes[r.Name]
		s.Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_READY
		s.Status.MainProcessInstanceId = "initial-main"
		s.Status.AgentPod = "initial-compute"
	})
	if _, err := c.ReadSandbox(t.Context(), r.Worker.Identity.Sandbox); err != nil {
		t.Fatal(err)
	}
	_ = custodyCreate(t, c, "unrelated")
	if c.Snapshot().Blocked != "" {
		t.Fatal("normal provisioning treated as restart")
	}
}

func testOpenShellCustodyCloseCancelsBlockedCreate(t *testing.T, fixture custodyFactory) {
	g, b, security, open := fixture(t)
	c := custodyOpen(t, b, security, nil, open)
	entered, release := make(chan struct{}), make(chan struct{})
	g.mutate(func() { g.afterCreate = func() { close(entered); <-release } })
	created := make(chan struct{})
	go func() {
		defer close(created)
		_, _ = c.Create(context.Background(), OpenShellTask{"blocked", "blocked", "blocked"}, &pb.SandboxSpec{Command: []string{"hold"}})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("create never reached native server")
	}
	closed := make(chan struct{})
	go func() { defer close(closed); _ = c.Close() }()
	stuck := false
	select {
	case <-closed:
	case <-time.After(time.Second):
		stuck = true
	}
	close(release)
	select {
	case <-created:
	case <-time.After(5 * time.Second):
		t.Fatal("create never returned")
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close never returned")
	}
	if stuck {
		t.Fatal("Close waited for native Create before cancellation")
	}
	if state := c.Snapshot(); state.Blocked == "" || !strings.Contains(state.Blocked, "unresolved") {
		t.Fatal("canceled Create lost unknown effect")
	}
}

func testOpenShellCustodyStoppedReadMustPersist(t *testing.T, fixture custodyFactory) {
	g, b, security, open := fixture(t)
	c := custodyOpen(t, b, security, nil, open)
	r := custodyCreate(t, c, "original")
	w, err := c.RecoverWorker(t.Context(), r.Worker.Identity.Sandbox)
	if err != nil {
		t.Fatal(err)
	}
	g.mutate(func() { g.sandboxes[r.Name].Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_STOPPED })
	path := filepath.Join(b.StateRoot, "owner.json")
	backup := filepath.Join(b.StateRoot, "before-stopped.json")
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := w.Readback(t.Context(), c); err == nil {
		t.Fatal("Stopped read certified before persistence")
	}
	assertOpenShellBoundary(t, w, OpenShellBoundaryUnknown)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	c = custodyOpen(t, b, security, nil, open)
	next := OpenShellTask{OrderID: r.Task.OrderID, AttemptID: "new-attempt", SessionID: "new-session"}
	if _, err := c.Create(t.Context(), next, &pb.SandboxSpec{Command: []string{"hold"}}); err == nil {
		t.Fatal("List replaced lost terminal Get evidence")
	}
	w, err = c.RecoverWorker(t.Context(), r.Worker.Identity.Sandbox)
	if err != nil {
		t.Fatal(err)
	}
	assertOpenShellBoundary(t, w, OpenShellBoundaryStopped)
	if _, err := c.Create(t.Context(), next, &pb.SandboxSpec{Command: []string{"hold"}}); err != nil {
		t.Fatalf("fresh durable stopped evidence rejected: %v", err)
	}
	if creates, _, _ := g.counts(); creates != 2 {
		t.Fatalf("unexpected native creates=%d", creates)
	}
}

func testOpenShellCustodyRejectsStateWithoutStoppedHistory(t *testing.T, fixture custodyFactory) {
	g, b, security, open := fixture(t)
	c := custodyOpen(t, b, security, nil, open)
	_ = custodyCreate(t, c, "original")
	prior := custodyDisk(t, b)
	prior.Version = 1 // The preceding format did not retain terminal observations.
	data, err := json.Marshal(prior)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	path := filepath.Join(b.StateRoot, "owner.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	c, err = open(t.Context(), b, security, nil)
	if c != nil {
		defer c.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "unsupported openshell custody version") {
		t.Fatalf("state missing historical guarantees admitted: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(data) {
		t.Fatal("unsupported history was rewritten or adopted")
	}
	if creates, stops, execs := g.counts(); creates != 1 || stops != 0 || execs != 0 {
		t.Fatal("unsupported history triggered native mutation")
	}
}

func TestOpenShellCustodyStoppedHistory(t *testing.T) {
	testOpenShellCustodyStoppedHistory(t, custodyFixture)
}
func TestOpenShellCustodyExplicitNewAttempt(t *testing.T) {
	testOpenShellCustodyExplicitNewAttempt(t, custodyFixture)
}
func TestOpenShellCustodyNewAttemptRequiresSettledHistory(t *testing.T) {
	testOpenShellCustodyNewAttemptRequiresSettledHistory(t, custodyFixture)
}
func TestOpenShellCustodyInitialProvisioningIsNotRestart(t *testing.T) {
	testOpenShellCustodyInitialProvisioningIsNotRestart(t, custodyFixture)
}
func TestOpenShellCustodyCloseCancelsBlockedCreate(t *testing.T) {
	testOpenShellCustodyCloseCancelsBlockedCreate(t, custodyFixture)
}

func TestOpenShellCustodyStoppedReadMustPersist(t *testing.T) {
	testOpenShellCustodyStoppedReadMustPersist(t, custodyFixture)
}

func TestOpenShellCustodyRejectsStateWithoutStoppedHistory(t *testing.T) {
	testOpenShellCustodyRejectsStateWithoutStoppedHistory(t, custodyFixture)
}
