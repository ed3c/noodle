package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

// These disposable call transcripts test the protocol, not physical Host exit.
func closedSuccessorFixture(t *testing.T) (closedSuccessor, hostReceipt, loopruntime.ProgressMaintenance) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, value any) loopruntime.SessionReadbackFileRef {
		t.Helper()
		file := filepath.Join(dir, name)
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		ref, err := loopruntime.ReadSessionReadbackFile(file, nil)
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	now := time.Now().UTC().Add(-time.Minute)
	binary := write("noodle", "fixture Host image")
	r := hostReceipt{Schema: 1, Protocol: hostProtocol, HostPID: 99999999, HostInstanceID: "successor", ContextID: "failed-context", ProjectDir: filepath.Join(dir, "project"), ContextState: "failed", Mode: "manual", BinaryPath: binary.Path, BinarySHA256: binary.SHA256, RequestID: "request", HostDir: dir, CleanupCompleted: true}
	launch := write("launch.json", map[string]any{"argv": []string{binary.Path, "host", "serve"}, "environment": map[string]string{"NOODLE_HOST_DIR": dir}, "binary_sha256": binary.SHA256, "launcher_pid": r.HostPID, "created_at": now})
	tool := func(name, input string, result any, started time.Time) loopruntime.SessionReadbackFileRef {
		t.Helper()
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		return write(name+".json", map[string]any{"schema": 1, "source_thread": "disposable-fixture", "records": []any{
			map[string]any{"record": map[string]any{"timestamp": started, "payload": map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": name, "input": input}}},
			map[string]any{"record": map[string]any{"timestamp": started.Add(time.Second), "payload": map[string]any{"type": "custom_tool_call_output", "call_id": name, "output": []any{map[string]any{"type": "text", "text": string(data)}}}}},
		}})
	}
	identity, err := json.Marshal(map[string]string{"intent": launch.Path, "launcher": dir})
	if err != nil {
		t.Fatal(err)
	}
	launchCall := tool("launch-call", "text(await tools.exec_command({}));", map[string]any{"session_id": 123, "output": string(identity)}, now)
	waitResult := map[string]any{"exit_code": 0, "output": ""}
	waitCall := tool("wait-call", `text(await tools.write_stdin({"session_id":123}));`, waitResult, now.Add(40*time.Second))
	resident := admission.Resident{ID: "gateway", Name: "gateway", PID: 99999998, ProcessGroup: 99999998, Started: "fixture-generation", Fingerprint: "fingerprint", GuardID: "guard", State: "exited", Released: []string{r.ContextID}, StopRequested: now.Add(20 * time.Second).Format(time.RFC3339Nano)}
	resources := write("resources.json", map[string]any{"schema": 1, "protocol": hostProtocol, "host_instance_id": r.HostInstanceID, "host_pid": r.HostPID, "host_dir": r.HostDir, "binary_sha256": r.BinarySHA256, "contexts": 1, "web_url": "http://127.0.0.1:9999/loops", "resources": map[string]any{"active_workers": 0, "active_residents": 0, "active_allocations": 0, "unresolved_residents": 0}, "residents": map[string]any{"records": []admission.Resident{resident}, "error": nil}})
	shutdown := write("shutdown.json", map[string]any{"host_instance_id": r.HostInstanceID, "pid": r.HostPID, "launcher_session": 123, "resources": resources, "requested_at": now.Add(30 * time.Second)})
	closed := closedSuccessor{OriginalHostLifetime: loopruntime.OriginalHostLifetime{Launcher: launch, LauncherCall: launchCall, WaitCall: waitCall, WaitResult: write("wait-result.json", waitResult), LauncherSession: 123, ObservedAt: now.Add(42 * time.Second)}, Schema: 1, HostInstance: r.HostInstanceID, HostPID: r.HostPID, Resources: resources, Shutdown: shutdown}
	return closed, r, loopruntime.ProgressMaintenance{Resident: resident}
}

func changeRetirementSource(t *testing.T, ref *loopruntime.SessionReadbackFileRef, change func(map[string]any)) {
	t.Helper()
	var value map[string]any
	if _, err := loopruntime.ReadSessionReadbackFile(ref.Path, &value); err != nil {
		t.Fatal(err)
	}
	change(value)
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ref.Path, data, 0600); err != nil {
		t.Fatal(err)
	}
	*ref, err = loopruntime.ReadSessionReadbackFile(ref.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestSuccessorRetirementRequiresOriginalNormalExitAndClosedResources(t *testing.T) {
	for _, kind := range []string{"valid", "nonzero-exit", "missing-exit", "running-waiter", "foreign-wait", "foreign-thread", "foreign-launch-pid", "foreign-launch-binary", "foreign-resources", "missing-accounting", "allocation", "foreign-generation", "unreleased", "unrequested-stop"} {
		t.Run(kind, func(t *testing.T) {
			closed, receipt, plan := closedSuccessorFixture(t)
			switch kind {
			case "nonzero-exit":
				changeRetirementSource(t, &closed.WaitResult, func(v map[string]any) { v["exit_code"] = 137 })
			case "missing-exit":
				changeRetirementSource(t, &closed.WaitResult, func(v map[string]any) { delete(v, "exit_code") })
			case "running-waiter":
				changeRetirementSource(t, &closed.WaitResult, func(v map[string]any) { v["session_id"] = 123 })
			case "foreign-wait":
				changeRetirementSource(t, &closed.WaitCall, func(v map[string]any) {
					records := v["records"].([]any)
					records[0].(map[string]any)["record"].(map[string]any)["payload"].(map[string]any)["input"] = `text(await tools.write_stdin({"session_id":124}));`
				})
			case "foreign-thread":
				changeRetirementSource(t, &closed.WaitCall, func(v map[string]any) { v["source_thread"] = "foreign" })
			case "foreign-launch-pid":
				changeRetirementSource(t, &closed.Launcher, func(v map[string]any) { v["launcher_pid"] = 44444 })
			case "foreign-launch-binary":
				changeRetirementSource(t, &closed.Launcher, func(v map[string]any) { v["binary_sha256"] = "foreign" })
			case "foreign-resources":
				changeRetirementSource(t, &closed.Resources, func(v map[string]any) { v["host_instance_id"] = "foreign" })
			case "missing-accounting", "allocation":
				changeRetirementSource(t, &closed.Resources, func(v map[string]any) {
					a := v["resources"].(map[string]any)
					if kind == "allocation" {
						a["active_allocations"] = 1
					} else {
						delete(a, "unresolved_residents")
					}
				})
			case "foreign-generation":
				plan.Resident.GuardID = "different-guard"
			case "unreleased":
				receipt.ContextID = "another-context"
			case "unrequested-stop":
				changeRetirementSource(t, &closed.Shutdown, func(v map[string]any) { v["requested_at"] = time.Now().Add(-time.Hour) })
			}
			if kind == "nonzero-exit" || kind == "missing-exit" || kind == "running-waiter" {
				data, err := os.ReadFile(closed.WaitResult.Path)
				if err != nil {
					t.Fatal(err)
				}
				changeRetirementSource(t, &closed.WaitCall, func(v map[string]any) {
					records := v["records"].([]any)
					output := records[1].(map[string]any)["record"].(map[string]any)["payload"].(map[string]any)["output"].([]any)
					output[0].(map[string]any)["text"] = string(data)
				})
			}
			if kind == "foreign-resources" || kind == "missing-accounting" || kind == "allocation" {
				changeRetirementSource(t, &closed.Shutdown, func(v map[string]any) { v["resources"] = closed.Resources })
			}
			err := validateClosedSuccessor(closed, receipt, plan)
			if (err != nil) != (kind != "valid") {
				t.Fatalf("original lifetime control: %v", err)
			}
		})
	}
}

func TestSuccessorRetirementHistoryAllowsAppendWithoutLosingBaseline(t *testing.T) {
	rows := func(ids ...string) []map[string]any {
		t.Helper()
		var typed []hostReceipt
		for _, id := range ids {
			typed = append(typed, hostReceipt{Schema: 1, RequestID: "request-" + id, ContextID: "context-" + id, ContextState: "failed"})
		}
		data, err := json.Marshal(typed)
		if err != nil {
			t.Fatal(err)
		}
		var result []map[string]any
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, kind := range []string{"same", "append", "missing-target", "missing-baseline", "replace", "reorder", "duplicate-request", "duplicate-context", "unsupported-field"} {
		t.Run(kind, func(t *testing.T) {
			before, live := rows("old", "retired"), rows("old", "retired")
			switch kind {
			case "append":
				live = rows("old", "retired", "new")
			case "missing-target":
				live = rows("old")
			case "missing-baseline":
				live = rows("retired")
			case "replace":
				live[1]["binary_sha256"] = "foreign"
			case "reorder":
				live = rows("retired", "old")
			case "duplicate-request":
				live[1]["request_id"] = live[0]["request_id"]
			case "duplicate-context":
				live[1]["context_id"] = live[0]["context_id"]
			case "unsupported-field":
				live[0]["unknown"] = true
			}
			err := requireRetirementHistoryPrefix(live, before)
			if (err == nil) != (kind == "same" || kind == "append") {
				t.Fatalf("history control %s: %v", kind, err)
			}
		})
	}
}

func TestResourceRetirementDoesNotReofferTheFailedNativeRequest(t *testing.T) {
	_, receipt, _ := closedSuccessorFixture(t)
	for _, operation := range []string{"attach", "status", "detach"} {
		t.Run(operation, func(t *testing.T) {
			h := &noodleHost{dir: receipt.HostDir, instanceID: "next-host", binarySHA: "next-binary", contexts: map[string]*hostContext{}, receipts: []hostReceipt{receipt}}
			req := hostRequest{Operation: operation, ProjectDir: receipt.ProjectDir, RequestID: receipt.RequestID, ConfigPath: receipt.ConfigPath, ConfigSHA256: receipt.ConfigSHA256, Mode: receipt.Mode, CleanupCompleted: receipt.CleanupCompleted, BinarySHA256: h.binarySHA}
			result := h.handle(context.Background(), req)
			if result.Status != "unknown_effect" || result.ContextState != "failed" || len(h.contexts) != 0 || h.receipts[0] != receipt {
				t.Fatalf("retired resource debt changed or replayed the failed outcome: %+v", result)
			}
		})
	}
}
