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

func sourceQuotaV3Fixture(t *testing.T, foreign bool) ([]byte, SourceResidentQuotaReturn) {
	t.Helper()
	raw, _ := sourceQuotaFixture(t, foreign)
	raw = bytes.ReplaceAll(raw, []byte(`"schema":2`), []byte(`"schema":3`))
	raw = bytes.ReplaceAll(raw, []byte(`"maximum":4`), []byte(`"maximum":9`))
	raw = bytes.ReplaceAll(raw, []byte(`"swap_used_limit_bytes":15000000000`), []byte(`"worker_admission_policy":"host-workers-v1"`))
	selected, err := PrepareSourceResidentQuotaReturnV3(raw, "source", []string{"context"}, "gateway")
	if err != nil {
		t.Fatal(err)
	}
	return raw, selected
}

func TestSourceQuotaV3ReturnPreservesHostPolicyAndOtherAllocations(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		raw, selected := sourceQuotaV3Fixture(t, foreign)
		dir, epoch := t.TempDir(), t.TempDir()
		path := filepath.Join(dir, "quota.json")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := ReturnSourceResidentQuotaV3(context.Background(), dir, epoch, raw, selected); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var beforeDoc, afterDoc map[string]json.RawMessage
		if json.Unmarshal(raw, &beforeDoc) != nil || json.Unmarshal(after, &afterDoc) != nil {
			t.Fatal("invalid quota document")
		}
		for key, value := range beforeDoc {
			if key != "leases" && !bytes.Equal(value, afterDoc[key]) {
				t.Fatalf("changed quota field %s: %s", key, after)
			}
		}
		var beforeRows, afterRows map[string]json.RawMessage
		if json.Unmarshal(beforeDoc["leases"], &beforeRows) != nil || json.Unmarshal(afterDoc["leases"], &afterRows) != nil {
			t.Fatal("invalid lease document")
		}
		delete(beforeRows, selected.LeaseID)
		if len(beforeRows) != len(afterRows) {
			t.Fatal("returned more than the selected resident")
		}
		for id, row := range beforeRows {
			if !bytes.Equal(row, afterRows[id]) {
				t.Fatal("changed another allocation", id)
			}
		}
		backup, err := os.ReadFile(filepath.Join(epoch, "quota-source-resident-before.json"))
		if err != nil || !bytes.Equal(backup, raw) {
			t.Fatal("lost original schema3 quota bytes", err)
		}
		info, _ := os.Stat(path)
		if err = ReturnSourceResidentQuotaV3(context.Background(), dir, epoch, raw, selected); err != nil {
			t.Fatal(err)
		}
		currentInfo, _ := os.Stat(path)
		if !os.SameFile(info, currentInfo) {
			t.Fatal("completed return rewrote quota")
		}
	}
}

func TestSourceQuotaV3ReturnRefusesWrongSchemaPolicyAndUnknownWrite(t *testing.T) {
	raw, selected := sourceQuotaV3Fixture(t, false)
	if _, err := PrepareSourceResidentQuotaReturn(raw, "source", []string{"context"}, "gateway"); err == nil {
		t.Fatal("schema2 preparation accepted schema3")
	}
	old, oldSelected := sourceQuotaFixture(t, false)
	if _, err := PrepareSourceResidentQuotaReturnV3(old, "source", []string{"context"}, "gateway"); err == nil {
		t.Fatal("schema3 preparation accepted schema2")
	}
	if err := ReturnSourceResidentQuotaV3(context.Background(), t.TempDir(), t.TempDir(), old, oldSelected); err == nil {
		t.Fatal("schema3 return accepted schema2")
	}
	if err := ReturnSourceResidentQuota(context.Background(), t.TempDir(), t.TempDir(), raw, selected); err == nil {
		t.Fatal("schema2 return accepted schema3")
	}
	for _, fault := range []string{"policy", "ceiling", "nonce", "row", "drift", "unknown-write"} {
		t.Run(fault, func(t *testing.T) {
			original, chosen := bytes.Clone(raw), selected
			current := bytes.Clone(raw)
			switch fault {
			case "policy":
				original = bytes.ReplaceAll(raw, []byte(`host-workers-v1`), []byte(`old-policy`))
				current = original
			case "ceiling":
				original = bytes.ReplaceAll(raw, []byte(`"maximum":9`), []byte(`"maximum":10`))
				current = original
			case "nonce":
				chosen.LeaseID = "another-allocation"
			case "row":
				chosen.Lease = bytes.ReplaceAll(chosen.Lease, []byte(`"keep":true`), []byte(`"keep":false`))
			case "drift":
				current = bytes.ReplaceAll(raw, []byte(`"last_start_window":42`), []byte(`"last_start_window":43`))
			}
			dir, epoch := t.TempDir(), t.TempDir()
			path := filepath.Join(dir, "quota.json")
			if err := os.WriteFile(path, current, 0600); err != nil {
				t.Fatal(err)
			}
			if fault == "unknown-write" {
				if err := ReturnSourceResidentQuotaV3(context.Background(), dir, epoch, original, chosen); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, current, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := ReturnSourceResidentQuotaV3(context.Background(), dir, epoch, original, chosen); err == nil {
				t.Fatal("invalid or unknown return accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, current) {
				t.Fatal("refusal changed quota bytes", err)
			}
		})
	}
}
