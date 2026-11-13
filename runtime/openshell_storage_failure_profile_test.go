package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func storageFailureProfileSnapshot(t *testing.T, f *storageFailureFixture) (string, map[string]any) {
	t.Helper()
	row := map[string]any{"path": f.profile.SourcePath, "profile_sha256": f.profile.SourceSHA256, "payload_sha256": f.profile.PayloadSHA256, "client_sha256": f.profile.ClientSHA256, "gateway_binding": f.profile.Gateway.Binding}
	path := filepath.Join(filepath.Dir(f.profile.SourcePath), "static-validation.json")
	storageFailureWrite(t, path, map[string]any{"schema": 1, "profiles": []any{row}})
	return path, row
}

func storageFailureChangedPayload(t *testing.T, f *storageFailureFixture) OpenShellProfile {
	t.Helper()
	path := filepath.Join(f.profile.PayloadDirectory, "noodle-progress")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed payload\n"), info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	current, err := LoadOpenShellProfile(f.profile.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if current.PayloadSHA256 == f.profile.PayloadSHA256 {
		t.Fatal("payload did not change")
	}
	return current
}

func TestStorageFailureHistoricalProfileSnapshot(t *testing.T) {
	f := newStorageFailureFixture(t, 1)
	storageFailureProfileSnapshot(t, f)
	current := storageFailureChangedPayload(t, f)
	before, err := os.ReadFile(f.plan.Dispatch.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readStorageFailureInputs(f.plan, current, false); err != nil {
		t.Fatalf("historical resource readback: %v", err)
	}
	if err := ValidateStorageFailureStop(f.plan, current); err == nil {
		t.Fatal("changed payload authorized a new live Stop")
	}
	after, err := os.ReadFile(f.plan.Dispatch.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("historical readback changed the frozen dispatch")
	}
}

func TestStorageFailureHistoricalProfileSnapshotRefusals(t *testing.T) {
	for _, name := range []string{"missing", "invalid_digest", "forged_digest", "wrong_schema", "wrong_profile", "duplicate", "symlink", "changed_route", "changed_gateway"} {
		t.Run(name, func(t *testing.T) {
			f := newStorageFailureFixture(t, 0)
			path, row := storageFailureProfileSnapshot(t, f)
			current := storageFailureChangedPayload(t, f)
			switch name {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "invalid_digest":
				row["profile_sha256"] = "invalid"
				storageFailureWrite(t, path, map[string]any{"schema": 1, "profiles": []any{row}})
			case "forged_digest":
				row["profile_sha256"] = strings.Repeat("0", 64)
				storageFailureWrite(t, path, map[string]any{"schema": 1, "profiles": []any{row}})
			case "wrong_schema":
				storageFailureWrite(t, path, map[string]any{"schema": 2, "profiles": []any{row}})
			case "wrong_profile":
				row["path"] = filepath.Join(t.TempDir(), "other.json")
				storageFailureWrite(t, path, map[string]any{"schema": 1, "profiles": []any{row}})
			case "duplicate":
				storageFailureWrite(t, path, map[string]any{"schema": 1, "profiles": []any{row, row}})
			case "symlink":
				other := filepath.Join(t.TempDir(), "snapshot.json")
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(other, b, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, path); err != nil {
					t.Fatal(err)
				}
			case "changed_route":
				route := current.Routes["codex"]
				route.Binary = "/sandbox/payload/other"
				current.Routes["codex"] = route
			case "changed_gateway":
				current.Gateway.Binding.ConfigFingerprint = "changed"
			}
			if _, err := readStorageFailureInputs(f.plan, current, false); err == nil {
				t.Fatal("unbound historical profile accepted")
			}
		})
	}
}

func TestStorageFailureHistoricalSnapshotKeepsMissingResourcesUnknown(t *testing.T) {
	f := newStorageFailureFixture(t, 1)
	coldStopFixture(t, f)
	storageFailureProfileSnapshot(t, f)
	current := storageFailureChangedPayload(t, f)
	if _, err := ValidateStorageFailureStopReceipt(f.ref, current); err != nil {
		t.Fatalf("original stop receipt: %v", err)
	}
	f.gateway.mutate(func() { clear(f.gateway.sandboxes) })
	if _, err := ObserveStorageFailureInventory(t.Context(), f.ref, current); err == nil {
		t.Fatal("missing resources accepted as Stopped")
	}
	creates, stops, execs := f.gateway.counts()
	if creates != 0 || stops != 0 || execs != 0 {
		t.Fatalf("readback sent effects: create=%d stop=%d exec=%d", creates, stops, execs)
	}
}
