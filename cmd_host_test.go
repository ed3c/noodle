package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/loop"
)

type fakeHostRuntime struct {
	ready         chan struct{}
	released      chan struct{}
	busy          bool
	cycles        int
	runErr        error
	cycleErr      error
	shutdowns     int
	releaseErr    error
	terminalCause loop.TerminalCause
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
	return f.release(loop.TerminalMigrationDetach)
}
func (f *fakeHostRuntime) release(cause loop.TerminalCause) error {
	if f.releaseErr != nil {
		return f.releaseErr
	}
	if f.busy {
		return loop.ErrReleaseBusy
	}
	f.terminalCause = cause
	close(f.released)
	return nil
}
func (f *fakeHostRuntime) CleanupAutomatic(context.Context) error {
	return f.release(loop.TerminalAutoCompleted)
}
func (f *fakeHostRuntime) Cleanup(ctx context.Context, apply bool) error {
	if f.busy {
		return loop.ErrReleaseBusy
	}
	if !apply {
		return f.releaseErr
	}
	return f.release(loop.TerminalExplicitCleanup)
}
func (f *fakeHostRuntime) Cycle(context.Context) error       { f.cycles++; return f.cycleErr }
func (f *fakeHostRuntime) Shutdown()                         { f.shutdowns++ }
func (f *fakeHostRuntime) FinishOnce() error                 { return nil }
func (f *fakeHostRuntime) TerminalCause() loop.TerminalCause { return f.terminalCause }

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

func TestHostConcurrentContextsKeepPIDAndIsolateEnvironment(t *testing.T) {
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
		observedRoots = append(observedRoots, deps.Execution.Root())
		secret, _ := deps.Execution.Lookup("CONTEXT_SECRET")
		observedSecrets = append(observedSecrets, secret)
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
	response2 := h.handle(context.Background(), other)
	if response2.Status != "ok" || len(runtimes) != 2 || len(h.contexts) != 2 {
		t.Fatalf("concurrent context: %+v", response2)
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
	if got := h.handle(context.Background(), detach); got.Status != "busy" || h.testContext() == nil || got.TerminalCause != "" {
		t.Fatalf("busy detach: %+v", got)
	}
	runtimes[0].busy = false
	if got := h.handle(context.Background(), detach); got.ContextState != "detached" || got.Status != "ok" || got.TerminalCause != loop.TerminalMigrationDetach {
		t.Fatalf("detach: %+v", got)
	}
	if os.Getenv("CONTEXT_SECRET") != "baseline" {
		t.Fatal("context environment was not restored")
	}
	if cwd, _ := os.Getwd(); cwd != originalDir {
		t.Fatalf("cwd after detach: %q", cwd)
	}
	response2 = h.handle(context.Background(), other)
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
	if h.testContext().request.Environment != nil {
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

func TestHostOnceIsManualAndCompletedContextNeverReplays(t *testing.T) {
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
	if got := h.handle(context.Background(), req); got.Status != "ok" || got.ContextState != "detached" || created != 1 {
		t.Fatalf("completed once was replayed: %+v", got)
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
			rt := &fakeHostRuntime{ready: make(chan struct{}), released: make(chan struct{}), terminalCause: loop.TerminalAutoCompleted}
			if op == "attach" {
				rt.runErr = errors.New("startup failed")
			} else {
				rt.cycleErr = errors.New("cycle failed")
			}
			h := &noodleHost{dir: t.TempDir(), binarySHA: req.BinarySHA256, instanceID: "instance", newRuntime: func(string, string, config.Config, loop.Dependencies) hostRuntime { return rt }}
			response := h.handle(context.Background(), req)
			if h.testContext() != nil {
				defer h.testContext().lock.Close()
			}
			if response.Status != "failed" || !h.testContext().failed || rt.shutdowns != 1 || response.ContextState != "failed" || response.TerminalCause != "" {
				t.Fatalf("failure cleanup: %+v shutdowns=%d", response, rt.shutdowns)
			}
			if got := h.handle(context.Background(), req); got.Status != "unknown_effect" {
				t.Fatalf("failed context replayed: %+v", got)
			}
			persisted, err := os.ReadFile(filepath.Join(h.dir, "receipts.json"))
			if err != nil || !strings.Contains(string(persisted), `"context_state": "failed"`) || strings.Contains(string(persisted), `"terminal_cause"`) {
				t.Fatalf("failed receipt missing: %s %v", persisted, err)
			}
		})
	}
}

func TestHostDetachClassifiesOnlyQuiescenceRefusalAsBusy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status string
	}{
		{"active-worker", fmt.Errorf("workers: %w", loop.ErrReleaseBusy), "busy"},
		{"accepted-timeout", context.DeadlineExceeded, "unknown_effect"},
		{"cancelled", context.Canceled, "unknown_effect"},
		{"flush-failed", errors.New("checkpoint write failed"), "unknown_effect"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := hostTestRequest(t, t.TempDir())
			rt := &fakeHostRuntime{ready: make(chan struct{}), released: make(chan struct{}), releaseErr: tc.err}
			h := &noodleHost{dir: t.TempDir(), binarySHA: req.BinarySHA256, instanceID: "instance", newRuntime: func(string, string, config.Config, loop.Dependencies) hostRuntime { return rt }}
			attached := h.handle(context.Background(), req)
			if attached.Status != "ok" {
				t.Fatalf("attach: %+v", attached)
			}
			defer func() {
				h.testContext().cancel()
				runErr := <-h.testContext().done
				h.finish(h.testContext(), runErr)
				h.testContext().lock.Close()
			}()
			detach := hostRequest{Operation: "detach", ProjectDir: req.ProjectDir, ContextID: attached.ContextID, InstanceID: attached.HostInstanceID, BinarySHA256: req.BinarySHA256}
			if got := h.handle(context.Background(), detach); got.Status != tc.status || h.testContext() == nil {
				t.Fatalf("release error %v classified as %+v", tc.err, got)
			}
		})
	}
}

func TestHostHistoricalDetachSurvivesFailedOrUnpersistedContext(t *testing.T) {
	for _, failure := range []string{"cycle", "save"} {
		t.Run(failure, func(t *testing.T) {
			h := &noodleHost{dir: t.TempDir(), binarySHA: "test-binary", instanceID: "instance"}
			rt := &fakeHostRuntime{}
			h.newRuntime = func(string, string, config.Config, loop.Dependencies) hostRuntime { return rt }
			first := hostTestRequest(t, t.TempDir())
			first.Operation = "once"
			detached := h.handle(context.Background(), first)
			if detached.Status != "ok" || detached.ContextState != "detached" || detached.TerminalCause != "" {
				t.Fatalf("first once: %+v", detached)
			}
			var persisted []hostReceipt
			data, err := os.ReadFile(filepath.Join(h.dir, "receipts.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &persisted); err != nil {
				t.Fatal(err)
			}
			if len(persisted) != 1 || persisted[0].ContextState != "detached" {
				t.Fatalf("historical detach was not durable: %+v", persisted)
			}

			second := hostTestRequest(t, t.TempDir())
			rt = &fakeHostRuntime{ready: make(chan struct{}), released: make(chan struct{})}
			var unresolved hostResponse
			if failure == "cycle" {
				second.Operation = "once"
				rt.cycleErr = errors.New("cycle failed")
				unresolved = h.handle(context.Background(), second)
			} else {
				unresolved = h.handle(context.Background(), second)
				if unresolved.Status != "ok" {
					t.Fatalf("second attach: %+v", unresolved)
				}
				h.dir = filepath.Join(h.dir, "missing") // Actual persistence failure after actor release.
				unresolved = h.handle(context.Background(), hostRequest{Operation: "detach", ProjectDir: second.ProjectDir, ContextID: unresolved.ContextID, InstanceID: unresolved.HostInstanceID, BinarySHA256: h.binarySHA})
			}
			if h.testContext() == nil || !h.testContext().failed {
				t.Fatalf("failure lost custody: %+v", unresolved)
			}
			defer h.testContext().lock.Close()
			lookup := hostRequest{Operation: "status", ProjectDir: second.ProjectDir, ContextID: unresolved.ContextID, InstanceID: unresolved.HostInstanceID, BinarySHA256: h.binarySHA}
			for _, op := range []string{"status", "detach"} {
				lookup.Operation = op
				if got := h.handle(context.Background(), lookup); got.Status != "unknown_effect" || got.TerminalCause != "" {
					t.Fatalf("unresolved %s became successful: %+v", op, got)
				}
			}
			lookup.ProjectDir, lookup.ContextID = first.ProjectDir, detached.ContextID
			for _, op := range []string{"status", "detach"} {
				lookup.Operation = op
				if got := h.handle(context.Background(), lookup); got.Status != "ok" || got.ContextState != "detached" {
					t.Errorf("durable historical %s blocked: %+v", op, got)
				}
			}
			if got := h.handle(context.Background(), second); got.Status != "unknown_effect" {
				t.Fatalf("unresolved request replayed: %+v", got)
			}
		})
	}
}

func TestHostUpgradeReadsExactHistoricalReceiptWithOriginalBinary(t *testing.T) {
	req := hostTestRequest(t, t.TempDir())
	req.Operation = "once"
	old := &noodleHost{dir: t.TempDir(), instanceID: "old-instance", binarySHA: req.BinarySHA256,
		newRuntime: func(string, string, config.Config, loop.Dependencies) hostRuntime { return &fakeHostRuntime{} }}
	detached := old.handle(context.Background(), req)
	if detached.Status != "ok" {
		t.Fatalf("original once: %+v", detached)
	}
	data, err := os.ReadFile(filepath.Join(old.dir, "receipts.json"))
	if err != nil {
		t.Fatal(err)
	}
	upgraded := &noodleHost{dir: old.dir, instanceID: "new-instance", binarySHA: "new-binary"}
	if err := json.Unmarshal(data, &upgraded.receipts); err != nil {
		t.Fatal(err)
	}
	lookup := hostRequest{Operation: "status", ProjectDir: req.ProjectDir, ContextID: detached.ContextID, InstanceID: detached.HostInstanceID, BinarySHA256: req.BinarySHA256}
	for _, op := range []string{"status", "detach"} {
		lookup.Operation = op
		if got := upgraded.handle(context.Background(), lookup); got.Status != "ok" || got.hostReceipt != detached.hostReceipt {
			t.Errorf("original binary historical %s: %+v", op, got)
		}
		for _, binary := range []string{"foreign-old-binary", upgraded.binarySHA} {
			foreign := lookup
			foreign.BinarySHA256 = binary
			if got := upgraded.handle(context.Background(), foreign); got.Status != "binary_mismatch" {
				t.Errorf("foreign receipt binary accepted: %+v", got)
			}
		}
	}
	for _, op := range []string{"attach", "once"} {
		req.Operation = op
		if got := upgraded.handle(context.Background(), req); got.Status != "binary_mismatch" {
			t.Fatalf("old binary %s accepted: %+v", op, got)
		}
	}
	lookup.Operation = "status"
	lookup.ContextID, lookup.InstanceID = "", ""
	lookup.ConfigSHA256, lookup.Mode = req.ConfigSHA256, "manual"
	if got := upgraded.handle(context.Background(), lookup); got.Status != "binary_mismatch" {
		t.Fatalf("inexact cross-binary lookup accepted: %+v", got)
	}
	lookup.RequestID = detached.RequestID
	if got := upgraded.handle(context.Background(), lookup); got.Status != "ok" {
		t.Errorf("original request lookup: %+v", got)
	}
	upgraded.receipts[0].ContextState = "active"
	for _, op := range []string{"status", "detach"} {
		lookup.Operation = op
		if got := upgraded.handle(context.Background(), lookup); got.Status != "binary_mismatch" {
			t.Fatalf("old binary active %s accepted: %+v", op, got)
		}
	}
}

func (h *noodleHost) testContext() *hostContext {
	for _, c := range h.contexts {
		return c
	}
	return nil
}

func TestHostCapabilitiesPreservesProtocolContract(t *testing.T) {
	root := NewRootCmd()
	var output strings.Builder
	root.SetOut(&output)
	root.SetArgs([]string{"host", "capabilities"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(output.String()), &value); err != nil {
		t.Fatal(err)
	}
	wantForce := float64(hostCapabilities(runtime.GOOS)["resident_force_after_grace"].(int))
	if len(value) != 5 || value["schema"] != float64(1) || value["protocol"] != "noodle-host-v1" || value["resident_request_binding"] != float64(1) || value["completed_context_cleanup"] != float64(1) || value["resident_force_after_grace"] != wantForce {
		t.Fatalf("host protocol changed: %v", value)
	}
}
