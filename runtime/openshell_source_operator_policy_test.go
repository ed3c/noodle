package runtime

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSourceOperatorPolicyCandidatePreservesSourceWithoutWorkerFiles(t *testing.T) {
	filename, _ := profileFixture(t)
	before, err := ReadSessionReadbackFile(filename, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(before.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"payload", "spec.json"} {
		if err = os.RemoveAll(filepath.Join(filepath.Dir(filename), path)); err != nil {
			t.Fatal(err)
		}
	}
	destination := filepath.Join(t.TempDir(), "operator-profile.json")
	candidate, err := PrepareSourceOperatorPolicy(before, filename, destination)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Schema != 1 || candidate.Before != before || candidate.BeforeBinding.ConfigFingerprint == candidate.AfterBinding.ConfigFingerprint {
		t.Fatal("candidate did not bind the original source and changed policy")
	}
	originalGateway, err := LoadOpenShellGatewaySpec(filename)
	if err != nil || candidate.BeforeSource != filename || candidate.BeforeBinding != originalGateway.Binding {
		t.Fatal("candidate changed the original configured path semantics", err)
	}
	if _, err = InspectSourceOperatorPolicy(before, candidate.After, candidate.After.Path); err == nil {
		t.Fatal("candidate accepted an unbound original source path")
	}
	updated := candidate.AfterBinding
	updated.ConfigFingerprint = candidate.BeforeBinding.ConfigFingerprint
	if updated != candidate.BeforeBinding {
		t.Fatal("candidate changed the Source deployment")
	}
	if _, err = LoadOpenShellProfile(candidate.After.Path); err == nil {
		t.Fatal("Source candidate bypassed missing worker payload admission")
	}
	repeated, err := PrepareSourceOperatorPolicy(before, filename, destination)
	if err != nil || candidate != repeated {
		t.Fatal("same candidate could not be read back", err)
	}
	if _, err = PrepareSourceOperatorPolicy(before, filename, before.Path); err == nil {
		t.Fatal("candidate overwrote the original profile")
	}
	if actual, err := os.ReadFile(before.Path); err != nil || !bytes.Equal(actual, data) {
		t.Fatal("original profile bytes changed", err)
	}
	if err = os.WriteFile(destination, []byte("foreign artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = PrepareSourceOperatorPolicy(before, filename, destination); err == nil {
		t.Fatal("candidate replaced another artifact")
	}
	if actual, err := os.ReadFile(destination); err != nil || string(actual) != "foreign artifact" {
		t.Fatal("conflicting artifact was changed", err)
	}
}

func TestSourceOperatorPolicyRejectsExtraChanges(t *testing.T) {
	for _, fault := range []string{"argv", "endpoint", "state-root", "environment", "worker-route", "retained", "duplicate-flag"} {
		t.Run(fault, func(t *testing.T) {
			filename, _ := profileFixture(t)
			before, err := ReadSessionReadbackFile(filename, nil)
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := PrepareSourceOperatorPolicy(before, filename, filepath.Join(t.TempDir(), "profile.json"))
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(candidate.After.Path)
			if err != nil {
				t.Fatal(err)
			}
			var f openShellProfileFile
			if err = json.Unmarshal(data, &f); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "argv":
				f.Gateway.Resident.Argv = append(f.Gateway.Resident.Argv, "--another-setting")
			case "endpoint":
				f.Gateway.Binding.Gateway = "https://127.0.0.1:1"
			case "state-root":
				f.Gateway.Binding.StateRoot = filepath.Join(t.TempDir(), "other-state")
			case "environment":
				f.Environment = append(f.Environment, "UNRELATED_POLICY=changed")
			case "worker-route":
				route := f.Routes["codex"]
				route.Binary = "/sandbox/payload/another-worker"
				f.Routes["codex"] = route
			case "retained":
				f.Gateway.Retained = append(f.Gateway.Retained, OpenShellRetainedSandbox{Name: "another-sandbox"})
			case "duplicate-flag":
				f.Gateway.Resident.Argv = append(f.Gateway.Resident.Argv, "--enable-operator-auth", "true")
			}
			writeProfileFixture(t, candidate.After.Path, f)
			after, err := ReadSessionReadbackFile(candidate.After.Path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = InspectSourceOperatorPolicy(before, after, filename); err == nil {
				t.Fatal("unrelated change accepted as auth-only", fault)
			}
		})
	}
}
