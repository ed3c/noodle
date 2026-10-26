package runtime

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/admission"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGatewayResidentChild(t *testing.T) {
	root := os.Getenv("NOODLE_GATEWAY_TEST_CHILD")
	if root == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(root, "ready"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(root, "exit")); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func gatewayResident(t *testing.T, c *OpenShellCustody) (*admission.Residents, *admission.Budget, admission.Resident, *admission.ResidentGuard, admission.ResidentSpec) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("graceful resident shutdown unsupported")
	}
	root := t.TempDir()
	r, err := admission.OpenResidents(root)
	if err != nil {
		t.Fatal(err)
	}
	b := admission.NewBudget(3)
	b.Register("gateway-a")
	b.Register("gateway-b")
	b.Observe(admission.Sample{At: time.Now(), Unsupported: true}, nil)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	guard, err := admission.NewResidentGuard("openshell/test-original-custody", time.Second, c.Quiesce)
	if err != nil {
		t.Fatal(err)
	}
	spec := admission.ResidentSpec{Name: "gateway", Argv: []string{exe, "-test.run=^TestGatewayResidentChild$"}, Directory: root, Environment: []string{"NOODLE_GATEWAY_TEST_CHILD=" + root}, Sharing: "shared", Transport: "network"}
	v, err := r.AcquireGuarded(spec, "gateway-a", b, guard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The helper exits itself for failed-case cleanup. No adoption or signal
		// to an unrelated PID; successful release still uses Residents' native TERM.
		if err := os.WriteFile(filepath.Join(root, "exit"), nil, 0600); err != nil {
			t.Error(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := r.WaitClosed(ctx); err != nil {
			t.Error(err)
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native gateway helper never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return r, b, v, guard, spec
}

func assertGatewayRetained(t *testing.T, r *admission.Residents, b *admission.Budget, first admission.Resident, cause string) {
	t.Helper()
	rows, err := r.Snapshot()
	if err != nil || len(rows) != 1 {
		t.Fatalf("resident evidence unavailable: %+v %v", rows, err)
	}
	v := rows[0]
	if v.PID != first.PID || v.State != "running" || len(v.Contexts) != 1 || v.Contexts[0] != "gateway-a" || len(v.Released) != 0 || v.StopRequested != "" || !strings.Contains(v.Error, cause) || b.Snapshot().Allocations != 1 {
		t.Fatalf("refusal lost child/context/allocation/evidence: %+v %+v", v, b.Snapshot())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := r.WaitClosed(ctx); err == nil {
		t.Fatal("guard refusal terminated original native child")
	}
}

func testOpenShellGatewayReleaseBarrier(t *testing.T, fixture custodyFactory) {
	for _, kind := range []string{"Ready", "StopPending", "extra VM", "identity drift", "spec drift", "disconnected", "persistence", "unknown exec", "unknown create"} {
		t.Run(kind, func(t *testing.T) {
			g, binding, security, open := fixture(t)
			c := custodyOpen(t, binding, security, nil, open)
			record := custodyCreate(t, c, "original")
			r, b, first, _, _ := gatewayResident(t, c)
			cause := ""
			switch kind {
			case "Ready":
				h, err := c.StartOwned(t.Context(), record.Worker.Identity.Sandbox, "completed-exec", openShellTestCommand())
				if h == nil {
					t.Fatal(err)
				}
				for range h.Output() {
				}
				if result, err := h.Wait(); err != nil || !result.ExitObserved {
					t.Fatalf("native exec exit unavailable: %+v %v", result, err)
				}
				cause = "READY"
			case "StopPending":
				g.mutate(func() { g.stopErr = status.Error(codes.Unavailable, "lost stop ACK") })
				if err := c.StopSandbox(t.Context(), record.Worker.Identity.Sandbox); err == nil {
					t.Fatal("lost stop acknowledgement accepted")
				}
				cause = "acknowledgement unresolved"
			case "extra VM":
				g.mutate(func() {
					g.sandboxes["external"] = custodySandbox("external", "external-id", pb.SandboxPhase_SANDBOX_PHASE_STOPPED)
				})
				cause = "unregistered"
			case "identity drift":
				g.mutate(func() { g.sandboxes[record.Name].Metadata.Id = "replacement" })
				cause = "identity"
			case "spec drift":
				g.mutate(func() { g.sandboxes[record.Name].Spec.Command = []string{"replacement"} })
				cause = "specification"
			case "disconnected":
				_ = c.conn.Close()
				cause = "inventory unavailable"
			case "persistence":
				if err := c.StopSandbox(t.Context(), record.Worker.Identity.Sandbox); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(binding.StateRoot, "owner.json")
				if err := os.Rename(path, path+".backup"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				cause = "persist openshell custody"
			case "unknown exec":
				g.mutate(func() { g.execErr = status.Error(codes.Unavailable, "lost exec ACK") })
				h, err := c.StartOwned(t.Context(), record.Worker.Identity.Sandbox, "exec", openShellTestCommand())
				if h == nil {
					t.Fatal(err)
				}
				for range h.Output() {
				}
				if result, err := h.Wait(); err == nil || result.ExitObserved {
					t.Fatal("unknown exec became terminal")
				}
				if err := c.StopSandbox(t.Context(), record.Worker.Identity.Sandbox); err != nil {
					t.Fatal(err)
				}
				cause = "acknowledgement unresolved"
			case "unknown create":
				g.mutate(func() { g.createErr = status.Error(codes.Unavailable, "lost create ACK") })
				if _, err := c.Create(t.Context(), OpenShellTask{"unknown", "unknown", "unknown"}, &pb.SandboxSpec{Command: []string{"hold"}}); err == nil {
					t.Fatal("unknown create accepted")
				}
				cause = "acknowledgement unresolved"
			}
			if err := r.ReleaseContext("gateway-a"); err == nil || !strings.Contains(err.Error(), cause) {
				t.Fatalf("release bypassed %s: %v", kind, err)
			}
			assertGatewayRetained(t, r, b, first, cause)
			if kind == "Ready" {
				// Ordinary busy refusal does not seal the owner: the original task
				// can still stop and obtain a complete ACK before retrying release.
				if err := c.StopSandbox(t.Context(), record.Worker.Identity.Sandbox); err != nil {
					t.Fatal(err)
				}
				got, err := r.Release(first.ID, "gateway-a")
				if err != nil || got.State != "stopping" {
					t.Fatalf("native last lease stop: %+v %v", got, err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				if err := r.WaitClosed(ctx); err != nil || b.Snapshot().Allocations != 0 {
					t.Fatalf("original child/quota did not terminate: %v %+v", err, b.Snapshot())
				}
			}
		})
	}
}

func TestOpenShellGatewayReleaseBarrier(t *testing.T) {
	testOpenShellGatewayReleaseBarrier(t, custodyFixture)
}

func TestOpenShellGatewayReleaseBarrierInProcessMTLS(t *testing.T) {
	testOpenShellGatewayReleaseBarrier(t, custodyInProcessFixture)
}

func testOpenShellGatewaySharedGuard(t *testing.T, fixture custodyFactory) {
	g, binding, security, open := fixture(t)
	c := custodyOpen(t, binding, security, nil, open)
	_ = custodyCreate(t, c, "original")
	r, b, first, guard, spec := gatewayResident(t, c)
	second, err := r.AcquireGuarded(spec, "gateway-b", b, guard)
	if err != nil || second.PID != first.PID || b.Snapshot().Allocations != 1 {
		t.Fatalf("second context lost original owner: %+v %v", second, err)
	}
	var lists, gets int
	g.mutate(func() { lists, gets = g.lists, g.gets })
	for range 2 {
		if err := r.ReleaseContext("gateway-b"); err != nil {
			t.Fatal(err)
		}
	}
	g.mutate(func() {
		if lists != g.lists || gets != g.gets {
			t.Error("nonlast/duplicate release called VM barrier")
		}
	})
	if _, err := r.AcquireGuarded(spec, "gateway-b", b, guard); err == nil {
		t.Fatal("released context replayed")
	}
	if _, err := r.Acquire(spec, "gateway-a", b); err == nil {
		t.Fatal("ordinary Acquire bypassed custody")
	}
	reopened, err := admission.OpenResidents(spec.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Release(first.ID, "gateway-a"); err == nil {
		t.Fatal("restart without child/callback released original lease")
	}
	if _, err := reopened.Acquire(spec, "gateway-a", b); err == nil {
		t.Fatal("restart dropped persistent guard")
	}
	if _, err := r.Release(first.ID, "gateway-a"); err == nil {
		t.Fatal("last shared context bypassed Ready VM")
	}
	rows, err := r.Snapshot()
	if err != nil || rows[0].State != "running" || len(rows[0].Contexts) != 1 || rows[0].PID != first.PID || b.Snapshot().Allocations != 1 {
		t.Fatalf("shared refusal lost ownership: %+v %v", rows, err)
	}
}

func TestOpenShellGatewaySharedGuard(t *testing.T) {
	testOpenShellGatewaySharedGuard(t, custodyFixture)
}

func TestOpenShellGatewaySharedGuardInProcessMTLS(t *testing.T) {
	testOpenShellGatewaySharedGuard(t, custodyInProcessFixture)
}

func TestOpenShellGatewayGuardCancelsNativeQuery(t *testing.T) {
	g, binding, security, open := custodyInProcessFixture(t)
	c := custodyOpen(t, binding, security, nil, open)
	_ = custodyCreate(t, c, "original")
	r, b, first, _, _ := gatewayResident(t, c)
	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) })
	g.mutate(func() { g.afterGet = func() { <-unblock } })
	start := time.Now()
	if _, err := r.Release(first.ID, "gateway-a"); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("blocked native Get stalled Host: %v", err)
	}
	rows, err := r.Snapshot()
	if err != nil || rows[0].State != "running" || len(rows[0].Contexts) != 1 || b.Snapshot().Allocations != 1 {
		t.Fatalf("query timeout lost child lease: %+v %v", rows, err)
	}
}

func testOpenShellGatewayConcurrentCreateRelease(t *testing.T, fixture custodyFactory) {
	g, binding, security, open := fixture(t)
	c := custodyOpen(t, binding, security, nil, open)
	r, b, first, _, _ := gatewayResident(t, c)
	entered, unblock := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	})
	g.mutate(func() { g.afterCreate = func() { close(entered); <-unblock } })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	created, released := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := c.Create(ctx, OpenShellTask{"race", "race", "race"}, &pb.SandboxSpec{Command: []string{"hold"}})
		created <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("native create never entered")
	}
	go func() { released <- r.ReleaseContext("gateway-a") }()
	close(unblock)
	select {
	case err := <-created:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("native create stuck")
	}
	select {
	case err := <-released:
		if err == nil || !strings.Contains(err.Error(), "READY") {
			t.Fatalf("last ReleaseContext raced original create: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("last ReleaseContext stuck")
	}
	assertGatewayRetained(t, r, b, first, "READY")
	g.mutate(func() { g.afterCreate = nil })
	if err := c.StopSandbox(ctx, c.Snapshot().Records[0].Worker.Identity.Sandbox); err != nil {
		t.Fatal("busy concurrent release sealed original owner", err)
	}
}

func TestOpenShellGatewayConcurrentCreateRelease(t *testing.T) {
	testOpenShellGatewayConcurrentCreateRelease(t, custodyFixture)
}

func TestOpenShellGatewayConcurrentCreateReleaseInProcessMTLS(t *testing.T) {
	testOpenShellGatewayConcurrentCreateRelease(t, custodyInProcessFixture)
}
