//go:build darwin && cgo

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/procx"
)

func TestSourceServiceControlsPreserveUnknownAndForeignStops(t *testing.T) {
	if !procx.NativeFixtureControlAvailable() {
		t.Skip("native generation control unavailable")
	}
	for _, fault := range []string{"", "preexisting-stop", "missing-ack", "wrong-generation"} {
		t.Run(fault, func(t *testing.T) {
			actor := ownedSourceComputeScope(t).Host
			dir := t.TempDir()
			if fault == "preexisting-stop" {
				suspendSourceComputeFixture(t, actor)
			}
			selected := actor.Process
			if fault == "wrong-generation" {
				selected.Version++
			}
			err := sourceServiceSignalOnce(dir, "host-stop", selected, "stop")
			if fault == "preexisting-stop" || fault == "wrong-generation" {
				if err == nil {
					t.Fatal("unowned Source stop accepted", fault)
				}
				if _, err := os.Stat(filepath.Join(dir, "host-stop.json")); !os.IsNotExist(err) {
					t.Fatal("refusal wrote control intent", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(time.Second); sourceServiceStopped(actor.Process) != nil; time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("owned stop not observed")
				}
			}
			ack := filepath.Join(dir, "host-stop-ack.json")
			before, err := os.ReadFile(ack)
			if err != nil {
				t.Fatal(err)
			}
			if fault == "missing-ack" {
				if err := os.Remove(ack); err != nil {
					t.Fatal(err)
				}
			}
			if err := procx.SetNativeSuspended(actor.Process, false); err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(time.Second); sourceServiceStopped(actor.Process) == nil; time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("fixture CONT not observed")
				}
			}
			err = sourceServiceSignalOnce(dir, "host-stop", actor.Process, "stop")
			if fault == "missing-ack" && err == nil {
				t.Fatal("unknown ACK was reconstructed")
			}
			if fault == "" && err != nil {
				t.Fatal(err)
			}
			if sourceServiceStopped(actor.Process) == nil {
				t.Fatal("recorded signal was sent again")
			}
			if err := sourceServiceSignalOnce(dir, "host-kill", actor.Process, "kill"); err == nil {
				t.Fatal("running Host retired")
			}
			if _, err := os.Stat(filepath.Join(dir, "host-kill.json")); !os.IsNotExist(err) {
				t.Fatal("running Host received retirement intent", err)
			}
			if fault == "" {
				after, err := os.ReadFile(ack)
				if err != nil || string(before) != string(after) {
					t.Fatal("original ACK changed", err)
				}
			}
		})
	}
}
