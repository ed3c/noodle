package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/loop"
)

type fakeHostRuntime struct {
	ready     chan struct{}
	released  chan struct{}
	busy      bool
	cycles    int
	runErr    error
	cycleErr  error
	shutdowns int
}

func (f *fakeHostRuntime) Run(ctx context.Context) error {
	if f.runErr != nil {
		return f.runErr
	}
	close(f.ready)
	select {
	case <-f.released:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (f *fakeHostRuntime) Ready() <-chan struct{} { return f.ready }
func (f *fakeHostRuntime) Release(context.Context) error {
	if f.busy {
		return loop.ErrReleaseBusy
	}
	close(f.released)
	return nil
}
func (f *fakeHostRuntime) Cycle(context.Context) error { f.cycles++; return f.cycleErr }
func (f *fakeHostRuntime) Shutdown()                   { f.shutdowns++ }
func (f *fakeHostRuntime) FinishOnce() error           { return nil }

func hostTestRequest(t *testing.T, root string) hostRequest {
	t.Helper()
	id, err := hostID()
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("mode = 'supervised'\n[server]\nenabled = false\n[adapters]\n")
	if err := os.WriteFile(filepath.Join(root, config.DefaultConfigPath), data, 0600); err != nil {
		t.Fatal(err)
	}
	return hostRequest{HostDir: t.TempDir(), RequestID: id, Operation: "attach", ProjectDir: root, Mode: "manual", ConfigSHA256: hostDigest(data), BinarySHA256: "test-binary", Environment: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "CONTEXT_SECRET=first"}}
}

func TestHostSerialContextsKeepPIDAndIsolateEnvironment(t *testing.T) {
	t.Setenv("CONTEXT_SECRET", "baseline")
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	first, second := t.TempDir(), t.TempDir()
	h := &noodleHost{dir: t.TempDir(), instanceID: "instance", binaryPath: "/native/noodle", binarySHA: "test-binary"}
	var runtimes []*fakeHostRuntime
	var observedRoots, observedSecrets []string
	h.newRuntime = func(root, bin string, cfg config.Config, deps loop.Dependencies) hostRuntime {
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		observedRoots = append(observedRoots, cwd)
		observedSecrets = append(observedSecrets, os.Getenv("CONTEXT_SECRET"))
		if cfg.Mode != "manual" || string(deps.ModeOverride) != "manual" {
			t.Fatalf("mode override missing: %+v", cfg)
		}
		rt := &fakeHostRuntime{ready: make(chan struct{}), released: make(chan struct{})}
		runtimes = append(runtimes, rt)
		return rt
	}
	req := hostTestRequest(t, first)
	response := h.handle(context.Background(), req)
	if response.Status != "ok" || response.ContextState != "active" {
		t.Fatalf("attach: %+v", response)
	}
	repeated := h.handle(context.Background(), req)
	if repeated.ContextID != response.ContextID || len(runtimes) != 1 {
		t.Fatalf("duplicate attach: %+v", repeated)
	}
	other := hostTestRequest(t, second)
	other.Environment = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	if got := h.handle(context.Background(), other); got.Status != "busy" || len(runtimes) != 1 {
		t.Fatalf("second context: %+v", got)
	}
	changed := req
	changed.Environment = []string{"CONTEXT_SECRET=changed"}
	if got := h.handle(context.Background(), changed); got.Status != "busy" {
		t.Fatalf("changed environment: %+v", got)
	}
	lookup := hostRequest{Operation: "status", ProjectDir: first, ConfigSHA256: req.ConfigSHA256, Mode: "manual", BinarySHA256: h.binarySHA}
	if got := h.handle(context.Background(), lookup); got.ContextID != response.ContextID {
		t.Fatalf("lost-response lookup: %+v", got)
	}
	detach := hostRequest{Operation: "detach", ProjectDir: first, ContextID: response.ContextID, InstanceID: "foreign", BinarySHA256: h.binarySHA}
	if got := h.handle(context.Background(), detach); got.Status != "not_found" {
		t.Fatalf("foreign instance: %+v", got)
	}
	detach.InstanceID = response.HostInstanceID
	runtimes[0].busy = true
	if got := h.handle(context.Background(), detach); got.Status != "busy" || h.active == nil {
		t.Fatalf("busy detach: %+v", got)
	}
	runtimes[0].busy = false
	if got := h.handle(context.Background(), detach); got.ContextState != "detached" || got.Status != "ok" {
		t.Fatalf("detach: %+v", got)
	}
	if os.Getenv("CONTEXT_SECRET") != "baseline" {
		t.Fatal("context environment was not restored")
	}
	if cwd, _ := os.Getwd(); cwd != originalDir {
		t.Fatalf("cwd after detach: %q", cwd)
	}
	response2 := h.handle(context.Background(), other)
	if response2.Status != "ok" || response2.HostPID != response.HostPID || response2.HostInstanceID != response.HostInstanceID || response2.ContextID == response.ContextID {
		t.Fatalf("second attach: %+v", response2)
	}
	if !slices.Equal(observedSecrets, []string{"first", ""}) {
		t.Fatalf("environment leaked: %q", observedSecrets)
	}
	wantFirst, _ := filepath.EvalSymlinks(first)
	wantSecond, _ := filepath.EvalSymlinks(second)
	if !slices.Equal(observedRoots, []string{wantFirst, wantSecond}) {
		t.Fatalf("wrong context directories: %q", observedRoots)
	}
	if h.active.request.Environment != nil {
		t.Fatal("host retained the request environment")
	}
	detach.ProjectDir = second
	detach.ContextID = response2.ContextID
	if got := h.handle(context.Background(), detach); got.Status != "ok" {
		t.Fatalf("second detach: %+v", got)
	}
	stored, err := os.ReadFile(filepath.Join(h.dir, "receipts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "CONTEXT_SECRET") || strings.Contains(string(stored), "environment") {
		t.Fatal("receipt contains environment")
	}
}

func TestHostOnceIsManualAndUnresolvedContextNeverReplays(t *testing.T) {
	h := &noodleHost{dir: t.TempDir(), instanceID: "instance", binarySHA: "test-binary"}
	created := 0
	h.newRuntime = func(root, bin string, cfg config.Config, deps loop.Dependencies) hostRuntime {
		created++
		if cfg.Mode != "manual" {
			t.Fatalf("once mode: %s", cfg.Mode)
		}
		return &fakeHostRuntime{}
	}
	req := hostTestRequest(t, t.TempDir())
	req.Operation = "once"
	req.Mode = "auto"
	got := h.handle(context.Background(), req)
	if got.Status != "ok" || got.Mode != "manual" || got.ContextState != "detached" || created != 1 {
		t.Fatalf("once: %+v", got)
	}
	h.blocked = true
	if got := h.handle(context.Background(), req); got.Status != "unknown_effect" || created != 1 {
		t.Fatalf("unresolved replay: %+v", got)
	}
	lookup := hostRequest{Operation: "status", ProjectDir: req.ProjectDir, ContextID: got.ContextID, InstanceID: got.HostInstanceID, BinarySHA256: h.binarySHA}
	if got := h.handle(context.Background(), lookup); got.Status != "unknown_effect" {
		t.Fatalf("unresolved readback: %+v", got)
	}
}

func TestHostBaselineDropsCredentials(t *testing.T) {
	t.Setenv("GH_TOKEN", "secret")
	t.Setenv("CONTEXT_SECRET", "secret")
	for _, entry := range hostBaselineEnvironment("/private/tmp/host") {
		if strings.HasPrefix(entry, "GH_TOKEN=") || strings.HasPrefix(entry, "CONTEXT_SECRET=") {
			t.Fatal("credential copied into daemon baseline")
		}
	}
}

func TestStartCannotBypassHostLock(t *testing.T) {
	t.Setenv("NOODLE_HOST_DIR", t.TempDir())
	dir, err := hostDirectory()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := lockfile.TryLock(filepath.Join(dir, "host.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	err = runStart(context.Background(), &App{}, startOptions{once: true})
	var locked *lockfile.AlreadyLockedError
	if !errors.As(err, &locked) {
		t.Fatalf("standalone bypassed global lock: %v", err)
	}
}

func TestHostRuntimeFailuresShutdownAndNeverReuseContext(t *testing.T) {
	for _, op := range []string{"attach", "once"} {
		t.Run(op, func(t *testing.T) {
			root := t.TempDir()
			req := hostTestRequest(t, root)
			req.Operation = op
			rt := &fakeHostRuntime{ready: make(chan struct{}), released: make(chan struct{})}
			if op == "attach" {
				rt.runErr = errors.New("startup failed")
			} else {
				rt.cycleErr = errors.New("cycle failed")
			}
			h := &noodleHost{dir: t.TempDir(), binarySHA: req.BinarySHA256, instanceID: "instance", newRuntime: func(string, string, config.Config, loop.Dependencies) hostRuntime { return rt }}
			response := h.handle(context.Background(), req)
			if h.active != nil {
				defer h.active.lock.Close()
				defer h.active.restore()
			}
			if response.Status != "failed" || !h.blocked || rt.shutdowns != 1 || response.ContextState != "failed" {
				t.Fatalf("failure cleanup: %+v shutdowns=%d", response, rt.shutdowns)
			}
			if got := h.handle(context.Background(), req); got.Status != "unknown_effect" {
				t.Fatalf("failed context replayed: %+v", got)
			}
			persisted, err := os.ReadFile(filepath.Join(h.dir, "receipts.json"))
			if err != nil || !strings.Contains(string(persisted), `"context_state": "failed"`) {
				t.Fatalf("failed receipt missing: %s %v", persisted, err)
			}
		})
	}
}
