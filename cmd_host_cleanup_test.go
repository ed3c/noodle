package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/loop"
)

func TestHostBoundCleanupProducesContinuationOnlyWhenEligible(t *testing.T) {
	request := filepath.Join(t.TempDir(), "request.json")
	response := hostResponse{hostReceipt: hostReceipt{BinaryPath: "/original/noodle", ProjectDir: "/original/project", HostInstanceID: "original-instance"}, Status: "eligible"}
	for _, status := range []string{"eligible", "busy", "unknown_effect"} {
		response.Status = status
		output, err := boundCleanupResult(response, request, false)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(output)
		if err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			Next *struct {
				Argv []string `json:"argv"`
			} `json:"next"`
		}
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if status != "eligible" {
			if decoded.Next != nil {
				t.Fatalf("%s produced mutation next", status)
			}
			continue
		}
		want := []string{"/original/noodle", "--project-dir", "/original/project", "host", "cleanup", "--request-file", request, "--apply", "--instance-id", "original-instance"}
		if decoded.Next == nil || !reflect.DeepEqual(decoded.Next.Argv, want) {
			t.Fatalf("bound continuation lost producer identity: %s", data)
		}
	}
}

func TestHostCapabilitiesDoNotClaimWindowsForceStop(t *testing.T) {
	if hostCapabilities("windows")["resident_force_after_grace"] != 0 || hostCapabilities("darwin")["resident_force_after_grace"] != 1 {
		t.Fatal("resident force capability does not match platform")
	}
}

func TestHostCleanupInspectApplyAndDuplicate(t *testing.T) {
	ctx := context.Background()
	req := hostTestRequest(t, t.TempDir())
	rt := &fakeHostRuntime{ready: make(chan struct{}), released: make(chan struct{})}
	h := &noodleHost{dir: t.TempDir(), binarySHA: req.BinarySHA256, instanceID: "instance", newRuntime: func(string, string, config.Config, loop.Dependencies) hostRuntime { return rt }}
	attached := h.handle(ctx, req)
	if attached.Status != "ok" {
		t.Fatalf("attach: %+v", attached)
	}
	cleanup := hostRequest{Operation: "cleanup", ProjectDir: req.ProjectDir, RequestID: attached.RequestID, ContextID: attached.ContextID, InstanceID: attached.HostInstanceID, ConfigSHA256: attached.ConfigSHA256, Mode: attached.Mode, BinarySHA256: req.BinarySHA256}
	if got := h.handle(ctx, cleanup); got.Status != "eligible" || h.contexts[attached.ContextID] == nil {
		t.Fatalf("inspect: %+v", got)
	}
	stale := cleanup
	stale.InstanceID = "old-instance"
	stale.Apply = true
	if got := h.handle(ctx, stale); got.Status != "refused" {
		t.Fatalf("stale cleanup accepted: %+v", got)
	}
	cleanup.Apply = true
	if got := h.handle(ctx, cleanup); got.Status != "ok" || got.ContextState != "detached" {
		t.Fatalf("apply: %+v", got)
	}
	if got := h.handle(ctx, cleanup); got.Status != "ok" || rt.shutdowns != 1 {
		t.Fatalf("duplicate: %+v shutdowns=%d", got, rt.shutdowns)
	}
}

func TestHostGlobalCleanupSelectsCurrentIdentityAndPreservesBusy(t *testing.T) {
	ctx := context.Background()
	h := &noodleHost{dir: t.TempDir(), instanceID: "current", binarySHA: "test-binary"}
	runtimes := []*fakeHostRuntime{}
	h.newRuntime = func(string, string, config.Config, loop.Dependencies) hostRuntime {
		rt := &fakeHostRuntime{ready: make(chan struct{}), released: make(chan struct{}), busy: len(runtimes) == 1}
		runtimes = append(runtimes, rt)
		return rt
	}
	for range 2 {
		if got := h.handle(ctx, hostTestRequest(t, t.TempDir())); got.Status != "ok" {
			t.Fatalf("attach: %+v", got)
		}
	}
	result := h.cleanupContexts(ctx, hostRequest{Operation: "cleanup_all", BinarySHA256: h.binarySHA, InstanceID: h.instanceID, Apply: true}).(map[string]any)
	rows := result["contexts"].([]hostResponse)
	if len(rows) != 2 || len(h.contexts) != 1 || result["status"] != "observed" {
		t.Fatalf("result: %+v", result)
	}
	if h.cleanupContexts(ctx, hostRequest{Operation: "cleanup_all", BinarySHA256: "stale", Apply: true}).(map[string]any)["status"] != "refused" {
		t.Fatal("binary mismatch accepted")
	}
	for _, c := range h.contexts {
		runtimes[1].busy = false
		if err := c.runtime.Release(ctx); err != nil {
			t.Fatal(err)
		}
		h.finish(c, <-c.done)
	}
}

func TestHostAutomaticCleanupUsesBoundPolicy(t *testing.T) {
	ctx := context.Background()
	h := &noodleHost{dir: t.TempDir(), instanceID: "current", binarySHA: "test-binary"}
	h.newRuntime = func(string, string, config.Config, loop.Dependencies) hostRuntime {
		return &fakeHostRuntime{ready: make(chan struct{}), released: make(chan struct{})}
	}
	for _, enabled := range []bool{false, true} {
		req := hostTestRequest(t, t.TempDir())
		req.CleanupCompleted = enabled
		if got := h.handle(ctx, req); got.Status != "ok" || got.CleanupCompleted != enabled {
			t.Fatalf("attach policy: %+v", got)
		}
	}
	h.cleanupAutomaticContexts(ctx)
	if len(h.contexts) != 1 {
		t.Fatalf("auto cleanup left %d contexts", len(h.contexts))
	}
	for _, c := range h.contexts {
		if c.receipt.CleanupCompleted {
			t.Fatal("policy context remained")
		}
		if err := c.runtime.Release(ctx); err != nil {
			t.Fatal(err)
		}
		h.finish(c, <-c.done)
	}
}

func TestHostGlobalCleanupStopsAtUnknownEffect(t *testing.T) {
	ctx := context.Background()
	h := &noodleHost{dir: t.TempDir(), instanceID: "current", binarySHA: "test-binary"}
	h.newRuntime = func(string, string, config.Config, loop.Dependencies) hostRuntime {
		return &fakeHostRuntime{ready: make(chan struct{}), released: make(chan struct{})}
	}
	var ids []string
	for range 2 {
		got := h.handle(ctx, hostTestRequest(t, t.TempDir()))
		if got.Status != "ok" {
			t.Fatalf("attach: %+v", got)
		}
		ids = append(ids, got.ContextID)
	}
	slices.Sort(ids)
	h.contexts[ids[0]].runtime.(*fakeHostRuntime).releaseErr = errors.New("original completion unreadable")
	result := h.cleanupContexts(ctx, hostRequest{Operation: "cleanup_all", BinarySHA256: h.binarySHA, InstanceID: h.instanceID, Apply: true}).(map[string]any)
	if result["status"] != "unknown_effect" || len(result["contexts"].([]hostResponse)) != 1 || len(h.contexts) != 2 {
		t.Fatalf("unknown cleanup continued effects: %+v", result)
	}
	for _, c := range h.contexts {
		c.runtime.(*fakeHostRuntime).releaseErr = nil
		if err := c.runtime.Release(ctx); err != nil {
			t.Fatal(err)
		}
		h.finish(c, <-c.done)
	}
}
