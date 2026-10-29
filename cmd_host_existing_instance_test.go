package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRequiredExistingHostNeverStartsReplacement(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NOODLE_HOST_DIR", dir)
	project := t.TempDir()
	requestFile := filepath.Join(project, "original-request.json")
	result := callHost(context.Background(), hostRequest{Operation: "attach", ProjectDir: project, Mode: "manual", RequestFile: requestFile, RequiredHostInstanceID: "original-owner"})
	if result.Status != "busy" {
		t.Fatalf("known no-send outcome: %+v", result)
	}
	for _, name := range []string{"host.sock", "host.lock", "receipts.json", "host.log", "launch.lock"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("missing owner started or produced state %s: %v", name, err)
		}
	}
	// A later caller cannot remove the persisted condition by omitting the flag.
	second := callHost(context.Background(), hostRequest{Operation: "attach", ProjectDir: project, Mode: "manual", RequestFile: requestFile})
	if second.Status != "busy" {
		t.Fatalf("bound retry: %+v", second)
	}
	if _, err := os.Stat(filepath.Join(dir, "host.sock")); !os.IsNotExist(err) {
		t.Fatal("bound retry started replacement")
	}
}
func TestRequiredExistingHostRejectsReplacementBeforeAdmission(t *testing.T) {
	h := &noodleHost{instanceID: "replacement", binarySHA: "binary"}
	result := h.handle(context.Background(), hostRequest{Operation: "attach", RequiredHostInstanceID: "original", BinarySHA256: "binary"})
	if result.Status != "refused" || len(h.contexts) != 0 || len(h.receipts) != 0 {
		t.Fatalf("replacement admitted request: %+v", result)
	}
}
func TestRequiredExistingHostBindingCannotChange(t *testing.T) {
	dir := t.TempDir()
	request := hostRequest{Operation: "attach", RequiredHostInstanceID: "original", ProjectDir: dir, HostDir: dir, RequestFile: filepath.Join(dir, "request.json"), BinarySHA256: "binary", Mode: "manual"}
	binding, unlock, err := bindHostRequest(&request)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if binding.RequiredHostInstanceID != "original" {
		t.Fatal("original owner condition not persisted")
	}
	request.RequiredHostInstanceID = "replacement"
	if _, unlock, err := bindHostRequest(&request); err == nil {
		unlock()
		t.Fatal("replacement owner condition accepted")
	}
}
