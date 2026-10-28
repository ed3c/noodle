//go:build darwin && cgo

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/loop"
)

// Opt-in native test: the immutable original legacy executable owns three
// manual fixture contexts. No model task or production request is admitted.
func TestRestrictedNativeLegacyMaintenance(t *testing.T) {
	legacy, candidate := os.Getenv("NOODLE_RESTRICTED_LEGACY_FIXTURE_BINARY"), os.Getenv("NOODLE_RESTRICTED_CANDIDATE_FIXTURE_BINARY")
	if legacy == "" || candidate == "" {
		t.Skip("native immutable fixture executables not selected")
	}
	if !procx.NativeFixtureControlAvailable() {
		t.Fatal("native generation-bound normal stop unavailable")
	}
	raw, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyLegacyBootstrapBinary(legacy, hostDigest(raw)); err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp("/tmp", "n-rm2-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("native fixture evidence retained:", base)
	root := filepath.Join(base, "h")
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOODLE_HOST_DIR", root)
	log, err := os.Create(filepath.Join(base, "legacy.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	old := exec.Command(legacy, "host", "serve")
	old.Env = hostBaselineEnvironment(root)
	old.Stdout, old.Stderr = log, log
	detachHostProcess(old)
	if err = old.Start(); err != nil {
		t.Fatal(err)
	}
	oldDone := make(chan error, 1)
	go func() { oldDone <- old.Wait() }()
	var original procx.NativeIdentity
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		original, err = procx.ObserveNativeIdentity(old.Process.Pid)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatal("fixture original identity unknown; retained without fallback control", err)
	}
	var plan hostMaintenancePlan
	var sourceStopAttempted bool
	defer func() {
		if !sourceStopAttempted {
			if observed, e := procx.ObserveNativeIdentity(original.PID); e == nil && original.SameGeneration(observed) {
				if e = procx.SignalNativeIdentity(original, false); e != nil {
					t.Error("fixture original normal stop unknown", e)
				}
			}
		}
		select {
		case e := <-oldDone:
			report, _ := json.Marshal(map[string]any{"native_identity": original, "exit_observed": true, "exit_error": func() string {
				if e != nil {
					return e.Error()
				}
				return ""
			}(), "maintenance_stop_attempted": sourceStopAttempted, "force": false})
			if writeErr := os.WriteFile(filepath.Join(base, "original-exit.json"), report, 0600); writeErr != nil {
				t.Error(writeErr)
			}
			if e != nil {
				t.Error("fixture original exit", e)
			}
		case <-time.After(10 * time.Second):
			t.Error("fixture original exit unknown; no second stop sent")
		}
	}()
	for deadline := time.Now().Add(8 * time.Second); ; time.Sleep(25 * time.Millisecond) {
		if _, err = readLegacyResources(context.Background(), root); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
	}
	var requests []string
	skills := filepath.Join(base, "skills")
	if err = os.MkdirAll(filepath.Join(skills, "fixture-task"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(skills, "fixture-task", "SKILL.md"), []byte("---\nname: fixture-task\ndescription: Controlled pending fixture.\nschedule: Fixture only.\n---\nNo execution is permitted.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"one", "two", "three"} {
		project := filepath.Join(base, name)
		if err = os.Mkdir(project, 0700); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@invalid", "commit", "--allow-empty", "-qm", "Create isolated fixture data"}} {
			cmd := exec.Command("git", args...)
			cmd.Dir = project
			if output, e := cmd.CombinedOutput(); e != nil {
				t.Fatal(e, string(output))
			}
		}
		orderID := "fixture-" + name
		config := []byte("mode = 'supervised'\n[server]\nenabled = false\n[adapters]\n[skills]\npaths = ['" + skills + "']\n[runtime.process]\nadmitted_orders = ['" + orderID + "']\n")
		if err = os.WriteFile(filepath.Join(project, ".noodle.toml"), config, 0600); err != nil {
			t.Fatal(err)
		}
		if err = os.Mkdir(filepath.Join(project, ".noodle"), 0700); err != nil {
			t.Fatal(err)
		}
		orders, _ := json.Marshal(loop.OrdersFile{GeneratedAt: time.Now(), Orders: []loop.Order{{ID: orderID, Status: loop.OrderStatusActive, Stages: []loop.Stage{{TaskKey: "fixture-task", Skill: "fixture-task", Prompt: "fixture data only; never dispatch", Status: loop.StageStatusPending}}}}})
		if err = os.WriteFile(filepath.Join(project, ".noodle", "orders.json"), orders, 0600); err != nil {
			t.Fatal(err)
		}
		custody := filepath.Join(base, name+"-admission")
		if err = os.Mkdir(custody, 0700); err != nil {
			t.Fatal(err)
		}
		request := filepath.Join(custody, "host-request.json")
		requests = append(requests, request)
		cmd := exec.Command(legacy, "--project-dir", project, "host", "attach", "--mode", "manual", "--request-file", request)
		cmd.Env = append(os.Environ(), "NOODLE_HOST_DIR="+root)
		output, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatal(e, string(output))
		}
		var response hostResponse
		if json.Unmarshal(output, &response) != nil || response.Status != "ok" {
			t.Fatal(string(output))
		}
		if err = os.WriteFile(filepath.Join(custody, "noodle.toml"), config, 0600); err != nil {
			t.Fatal(err)
		}
		if i < 2 {
			if err = os.Remove(filepath.Join(project, ".noodle.toml")); err != nil {
				t.Fatal(err)
			}
			// Simulate the verified App deletion pattern in fixture data only.
			for _, name := range []string{"status.json", "state.snapshot.json", "noodle.lock"} {
				if err = os.Remove(filepath.Join(project, ".noodle", name)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	pastID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bootstrap := hostBootstrap{Schema: 1, Phase: "active", Target: legacy, TargetSHA: hostDigest(raw), ProcessIdentity: "fixture historical source", Source: hostGeneration{ID: pastID, Directory: filepath.Join(root, "g", pastID)}}
	if err = saveHostBootstrap(root, bootstrap); err != nil {
		t.Fatal(err)
	}
	pool := hostPool{Schema: 1, Generations: map[string]hostGeneration{}, Owners: map[string]hostTaskOwner{"fixture-history": {State: "ready", RebuildError: "fixture retained history"}}, Queue: []hostVersionRequest{{ID: "fixture-version", State: "queued", Binary: legacy, SHA: hostDigest(raw)}}}
	if err = updateHostPool(root, func(p *hostPool) error { *p = pool; return nil }); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(root, "quota"), 0700); err != nil {
		t.Fatal(err)
	}
	quota := []byte(`{"schema":1,"maximum":4,"swap_used_limit_bytes":15000000000,"last_start_window":123,"leases":{}}`)
	if err = os.WriteFile(filepath.Join(root, "quota", "quota.json"), quota, 0600); err != nil {
		t.Fatal(err)
	}
	target, err := os.ReadFile(candidate)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = planHostMaintenance(context.Background(), root, candidate, hostDigest(target), requests)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.MarshalIndent(plan, "", "  ")
	if err = os.WriteFile(filepath.Join(base, "plan.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = validateRestrictedPlan(plan); err != nil {
		t.Fatal(err, string(data))
	}
	if !plan.Contexts[0].RecoveryRequired && !plan.Contexts[1].RecoveryRequired && !plan.Contexts[2].RecoveryRequired {
		t.Fatal("missing native snapshots were not identified")
	}
	dir := filepath.Join(root, "maintenance", plan.PlanSHA)
	var output bytes.Buffer
	for deadline := time.Now().Add(45 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		err = runRestrictedMaintenance(context.Background(), plan, dir, json.NewEncoder(&output))
		j, readErr := readRestrictedMaintenance(dir)
		if readErr == nil && (j.Phase == "stopping" || j.Phase == "stopped" || j.Phase == "imported" || j.Phase == "launching" || j.Phase == "active") {
			sourceStopAttempted = true
		}
		if j.Generation != nil && j.Generation.Process.PID > 0 {
			// This test owns the launch request. Teardown is one normal stop of
			// the exact observed generation after verifying zero fixture work.
			g := *j.Generation
			t.Cleanup(func() {
				if e := verifyRestrictedTarget(context.Background(), j); e != nil {
					t.Error("fixture target teardown readback", e)
					return
				}
				identity, e := procx.ObserveNativeIdentity(g.Process.PID)
				if e != nil {
					t.Error(e)
					return
				}
				if e = procx.SignalNativeIdentity(identity, false); e != nil {
					t.Error("fixture target normal stop", e)
					return
				}
				for until := time.Now().Add(10 * time.Second); time.Now().Before(until); time.Sleep(25 * time.Millisecond) {
					if _, e = procx.ObserveNativeIdentity(identity.PID); e == procx.ErrProcessGone {
						raw, _ := json.Marshal(map[string]any{"native_identity": identity, "generation_absent": true, "normal_stop_once": true, "force": false, "exit_status": "unknown"})
						if writeErr := os.WriteFile(filepath.Join(base, "target-exit.json"), raw, 0600); writeErr != nil {
							t.Error(writeErr)
						}
						return
					}
				}
				t.Error("fixture target exit unknown; no second stop sent")
			})
		}
		if err != nil {
			t.Fatal(err, "phase", j.Phase, "evidence", base)
		}
		if j.Phase == "active" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture transition incomplete", j.Phase, "evidence", base)
		}
	}
	if err = admission.NewBudgetWithSwapUsedLimit(4, admission.DefaultSwapUsedLimit).BindSharedQuota(filepath.Join(root, "quota"), "fixture-readback"); err != nil {
		t.Fatal(err)
	}
	result, err := readHostPool(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Owners) != 4 || result.Queue[0].State != "maintenance-held" {
		t.Fatal("fixture history lost or resumed", result)
	}
	for _, task := range plan.Contexts {
		snapshot, e := reducer.ReadSnapshot(filepath.Join(dir, task.Original.RequestID, "state.snapshot.json"))
		if e != nil {
			t.Fatal(e)
		}
		order, ok := snapshot.State.Orders["fixture-"+filepath.Base(task.Original.ProjectDir)]
		if !ok || len(order.Stages) != 1 || order.Stages[0].Status != "pending" {
			t.Fatal("fixture business work was changed, dropped or resumed", order)
		}
	}
	if err = os.WriteFile(filepath.Join(base, "transitions.jsonl"), output.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}
