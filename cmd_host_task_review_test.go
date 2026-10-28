package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestHostTaskReviewCapabilitiesAreOfflineAndSeparate(t *testing.T) {
	for _, name := range []string{"capabilities", "review-capabilities"} {
		cmd := newHostCmd(&App{})
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetArgs([]string{"task", name})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if name == "capabilities" {
			if len(result) != 5 || result["schema_version"] != float64(2) || result["runtime_admitted_orders"] != true || result["config_key"] != "runtime.admitted_orders" || result["scope"] != "runtime_dispatch" || result["empty_unrestricted"] != true {
				t.Fatal("runtime admission capability differs")
			}
		} else if len(result) != 11 || result["schema_version"] != float64(1) || result["candidate_bound_merge"] != true || result["local_task_review_recovery"] != true || result["local_task_merge_recovery"] != true || result["local_task_completion_recovery"] != true || result["local_task_completion_recovery_version"] != float64(2) || result["local_task_roles"] != float64(1) || result["local_task_directory_scope"] != float64(1) || result["openshell_local_task_review"] != float64(1) || result["openshell_instruction_materialization"] != float64(1) || result["openshell_saved_progress_attempt"] != float64(1) {
			t.Fatal(result)
		}
	}
}
