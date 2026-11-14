//go:build darwin && cgo

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/procx"
)

func TestSourceKernelParentFixtureChild(t *testing.T) {
	root := os.Getenv("NOODLE_SOURCE_KERNEL_PARENT_FIXTURE")
	if root == "" {
		return
	}
	child := exec.Command(os.Args[0], "-test.run=^TestMaintenanceDrainFixtureChild$")
	child.Env = append(os.Environ(), "NOODLE_MAINTENANCE_DRAIN_FIXTURE="+root)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture gateway did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	identity, err := procx.ObserveNativeIdentity(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "gateway.json"), sourcePolicyTestJSON(t, identity), 0600); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "reap")); err == nil {
			_ = child.Wait()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSourcePolicyStatusReadsKernelExitWithParentStillStopped(t *testing.T) {
	if !procx.NativeFixtureControlAvailable() {
		t.Skip("generation-bound native fixture control unavailable")
	}
	for _, signalExit := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "signal"}[signalExit], func(t *testing.T) {
			root := t.TempDir()
			parent := exec.Command(os.Args[0], "-test.run=^TestSourceKernelParentFixtureChild$")
			parent.Env = append(os.Environ(), "NOODLE_SOURCE_KERNEL_PARENT_FIXTURE="+root)
			if err := parent.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = os.WriteFile(filepath.Join(root, "drain-completed"), nil, 0600)
				_ = os.WriteFile(filepath.Join(root, "reap"), nil, 0600)
				// Cleanup alone resumes this test's unreaped original child handle.
				_ = parent.Process.Signal(syscall.SIGCONT)
				_ = parent.Wait()
			})
			deadline := time.Now().Add(3 * time.Second)
			var gateway procx.NativeIdentity
			for {
				data, err := os.ReadFile(filepath.Join(root, "gateway.json"))
				if err == nil && json.Unmarshal(data, &gateway) == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("fixture original gateway binding unavailable", err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			host, err := procx.ObserveNativeIdentity(parent.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			if err = procx.SetNativeSuspended(host, true); err != nil {
				t.Fatal(err)
			}
			for {
				current, err := procx.ObserveNativeIdentity(host.PID)
				if err == nil && host.SameGeneration(current) && current.Stopped {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("fixture parent STOP unobserved", err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if signalExit {
				err = procx.SignalNativeIdentity(gateway, true)
			} else {
				err = os.WriteFile(filepath.Join(root, "drain-completed"), nil, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			p, _ := sourcePolicyTestPlan(t)
			op, err := readSourcePolicySnapshot(p.SourcePolicy.Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			op.NativeScope.Host.Process, op.NativeScope.Gateway.Process = host, gateway
			op.Resident.PID = gateway.PID
			var source map[string]any
			if err = json.Unmarshal(op.Source, &source); err != nil {
				t.Fatal(err)
			}
			source["host_pid"] = host.PID
			op.Source = sourcePolicyTestJSON(t, source)
			for i := range op.Contexts {
				op.Contexts[i].HostPID = host.PID
			}
			snapshot := filepath.Join(p.Root, "kernel-snapshot.json")
			if err = os.WriteFile(snapshot, sourcePolicyTestJSON(t, op), 0600); err != nil {
				t.Fatal(err)
			}
			p, err = prepareSourcePolicyHold(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(p.Root, "maintenance", p.PlanSHA)
			if err = os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err = saveRestrictedMaintenance(dir, restrictedMaintenance{Schema: 1, Plan: p, Phase: "source-policy-admission-held"}); err != nil {
				t.Fatal(err)
			}
			plan := filepath.Join(p.Root, "kernel-plan.json")
			if err = os.WriteFile(plan, sourcePolicyTestJSON(t, p), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("NOODLE_HOST_DIR", p.Root)
			for {
				if _, err = procx.ObserveNativeExitStatus(gateway); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("fixture zombie status unavailable", err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			var output bytes.Buffer
			cmd := newHostRestrictedCmd()
			cmd.SetOut(&output)
			cmd.SetArgs([]string{"--plan", plan, "--status"})
			if err = cmd.ExecuteContext(context.Background()); err != nil {
				t.Fatal(err)
			}
			var result struct {
				Completion sourcePolicyNormalExit `json:"source_gateway_normal_exit"`
			}
			if err = json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			want := "observed"
			if signalExit {
				want = "not_observed"
			}
			if result.Completion.State != want || result.Completion.Method != "kernel-zombie" || result.Completion.KernelExit == nil {
				t.Fatal("original status lost the kernel normal-exit boundary", output.String())
			}
			current, err := procx.ObserveNativeIdentity(host.PID)
			if err != nil || !host.SameGeneration(current) || !current.Stopped {
				t.Fatal("status resumed or replaced the original parent", current, err)
			}
		})
	}
}
