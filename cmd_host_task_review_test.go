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
		if result["schema_version"] != float64(1) {
			t.Fatal(result)
		}
		if name == "capabilities" {
			if len(result) != 5 || result["process_admitted_orders"] != true {
				t.Fatal("original admission capability changed")
			}
		} else if len(result) != 4 || result["candidate_bound_merge"] != true || result["local_task_review_recovery"] != true || result["local_task_merge_recovery"] != true {
			t.Fatal(result)
		}
	}
}
