package main

import (
	"context"
	"github.com/poteto/noodle/internal/admission"
	"path/filepath"
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
	b.Observe(admission.Sample{At: time.Now(), Unsupported: true}, nil)
	h := &noodleHost{dir: dir, instanceID: "actual-host", binaryPath: "/native/noodle", budget: b, residents: r, contexts: map[string]*hostContext{}}
	result := h.residentOperation(context.Background(), hostRequest{Operation: "resident_acquire", ProjectDir: dir}).(map[string]any)
	if result["status"] != "refused" || result["error"] != "active native context missing for resident lease" {
		t.Fatal(result)
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	h.contexts["original"] = &hostContext{receipt: hostReceipt{ProjectDir: root, ContextID: "original", ContextState: "active"}}
	result = h.residentOperation(context.Background(), hostRequest{Operation: "resident_acquire", ProjectDir: dir}).(map[string]any)
	if result["error"] != "resident spec missing" {
		t.Fatal(result)
	}
	h.blocked = true
	result = h.residentOperation(context.Background(), hostRequest{Operation: "resident_acquire", ProjectDir: dir}).(map[string]any)
	if result["error"] != "Host recovery pending" {
		t.Fatal(result)
	}
	result = h.residentOperation(context.Background(), hostRequest{Operation: "resident_inspect"}).(map[string]any)
	if result["status"] != "observed" || result["host_instance_id"] != "actual-host" {
		t.Fatal(result)
	}
}
