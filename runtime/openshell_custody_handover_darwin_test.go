//go:build darwin && cgo

package runtime

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
)

func handoverTestFile(t *testing.T, path string, value any) SessionReadbackFileRef {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := ReadSessionReadbackFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestOpenShellCustodyHandoverRequiresOriginalExitAndIsResumable(t *testing.T) {
	testOpenShellCustodyHandover(t, 1)
}

func TestOpenShellCustodyHandoverDoesNotRequireHostExit(t *testing.T) {
	testOpenShellCustodyHandover(t, 2)
}

func testOpenShellCustodyHandover(t *testing.T, schema int) {
	t.Helper()
	_, b, security, open := custodyFixture(t)
	c := custodyOpen(t, b, security, nil, open)
	state := c.Snapshot()
	b = state.Binding
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	state.Version, state.ControlRoot, state.Handover = 3, "", nil
	owner := filepath.Join(b.StateRoot, "owner.json")
	handoverTestFile(t, owner, state)
	dir := t.TempDir()
	sourceRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := handoverTestFile(t, filepath.Join(dir, "before.json"), state)
	child := exec.Command("/bin/sleep", "30")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	native, err := procx.ObserveNativeIdentity(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	plan := handoverTestFile(t, filepath.Join(dir, "plan.json"), map[string]any{"host_dir": sourceRoot, "process_generation": native, "native_source": map[string]any{"host_instance_id": "source"}, "handover": map[string]any{"SharedRoot": target}})
	empty := handoverTestFile(t, filepath.Join(dir, "empty.json"), map[string]any{"host_instance_id": "source", "host_pid": native.PID, "contexts": 0, "resources": map[string]any{"active_workers": 0, "active_residents": 0, "active_allocations": 0, "unresolved_residents": 0}, "residents": map[string]any{"error": nil}})
	release := handoverTestFile(t, filepath.Join(dir, "release.json"), OpenShellHostRelease{Schema: schema, SourceRoot: sourceRoot, SourceInstance: "source", ControlRoot: target, Process: native, Plan: plan, EmptyResources: empty})
	receipt := filepath.Join(dir, "handover.json")
	lease, err := lockfile.TryLock(filepath.Join(b.StateRoot, "owner.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = UpgradeOpenShellCustodyV3(b, target, before, release, receipt); err == nil {
		t.Fatal("held original custody upgraded")
	}
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(receipt); !os.IsNotExist(err) {
		t.Fatal("live-owner refusal wrote a receipt", err)
	}
	if schema == 1 {
		if _, err = UpgradeOpenShellCustodyV3(b, target, before, release, receipt); err == nil {
			t.Fatal("historical exit proof accepted a live process")
		}
		if err = child.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		_ = child.Wait()
	}
	first, err := UpgradeOpenShellCustodyV3(b, target, before, release, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if schema == 2 {
		observed, err := procx.ObserveNativeIdentity(native.PID)
		if err != nil || !native.SameGeneration(observed) {
			t.Fatal("custody handover retired the source process", err)
		}
	}
	after, err := os.ReadFile(owner)
	if err != nil {
		t.Fatal(err)
	}
	second, err := UpgradeOpenShellCustodyV3(b, target, before, release, receipt)
	if err != nil || first != second {
		t.Fatal("same transaction failed to resume", first, second, err)
	}
	repeated, err := os.ReadFile(owner)
	if err != nil || !bytes.Equal(after, repeated) {
		t.Fatal("completed handover rewrote ledger", err)
	}
	c, err = OpenOpenShellCustody(t.Context(), b, security, nil, target)
	if err != nil {
		t.Fatal(err)
	}
	if c.Snapshot().ControlRoot != target || c.Snapshot().Version != 4 {
		t.Fatal("shared owner was not bound")
	}
	projection := c.Snapshot()
	projection.Handover.Path = "/different/receipt"
	if c.Snapshot().Handover.Path != first.Path {
		t.Fatal("snapshot mutated custody authority")
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenOpenShellCustody(t.Context(), b, security, nil, sourceRoot); err == nil {
		t.Fatal("retired private root reopened custody")
	}
	if err = os.WriteFile(receipt, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenOpenShellCustody(t.Context(), b, security, nil, target); err == nil {
		t.Fatal("changed handover evidence accepted")
	}
}
