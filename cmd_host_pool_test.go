package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/loop"
)

func TestRollingControlRoutesToOldOwnerFromNewFrontend(t *testing.T) {
	root := t.TempDir()
	oldID, newID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	receipt := hostReceipt{RequestID: "original-request", ProjectDir: "/original/worktree", Mode: "manual", ConfigSHA256: "config", BinarySHA256: "source", HostInstanceID: oldID}
	if err := updateHostPool(root, func(p *hostPool) error {
		p.Active = newID
		for id, sha := range map[string]string{oldID: "source", newID: "selected"} {
			p.Generations[id] = hostGeneration{ID: id, Directory: filepath.Join(root, "g", id), SHA: sha}
		}
		p.Owners[receipt.RequestID] = hostTaskOwner{Original: receipt, Current: receipt, State: "owned"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	binding := hostRequestBinding{RequestID: receipt.RequestID, ProjectDir: receipt.ProjectDir, Mode: receipt.Mode, ConfigSHA256: receipt.ConfigSHA256, BinarySHA256: receipt.BinarySHA256}
	if !poolBindingCompatible(root, binding, "selected") {
		t.Fatal("new frontend cannot control the still-running original round")
	}
	req := hostRequest{RequestID: receipt.RequestID, ProjectDir: receipt.ProjectDir, Mode: receipt.Mode, ConfigSHA256: receipt.ConfigSHA256}
	g, err := poolRequestOwner(root, req)
	if err != nil || g.ID != oldID {
		t.Fatalf("original round was rerouted to the new executor: %+v %v", g, err)
	}
	if poolBindingCompatible(root, binding, "foreign") {
		t.Fatal("unregistered frontend consumed original task")
	}
	binding.ConfigSHA256 = "changed"
	if poolBindingCompatible(root, binding, "selected") {
		t.Fatal("version selection authorized a changed task")
	}
	req.RequestID = "new-request"
	g, err = poolRequestOwner(root, req)
	if err != nil || g.ID != newID {
		t.Fatalf("new work did not select new generation: %+v %v", g, err)
	}
}

type rollingBusyRuntime struct{ fakeHostRuntime }

func (f *rollingBusyRuntime) ReleaseForSwitch(context.Context) (loop.HostContinuation, error) {
	err := f.release(loop.TerminalMigrationDetach)
	return loop.HostContinuation{Mode: "supervised", DispatchState: loop.StatePaused}, err
}
func (f *rollingBusyRuntime) HostRoundBoundary() (loop.HostContinuation, bool) {
	return loop.HostContinuation{Mode: "supervised", DispatchState: loop.StatePaused}, f.terminalCause == loop.TerminalMigrationDetach
}

func TestRollingBusyRoundStaysWithOldRuntimeUntilNativeRelease(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "nround-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	oldID, nextID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	// The socket is intentionally outside t.TempDir for macOS path limits.
	socketDir, err := os.MkdirTemp("/tmp", "nround-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	listener, err := net.Listen("unix", filepath.Join(socketDir, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err = updateHostPool(root, func(p *hostPool) error {
		p.Active = oldID
		for id, sha := range map[string]string{oldID: "old", nextID: "new"} {
			p.Generations[id] = hostGeneration{ID: id, Directory: filepath.Join(root, "g", id), SHA: sha, State: "active"}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	budget := admission.NewBudget(4)
	if err = budget.BindSharedQuota(filepath.Join(root, "quota"), oldID); err != nil {
		t.Fatal(err)
	}
	observeKnownHostFixtureSamples(budget)
	rt := &rollingBusyRuntime{fakeHostRuntime{ready: make(chan struct{}), released: make(chan struct{}), busy: true}}
	h := &noodleHost{dir: root, instanceID: oldID, binaryPath: "/old/noodle", binarySHA: "old", budget: budget,
		rolling:    &hostRolling{root: root, carry: map[string]rollingCarry{}},
		newRuntime: func(string, string, config.Config, loop.Dependencies) hostRuntime { return rt }}
	req := hostTestRequest(t, t.TempDir())
	req.BinarySHA256, req.AdmissionSHA, req.AdmissionPath = "old", "old", "/old/noodle"
	// Record the exact original binary in the native family before admission.
	if err = updateHostPool(root, func(p *hostPool) error {
		g := p.Generations[oldID]
		g.Binary = "/old/noodle"
		p.Generations[oldID] = g
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response := h.attachRolling(ctx, req, nil)
	if response.Status != "ok" {
		t.Fatalf("attach: %+v", response)
	}
	worker, err := budget.TryAcquire(response.ContextID)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Release()
	if err = updateHostPool(root, func(p *hostPool) error {
		p.Active = nextID
		g := p.Generations[oldID]
		g.State = "retiring"
		p.Generations[oldID] = g
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if h.advanceRolling(context.Background()) || len(h.contexts) != 1 || rt.shutdowns != 0 || budget.Snapshot().OwnedAllocations != 1 {
		t.Fatal("busy old round was stopped or its allocation was released")
	}
	if _, err = budget.TryAcquire(response.ContextID); !errors.Is(err, admission.ErrDeferred) {
		t.Fatal("old generation dispatched the next round")
	}
	// The fixture models the exit producer returning its permit before detach.
	worker.Release()
	rt.busy = false
	// Use the native generation path for validation, a short symlink for transport.
	nextDir := filepath.Join(root, "g", nextID)
	if err = os.MkdirAll(nextDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(socketDir, "host.sock"), filepath.Join(nextDir, "host.sock")); err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		var next hostRequest
		if err = json.NewDecoder(conn).Decode(&next); err != nil {
			serverDone <- err
			return
		}
		if next.RequestID != req.RequestID || next.RoundState == nil || next.RoundState.DispatchState != loop.StatePaused {
			serverDone <- errors.New("original round identity/hold lost")
			return
		}
		current := response.hostReceipt
		current.HostInstanceID = nextID
		current.ContextID = "next-native-context"
		current.BinarySHA256 = "new"
		err = updateHostPool(root, func(p *hostPool) error {
			o := p.Owners[req.RequestID]
			o.Current = current
			o.State = "owned"
			p.Owners[req.RequestID] = o
			return nil
		})
		if err == nil {
			err = json.NewEncoder(conn).Encode(hostResponse{current, "ok", ""})
		}
		serverDone <- err
	}()
	if !h.advanceRolling(context.Background()) {
		t.Fatalf("quiescent round did not converge: %s", h.rolling.failure)
	}
	if err = <-serverDone; err != nil {
		t.Fatal(err)
	}
	if len(h.contexts) != 0 || budget.Snapshot().OwnedAllocations != 0 || rt.shutdowns != 1 {
		t.Fatal("old runtime retained after confirmed native release")
	}
}

func TestRollingLostAckReconcilesOriginalReceiptWithoutDispatch(t *testing.T) {
	root := t.TempDir()
	oldID, nextID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	original := hostReceipt{Schema: 1, ProjectDir: "/original/worktree", RequestID: "request", Mode: "supervised", ConfigSHA256: "config", BinarySHA256: "old", HostInstanceID: oldID, ContextID: "original-context", ContextState: "detached", TerminalCause: loop.TerminalMigrationDetach}
	current := original
	current.HostInstanceID, current.ContextID, current.BinarySHA256 = nextID, "new-native-context", "new"
	current.ContextState, current.TerminalCause = "active", ""
	if err := updateHostPool(root, func(p *hostPool) error {
		p.Active = nextID
		for id, sha := range map[string]string{oldID: "old", nextID: "new"} {
			p.Generations[id] = hostGeneration{ID: id, Directory: filepath.Join(root, "g", id), SHA: sha}
		}
		p.Owners[original.RequestID] = hostTaskOwner{Original: original, Current: original, State: "ready"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h := &noodleHost{instanceID: nextID, binarySHA: "new", receipts: []hostReceipt{current}, rolling: &hostRolling{root: root}}
	req := hostRequest{RequestID: "request", ProjectDir: original.ProjectDir, Mode: original.Mode, ConfigSHA256: original.ConfigSHA256, RoundSource: oldID}
	for i := 0; i < 2; i++ {
		got := h.reconcileRollingRound(req)
		if got.Status != "ok" || got.ContextID != current.ContextID {
			t.Fatalf("lost ACK recovery: %+v", got)
		}
		p, err := readHostPool(root)
		if err != nil || len(p.Owners[req.RequestID].Previous) != 1 {
			t.Fatalf("readback duplicated history or lost original identity: %+v %v", p, err)
		}
		raw, _ := json.Marshal(got)
		if err := confirmRollingRound(root, req, p.Generations[nextID], raw); err != nil {
			t.Fatal(err)
		}
	}
	// No runtime constructor is installed: reconciliation must never dispatch.
	h.receipts = nil
	before, _ := os.ReadFile(filepath.Join(root, "pool.json"))
	if got := h.reconcileRollingRound(req); got.Status != "unknown_effect" {
		t.Fatalf("missing native evidence was treated as permission to replay: %+v", got)
	}
	after, _ := os.ReadFile(filepath.Join(root, "pool.json"))
	if string(before) != string(after) {
		t.Fatal("unknown readback changed original ownership")
	}
	h.receipts = []hostReceipt{current}
	req.ConfigSHA256 = "foreign-task"
	if got := h.reconcileRollingRound(req); got.Status != "refused" {
		t.Fatalf("changed scope consumed historical ACK: %+v", got)
	}
}

func TestRollingAckAloneCannotRetireOldOwner(t *testing.T) {
	root := t.TempDir()
	id := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	target := hostGeneration{ID: id, Directory: filepath.Join(root, "g", id), SHA: "target"}
	req := hostRequest{ProjectDir: "/original/worktree", RequestID: "request", Mode: "manual", ConfigSHA256: "config"}
	receipt := hostReceipt{ProjectDir: req.ProjectDir, RequestID: req.RequestID, Mode: req.Mode, ConfigSHA256: req.ConfigSHA256, HostInstanceID: id, ContextID: "native-context", BinarySHA256: "target"}
	raw, _ := json.Marshal(hostResponse{receipt, "ok", ""})
	if err := updateHostPool(root, func(p *hostPool) error {
		p.Generations[id] = target
		p.Owners[req.RequestID] = hostTaskOwner{Original: receipt, Current: receipt, State: "admitting"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := confirmRollingRound(root, req, target, raw); err == nil {
		t.Fatal("ACK without confirmed native ownership released the old controller")
	}
}

func TestRollingSelectedEntryRepairsInterruptedPublication(t *testing.T) {
	root := t.TempDir()
	id := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	dir := filepath.Join(root, "g", id)
	if err := updateHostPool(root, func(p *hostPool) error {
		p.Active = id
		p.Generations[id] = hostGeneration{ID: id, Directory: dir, State: "active"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, missing := range []bool{true, false} {
		if !missing {
			os.Remove(filepath.Join(root, "host.sock"))
			if err := os.Symlink("/old/generation/host.sock", filepath.Join(root, "host.sock")); err != nil {
				t.Fatal(err)
			}
		}
		if err := repairPoolSocket(root, id); err != nil {
			t.Fatal(err)
		}
		actual, err := os.Readlink(filepath.Join(root, "host.sock"))
		if err != nil || actual != filepath.Join(dir, "host.sock") {
			t.Fatalf("fixed entry not restored: %s %v", actual, err)
		}
	}
	if err := repairPoolSocket(root, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err == nil {
		t.Fatal("retired generation replaced the selected entry")
	}
}

func TestRollingSharedFenceBlocksIndependentLegacyHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.lock")
	first, err := lockfile.TryLockShared(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := lockfile.TryLockShared(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if exclusive, err := lockfile.TryLock(path); err == nil {
		exclusive.Close()
		t.Fatal("legacy Host started an independent budget during two-generation service")
	}
}

func TestRollingPoolDoesNotHideCorruptOwnerState(t *testing.T) {
	root := t.TempDir()
	if err := updateHostPool(root, func(p *hostPool) error {
		p.Generations["not-native-id"] = hostGeneration{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := poolRequestOwner(root, hostRequest{}); err == nil {
		t.Fatal("invalid generation silently selected")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	held, err := lockfile.TryLock(filepath.Join(root, "pool.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if _, err := lockfile.Acquire(ctx, filepath.Join(root, "pool.lock")); !errors.Is(err, context.Canceled) {
		t.Fatalf("lock wait ignored cancellation: %v", err)
	}
}

func TestRollingRejectedAdmissionKeepsOriginalCauseWithoutReplay(t *testing.T) {
	root := t.TempDir()
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h := &noodleHost{dir: root, instanceID: id, binaryPath: "/exact/noodle", binarySHA: "original", rolling: &hostRolling{root: root}}
	if err := updateHostPool(root, func(p *hostPool) error {
		p.Active = id
		p.Generations[id] = hostGeneration{ID: id, Directory: filepath.Join(root, "g", id), Binary: h.binaryPath, SHA: h.binarySHA}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	req := hostRequest{ProjectDir: t.TempDir(), RequestID: "original-task", BinarySHA256: h.binarySHA, AdmissionSHA: h.binarySHA, AdmissionPath: h.binaryPath, ConfigSHA256: "not-current"}
	reply := h.attachRolling(context.Background(), req, nil)
	if reply.Status != "source_drift" {
		t.Fatalf("unexpected admission: %+v", reply)
	}
	req.RoundSource = id
	got := h.reconcileRollingRound(req)
	if got.Status != reply.Status || got.Error != reply.Error || len(h.receipts) != 0 || len(h.contexts) != 0 {
		t.Fatalf("original rejection lost or execution replayed: %+v", got)
	}
}
