package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
)

func TestArchiveRecoveryPreSignalNativeAdmissionFence(t *testing.T) {
	root := t.TempDir()
	const generationID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const originalID = "original-request"
	old := hostReceipt{
		RequestID: originalID, ContextID: "original-context", ContextState: "active",
		HostInstanceID: generationID, HostPID: os.Getpid(), ProjectDir: filepath.Join(root, "original-project"),
		BinaryPath: "/original/noodle", BinarySHA256: "original-binary", HostDir: root,
	}
	g := hostGeneration{
		ID: generationID, Directory: filepath.Join(root, "g", generationID), Binary: old.BinaryPath, SHA: old.BinarySHA256,
		State: "active", Process: procx.ProcessRecord{PID: old.HostPID, SessionID: generationID, StartedAt: time.Now()},
	}
	if err := updateHostPool(root, func(pool *hostPool) error {
		pool.Active, pool.Generations[g.ID] = g.ID, g
		pool.Owners[originalID] = hostTaskOwner{State: "owned", Current: old, Original: old}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	lifetime, err := lockfile.TryLockShared(filepath.Join(root, "host.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lifetime.Close()
	held, err := lockfile.TryLockExisting(filepath.Join(root, "pool.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if held != nil {
			_ = held.Close()
		}
	}()

	var preparations atomic.Int32
	entered := make(chan struct{}, 1)
	unblock := make(chan struct{})
	h := &noodleHost{
		dir: g.Directory, instanceID: g.ID, binaryPath: g.Binary, binarySHA: g.SHA, rolling: &hostRolling{root: root},
		prepareContext: func(ctx context.Context, _ hostRequest, _ []byte) (worktree.TaskControl, hostTaskPreparationPlan, error) {
			preparations.Add(1)
			entered <- struct{}{}
			select {
			case <-unblock:
			case <-ctx.Done():
			}
			return worktree.TaskControl{}, hostTaskPreparationPlan{}, errors.New("end isolated competing preparation before effects")
		},
	}

	req := hostTestRequest(t, t.TempDir())
	req.BinarySHA256, req.AdmissionSHA, req.AdmissionPath = g.SHA, g.SHA, g.Binary
	poolPath := filepath.Join(root, "pool.json")
	before, err := loopruntime.ReadSessionReadbackFile(poolPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Call the generation's actual admission method directly. Public routing
	// cannot stand in for this producer.
	blockedCtx, cancelBlocked := context.WithTimeout(t.Context(), 2*time.Second)
	response := h.attachRolling(blockedCtx, req, nil)
	cancelBlocked()
	if response.Status != "unknown_effect" || response.Error != context.DeadlineExceeded.Error() || preparations.Load() != 0 {
		t.Fatalf("fresh direct-generation admission passed the pool fence: %+v", response)
	}
	after, err := loopruntime.ReadSessionReadbackFile(poolPath, nil)
	if err != nil || after != before {
		t.Fatalf("blocked direct admission changed the original pool: %v", err)
	}

	if err = held.Close(); err != nil {
		t.Fatal(err)
	}
	held = nil
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result := make(chan hostResponse, 1)
	go func() { result <- h.attachRolling(ctx, req, nil) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("direct-generation preparation did not reach the native in-flight boundary")
	}
	held, err = lockfile.TryLockExisting(filepath.Join(root, "pool.lock"))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := readHostPool(root)
	if err != nil {
		t.Fatal(err)
	}
	if pool.Owners[req.RequestID].State != "admitting" {
		t.Fatal("direct admission did not leave its original in-flight owner row")
	}
	owner := &hostOwnerBinding{GenerationDir: g.Directory, ExecutionBinary: loopruntime.SessionReadbackFileRef{Path: g.Binary, SHA256: g.SHA}, Current: old}
	if _, err = archiveRecoveryExitGeneration(pool, owner); err == nil || !strings.Contains(err.Error(), "another unresolved request") {
		t.Fatalf("retirement let an already in-flight direct admission cross: %v", err)
	}
	if err = held.Close(); err != nil {
		t.Fatal(err)
	}
	held = nil
	close(unblock)
	select {
	case <-result:
	case <-ctx.Done():
		t.Fatal("isolated competing producer did not finish")
	}
}
