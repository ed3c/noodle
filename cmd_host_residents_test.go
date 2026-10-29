package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/testutil/residentchild"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestHostResidentsRequireNativeContextAndExposeReadback(t *testing.T) {
	dir := t.TempDir()
	r, err := admission.OpenResidents(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := admission.NewBudget(2)
	observeKnownHostFixtureSamples(b)
	h := &noodleHost{dir: dir, instanceID: "actual-host", binaryPath: "/native/noodle", binarySHA: "bound-sha", budget: b, residents: r, contexts: map[string]*hostContext{}}
	result := h.residentOperation(context.Background(), hostRequest{Operation: "resident_acquire_bound", ProjectDir: dir, ContextID: "original", RequestID: "request", InstanceID: "actual-host", BinarySHA256: "bound-sha"}).(map[string]any)
	if result["status"] != "refused" || result["error"] != "active native context missing for resident lease" {
		t.Fatal(result)
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	h.contexts["original"] = &hostContext{receipt: hostReceipt{ProjectDir: root, ContextID: "original", RequestID: "request", ContextState: "active"}}
	result = h.residentOperation(context.Background(), hostRequest{Operation: "resident_acquire_bound", ProjectDir: dir, ContextID: "original", RequestID: "request", InstanceID: "actual-host", BinarySHA256: "bound-sha"}).(map[string]any)
	if result["error"] != "resident spec missing" {
		t.Fatal(result)
	}
	h.blocked = true
	result = h.residentOperation(context.Background(), hostRequest{Operation: "resident_acquire_bound", ProjectDir: dir, ContextID: "original", RequestID: "request", InstanceID: "actual-host", BinarySHA256: "bound-sha"}).(map[string]any)
	if result["error"] != "Host recovery pending" {
		t.Fatal(result)
	}
	result = h.residentOperation(context.Background(), hostRequest{Operation: "resident_inspect"}).(map[string]any)
	if result["status"] != "observed" || result["host_instance_id"] != "actual-host" {
		t.Fatal(result)
	}
}

func TestHostResidentsRefuseStaleAndUnboundMutations(t *testing.T) {
	dir := t.TempDir()
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := admission.OpenResidents(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := &noodleHost{instanceID: "host", binarySHA: "binary", budget: admission.NewBudget(1), residents: r,
		contexts: map[string]*hostContext{"new": {receipt: hostReceipt{ContextID: "new", RequestID: "new-request", ProjectDir: root, ContextState: "active"}}}}
	for _, operation := range []string{"resident_acquire", "resident_release", "resident_acquire_bound", "resident_release_bound"} {
		req := hostRequest{Operation: operation, ProjectDir: root, ContextID: "old", RequestID: "old-request", InstanceID: "host", BinarySHA256: "binary"}
		result := h.residentOperation(context.Background(), req).(map[string]any)
		if result["status"] != "refused" {
			t.Fatal(result)
		}
		req.ContextID, req.RequestID, req.InstanceID = "new", "new-request", "old-host"
		result = h.residentOperation(context.Background(), req).(map[string]any)
		if result["status"] != "refused" {
			t.Fatal(result)
		}
	}
	if got := h.budget.Snapshot(); got.Allocations != 0 {
		t.Fatal(got)
	}
}

func TestHostResidentChildHelper(t *testing.T) {
	if os.Getenv("NOODLE_HOST_RESIDENT_TEST") != "1" {
		return
	}
	if err := residentchild.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestHostResidentLeaseRejectsProjectOnlyRelease(t *testing.T) {
	dir := t.TempDir()
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := admission.OpenResidents(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := admission.NewBudget(1)
	b.Register("current")
	observeKnownHostFixtureSamples(b)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h := &noodleHost{instanceID: "host", binarySHA: "binary", budget: b, residents: r, contexts: map[string]*hostContext{
		"current": {receipt: hostReceipt{ContextID: "current", RequestID: "request", ProjectDir: root, ContextState: "active"}}}}
	spec := admission.ResidentSpec{Name: "tool", Argv: []string{exe, "-test.run=^TestHostResidentChildHelper$"}, Directory: root,
		Environment: []string{"NOODLE_HOST_RESIDENT_TEST=1"}, Sharing: "shared", Transport: "network"}
	live, err := r.Acquire(spec, "current", b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.ReleaseContext("current"); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(filepath.Join(spec.Directory, "exit"), nil, 0600); err != nil {
			t.Error(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := r.WaitClosed(ctx); err != nil {
			t.Error(err)
		}
	})
	for _, req := range []hostRequest{
		{Operation: "resident_release", ProjectDir: root, ResidentID: live.ID},
		{Operation: "resident_release_bound", ProjectDir: root, ResidentID: live.ID, ContextID: "old", RequestID: "old-request", InstanceID: "host", BinarySHA256: "binary"},
	} {
		result := h.residentOperation(context.Background(), req).(map[string]any)
		if result["status"] != "refused" {
			t.Fatalf("foreign request released current lease: %+v", result)
		}
		rows, _ := r.Snapshot()
		if len(rows) != 1 || rows[0].State != "running" || len(rows[0].Contexts) != 1 {
			t.Fatal(rows)
		}
	}
	req := hostRequest{Operation: "resident_release_bound", ProjectDir: root, ResidentID: live.ID, ContextID: "current", RequestID: "request", InstanceID: "host", BinarySHA256: "binary"}
	result := h.residentOperation(context.Background(), req).(map[string]any)
	if result["status"] != "stopping" {
		t.Fatal(result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.WaitClosed(ctx); err != nil {
		t.Fatal(err)
	}
	result = h.residentOperation(context.Background(), req).(map[string]any)
	if result["status"] != "exited" || b.Snapshot().Allocations != 0 {
		t.Fatal(result)
	}
}

func TestHostResidentReadbackRetainsFinishedContextAndRefusesOtherHost(t *testing.T) {
	r, err := admission.OpenResidents(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, cleanup := range []bool{false, true} {
		receipt := hostReceipt{ContextID: "finished", RequestID: "original", HostInstanceID: "host", ProjectDir: "/original", ContextState: "detached", ConfigSHA256: "config", Mode: "manual", CleanupCompleted: cleanup}
		h := &noodleHost{instanceID: "host", binarySHA: "binary", budget: admission.NewBudget(1), residents: r, receipts: []hostReceipt{receipt}}
		req := hostRequest{Operation: "resident_inspect_bound", ProjectDir: receipt.ProjectDir, ContextID: receipt.ContextID, RequestID: receipt.RequestID, InstanceID: receipt.HostInstanceID, BinarySHA256: "binary", ConfigSHA256: receipt.ConfigSHA256, Mode: receipt.Mode, CleanupCompleted: cleanup}
		result := h.residentOperation(context.Background(), req).(map[string]any)
		if result["status"] != "observed" {
			t.Fatal(result)
		}
		req.CleanupCompleted = !cleanup
		if result = h.residentOperation(context.Background(), req).(map[string]any); result["status"] != "refused" {
			t.Fatal(result)
		}
		req.CleanupCompleted = cleanup
		req.InstanceID = "another-host"
		if result = h.residentOperation(context.Background(), req).(map[string]any); result["status"] != "refused" {
			t.Fatal(result)
		}
	}
}

func TestHostResidentCLIContinuesOnOriginalSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "n-res-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(dir, "project")
	if err = os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	// A different default must not be used for either effect or returned readback.
	t.Setenv("NOODLE_HOST_DIR", filepath.Join(dir, "default"))
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	requestFile := filepath.Join(dir, "request.json")
	binding := hostRequestBinding{Schema: 1, RequestID: "request", Operation: "attach", ProjectDir: project, ConfigSHA256: "config", Mode: "manual", BinarySHA256: hostDigest(raw), Status: "ok", HostDir: dir, CleanupCompleted: true}
	if err = saveHostRequest(requestFile, binding); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	seen := make(chan hostRequest, 4)
	done := make(chan error, 1)
	receipt := hostReceipt{Schema: 1, ContextID: "context", RequestID: "request", HostInstanceID: "instance", ProjectDir: project, ContextState: "active", ConfigSHA256: "config", Mode: "manual", BinarySHA256: binding.BinarySHA256, HostDir: dir, CleanupCompleted: binding.CleanupCompleted}
	go func() {
		for i := 0; i < 4; i++ {
			conn, e := listener.Accept()
			if e != nil {
				done <- e
				return
			}
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			var req hostRequest
			e = json.NewDecoder(conn).Decode(&req)
			if e == nil {
				seen <- req
				if req.Operation == "status" {
					e = json.NewEncoder(conn).Encode(hostResponse{hostReceipt: receipt, Status: "ok"})
				} else {
					e = json.NewEncoder(conn).Encode(map[string]any{"owner": "noodle.swap-manager", "status": "observed", "next": map[string]any{"argv": []string{"wrong-default"}}})
				}
			}
			conn.Close()
			if e != nil {
				done <- e
				return
			}
		}
		done <- nil
	}()
	for _, op := range []string{"release", "inspect"} {
		args := []string{op, "--request-file", requestFile}
		if op == "release" {
			args = append(args, "--id", "service")
		}
		cmd := newHostResidentsCmd(&App{projectDir: project})
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetArgs(args)
		if err = cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		var result struct {
			Next struct {
				Argv []string `json:"argv"`
			} `json:"next"`
			Readback struct {
				Argv []string `json:"argv"`
			} `json:"readback"`
		}
		if err = json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		want := []string{os.Args[0], "--project-dir", project, "host", "resources", "residents", "inspect", "--request-file", requestFile}
		if !slices.Equal(result.Next.Argv, want) || !slices.Equal(result.Readback.Argv, want) {
			t.Fatal(output.String())
		}
		status, operation := <-seen, <-seen
		if status.Operation != "status" || operation.Operation != "resident_"+op+"_bound" || operation.InstanceID != "instance" || operation.ContextID != "context" || operation.RequestID != "request" || !operation.CleanupCompleted {
			t.Fatal(status, operation)
		}
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
