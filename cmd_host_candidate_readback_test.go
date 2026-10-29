package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCandidateReadbackUsesOriginalRuntimeAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		admitted     bool
	}{
		{"openshell", "[runtime]\ndefault='openshell'\nadmitted_orders=['task']\n", true},
		{"native-process", "[runtime]\nadmitted_orders=['task']\n", true},
		{"legacy-process", "[runtime.process]\nadmitted_orders=['task']\n", true},
		{"openshell-foreign", "[runtime]\ndefault='openshell'\nadmitted_orders=['other']\n", false},
		{"openshell-no-process-fallback", "[runtime]\ndefault='openshell'\n[runtime.process]\nadmitted_orders=['task']\n", false},
		{"legacy-not-widened", "[runtime]\nadmitted_orders=['task']\n[runtime.process]\nadmitted_orders=['other']\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, request, _ := ownerBindingFixture(t)
			data := []byte(tc.config)
			if err := os.WriteFile(filepath.Join(root, ".noodle.toml"), data, 0600); err != nil {
				t.Fatal(err)
			}
			var binding hostRequestBinding
			raw, err := os.ReadFile(request)
			if err != nil || json.Unmarshal(raw, &binding) != nil {
				t.Fatal("fixture request unavailable", err)
			}
			binding.ConfigSHA256 = hostDigest(data)
			writeOwnerJSON(t, request, binding)
			if err := updateHostPool(root, func(p *hostPool) error {
				o := p.Owners[binding.RequestID]
				o.Original.ConfigSHA256, o.Current.ConfigSHA256 = binding.ConfigSHA256, binding.ConfigSHA256
				p.Owners[binding.RequestID] = o
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// An empty canonical snapshot stops the read before worktree access.
			if err := os.MkdirAll(filepath.Join(root, ".noodle"), 0700); err != nil {
				t.Fatal(err)
			}
			writeOwnerJSON(t, filepath.Join(root, ".noodle/state.snapshot.json"), map[string]any{})
			cmd := newHostCandidateReadbackCmd()
			cmd.SetArgs([]string{"--request-file", request, "--order-id", "task"})
			err = cmd.Execute()
			want := "order is outside original admitted task"
			if tc.admitted {
				want = "original task stage unavailable"
			}
			if err == nil || err.Error() != want {
				t.Fatalf("got %v, want %s", err, want)
			}
		})
	}
}
