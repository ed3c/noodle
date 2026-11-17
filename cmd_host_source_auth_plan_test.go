package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/statever"
	loopruntime "github.com/poteto/noodle/runtime"
)

func sourceAuthOwnerFixture(t *testing.T) (string, []string) {
	t.Helper()
	root, request, current := ownerBindingFixture(t)
	path := filepath.Join(root, "noodle.toml")
	raw := []byte("[runtime.openshell]\nprofile=\"/original/frozen/profile.json\"\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var binding hostRequestBinding
	rawRequest, err := os.ReadFile(request)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(rawRequest, &binding); err != nil {
		t.Fatal(err)
	}
	binding.ConfigPath, binding.ConfigSHA256 = path, hostDigest(raw)
	writeOwnerJSON(t, request, binding)
	current.ConfigPath, current.ConfigSHA256 = path, binding.ConfigSHA256
	writeOwnerJSON(t, filepath.Join(root, "g", current.HostInstanceID, "receipts.json"), []hostReceipt{current})
	if err = updateHostPool(root, func(p *hostPool) error {
		owner := p.Owners[binding.RequestID]
		owner.Original.ConfigPath, owner.Original.ConfigSHA256 = path, binding.ConfigSHA256
		owner.Current = current
		p.Owners[binding.RequestID] = owner
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(root, ".noodle", "state.snapshot.json")
	if err = os.MkdirAll(filepath.Dir(snapshot), 0700); err != nil {
		t.Fatal(err)
	}
	var state reducer.DurableSnapshot
	state.State.SchemaVersion = statever.Current
	writeOwnerJSON(t, snapshot, state)
	return request, []string{request, path, snapshot, filepath.Join(root, "pool.json")}
}

func TestSourceAuthPlanCLIReportsMissingOriginalSessionWithoutEffects(t *testing.T) {
	request, paths := sourceAuthOwnerFixture(t)
	before := map[string][]byte{}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		before[p] = raw
	}
	cmd := newHostSourceAuthPlanCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--request-file", request})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var plan loopruntime.SourceAuthPlan
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Status != "unknown" || plan.Executable || len(plan.Effects) != 0 || len(plan.Requires) != 1 || plan.Requires[0] != "unique_original_session_file" {
		t.Fatalf("missing original session was executable: %s", out.String())
	}
	for p, raw := range before {
		after, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(raw, after) {
			t.Fatalf("read-only CLI changed %s: %v", p, err)
		}
	}
}

func TestSourceAuthPlanCLIRejectsOriginalConfigOrSnapshotDrift(t *testing.T) {
	for _, kind := range []string{"config", "snapshot", "default-config"} {
		t.Run(kind, func(t *testing.T) {
			request, paths := sourceAuthOwnerFixture(t)
			if kind == "default-config" {
				// The original owner binding remains exact when the producer used the default path.
				root := filepath.Dir(paths[1])
				target := filepath.Join(root, ".noodle.toml")
				if err := os.Rename(paths[1], target); err != nil {
					t.Fatal(err)
				}
				var binding hostRequestBinding
				raw, err := os.ReadFile(request)
				if err != nil {
					t.Fatal(err)
				}
				if err = json.Unmarshal(raw, &binding); err != nil {
					t.Fatal(err)
				}
				binding.ConfigPath = ""
				writeOwnerJSON(t, request, binding)
				if err = updateHostPool(root, func(p *hostPool) error {
					owner := p.Owners[binding.RequestID]
					owner.Original.ConfigPath = ""
					owner.Current.ConfigPath = ""
					p.Owners[binding.RequestID] = owner
					writeOwnerJSON(t, filepath.Join(root, "g", owner.Current.HostInstanceID, "receipts.json"), []hostReceipt{owner.Current})
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				plan, err := prepareHostSourceAuthPlan(request, "")
				if err != nil || plan.Status != "unknown" {
					t.Fatalf("default config refused: %s %v", plan.Status, err)
				}
				return
			}
			if kind == "config" {
				f, err := os.OpenFile(paths[1], os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.WriteString("\n"); err != nil {
					t.Fatal(err)
				}
				if err = f.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				writeOwnerJSON(t, paths[2], reducer.DurableSnapshot{})
			}
			plan, err := prepareHostSourceAuthPlan(request, "")
			if err == nil || plan.Executable || len(plan.Effects) != 0 {
				t.Fatalf("%s drift accepted", kind)
			}
		})
	}
}

func TestSourceAuthPlanCLIRejectsSpawnOutsideCanonicalAttempt(t *testing.T) {
	for _, kind := range []string{"order_id", "stage_index", "session_id", "worktree_path", "provider", "model", "runtime"} {
		t.Run(kind, func(t *testing.T) {
			request, paths := sourceAuthOwnerFixture(t)
			root := filepath.Dir(request)
			snapshot := map[string]any{"state": map[string]any{"schema_version": statever.Current, "orders": map[string]any{"order": map[string]any{"order_id": "order", "stages": []any{map[string]any{"stage_index": 0, "runtime": "openshell", "provider": "codex", "model": "selected-model", "attempts": []any{map[string]any{"attempt_id": "canonical-attempt", "session_id": "session", "worktree_name": "original-worktree"}}}}}}}}
			writeOwnerJSON(t, paths[2], snapshot)
			dir := filepath.Join(root, ".noodle", "sessions", "session")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			spawn := map[string]string{"order_id": "order", "stage_index": "0", "session_id": "session", "worktree_path": filepath.Join(root, ".worktrees", "original-worktree"), "provider": "codex", "model": "selected-model", "runtime": "openshell"}
			spawn[kind] = "foreign"
			writeOwnerJSON(t, filepath.Join(dir, "spawn.json"), spawn)
			plan, err := prepareHostSourceAuthPlan(request, "")
			if err == nil || err.Error() != "original task spawn differs from canonical attempt" || plan.Executable || len(plan.Effects) != 0 {
				t.Fatalf("%s spawn drift was not refused at original binding: %v", kind, err)
			}
		})
	}
}
