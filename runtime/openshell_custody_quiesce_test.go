package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
)

func testOpenShellCustodyQuiesce(t *testing.T, fixture custodyFactory) {
	t.Run("all fresh reads and sealed effects", func(t *testing.T) {
		g, b, security, open := fixture(t)
		c := custodyOpen(t, b, security, nil, open)
		a := custodyCreate(t, c, "a")
		z := custodyCreate(t, c, "z")
		var before int
		g.mutate(func() { before = g.gets })
		if err := c.Quiesce(t.Context()); err == nil {
			t.Fatal("Ready VMs allowed gateway release")
		}
		g.mutate(func() {
			if g.gets-before != 2 {
				t.Errorf("busy check omitted fresh Gets: %d", g.gets-before)
			}
		})
		for _, r := range []OpenShellCustodyRecord{a, z} {
			if err := c.StopSandbox(t.Context(), r.Worker.Identity.Sandbox); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.Quiesce(t.Context()); err != nil {
			t.Fatal(err)
		}
		creates, stops, execs := g.counts()
		if _, err := c.Create(t.Context(), OpenShellTask{"new", "new", "new"}, &pb.SandboxSpec{}); err == nil || !strings.Contains(err.Error(), "quiesced") {
			t.Fatalf("sealed create: %v", err)
		}
		if _, err := c.StartOwned(t.Context(), a.Worker.Identity.Sandbox, "exec", openShellTestCommand()); err == nil || !strings.Contains(err.Error(), "quiesced") {
			t.Fatalf("sealed exec: %v", err)
		}
		if err := c.StopSandbox(t.Context(), a.Worker.Identity.Sandbox); err == nil || !strings.Contains(err.Error(), "quiesced") {
			t.Fatalf("sealed stop: %v", err)
		}
		if ca, st, ex := g.counts(); ca != creates || st != stops || ex != execs {
			t.Fatal("quiesced owner sent new effects")
		}
		if got, err := c.ReadSandbox(t.Context(), a.Worker.Identity.Sandbox); err != nil || got.Phase != "Stopped" {
			t.Fatalf("sealed readback unavailable: %+v %v", got, err)
		}
		var lists int
		g.mutate(func() { before, lists = g.gets, g.lists })
		if err := c.Quiesce(t.Context()); err != nil {
			t.Fatal(err)
		}
		g.mutate(func() {
			if g.gets-before != 2 || g.lists-lists != 2 {
				t.Errorf("repeat reused PASS or skipped pages/Gets: gets=%d lists=%d", g.gets-before, g.lists-lists)
			}
			g.sandboxes[z.Name].Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_READY
		})
		if err := c.Quiesce(t.Context()); err == nil || c.Snapshot().Blocked == "" {
			t.Fatal("repeat PASS hid stopped history drift")
		}
	})
	t.Run("empty inventory still fresh", func(t *testing.T) {
		g, b, security, open := fixture(t)
		c := custodyOpen(t, b, security, nil, open)
		if err := c.Quiesce(t.Context()); err != nil {
			t.Fatal(err)
		}
		g.mutate(func() { g.sandboxes["extra"] = custodySandbox("extra", "extra", pb.SandboxPhase_SANDBOX_PHASE_STOPPED) })
		if err := c.Quiesce(t.Context()); err == nil {
			t.Fatal("empty ledger substituted for native inventory")
		}
	})
	t.Run("retained same ID Get after List", func(t *testing.T) {
		g, b, security, open := fixture(t)
		g.mutate(func() {
			g.sandboxes["retained"] = custodySandbox("retained", "original", pb.SandboxPhase_SANDBOX_PHASE_STOPPED)
		})
		c := custodyOpen(t, b, security, []OpenShellRetainedSandbox{{"retained", "original", b.Workspace}}, open)
		if err := c.Quiesce(t.Context()); err != nil {
			t.Fatal(err)
		}
		g.mutate(func() {
			g.afterList = func() { g.sandboxes["retained"].Metadata.Id = "replaced-after-list" }
		})
		if err := c.Quiesce(t.Context()); err == nil || !strings.Contains(err.Error(), "identity") {
			t.Fatalf("retained Get skipped or adopted new ID: %v", err)
		}
	})
	t.Run("new instance does not inherit sealing", func(t *testing.T) {
		_, b, security, open := fixture(t)
		c := custodyOpen(t, b, security, nil, open)
		if err := c.Quiesce(t.Context()); err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
		c = custodyOpen(t, b, security, nil, open)
		if len(c.Snapshot().Records) != 0 {
			t.Fatal("reopen replayed effects")
		}
		_ = custodyCreate(t, c, "explicit-new-instance")
	})
	for _, winner := range []string{"create", "quiesce"} {
		t.Run("concurrent "+winner+" first", func(t *testing.T) {
			g, b, security, open := fixture(t)
			c := custodyOpen(t, b, security, nil, open)
			entered, unblock := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-unblock:
				default:
					close(unblock)
				}
			})
			g.mutate(func() {
				hook := func() { close(entered); <-unblock }
				if winner == "create" {
					g.afterCreate = hook
				} else {
					g.afterList = hook
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			created, checked := make(chan error, 1), make(chan error, 1)
			create := func() {
				_, err := c.Create(ctx, OpenShellTask{"race", "race", "race"}, &pb.SandboxSpec{Command: []string{"hold"}})
				created <- err
			}
			check := func() { checked <- c.Quiesce(ctx) }
			if winner == "create" {
				go create()
			} else {
				go check()
			}
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("first native operation never entered")
			}
			if winner == "create" {
				go check()
			} else {
				go create()
			}
			close(unblock)
			var createErr, checkErr error
			select {
			case createErr = <-created:
			case <-ctx.Done():
				t.Fatal("concurrent create stuck")
			}
			select {
			case checkErr = <-checked:
			case <-ctx.Done():
				t.Fatal("concurrent quiesce stuck")
			}
			if winner == "create" {
				if createErr != nil || checkErr == nil {
					t.Fatalf("create first: create=%v quiesce=%v", createErr, checkErr)
				}
				g.mutate(func() { g.afterCreate = nil })
				if err := c.StopSandbox(ctx, c.Snapshot().Records[0].Worker.Identity.Sandbox); err != nil {
					t.Fatal("busy race permanently sealed owner", err)
				}
			} else {
				if checkErr != nil || createErr == nil || !strings.Contains(createErr.Error(), "quiesced") {
					t.Fatalf("quiesce first: create=%v quiesce=%v", createErr, checkErr)
				}
				if n, _, _ := g.counts(); n != 0 {
					t.Fatal("create slipped past successful quiesce")
				}
			}
		})
	}
}

func TestOpenShellCustodyQuiesce(t *testing.T) {
	testOpenShellCustodyQuiesce(t, custodyFixture)
}

func TestOpenShellCustodyQuiesceInProcessMTLS(t *testing.T) {
	testOpenShellCustodyQuiesce(t, custodyInProcessFixture)
}
