package main

import (
	"encoding/json"
	"testing"

	loopruntime "github.com/poteto/noodle/runtime"
)

func TestOriginalArchiveHostRetirementReportsOnlyProvenOccupancy(t *testing.T) {
	owner := &hostOwnerBinding{
		HostRoot: "/original/host", GenerationDir: "/original/host/g/original",
		Current:         hostReceipt{HostInstanceID: "original", HostPID: 81},
		ExecutionBinary: loopruntime.SessionReadbackFileRef{Path: "/original/noodle", SHA256: "bound-old-executable"},
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any, map[string]int)
		want   string
	}{
		{"retained worker", func(map[string]any, map[string]int) {}, "original_host_worker_handle_retained"},
		{"drained count still requires retirement witness", func(_ map[string]any, r map[string]int) {
			r["active_workers"], r["owned_allocations"] = 0, 1
		}, "original_host_retirement_witness_required"},
		{"missing native worker gauge", func(_ map[string]any, r map[string]int) { delete(r, "active_workers") }, ""},
		{"different generation", func(row map[string]any, _ map[string]int) { row["host_instance_id"] = "foreign" }, ""},
		{"different binary", func(row map[string]any, _ map[string]int) { row["binary_sha256"] = "foreign" }, ""},
		{"other context", func(row map[string]any, _ map[string]int) { row["contexts"] = 2 }, ""},
		{"shared quota is unknown", func(row map[string]any, _ map[string]int) {
			row["shared_quota"] = map[string]any{"error": "original quota readback failed"}
		}, ""},
		{"unresolved native resident", func(_ map[string]any, r map[string]int) { r["unresolved_residents"] = 1 }, ""},
		{"shutdown already in progress", func(row map[string]any, _ map[string]int) {
			row["shutdown"] = map[string]bool{"requested": true}
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gauges := map[string]int{"active_workers": 1, "active_residents": 1, "owned_allocations": 2, "unresolved_residents": 0}
			row := map[string]any{"protocol": hostProtocol, "host_root": owner.HostRoot, "host_dir": owner.GenerationDir,
				"host_instance_id": owner.Current.HostInstanceID, "host_pid": owner.Current.HostPID, "binary_path": owner.ExecutionBinary.Path,
				"binary_sha256": owner.ExecutionBinary.SHA256, "contexts": 1, "shutdown": map[string]bool{"requested": false}, "resources": gauges}
			tc.change(row, gauges)
			quota, hasQuota := row["shared_quota"]
			if !hasQuota {
				quota = map[string]int{"maximum": 9, "allocations": gauges["owned_allocations"], "owned_allocations": gauges["owned_allocations"], "residents": gauges["active_residents"]}
			}
			delete(row, "shared_quota")
			resources := map[string]any{"shared_quota": quota}
			for name, value := range gauges {
				resources[name] = value
			}
			row["resources"] = resources
			raw, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			var result archiveRecoveryHostRetirementReadback
			err = readOriginalArchiveHostOccupancy(owner, raw, &result)
			if tc.want == "" {
				if err == nil || result.Status != "" || result.Workers != nil {
					t.Fatalf("unproven source established Host eligibility: %+v, %v", result, err)
				}
				return
			}
			if err != nil || result.Status != tc.want || result.Workers == nil || *result.Workers != gauges["active_workers"] ||
				result.Residents == nil || *result.Residents != gauges["active_residents"] {
				t.Fatalf("original Host occupancy differs: %+v, %v", result, err)
			}
		})
	}
}
