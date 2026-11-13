package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func sourceQuotaFixture(t *testing.T, foreign bool) ([]byte, SourceResidentQuotaReturn) {
	t.Helper()
	raw := []byte(`{"schema":2,"maximum":4,"swap_used_limit_bytes":15000000000,"last_start_window":42,"opaque":{"number":9007199254740993},"leases":{"nonce":{ "owner":"source", "context":"context", "resident":true, "resident_key":"gateway", "opaque":{"keep":true} }}}`)
	if foreign {
		var root map[string]json.RawMessage
		if err := json.Unmarshal(raw, &root); err != nil {
			t.Fatal(err)
		}
		var rows map[string]json.RawMessage
		if err := json.Unmarshal(root["leases"], &rows); err != nil {
			t.Fatal(err)
		}
		rows["foreign-worker"] = json.RawMessage(`{"owner":"other","context":"worker","resident":false,"opaque":{"number":9007199254740993}}`)
		root["leases"], _ = json.Marshal(rows)
		raw, _ = json.Marshal(root)
	}
	selected, err := PrepareSourceResidentQuotaReturn(raw, "source", []string{"context"}, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	return raw, selected
}

func TestSourceQuotaReturnPreservesPolicyForeignRowsAndExactCarrier(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		raw, selected := sourceQuotaFixture(t, foreign)
		encoded, err := json.Marshal(selected)
		if err != nil {
			t.Fatal(err)
		}
		var carried SourceResidentQuotaReturn
		if err = json.Unmarshal(encoded, &carried); err != nil || !bytes.Equal(selected.Lease, carried.Lease) {
			t.Fatal("plan carrier changed exact original lease bytes", err)
		}
		dir, epoch := t.TempDir(), t.TempDir()
		path := filepath.Join(dir, "quota.json")
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err = ReturnSourceResidentQuota(context.Background(), dir, epoch, raw, carried); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var root map[string]json.RawMessage
		if err = json.Unmarshal(after, &root); err != nil {
			t.Fatal(err)
		}
		if string(root["schema"]) != "2" || string(root["maximum"]) != "4" || string(root["swap_used_limit_bytes"]) != "15000000000" || string(root["last_start_window"]) != "42" || string(root["opaque"]) != `{"number":9007199254740993}` {
			t.Fatal("return changed policy or opaque metadata", string(after))
		}
		var rows map[string]json.RawMessage
		if err = json.Unmarshal(root["leases"], &rows); err != nil || rows["nonce"] != nil || (foreign && string(rows["foreign-worker"]) != `{"owner":"other","context":"worker","resident":false,"opaque":{"number":9007199254740993}}`) {
			t.Fatal("return changed another allocation", string(after), err)
		}
		backup, err := os.ReadFile(filepath.Join(epoch, "quota-source-resident-before.json"))
		if err != nil || !bytes.Equal(backup, raw) {
			t.Fatal("original quota bytes not preserved", err)
		}
		beforeInfo, _ := os.Stat(path)
		if err = ReturnSourceResidentQuota(context.Background(), dir, epoch, raw, selected); err != nil {
			t.Fatal("exact completed outcome could not be read back", err)
		}
		afterInfo, _ := os.Stat(path)
		if !os.SameFile(beforeInfo, afterInfo) {
			t.Fatal("completed return rewrote the quota")
		}
		if foreign {
			if err = MigrateEmptySourceQuotaV2(context.Background(), dir, epoch, after); err == nil {
				t.Fatal("foreign worker allocation was silently migrated away")
			}
		} else {
			if err = MigrateEmptySharedQuota(context.Background(), dir, epoch, after); err == nil {
				t.Fatal("original schema1 entry accepted schema2")
			}
			if err = MigrateEmptySourceQuotaV2(context.Background(), dir, epoch, after); err != nil {
				t.Fatal("explicit empty Source v2 migration failed", err)
			}
			migrated, _ := os.ReadFile(path)
			var state quotaState
			if json.Unmarshal(migrated, &state) != nil || state.Schema != sharedQuotaSchema || state.Maximum != DefaultWorkerLimit || state.Policy != WorkerAdmissionPolicy || len(state.Leases) != 0 {
				t.Fatal("Source migration did not adopt the existing Host policy", string(migrated))
			}
			old, err := os.ReadFile(filepath.Join(epoch, "quota-source-schema2.json"))
			if err != nil || !bytes.Equal(old, after) {
				t.Fatal("original empty schema2 not preserved", err)
			}
		}
	}
}

func TestSourceQuotaReturnRefusesDriftAmbiguityAndUnknownWrite(t *testing.T) {
	for _, fault := range []string{"nonce", "row", "worker", "ambiguous", "top-level", "unknown-write", "new-lease"} {
		t.Run(fault, func(t *testing.T) {
			raw, selected := sourceQuotaFixture(t, false)
			dir, epoch := t.TempDir(), t.TempDir()
			path := filepath.Join(dir, "quota.json")
			current := bytes.Clone(raw)
			switch fault {
			case "nonce":
				selected.LeaseID = "replacement-nonce"
			case "row":
				selected.Lease = bytes.ReplaceAll(selected.Lease, []byte(`"keep":true`), []byte(`"keep":false`))
			case "worker":
				current = bytes.ReplaceAll(raw, []byte(`"resident":true`), []byte(`"resident":false`))
				raw = current
			case "ambiguous":
				var root map[string]json.RawMessage
				_ = json.Unmarshal(raw, &root)
				var rows map[string]json.RawMessage
				_ = json.Unmarshal(root["leases"], &rows)
				rows["duplicate"] = rows["nonce"]
				root["leases"], _ = json.Marshal(rows)
				current, _ = json.Marshal(root)
				raw = current
			case "top-level":
				current = bytes.ReplaceAll(raw, []byte(`"last_start_window":42`), []byte(`"last_start_window":43`))
			}
			if err := os.WriteFile(path, current, 0600); err != nil {
				t.Fatal(err)
			}
			if fault == "unknown-write" || fault == "new-lease" {
				if err := ReturnSourceResidentQuota(context.Background(), dir, epoch, raw, selected); err != nil {
					t.Fatal(err)
				}
				if fault == "new-lease" {
					current = bytes.ReplaceAll(raw, []byte(`"nonce"`), []byte(`"new-allocation"`))
				}
				if err := os.WriteFile(path, current, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := ReturnSourceResidentQuota(context.Background(), dir, epoch, raw, selected); err == nil {
				t.Fatal("changed or unknown original return was replayed", fault)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, current) {
				t.Fatal("refusal rewrote the original quota", err)
			}
		})
	}
}
