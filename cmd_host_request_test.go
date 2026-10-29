package main

import (
	"context"
	"encoding/json"
	"github.com/poteto/noodle/internal/lockfile"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostRequestCleanupPolicyIsBoundAndLegacyDefaultsOff(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		req := hostTestRequest(t, t.TempDir())
		req.RequestFile = filepath.Join(t.TempDir(), "request.json")
		req.CleanupCompleted = enabled
		binding, unlock, err := bindHostRequest(&req)
		if err != nil {
			t.Fatal(err)
		}
		unlock()
		if binding.CleanupCompleted != enabled {
			t.Fatal("policy not retained")
		}
		lookup := hostRequest{Operation: "status", ProjectDir: req.ProjectDir, BinarySHA256: req.BinarySHA256, RequestFile: req.RequestFile}
		_, unlock, err = bindHostRequest(&lookup)
		if err != nil {
			t.Fatal(err)
		}
		unlock()
		if lookup.CleanupCompleted != enabled {
			t.Fatal("readback lost original policy")
		}
		changed := req
		changed.CleanupCompleted = !enabled
		if _, unlock, err = bindHostRequest(&changed); err == nil {
			unlock()
			t.Fatal("policy changed in existing admission")
		}
		if !enabled {
			data, err := os.ReadFile(req.RequestFile)
			if err != nil || strings.Contains(string(data), "cleanup_completed") {
				t.Fatalf("legacy bytes changed: %s %v", data, err)
			}
		}
	}
}

func TestHostRequestBindingSeparatesOldReceiptAndNeverReplaysUnknown(t *testing.T) {
	root := t.TempDir()
	req := hostTestRequest(t, root)
	req.RequestID = ""
	req.RequestFile = filepath.Join(t.TempDir(), "attach.json")
	binding, unlock, err := bindHostRequest(&req)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if binding.RequestID == "" || req.Operation != "attach" {
		t.Fatalf("new request: %+v", binding)
	}
	retry := req
	retry.RequestID = ""
	_, unlock, err = bindHostRequest(&retry)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if retry.Operation != "status" || retry.RequestID != binding.RequestID {
		t.Fatalf("unknown request was replayed: %+v", retry)
	}
	h := &noodleHost{binarySHA: req.BinarySHA256, receipts: []hostReceipt{{ProjectDir: root, RequestID: "old-request", ConfigSHA256: req.ConfigSHA256, Mode: req.Mode, ContextState: "detached"}}}
	if got := h.handle(context.Background(), retry); got.Status != "not_found" {
		t.Fatalf("old receipt satisfied new request: %+v", got)
	}
	if err := os.WriteFile(filepath.Join(root, ".noodle.toml"), []byte("broken [ config"), 0600); err != nil {
		t.Fatal(err)
	}
	lookup := hostRequest{Operation: "status", ProjectDir: root, RequestFile: req.RequestFile, BinarySHA256: req.BinarySHA256}
	_, unlock, err = bindHostRequest(&lookup)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if lookup.ConfigSHA256 != req.ConfigSHA256 || lookup.Mode != req.Mode || lookup.RequestID != binding.RequestID {
		t.Fatalf("lookup lost original inputs: %+v", lookup)
	}
	stored, err := os.ReadFile(req.RequestFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "CONTEXT_SECRET") || strings.Contains(string(stored), "=first") {
		t.Fatal("request file stores credentials")
	}
}

func TestHostRequestKnownBusyCanRefreshEnvironment(t *testing.T) {
	req := hostTestRequest(t, t.TempDir())
	req.RequestFile = filepath.Join(t.TempDir(), "request.json")
	binding, unlock, err := bindHostRequest(&req)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	binding.Status = "busy"
	if err := saveHostRequest(req.RequestFile, *binding); err != nil {
		t.Fatal(err)
	}
	req.Environment = []string{"GH_TOKEN=replacement"}
	updated, unlock, err := bindHostRequest(&req)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if req.Operation != "attach" || updated.RequestID != binding.RequestID || updated.EnvironmentSHA256 == binding.EnvironmentSHA256 {
		t.Fatalf("known busy request was not refreshed: %+v", updated)
	}
}

func TestHostRequestRetainsEndpointAndChecksContextIdentity(t *testing.T) {
	req := hostTestRequest(t, t.TempDir())
	req.RequestFile = filepath.Join(t.TempDir(), "request.json")
	binding, unlock, err := bindHostRequest(&req)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	lookup := hostRequest{Operation: "detach", ProjectDir: req.ProjectDir, BinarySHA256: req.BinarySHA256, RequestFile: req.RequestFile, HostDir: "/different-host", ContextID: "context", InstanceID: "instance"}
	_, unlock, err = bindHostRequest(&lookup)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if lookup.HostDir != binding.HostDir || lookup.RequestID != binding.RequestID || lookup.Operation != "detach" {
		t.Fatalf("binding lost: %+v", lookup)
	}
	receipt := hostReceipt{ProjectDir: req.ProjectDir, ConfigSHA256: req.ConfigSHA256, Mode: req.Mode, RequestID: req.RequestID, ContextID: "context", HostInstanceID: "instance"}
	if !hostReceiptMatches(lookup, receipt) {
		t.Fatal("original identities did not match")
	}
	lookup.InstanceID = "foreign"
	if hostReceiptMatches(lookup, receipt) {
		t.Fatal("request file bypassed foreign context identity")
	}
}

func TestHostConcurrentFirstClientsKeepKnownUnsentRequestsRetryable(t *testing.T) {
	dir, err := os.MkdirTemp("", "nh-launch-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("NOODLE_HOST_DIR", dir)
	lock, err := lockfile.TryLock(filepath.Join(dir, "launch.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	requests := []hostRequest{hostTestRequest(t, t.TempDir()), hostTestRequest(t, t.TempDir())}
	results := make(chan hostResponse, len(requests))
	for i := range requests {
		requests[i].RequestFile = filepath.Join(requests[i].ProjectDir, "request.json")
		go func(req hostRequest) { results <- callHost(context.Background(), req) }(requests[i])
	}
	for range requests {
		if got := <-results; got.Status != "busy" {
			t.Fatalf("known unsent response: %+v", got)
		}
	}
	for _, req := range requests {
		data, err := os.ReadFile(req.RequestFile)
		if err != nil {
			t.Fatal(err)
		}
		var binding hostRequestBinding
		if err := json.Unmarshal(data, &binding); err != nil {
			t.Fatal(err)
		}
		if binding.Status != "busy" {
			t.Fatalf("request cannot retry: %+v", binding)
		}
		req.BinarySHA256 = binding.BinarySHA256
		req.ProjectDir = binding.ProjectDir
		_, unlock, err := bindHostRequest(&req)
		if err != nil {
			t.Fatal(err)
		}
		unlock()
		if req.Operation != "attach" || req.RequestID != binding.RequestID {
			t.Fatalf("unsent request became read-only: %+v", req)
		}
	}
}
