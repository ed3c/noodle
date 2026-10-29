package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHostPoolRetainsOpaqueExecutionMetadata(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "pool.json")
	raw := []byte(`{"schema":1,"active":"","generations":{},"owners":{"historical":{"maintenance_hold":"epoch","execution_profile":{"kind":"original","threads":[{"thread_id":"original-thread","path":"/metadata","metadata_sha256":"hash","future_thread":true}],"future":{"unknown":true}},"context_inputs":{"home":"/old","native_config_sha256":"old-config","opaque":[1,2,3]}}},"queue":[]}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := updateHostPool(root, func(p *hostPool) error {
		owner := p.Owners["historical"]
		owner.ExecutionProfile.Kind = "updated"
		owner.ContextInputs.Home = "/new"
		owner.ContextInputs.NativeConfigSHA = ""
		p.Owners["historical"] = owner
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pool struct {
		Owners map[string]struct {
			Hold    string                     `json:"maintenance_hold"`
			Profile map[string]json.RawMessage `json:"execution_profile"`
			Inputs  map[string]json.RawMessage `json:"context_inputs"`
		} `json:"owners"`
	}
	if err := json.Unmarshal(saved, &pool); err != nil {
		t.Fatal(err)
	}
	owner := pool.Owners["historical"]
	if owner.Hold != "epoch" || string(owner.Profile["kind"]) != `"updated"` || string(owner.Inputs["home"]) != `"/new"` {
		t.Fatal("original held owner or typed changes lost", string(saved))
	}
	var future struct {
		Unknown bool `json:"unknown"`
	}
	if err := json.Unmarshal(owner.Profile["future"], &future); err != nil || !future.Unknown {
		t.Fatal("opaque execution profile lost", string(saved), err)
	}
	var threads []map[string]json.RawMessage
	if err := json.Unmarshal(owner.Profile["threads"], &threads); err != nil || len(threads) != 1 || string(threads[0]["future_thread"]) != "true" {
		t.Fatal("opaque thread metadata lost", string(saved), err)
	}
	var opaque []int
	if err := json.Unmarshal(owner.Inputs["opaque"], &opaque); err != nil || len(opaque) != 3 || opaque[0] != 1 || opaque[1] != 2 || opaque[2] != 3 {
		t.Fatal("opaque context inputs lost", string(saved), err)
	}
	if _, exists := owner.Inputs["native_config_sha256"]; exists {
		t.Fatal("cleared typed field restored by opaque metadata", string(saved))
	}
}
