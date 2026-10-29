package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	loopruntime "github.com/poteto/noodle/runtime"
)

func TestOpenShellRetainedRecoveryCLIInspectThenApply(t *testing.T) {
	profilePath := hostDeferralProfile(t)
	raw, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	var profileJSON map[string]any
	if err = json.Unmarshal(raw, &profileJSON); err != nil {
		t.Fatal(err)
	}
	refs := []loopruntime.OpenShellRetainedSandbox{{Name: "retained", SandboxID: "retained-id", Workspace: "default"}}
	profileJSON["gateway"].(map[string]any)["retained"] = refs
	raw, _ = json.Marshal(profileJSON)
	if err = os.WriteFile(profilePath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	profile, err := loopruntime.LoadOpenShellProfile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	root := profile.Gateway.Binding.StateRoot
	if err = os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	binding := profile.Gateway.Binding
	binding.StateRoot = canonical
	state := loopruntime.OpenShellCustodyState{Version: 4, Binding: binding, Blocked: "retained initialization supplied for existing custody", PendingRetained: refs}
	before, _ := json.Marshal(state)
	path := filepath.Join(root, "owner.json")
	if err = os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	for _, apply := range []bool{false, true} {
		cmd := NewRootCmd()
		var output bytes.Buffer
		cmd.SetOut(&output)
		args := []string{"--project-dir", "/nonexistent-project", "openshell", "recover-retained-init", "--profile", profilePath, "--expected-owner-sha256", hostDigest(before)}
		if apply {
			args = append(args, "--apply")
		}
		cmd.SetArgs(args)
		if err = cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		var result loopruntime.OpenShellRetainedRecovery
		if err = json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if !apply && result.Status != "eligible" || apply && result.Status != "applied" {
			t.Fatalf("unexpected result: %+v", result)
		}
		after, _ := os.ReadFile(path)
		if !apply && !bytes.Equal(after, before) {
			t.Fatal("default inspect mutated custody")
		}
	}
}
