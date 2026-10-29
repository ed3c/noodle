package runtime

import (
	"os"
	"path/filepath"
	"testing"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
)

func testOpenShellCustodyActiveInstanceReplacement(t *testing.T, fixture custodyFactory) {
	for _, action := range []string{"create", "exec", "fresh Get", "exec last Get", "reopen"} {
		t.Run(action, func(t *testing.T) {
			g, b, security, open := fixture(t)
			g.mutate(func() {
				g.afterCreate = func() {
					for _, s := range g.sandboxes {
						s.Status.MainProcessInstanceId = "original-runtime"
					}
				}
			})
			c := custodyOpen(t, b, security, nil, open)
			r := custodyCreate(t, c, "original")
			// Create's Ready response is evidence too, even before RecoverWorker/Get.
			if got := custodyDisk(t, b).Records[0].MainProcessInstanceID; got != "original-runtime" {
				t.Fatalf("native create instance not durable: %q", got)
			}
			_ = c.Close()
			c = custodyOpen(t, b, security, nil, open)
			g.mutate(func() {
				if action == "exec last Get" {
					// Owner Get returns the old generation. The transport's own final Get
					// sees the new generation and must reject it before opening an exec stream.
					g.afterGet = func() { g.sandboxes[r.Name].Status.MainProcessInstanceId = "external-runtime" }
				} else {
					g.sandboxes[r.Name].Status.MainProcessInstanceId = "external-runtime"
				}
			})
			switch action {
			case "create":
				if _, err := c.Create(t.Context(), OpenShellTask{"another", "another", "another"}, &pb.SandboxSpec{Command: []string{"hold"}}); err == nil {
					t.Fatal("native replacement allowed create")
				}
			case "exec", "exec last Get":
				handle, err := c.StartOwned(t.Context(), r.Worker.Identity.Sandbox, "exec-original", openShellTestCommand())
				if handle != nil {
					_ = handle.Close()
					t.Fatal("replacement created an exec handle")
				}
				if err == nil {
					t.Fatal("native replacement allowed exec")
				}
			case "fresh Get":
				if _, err := c.ReadSandbox(t.Context(), r.Worker.Identity.Sandbox); err == nil {
					t.Fatal("native replacement accepted by Get")
				}
			case "reopen":
				_ = c.Close()
				var err error
				c, err = open(t.Context(), b, security, nil)
				if c == nil || err == nil {
					t.Fatal("reopen adopted replacement")
				}
				defer c.Close()
			}
			if c.Snapshot().Blocked == "" || custodyDisk(t, b).Blocked == "" {
				t.Fatal("native replacement block not durable")
			}
			if creates, stops, execs := g.counts(); creates != 1 || stops != 0 || execs != 0 {
				t.Fatalf("replacement dispatched mutations: %d %d %d", creates, stops, execs)
			}
			// Restoring the old generation cannot clear a sticky block on reopen.
			g.mutate(func() { g.afterGet = nil; g.sandboxes[r.Name].Status.MainProcessInstanceId = "original-runtime" })
			_ = c.Close()
			c, err := open(t.Context(), b, security, nil)
			if c == nil || err == nil {
				t.Fatal("reopen erased instance replacement block")
			}
			defer c.Close()
		})
	}
}

func testOpenShellCustodyFirstReadyInstance(t *testing.T, fixture custodyFactory) {
	for _, origin := range []string{"Get", "inventory on open", "inventory before create", "completed before Ready"} {
		t.Run(origin, func(t *testing.T) {
			g, b, security, open := fixture(t)
			g.mutate(func() {
				g.afterCreate = func() {
					for _, s := range g.sandboxes {
						s.Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_PROVISIONING
						s.Status.AgentPod = "provisioning-driver-hint"
					}
				}
			})
			c := custodyOpen(t, b, security, nil, open)
			r := custodyCreate(t, c, "original")
			if _, err := c.ReadSandbox(t.Context(), r.Worker.Identity.Sandbox); err != nil {
				t.Fatal(err)
			}
			if got := custodyDisk(t, b).Records[0].MainProcessInstanceID; got != "" {
				t.Fatalf("provisioning established a generation: %q", got)
			}
			g.mutate(func() {
				g.afterCreate = nil
				s := g.sandboxes[r.Name]
				s.Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_READY
				s.Status.MainProcessInstanceId = "initial-ready-runtime"
				if origin == "completed before Ready" {
					s.Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_COMPLETED
				}
			})
			switch origin {
			case "Get", "completed before Ready":
				if _, err := c.ReadSandbox(t.Context(), r.Worker.Identity.Sandbox); err != nil {
					t.Fatal(err)
				}
			case "inventory on open":
				_ = c.Close()
				c = custodyOpen(t, b, security, nil, open)
			case "inventory before create":
				_ = custodyCreate(t, c, "unrelated")
			}
			if got := custodyDisk(t, b).Records[0].MainProcessInstanceID; got != "initial-ready-runtime" {
				t.Fatalf("first Ready instance not durable: %q", got)
			}
			_ = c.Close()
			c = custodyOpen(t, b, security, nil, open)
			// Driver endpoint/resource-version noise does not replace the supervisor.
			g.mutate(func() {
				s := g.sandboxes[r.Name]
				s.Metadata.ResourceVersion++
				s.Status.AgentPod = "temporary-driver-view"
				s.Status.AgentFd = "new-agent-fd"
				s.Status.SandboxFd = "new-sandbox-fd"
			})
			if _, err := c.ReadSandbox(t.Context(), r.Worker.Identity.Sandbox); err != nil {
				t.Fatalf("temporary fields treated as generation: %v", err)
			}
			before, _, _ := g.counts()
			g.mutate(func() {
				g.sandboxes[r.Name].Status.MainProcessInstanceId = "external-runtime"
				g.sandboxes[r.Name].Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_READY
			})
			if _, err := c.Create(t.Context(), OpenShellTask{"next", "next", "next"}, &pb.SandboxSpec{Command: []string{"hold"}}); err == nil {
				t.Fatal("first Ready binding lost on reopen")
			}
			if creates, _, _ := g.counts(); creates != before {
				t.Fatal("replacement created a VM")
			}
		})
	}
}

func testOpenShellCustodyDriverStopCleanup(t *testing.T, fixture custodyFactory) {
	for _, when := range []string{"at Stop", "after terminal read"} {
		t.Run(when, func(t *testing.T) {
			g, b, security, open := fixture(t)
			g.mutate(func() {
				g.afterCreate = func() {
					for _, s := range g.sandboxes {
						s.Status.MainProcessInstanceId = "original-runtime"
						s.Status.AgentPod = "original-driver-instance"
					}
				}
			})
			c := custodyOpen(t, b, security, nil, open)
			r := custodyCreate(t, c, "original")
			w, err := c.RecoverWorker(t.Context(), r.Worker.Identity.Sandbox)
			if err != nil {
				t.Fatal(err)
			}
			clear := func() {
				s := g.sandboxes[r.Name]
				s.Status.AgentPod = ""
				s.Status.AgentFd = ""
				s.Status.SandboxFd = ""
			}
			if when == "at Stop" {
				g.mutate(func() { g.afterStop = clear })
			}
			if err := w.Stop(t.Context(), c, c); err != nil {
				t.Fatalf("normal stop cleanup blocked: %v", err)
			}
			assertOpenShellBoundary(t, w, OpenShellBoundaryStopped)
			if when == "after terminal read" {
				g.mutate(clear)
			}
			_ = c.Close()
			c = custodyOpen(t, b, security, nil, open)
			if got := custodyDisk(t, b).Records[0].MainProcessInstanceID; got != "original-runtime" {
				t.Fatal("driver cleanup erased gateway generation")
			}
			g.mutate(func() { g.afterCreate = nil; g.afterStop = nil })
			if _, err := c.Create(t.Context(), OpenShellTask{OrderID: r.Task.OrderID, AttemptID: "explicit-new-attempt", SessionID: "explicit-new-session"}, &pb.SandboxSpec{Command: []string{"hold"}}); err != nil {
				t.Fatalf("normal stopped history rejected new attempt: %v", err)
			}
		})
	}
}

func testOpenShellCustodyInstancePersistenceFailure(t *testing.T, fixture custodyFactory) {
	g, b, security, open := fixture(t)
	c := custodyOpen(t, b, security, nil, open)
	r := custodyCreate(t, c, "original")
	g.mutate(func() { g.sandboxes[r.Name].Status.MainProcessInstanceId = "initial-ready-runtime" })
	path := filepath.Join(b.StateRoot, "owner.json")
	backup := filepath.Join(b.StateRoot, "before-instance.json")
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Create(t.Context(), OpenShellTask{"next", "next", "next"}, &pb.SandboxSpec{Command: []string{"hold"}}); err == nil {
		t.Fatal("create preceded durable instance binding")
	}
	if creates, _, _ := g.counts(); creates != 1 {
		t.Fatal("failed instance persistence dispatched native create")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	c = custodyOpen(t, b, security, nil, open)
	if got := custodyDisk(t, b).Records[0].MainProcessInstanceID; got != "initial-ready-runtime" {
		t.Fatal("inventory did not durably establish first Ready instance")
	}
	g.mutate(func() { g.sandboxes[r.Name].Status.MainProcessInstanceId = "external-runtime" })
	if _, err := c.StartOwned(t.Context(), r.Worker.Identity.Sandbox, "exec-original", openShellTestCommand()); err == nil {
		t.Fatal("replacement adopted after failed persistence/reopen")
	}
}

func TestOpenShellCustodyActiveInstanceReplacement(t *testing.T) {
	testOpenShellCustodyActiveInstanceReplacement(t, custodyFixture)
}
func TestOpenShellCustodyFirstReadyInstance(t *testing.T) {
	testOpenShellCustodyFirstReadyInstance(t, custodyFixture)
}
func TestOpenShellCustodyDriverStopCleanup(t *testing.T) {
	testOpenShellCustodyDriverStopCleanup(t, custodyFixture)
}
func TestOpenShellCustodyInstancePersistenceFailure(t *testing.T) {
	testOpenShellCustodyInstancePersistenceFailure(t, custodyFixture)
}
