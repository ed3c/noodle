package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
)

func TestHostDrainingResidentChild(t *testing.T) {
	root := os.Getenv("NOODLE_DRAINING_CHILD")
	if root == "" {
		return
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	if err := os.WriteFile(filepath.Join(root, "ready"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			if err := os.WriteFile(filepath.Join(root, "signalled"), nil, 0600); err != nil {
				t.Fatal(err)
			}
		case <-tick.C:
			if _, err := os.Stat(filepath.Join(root, "exit")); err == nil {
				return
			}
		case <-timer.C:
			t.Fatal("isolated resident owner did not release child")
		}
	}
}

func waitHostTestFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("isolated child did not write %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func drainingResidentSpec(t *testing.T) (admission.ResidentSpec, string) {
	t.Helper()
	root := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return admission.ResidentSpec{Name: "draining", Argv: []string{exe, "-test.run=^TestHostDrainingResidentChild$"}, Directory: root, Environment: []string{"NOODLE_DRAINING_CHILD=" + root}, Sharing: "context", Transport: "network"}, root
}

func hostSocketTestCall(t *testing.T, dir string, req hostRequest, result any) {
	t.Helper()
	conn, err := net.DialTimeout("unix", filepath.Join(dir, "host.sock"), time.Second)
	if err != nil {
		t.Fatalf("original Host socket unavailable: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(conn).Decode(result); err != nil {
		t.Fatalf("original Host did not respond while draining: %v", err)
	}
}

type hostShutdownTestSampler struct{}

func (hostShutdownTestSampler) Read(context.Context) (admission.Sample, error) {
	return admission.Sample{At: time.Now(), Pressure: admission.Normal, SwapUsedBytes: 0, TotalMemoryBytes: 16 << 30, CPUs: 8}, nil
}

func TestHostShutdownKeepsSocketUntilOriginalResidentExits(t *testing.T) {
	dir, err := os.MkdirTemp("", "nh-drain-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("NOODLE_HOST_DIR", dir)
	ctx, shutdown := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveHostWithSampler(ctx, hostShutdownTestSampler{}) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", filepath.Join(dir, "host.sock"), 50*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			shutdown()
			t.Fatal("isolated Host socket unavailable", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	req := hostTestRequest(t, t.TempDir())
	req.RequestFile = filepath.Join(dir, "request.json")
	original := callHost(context.Background(), req)
	if original.Status != "ok" {
		shutdown()
		t.Fatalf("attach: %+v", original)
	}
	originalBytes, err := os.ReadFile(req.RequestFile)
	if err != nil {
		t.Fatal(err)
	}
	spec, childRoot := drainingResidentSpec(t)
	defer func() {
		_ = os.WriteFile(filepath.Join(childRoot, "exit"), nil, 0600)
		shutdown()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("isolated Host did not settle after original child exit")
		}
	}()
	bound := hostRequest{Operation: "resident_acquire_bound", ProjectDir: original.ProjectDir, ContextID: original.ContextID, RequestID: original.RequestID, InstanceID: original.HostInstanceID, BinarySHA256: original.BinarySHA256, ResidentSpec: &spec}
	var acquired struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	deadline = time.Now().Add(4*admission.PollInterval + admission.SampleTimeout)
	for {
		hostSocketTestCall(t, dir, bound, &acquired)
		if acquired.Status == "running" || !strings.Contains(acquired.Error, "admission deferred") || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if acquired.Status != "running" {
		t.Fatalf("isolated service acquisition: %+v", acquired)
	}
	waitHostTestFile(t, filepath.Join(childRoot, "ready"))
	shutdown()
	waitHostTestFile(t, filepath.Join(childRoot, "signalled"))
	// Exceed the old total shutdown deadline while the original child drains.
	select {
	case err := <-done:
		done <- err
		t.Fatal("Host abandoned original resident before its exit", err)
	case <-time.After(3200 * time.Millisecond):
	}
	var resources struct {
		Shutdown struct {
			Requested bool `json:"requested"`
		} `json:"shutdown"`
	}
	hostSocketTestCall(t, dir, hostRequest{Operation: "resources"}, &resources)
	if !resources.Shutdown.Requested {
		t.Fatal("Host hid its shutdown state")
	}
	bound.Operation, bound.ResidentSpec = "status", nil
	bound.ConfigSHA256, bound.ConfigPath, bound.Mode = original.ConfigSHA256, original.ConfigPath, original.Mode
	var observed hostResponse
	hostSocketTestCall(t, dir, bound, &observed)
	if observed.Status != "unknown_effect" || observed.ContextState != "failed" || observed.ContextID != original.ContextID || observed.TerminalCause != "" {
		t.Fatalf("shutdown changed the original task outcome: %+v", observed)
	}
	var refused hostResponse
	hostSocketTestCall(t, dir, hostRequest{Operation: "attach"}, &refused)
	if refused.Status != "refused" {
		t.Fatalf("draining admitted new work: %+v", refused)
	}
	after, err := os.ReadFile(req.RequestFile)
	if err != nil || string(after) != string(originalBytes) {
		t.Fatal("shutdown modified original request", err)
	}
}

func newHostShutdownFixture(t *testing.T) (*noodleHost, *hostContext) {
	t.Helper()
	dir := t.TempDir()
	r, err := admission.OpenResidents(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := admission.NewBudget(1)
	b.Register("context")
	for offset := 3; offset >= 0; offset-- {
		b.Observe(admission.Sample{At: time.Now().Add(-time.Duration(offset) * admission.PollInterval), Pressure: admission.Normal}, nil)
	}
	r.BindBudget(b)
	project := t.TempDir()
	lock, err := lockfile.TryLock(filepath.Join(project, "context.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := &hostContext{receipt: hostReceipt{ContextID: "context", RequestID: "request", ProjectDir: project, HostInstanceID: "host", BinarySHA256: "binary", ContextState: "active"}, runtime: &fakeHostRuntime{}, done: make(chan error, 1), cancel: cancel, lock: lock}
	h := &noodleHost{dir: dir, instanceID: "host", binarySHA: "binary", contexts: map[string]*hostContext{"context": c}, receipts: []hostReceipt{c.receipt}, residents: r, budget: b}
	return h, c
}

func hostShutdownResultForTest(t *testing.T, h *noodleHost) hostShutdownResult {
	t.Helper()
	select {
	case result := <-h.shutdown.results:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("original finalization did not return")
		return hostShutdownResult{}
	}
}

func TestHostShutdownReleaseGuardDoesNotBlockActorAndRequiresExactRetry(t *testing.T) {
	h, c := newHostShutdownFixture(t)
	spec, childRoot := drainingResidentSpec(t)
	entered, answer := make(chan struct{}, 2), make(chan error, 2)
	guard, err := admission.NewResidentGuard("original", time.Second, func(ctx context.Context) error {
		entered <- struct{}{}
		select {
		case err := <-answer:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	live, err := h.residents.AcquireGuarded(spec, c.receipt.ContextID, h.budget, guard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(childRoot, "exit"), nil, 0600)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := h.residents.WaitClosed(ctx); err != nil {
			t.Error(err)
		}
	})
	waitHostTestFile(t, filepath.Join(childRoot, "ready"))
	service, cancelService := context.WithCancel(context.Background())
	defer cancelService()
	h.beginShutdown(service)
	originalShutdown := h.shutdown
	h.beginShutdown(service)
	if h.shutdown != originalShutdown {
		t.Fatal("repeated signal replaced the shutdown owner")
	}
	if err := h.shutdownReady(); err == nil {
		t.Fatal("Host ignored original Run completion")
	}
	h.completeContext(c, context.Canceled)
	<-entered
	reply := make(chan any, 1)
	if h.handleShutdownCall(hostRequest{Operation: "resources"}, reply) {
		t.Fatal("draining blocked resource readback")
	}
	if h.resources() == nil || h.shutdownReady() == nil {
		t.Fatal("pending guard did not retain original resource ownership")
	}
	answer <- errors.New("native VM stop unconfirmed")
	h.applyShutdownResult(hostShutdownResultForTest(t, h))
	if c.releaseErr == nil || !c.receiptSaved || !c.failed || c.receipt.ContextState != "failed" {
		t.Fatalf("release failure was lost: %+v", c)
	}
	req := hostRequest{Operation: "resident_release_bound", ProjectDir: c.receipt.ProjectDir, ContextID: c.receipt.ContextID, RequestID: c.receipt.RequestID, InstanceID: h.instanceID, BinarySHA256: h.binarySHA, ResidentID: live.ID}
	for _, alter := range []func(*hostRequest){
		func(r *hostRequest) { r.RequestID = "foreign" },
		func(r *hostRequest) { r.ConfigSHA256 = "changed" },
		func(r *hostRequest) { r.ResidentID = "different-service" },
		func(r *hostRequest) { r.InstanceID = "replacement" },
		func(r *hostRequest) { r.Operation = "resident_acquire_bound" },
	} {
		foreign := req
		alter(&foreign)
		if !h.handleShutdownCall(foreign, reply) {
			t.Fatal("foreign mutation fell through shutdown guard")
		}
		response := <-reply
		switch response := response.(type) {
		case map[string]any:
			if response["status"] != "refused" {
				t.Fatal(response)
			}
		case hostResponse:
			if response.Status != "refused" {
				t.Fatal(response)
			}
		default:
			t.Fatalf("unexpected refusal: %#v", response)
		}
	}
	if !h.handleShutdownCall(req, reply) {
		t.Fatal("original release was not handled")
	}
	<-entered
	if _, err := h.shutdownReleaseOwner(req); err == nil {
		t.Fatal("concurrent release was admitted")
	}
	answer <- nil
	h.applyShutdownResult(hostShutdownResultForTest(t, h))
	result := (<-reply).(map[string]any)
	if result["status"] != "stopping" || c.releaseErr != nil || !c.failed || c.receipt.ContextState != "failed" {
		t.Fatalf("release retry changed task result: %+v %+v", result, c.receipt)
	}
	if h.shutdownReady() == nil {
		t.Fatal("signal acknowledgement replaced original child exit")
	}
	if err := os.WriteFile(filepath.Join(childRoot, "exit"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.residents.Released():
	case <-time.After(3 * time.Second):
		t.Fatal("original resident exit not observed")
	}
	if err := h.shutdownReady(); err != nil {
		t.Fatal(err)
	}
	if c.runtime.(*fakeHostRuntime).shutdowns != 1 {
		t.Fatal("explicit resource retry restarted runtime shutdown")
	}
}

func TestHostShutdownRetainsUnresolvedRecordsAndFailedPersistence(t *testing.T) {
	for _, cause := range []string{"owner-record", "receipt"} {
		t.Run(cause, func(t *testing.T) {
			h, c := newHostShutdownFixture(t)
			if cause == "owner-record" {
				data, err := json.Marshal(map[string]admission.Resident{"lost": {ID: "lost", Name: "original", State: "running"}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(h.dir, "residents.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
				h.residents, err = admission.OpenResidents(h.dir)
				if err != nil {
					t.Fatal(err)
				}
				// No child exists: WaitClosed succeeds, but the original record is unresolved.
				if err := h.residents.WaitClosed(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else {
				h.dir = filepath.Join(h.dir, "missing")
			}
			service, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.beginShutdown(service)
			h.completeContext(c, context.Canceled)
			h.applyShutdownResult(hostShutdownResultForTest(t, h))
			if err := h.shutdownReady(); err == nil {
				t.Fatal("unresolved responsibility allowed Host exit")
			} else if cause == "receipt" && !strings.Contains(err.Error(), "persistence") {
				t.Fatal(err)
			}
		})
	}
}

func TestHostShutdownBoundReleaseRepersistsOriginalFailedReceipt(t *testing.T) {
	h, c := newHostShutdownFixture(t)
	data, err := json.Marshal(map[string]admission.Resident{"original-service": {
		ID: "original-service", Name: "original", State: "exited", Released: []string{c.receipt.ContextID},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, "residents.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	h.residents, err = admission.OpenResidents(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	// Refuse only Host receipt writes; original resident persistence still works.
	h.dir = filepath.Join(h.dir, "repaired-receipts")
	service, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.beginShutdown(service)
	h.completeContext(c, nil)
	h.applyShutdownResult(hostShutdownResultForTest(t, h))
	if c.receiptSaved || !c.failed || h.shutdownReady() == nil {
		t.Fatal("failed receipt write did not retain original owner")
	}
	original := c.receipt
	req := hostRequest{Operation: "resident_release_bound", ProjectDir: original.ProjectDir, ContextID: original.ContextID, RequestID: original.RequestID, InstanceID: h.instanceID, BinarySHA256: h.binarySHA, ResidentID: "original-service"}
	reply := make(chan any, 1)
	for _, repaired := range []bool{false, true} {
		if repaired {
			if err := os.Mkdir(h.dir, 0700); err != nil {
				t.Fatal(err)
			}
		}
		if !h.handleShutdownCall(req, reply) {
			t.Fatal("explicit original release was not handled")
		}
		h.applyShutdownResult(hostShutdownResultForTest(t, h))
		response := (<-reply).(map[string]any)
		if c.receiptSaved != repaired || c.receipt != original || !c.failed || c.releaseErr != nil || c.runtime.(*fakeHostRuntime).shutdowns != 1 {
			t.Fatalf("receipt retry changed task outcome or resource proof: %+v %+v", response, c)
		}
		if !repaired && (response["status"] != "refused" || h.shutdownReady() == nil) {
			t.Fatalf("unpersisted retry allowed exit: %+v", response)
		}
	}
	if err := h.shutdownReady(); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(h.dir, "receipts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved []hostReceipt
	if err := json.Unmarshal(data, &saved); err != nil || len(saved) != 1 || saved[0] != original {
		t.Fatalf("original failed receipt was not preserved: %s %v", data, err)
	}
}
