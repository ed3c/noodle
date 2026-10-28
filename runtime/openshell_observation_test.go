package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
)

func completedObservationFixture(t *testing.T) (*custodyGateway, *OpenShellCustody, OpenShellCustodyBinding, OpenShellIdentity) {
	t.Helper()
	g, b, security, open := custodyInProcessFixture(t)
	g.mutate(func() {
		g.afterCreate = func() {
			for _, s := range g.sandboxes {
				s.Status.MainProcessInstanceId = "original-main"
				s.Status.AgentPod = "original-driver"
			}
		}
	})
	c := custodyOpen(t, b, security, nil, open)
	name, err := c.Create(t.Context(), OpenShellTask{OrderID: "original-order", AttemptID: "native-task-attempt", SessionID: "original-session"}, &pb.SandboxSpec{Command: []string{"private-spec-value"}})
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	r := &c.state.Records[0]
	r.Exec = &OpenShellCustodyExec{AttemptID: "independent-exec-attempt", Command: OpenShellCommand{RequestID: openShellRequestID, Argv: []string{"private-command"}, Env: map[string]string{"SECRET": "private-environment"}}, Result: OpenShellExecResult{RequestID: openShellRequestID, StartAttempted: true, ExitObserved: true, ExitCode: 0}}
	r.Worker.ExecResult = r.Exec.Result
	r.StopRequestID = "d593da59-10b3-477d-8760-c01d52139a47"
	r.StopAcknowledged = true
	r.Stopped = &OpenShellStoppedObservation{ComputeInstanceID: "original-driver"}
	identity := r.Worker.Identity
	err = c.save()
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	g.mutate(func() { g.sandboxes[name].Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_STOPPED })
	return g, c, b, identity
}

type observationBefore struct {
	state                              OpenShellCustodyState
	disk                               []byte
	info                               os.FileInfo
	creates, stops, execs, lists, gets int
}

func beforeObservation(t *testing.T, g *custodyGateway, c *OpenShellCustody, b OpenShellCustodyBinding) observationBefore {
	t.Helper()
	before := observationBefore{state: c.Snapshot()}
	var err error
	before.disk, err = os.ReadFile(filepath.Join(b.StateRoot, "owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	before.info, err = os.Stat(filepath.Join(b.StateRoot, "owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	before.creates, before.stops, before.execs, before.lists, before.gets = g.creates, g.stops, g.execs, g.lists, g.gets
	g.mu.Unlock()
	return before
}

func assertObservationReadOnly(t *testing.T, g *custodyGateway, c *OpenShellCustody, b OpenShellCustodyBinding, before observationBefore, gets int) {
	t.Helper()
	after := beforeObservation(t, g, c, b)
	if !reflect.DeepEqual(before.state, after.state) || !bytes.Equal(before.disk, after.disk) || !os.SameFile(before.info, after.info) || !before.info.ModTime().Equal(after.info.ModTime()) {
		t.Fatal("completion observation changed memory or durable custody ledger")
	}
	if before.creates != after.creates || before.stops != after.stops || before.execs != after.execs || before.lists != after.lists || after.gets-before.gets != gets {
		t.Fatalf("completion observation performed unexpected RPCs: before=%+v after=%+v", before, after)
	}
}

func TestOpenShellObserveCompletedFreshReadOnly(t *testing.T) {
	g, c, b, identity := completedObservationFixture(t)
	before := beforeObservation(t, g, c, b)
	for i := 0; i < 2; i++ {
		observed, err := c.ObserveCompleted(t.Context(), identity)
		if err != nil {
			t.Fatal(err)
		}
		if observed.Identity != identity || observed.Task != before.state.Records[0].Task || observed.ExecAttemptID != "independent-exec-attempt" || observed.ExecRequestID != openShellRequestID || observed.ExecExitCode != 0 || observed.MainProcessInstanceID != "original-main" || observed.Phase != "Stopped" || !observed.CreateAcknowledged || !observed.StopAcknowledged {
			t.Fatalf("observation=%+v", observed)
		}
		encoded, err := json.Marshal(observed)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(encoded, []byte("private-")) {
			t.Fatalf("observation exposed private specification or command: %s", encoded)
		}
	}
	assertObservationReadOnly(t, g, c, b, before, 2)
	g.mutate(func() { g.sandboxes[identity.SandboxName].Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_READY })
	if _, err := c.ObserveCompleted(t.Context(), identity); err == nil {
		t.Fatal("historical success replaced fresh stopped evidence")
	}
	assertObservationReadOnly(t, g, c, b, before, 3)
}

func TestOpenShellObserveCompletedNativeDriftDoesNotBlockOrSave(t *testing.T) {
	for _, drift := range []string{"missing", "id", "name", "workspace", "spec", "missing spec", "main", "missing main", "phase", "driver"} {
		t.Run(drift, func(t *testing.T) {
			g, c, b, identity := completedObservationFixture(t)
			g.mutate(func() {
				s := g.sandboxes[identity.SandboxName]
				switch drift {
				case "missing":
					delete(g.sandboxes, identity.SandboxName)
				case "id":
					s.Metadata.Id = "replacement"
				case "name":
					s.Metadata.Name = "replacement"
				case "workspace":
					s.Metadata.Workspace = "replacement"
				case "spec":
					s.Spec.Command = []string{"replacement"}
				case "missing spec":
					s.Spec = nil
				case "main":
					s.Status.MainProcessInstanceId = "replacement"
				case "missing main":
					s.Status.MainProcessInstanceId = ""
				case "phase":
					s.Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_COMPLETED
				case "driver":
					s.Status.AgentPod = "replacement"
				}
			})
			before := beforeObservation(t, g, c, b)
			if _, err := c.ObserveCompleted(t.Context(), identity); err == nil {
				t.Fatal("native drift accepted")
			}
			assertObservationReadOnly(t, g, c, b, before, 1)
		})
	}
}

func TestOpenShellObserveCompletedPermitsStoppedDriverCleanup(t *testing.T) {
	g, c, b, identity := completedObservationFixture(t)
	g.mutate(func() {
		s := g.sandboxes[identity.SandboxName]
		s.Status.AgentPod = ""
		s.Status.AgentFd = "temporary"
		s.Status.SandboxFd = "temporary"
		s.Metadata.ResourceVersion++
	})
	before := beforeObservation(t, g, c, b)
	if _, err := c.ObserveCompleted(t.Context(), identity); err != nil {
		t.Fatal(err)
	}
	assertObservationReadOnly(t, g, c, b, before, 1)
}

func TestOpenShellObserveCompletedRejectsUnresolvedLocalEvidence(t *testing.T) {
	for _, missing := range []string{"not initialized", "blocked", "fault", "retained", "create ack", "create request", "exec", "exec attempt", "exec request", "exec binding", "not submitted", "exec exit", "failed exit", "input error", "stream error", "stop ack", "stop request", "stop pending", "stop error", "stopped history", "main identity", "task identity"} {
		t.Run(missing, func(t *testing.T) {
			g, c, b, identity := completedObservationFixture(t)
			c.mu.Lock()
			r := &c.state.Records[0]
			switch missing {
			case "not initialized":
				c.initialized = false
			case "blocked":
				c.state.Blocked = "existing block"
			case "fault":
				c.fault = errors.New("existing fault")
			case "retained":
				r.Retained = true
			case "create ack":
				r.CreateAcknowledged = false
			case "create request":
				r.CreateRequestID = ""
			case "exec":
				r.Exec = nil
			case "exec attempt":
				r.Exec.AttemptID = ""
			case "exec request":
				r.Exec.Command.RequestID = ""
			case "exec binding":
				r.Exec.Result.RequestID = "another request"
			case "not submitted":
				r.Exec.Result.StartAttempted = false
			case "exec exit":
				r.Exec.Result.ExitObserved = false
			case "failed exit":
				r.Exec.Result.ExitCode = 7
			case "input error":
				r.Exec.Result.InputError = "input unavailable"
			case "stream error":
				r.Exec.Result.StreamError = "stream unavailable"
			case "stop ack":
				r.StopAcknowledged = false
			case "stop request":
				r.StopRequestID = ""
			case "stop pending":
				r.Worker.StopPending = true
			case "stop error":
				r.Worker.StopError = "stop unavailable"
			case "stopped history":
				r.Stopped = nil
			case "main identity":
				r.MainProcessInstanceID = ""
			case "task identity":
				r.Task.AttemptID = "canonical-attempt-is-not-native-attempt"
			}
			fault, initialized := c.fault, c.initialized
			c.mu.Unlock()
			before := beforeObservation(t, g, c, b)
			if _, err := c.ObserveCompleted(t.Context(), identity); err == nil {
				t.Fatal("unresolved local evidence accepted")
			}
			assertObservationReadOnly(t, g, c, b, before, 0)
			c.mu.Lock()
			if c.fault != fault || c.initialized != initialized {
				t.Error("observation changed custody availability")
			}
			c.mu.Unlock()
		})
	}
}

func TestOpenShellObserveCompletedOriginalIdentityAndBusyGate(t *testing.T) {
	g, c, b, identity := completedObservationFixture(t)
	before := beforeObservation(t, g, c, b)
	wrong := identity
	wrong.AttemptID = "canonical-attempt-is-not-native-attempt"
	if _, err := c.ObserveCompleted(t.Context(), wrong); err == nil {
		t.Fatal("wrong original identity accepted")
	}
	c.mu.Lock()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	_, err := c.ObserveCompleted(ctx, identity)
	cancel()
	c.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("busy custody waited or succeeded: %v", err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	if _, err := c.ObserveCompleted(ctx, identity); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled observation: %v", err)
	}
	assertObservationReadOnly(t, g, c, b, before, 0)
}

func TestOpenShellObserveCompletedCancelledNativeReadIsReadOnly(t *testing.T) {
	g, c, b, identity := completedObservationFixture(t)
	before := beforeObservation(t, g, c, b)
	started, release := make(chan struct{}), make(chan struct{})
	g.mutate(func() { g.afterGet = func() { close(started); <-release } })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := c.ObserveCompleted(ctx, identity); done <- err }()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("cancelled native read accepted")
		}
	case <-time.After(time.Second):
		t.Error("native read ignored cancellation")
	}
	close(release)
	assertObservationReadOnly(t, g, c, b, before, 1)
}

func TestOpenShellObserveCompletedConcurrentReads(t *testing.T) {
	g, c, b, identity := completedObservationFixture(t)
	before := beforeObservation(t, g, c, b)
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := c.ObserveCompleted(t.Context(), identity); results <- err }()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !strings.Contains(err.Error(), "in progress") {
			t.Error(err)
		}
	}
	if success == 0 {
		t.Fatal("no completion observation succeeded")
	}
	assertObservationReadOnly(t, g, c, b, before, success)
}

func TestOpenShellObserveCompletedGatewayDoesNotStartUnreadyOwner(t *testing.T) {
	f := newManagedFixture(t, false)
	o := f.open(t)
	before, err := os.ReadFile(filepath.Join(o.spec.Binding.StateRoot, "owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.ObserveCompleted(t.Context(), "a", OpenShellIdentity{}); err == nil {
		t.Fatal("unready gateway observed completion")
	}
	f.spawns(t, 0)
	rows, err := f.residents.Snapshot()
	if err != nil || len(rows) != 0 {
		t.Fatalf("observation acquired resident: %+v %v", rows, err)
	}
	after, err := os.ReadFile(filepath.Join(o.spec.Binding.StateRoot, "owner.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("unready observation changed custody")
	}
}

func TestOpenShellObserveCompletedGatewayRejectsReleasedLease(t *testing.T) {
	f := newManagedFixture(t, true)
	o := f.open(t)
	if _, err := o.Acquire(t.Context(), "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Acquire(t.Context(), "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Release("a"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(f.root, "residents.json"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := os.ReadFile(filepath.Join(f.root, "events"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.ObserveCompleted(t.Context(), "a", OpenShellIdentity{}); err == nil || !strings.Contains(err.Error(), "lease") {
		t.Fatalf("released context accepted: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(f.root, "residents.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("observation renewed released context")
	}
	afterEvents, err := os.ReadFile(filepath.Join(f.root, "events"))
	if err != nil || !bytes.Equal(events, afterEvents) {
		t.Fatal("released context performed native RPC")
	}
	f.spawns(t, 1)
}
