package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/loop"
)

func hostConfigDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestHostExternalConfigSurvivesExitWithoutChangingProject(t *testing.T) {
	root := hostConfigDir(t)
	req := hostTestRequest(t, root)
	before, err := os.ReadFile(filepath.Join(root, ".noodle.toml"))
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("mode = 'manual'\n[server]\nenabled = false\n[routing.defaults]\nprovider = 'codex'\nmodel = 'selected-model'\n[runtime.process]\nadmitted_orders = ['selected-order']\n")
	req.ConfigPath = filepath.Join(hostConfigDir(t), "task.toml")
	if err := os.WriteFile(req.ConfigPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	req.ConfigSHA256 = hostDigest(data)
	h := &noodleHost{dir: t.TempDir(), instanceID: "host", binaryPath: "/native/noodle", binarySHA: req.BinarySHA256}
	started := 0
	h.newRuntime = func(_, _ string, cfg config.Config, _ loop.Dependencies) hostRuntime {
		started++
		if cfg.Routing.Defaults.Model != "selected-model" || len(cfg.Runtime.Process.AdmittedOrders) != 1 || cfg.Runtime.Process.AdmittedOrders[0] != "selected-order" {
			t.Fatalf("runtime consumed repository config: %+v", cfg)
		}
		return &fakeHostRuntime{ready: make(chan struct{}), released: make(chan struct{})}
	}
	response := h.handle(context.Background(), req)
	if response.Status != "ok" || response.ConfigPath != req.ConfigPath || response.ConfigSHA256 != req.ConfigSHA256 {
		t.Fatalf("attach: %+v", response)
	}
	stop := req
	stop.Operation = "detach"
	stop.ContextID, stop.InstanceID = response.ContextID, response.HostInstanceID
	if got := h.handle(context.Background(), stop); got.Status != "ok" {
		t.Fatalf("detach: %+v", got)
	}
	cfg, _, err := config.LoadRuntime(root)
	if err != nil || len(cfg.Runtime.Process.AdmittedOrders) != 1 || cfg.Runtime.Process.AdmittedOrders[0] != "selected-order" {
		t.Fatalf("offline config: %+v %v", cfg, err)
	}
	after, err := os.ReadFile(filepath.Join(root, ".noodle.toml"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("repository config changed")
	}
	// A new request can use the same original source, but cannot replace it.
	req.RequestID = "successor"
	response = h.handle(context.Background(), req)
	if response.Status != "ok" {
		t.Fatalf("same-source successor: %+v", response)
	}
	stop.RequestID, stop.ContextID = req.RequestID, response.ContextID
	if got := h.handle(context.Background(), stop); got.Status != "ok" {
		t.Fatalf("successor detach: %+v", got)
	}
	req.RequestID = "foreign"
	req.ConfigPath = filepath.Join(hostConfigDir(t), "other.toml")
	if err := os.WriteFile(req.ConfigPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if got := h.handle(context.Background(), req); got.Status != "refused" {
		t.Fatalf("changed source admitted: %+v", got)
	}
	if started != 2 {
		t.Fatalf("unexpected runtime starts: %d", started)
	}
}

func TestHostExternalConfigDriftRefusesBeforeRuntime(t *testing.T) {
	req := hostTestRequest(t, hostConfigDir(t))
	req.ConfigPath = filepath.Join(hostConfigDir(t), "task.toml")
	data := []byte("mode = 'manual'\n[server]\nenabled = false\n")
	if err := os.WriteFile(req.ConfigPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	req.ConfigSHA256 = hostDigest(data)
	h := &noodleHost{binarySHA: req.BinarySHA256}
	if err := os.WriteFile(req.ConfigPath, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if got := h.handle(context.Background(), req); got.Status != "source_drift" {
		t.Fatalf("drift: %+v", got)
	}
	if err := os.Remove(req.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if got := h.handle(context.Background(), req); got.Status != "refused" {
		t.Fatalf("missing source: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(req.ProjectDir, ".noodle")); !os.IsNotExist(err) {
		t.Fatal("refused source created runtime state")
	}
}

func TestHostRequestBindsConfigPathAndReadsOriginalAfterSourceRemoval(t *testing.T) {
	req := hostTestRequest(t, hostConfigDir(t))
	req.ConfigPath = "/original/task.toml"
	req.RequestFile = filepath.Join(t.TempDir(), "request.json")
	binding, unlock, err := bindHostRequest(&req)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	changed := req
	changed.ConfigPath = "/other/task.toml"
	if _, release, err := bindHostRequest(&changed); err == nil {
		release()
		t.Fatal("same bytes from different source accepted")
	}
	lookup := hostRequest{Operation: "status", ProjectDir: req.ProjectDir, BinarySHA256: req.BinarySHA256, RequestFile: req.RequestFile}
	_, unlock, err = bindHostRequest(&lookup)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if lookup.ConfigPath != binding.ConfigPath || lookup.ConfigSHA256 != binding.ConfigSHA256 {
		t.Fatal("readback lost source identity")
	}
	r := hostReceipt{ProjectDir: req.ProjectDir, RequestID: req.RequestID, ConfigSHA256: req.ConfigSHA256, ConfigPath: changed.ConfigPath, Mode: req.Mode}
	if hostReceiptMatches(lookup, r) {
		t.Fatal("foreign config receipt matched")
	}
}

func TestHostReceiptFailureRollsBackOnlyNewConfigBinding(t *testing.T) {
	for _, existing := range []bool{false, true} {
		root := hostConfigDir(t)
		req := hostTestRequest(t, root)
		data, err := os.ReadFile(filepath.Join(root, ".noodle.toml"))
		if err != nil {
			t.Fatal(err)
		}
		req.ConfigPath = filepath.Join(hostConfigDir(t), "task.toml")
		if err := os.WriteFile(req.ConfigPath, data, 0600); err != nil {
			t.Fatal(err)
		}
		if existing {
			if err := os.Mkdir(filepath.Join(root, ".noodle"), 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := config.SaveRuntimeSource(root, config.RuntimeSource{Path: req.ConfigPath, SHA256: req.ConfigSHA256}); err != nil {
				t.Fatal(err)
			}
		}
		h := &noodleHost{dir: filepath.Join(t.TempDir(), "absent"), binarySHA: req.BinarySHA256,
			newRuntime: func(string, string, config.Config, loop.Dependencies) hostRuntime {
				t.Fatal("failed receipt started runtime")
				return nil
			}}
		if got := h.handle(context.Background(), req); got.Status != "refused" {
			t.Fatalf("receipt failure: %+v", got)
		}
		_, err = os.Stat(filepath.Join(root, ".noodle/host-config.json"))
		if (existing && err != nil) || (!existing && !os.IsNotExist(err)) {
			t.Fatalf("binding rollback existing=%v: %v", existing, err)
		}
		if len(h.receipts) != 0 || len(h.contexts) != 0 {
			t.Fatal("failed receipt retained accepted context")
		}
	}
}
