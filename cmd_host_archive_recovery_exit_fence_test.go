package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestArchiveRecoveryExitFenceHoldsOriginalLocksWithoutEditingThem(t *testing.T) {
	root := t.TempDir()
	host, work, custody := filepath.Join(root, "host"), filepath.Join(root, "runtime"), filepath.Join(root, "custody")
	session := filepath.Join(work, "sessions", "original")
	for _, dir := range []string{host, session, custody} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	paths := []string{filepath.Join(host, "host.lock"), filepath.Join(host, "pool.lock"),
		filepath.Join(work, "noodle.lock"), filepath.Join(work, "control.lock"),
		filepath.Join(session, "maintenance.lock"), filepath.Join(custody, "owner.lock")}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("81\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	invoked := false
	check := func(custodyLock *lockfile.Lock) error {
		invoked = true
		if !custodyLock.HoldsExisting(paths[5]) || custodyLock.HoldsExisting(paths[4]) {
			t.Fatal("borrowed custody lock does not bind the exact original inode")
		}
		for _, path := range paths {
			lock, err := lockfile.TryLockExisting(path)
			if err == nil {
				_ = lock.Close()
				t.Fatalf("original lock %s was released within the owner callback", path)
			}
		}
		revival, err := lockfile.TryLockShared(paths[0])
		if err == nil {
			_ = revival.Close()
			t.Fatal("rolling Host can revive while the original exit fence holds")
		}
		return nil
	}
	if err := withArchiveRecoveryExitLocks(t.Context(), host, work, session, custody, check); err != nil || !invoked {
		t.Fatalf("original owner lock transaction did not run: %v", err)
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != "81\n" {
			t.Fatalf("owner lock history changed: %s, %q, %v", path, raw, err)
		}
		lock, err := lockfile.TryLockExisting(path)
		if err != nil {
			t.Fatalf("original lock %s was not released after action: %v", path, err)
		}
		_ = lock.Close()
	}
	other, err := lockfile.TryLockExisting(paths[5])
	if err != nil {
		t.Fatal(err)
	}
	invoked = false
	if err = withArchiveRecoveryExitLocks(t.Context(), host, work, session, custody, check); err == nil || invoked {
		t.Fatal("competing custody entered the fenced operation")
	}
	_ = other.Close()
	hostLock, err := lockfile.TryLockExisting(paths[0])
	if err != nil {
		t.Fatal("failure retained an earlier Host lock", err)
	}
	_ = hostLock.Close()
	if err = os.Remove(paths[5]); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(paths[4], paths[5]); err != nil {
		t.Fatal(err)
	}
	if err = withArchiveRecoveryExitLocks(t.Context(), host, work, session, custody, check); err == nil || invoked {
		t.Fatal("indirect old custody lock entered the fenced operation")
	}
}

func TestOriginalArchiveHostKernelExitRequiresProcessAndGroupAbsence(t *testing.T) {
	if _, err := procx.ObserveNativeIdentity(os.Getpid()); err != nil {
		t.Skipf("kernel identity unavailable: %v", err)
	}
	leader := exec.CommandContext(t.Context(), "/bin/sh", "-c", "sleep 2 & wait")
	detachHostProcess(leader)
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	process := procx.ProcessRecord{PID: leader.Process.Pid, SessionID: "original", StartedAt: time.Now().UTC()}
	if err := verifyOriginalArchiveHostKernelExit(process); err == nil {
		t.Fatal("live original Host accepted as retired")
	}
	if err := leader.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := verifyOriginalArchiveHostKernelExit(process); err != nil {
		t.Fatalf("original independent kernel absence was not recognized: %v", err)
	}
	process.StartedAt = time.Time{}
	if err := verifyOriginalArchiveHostKernelExit(process); err == nil {
		t.Fatal("missing original generation identity allowed retirement")
	}
}

func TestOriginalArchiveHostKernelExitRefusesSurvivingGroup(t *testing.T) {
	if _, err := procx.ObserveNativeIdentity(os.Getpid()); err != nil {
		t.Skipf("kernel identity unavailable: %v", err)
	}
	leader := exec.CommandContext(t.Context(), "/bin/sh", "-c", "sleep 1 &")
	detachHostProcess(leader)
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	process := procx.ProcessRecord{PID: leader.Process.Pid, SessionID: "original", StartedAt: time.Now().UTC()}
	if err := leader.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := procx.ObserveNativeIdentity(process.PID); !errors.Is(err, procx.ErrProcessGone) {
		t.Fatalf("isolated group leader still present, %v", err)
	}
	if err := verifyOriginalArchiveHostKernelExit(process); err == nil {
		t.Fatal("an exited leader substituted for its still-owned process group")
	}
	wait, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for {
		if absent, err := procx.ProcessGroupAbsent(process.PID); err == nil && absent {
			break
		}
		select {
		case <-wait.Done():
			t.Fatal("isolated process group did not exit")
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err := verifyOriginalArchiveHostKernelExit(process); err != nil {
		t.Fatalf("complete process-group exit did not qualify: %v", err)
	}
}

func TestArchiveRecoveryExitGenerationRequiresExactOriginalScope(t *testing.T) {
	instance := "original-instance"
	request := "original-request"
	binary := loopruntime.SessionReadbackFileRef{Path: "/original/noodle", SHA256: "original-sha"}
	owner := &hostOwnerBinding{GenerationDir: "/original/host/g/original-instance", ExecutionBinary: binary,
		Current: hostReceipt{HostInstanceID: instance, RequestID: request, HostPID: 81}}
	generation := hostGeneration{ID: instance, State: "active", Directory: owner.GenerationDir, Binary: binary.Path, SHA: binary.SHA256,
		Process: procx.ProcessRecord{PID: 81, SessionID: instance, StartedAt: time.Now()}}
	pool := hostPool{Active: instance, Owners: map[string]hostTaskOwner{request: {State: "owned", Current: owner.Current}},
		Generations: map[string]hostGeneration{instance: generation}}
	if got, err := archiveRecoveryExitGeneration(pool, owner); err != nil || got.Process != generation.Process {
		t.Fatalf("original owned generation did not match: %+v, %v", got, err)
	}
	pool.Owners["foreign"] = hostTaskOwner{Current: hostReceipt{ContextState: "active"}}
	if _, err := archiveRecoveryExitGeneration(pool, owner); err == nil {
		t.Fatal("foreign context was accepted")
	}
	delete(pool.Owners, "foreign")
	pool.Queue = []hostVersionRequest{{State: "starting"}}
	if _, err := archiveRecoveryExitGeneration(pool, owner); err == nil {
		t.Fatal("pending generation launch was accepted")
	}
	pool.Queue = nil
	generation.Process.SessionID = "other"
	pool.Generations[instance] = generation
	if _, err := archiveRecoveryExitGeneration(pool, owner); err == nil {
		t.Fatal("foreign original generation resource identity was accepted")
	}
}

func TestArchiveRecoveryExitFenceDoesNotCreateMissingLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner.lock")
	lock, err := lockfile.TryLockExisting(path)
	if !errors.Is(err, os.ErrNotExist) || lock != nil {
		t.Fatalf("missing original lock was accepted: %v", err)
	}
	if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created missing original lock: %v", err)
	}
}
