package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSourceQuotaSelectionDerivesExactOriginalAllocationWithoutWrites(t *testing.T) {
	for _, fault := range []string{"", "foreign-owner", "worker", "missing-key", "missing-ledger", "other-schema"} {
		t.Run(fault, func(t *testing.T) {
			p, _ := sourcePolicyTestPlan(t)
			op, err := readSourcePolicySnapshot(p.SourcePolicy.Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			op.Resident.ID = "source-gateway"
			op.Resident.Contexts = []string{op.Contexts[0].ContextID}
			row := map[string]any{"owner": p.Source.Instance, "context": op.Resident.Contexts[0], "resident": true, "resident_key": op.Resident.ID, "opaque": "original"}
			if fault == "foreign-owner" {
				row["owner"] = "another-generation"
			} else if fault == "worker" {
				row["resident"] = false
			} else if fault == "missing-key" {
				delete(row, "resident_key")
			}
			schema := 2
			if fault == "other-schema" {
				schema = 3
			}
			raw := sourcePolicyTestJSON(t, map[string]any{"schema": schema, "maximum": 4, "swap_used_limit_bytes": 15000000000, "last_start_window": 42, "leases": map[string]any{"original-nonce": row}})
			quota := filepath.Join(p.Root, "quota")
			if err = os.MkdirAll(quota, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(quota, "quota.json")
			if fault != "missing-ledger" {
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			selected, err := observeSourceQuota(op)
			if fault == "" {
				if err != nil || selected.Schema != 2 || selected.ResidentReturn == nil || selected.ResidentReturn.LeaseID != "original-nonce" || selected.ResidentReturn.Owner != p.Source.Instance {
					t.Fatal("producer did not bind original quota row", selected, err)
				}
				encoded, err := json.Marshal(selected)
				if err != nil {
					t.Fatal(err)
				}
				var carried hostSourceQuotaSnapshot
				if err = json.Unmarshal(encoded, &carried); err != nil || !bytes.Equal(carried.ResidentReturn.Lease, selected.ResidentReturn.Lease) {
					t.Fatal("Source snapshot lost complete original row", err)
				}
			} else if fault == "other-schema" {
				if err != nil || selected.Schema != 3 || selected.ResidentReturn != nil {
					t.Fatal("reader invented a legacy return for another quota owner", selected, err)
				}
			} else if err == nil {
				t.Fatal("changed original Source quota accepted", fault)
			}
			if fault != "missing-ledger" {
				actual, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(actual, raw) {
					t.Fatal("readonly quota selection changed original bytes", err)
				}
			}
			entries, err := os.ReadDir(quota)
			if err != nil || (fault != "missing-ledger" && len(entries) != 1) || (fault == "missing-ledger" && len(entries) != 0) {
				t.Fatal("readonly selection created lock/intent/state", entries, err)
			}
		})
	}
}
