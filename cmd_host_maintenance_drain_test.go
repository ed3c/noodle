package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestMaintenanceDrainFixtureChild(t *testing.T) {
	root := os.Getenv("NOODLE_MAINTENANCE_DRAIN_FIXTURE")
	if root == "" {
		return
	}
	requests := make(chan os.Signal, 4)
	signal.Notify(requests, syscall.SIGTERM)
	defer signal.Stop(requests)
	if err := os.WriteFile(filepath.Join(root, "ready"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	count := 0
	for {
		select {
		case <-requests:
			count++
			if err := os.WriteFile(filepath.Join(root, "terms"), []byte(strconv.Itoa(count)), 0600); err != nil {
				t.Fatal(err)
			}
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(root, "drain-completed")); err == nil {
				return
			}
		}
	}
}

func TestGracefulMaintenanceWaitsForDrainWithoutEscalationOrReplay(t *testing.T) {
	if !procx.NativeFixtureControlAvailable() {
		t.Skip("generation-bound native control unavailable")
	}
	root := t.TempDir()
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
			t.Fatal("drain fixture did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	identity, err := procx.ObserveNativeIdentity(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	actor := loopruntime.OpenShellRetiredActor{Process: identity}
	writes := 0
	save := func() error {
		writes++
		if writes == 1 && (actor.IntentAt.IsZero() || !actor.ExitedAt.IsZero() || actor.ControlMethod != "term" || !actor.ControlAcceptedAt.IsZero()) {
			t.Fatal("graceful control preceded its durable intent")
		}
		if writes == 2 && (actor.ControlAcceptedAt.IsZero() || actor.ControlAcceptedAt.Before(actor.IntentAt) || !actor.ExitedAt.IsZero()) {
			t.Fatal("accepted TERM was not recorded independently from shutdown")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err = retireMaintenanceActor(ctx, &actor, false, save); !errors.Is(err, context.DeadlineExceeded) || writes != 2 || !actor.ExitedAt.IsZero() {
		t.Fatal("pending drain was escalated or declared complete", err, writes, actor)
	}
	if err = retireMaintenanceActor(context.Background(), &actor, false, save); err == nil || writes != 2 {
		t.Fatal("pending drain repeated control or lost its unknown state", err, writes)
	}
	if err = retireMaintenanceActor(context.Background(), &actor, true, save); err == nil || writes != 2 || actor.ControlMethod != "term" {
		t.Fatal("unknown TERM intent was escalated or relabeled as KILL", err)
	}
	deadline = time.Now().Add(time.Second)
	for {
		data, err := os.ReadFile(filepath.Join(root, "terms"))
		if err == nil && string(data) == "1" {
			break
		}
		if err == nil || time.Now().After(deadline) {
			t.Fatal("fixture did not receive exactly one graceful control", string(data), err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, err := procx.ObserveNativeIdentity(identity.PID)
	if err != nil || current.Zombie || current.Stopped || !identity.SameGeneration(current) {
		t.Fatal("pending drain fixture was force-killed or suspended", current, err)
	}
	if err = os.WriteFile(filepath.Join(root, "drain-completed"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		gone, readErr := procx.ObserveNativeExit(identity)
		if readErr == nil && gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("drained fixture exit remains unobserved", readErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = retireMaintenanceActor(context.Background(), &actor, false, save); err != nil || writes != 3 || actor.ExitedAt.IsZero() {
		t.Fatal("graceful drain exit did not settle the original intent", err, writes, actor)
	}
	if data, err := os.ReadFile(filepath.Join(root, "terms")); err != nil || string(data) != "1" {
		t.Fatal("graceful control was replayed during exit readback", string(data), err)
	}
}

func TestMaintenanceRetirementDoesNotRelabelLegacyOrForcedExit(t *testing.T) {
	forced := loopruntime.OpenShellRetiredActor{ControlMethod: "kill", IntentAt: time.Now(), ExitedAt: time.Now()}
	writes := 0
	if err := retireMaintenanceActor(context.Background(), &forced, false, func() error { writes++; return nil }); err == nil || writes != 0 || forced.ControlMethod != "kill" {
		t.Fatal("completed forced exit was relabeled as graceful", err)
	}
	legacy := loopruntime.OpenShellRetiredActor{IntentAt: time.Now(), ExitedAt: time.Now()}
	if err := retireMaintenanceActor(context.Background(), &legacy, false, func() error { writes++; return nil }); err != nil || writes != 0 || legacy.ControlMethod != "" || !legacy.ControlAcceptedAt.IsZero() {
		t.Fatal("legacy unknown exit acquired new control provenance", err)
	}
}
