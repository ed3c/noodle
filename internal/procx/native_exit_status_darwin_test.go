//go:build darwin && cgo

package procx

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNativeIdentityReplacementHelper(t *testing.T) {
	if os.Getenv("NOODLE_NATIVE_IDENTITY_HELPER") != "exec-exit" {
		return
	}
	if err := os.WriteFile("exec-ready", nil, 0600); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat("exit"); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNativeExitStatusSeparatesNormalSignalAndExecWithoutReaping(t *testing.T) {
	for _, mode := range []string{"normal", "nonzero", "signal", "exec-exit"} {
		t.Run(mode, func(t *testing.T) {
			child, original := nativeFixture(t, mode)
			if status, err := ObserveNativeExitStatus(original); err == nil || status.NormalZero() {
				t.Fatal("live process default status was accepted as normal exit", status, err)
			}
			marker := "exit"
			if mode == "signal" {
				if err := SignalNativeIdentity(original, true); err != nil {
					t.Fatal(err)
				}
			} else {
				if mode == "exec-exit" {
					marker = "exec"
				}
				if err := os.WriteFile(filepath.Join(child.Dir, marker), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			deadline := time.Now().Add(3 * time.Second)
			observedIdentity := original
			if mode == "exec-exit" {
				for {
					if _, err := os.Stat(filepath.Join(child.Dir, "exec-ready")); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("replacement fixture did not start")
					}
					time.Sleep(10 * time.Millisecond)
				}
				var err error
				observedIdentity, err = ObserveNativeIdentity(original.PID)
				if err != nil || observedIdentity.Version == original.Version {
					t.Fatal("kernel did not distinguish the exec replacement", observedIdentity, err)
				}
				if err := os.WriteFile(filepath.Join(child.Dir, "exit"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			var status NativeExitStatus
			for {
				var err error
				status, err = ObserveNativeExitStatus(observedIdentity)
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("kernel exit status remains unqualified", status, err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if mode == "exec-exit" {
				if !status.NormalZero() {
					t.Fatal("replacement did not reach a positively observed zombie exit0", status)
				}
				if wrong, err := ObserveNativeExitStatus(original); err == nil || !strings.Contains(err.Error(), "exec version") || wrong.NormalZero() {
					t.Fatal("original identity accepted replacement zombie exit0", wrong, err)
				}
			}
			if mode != "exec-exit" {
				if status.NormalZero() != (mode == "normal") {
					t.Fatal("kernel exit classification differs", mode, status)
				}
				if status.Version != original.Version || status.ParentPID != original.ParentPID {
					t.Fatal("kernel status lost original identity", status)
				}
				for _, fault := range []string{"version", "parent", "start"} {
					changed := original
					switch fault {
					case "version":
						changed.Version++
					case "parent":
						changed.ParentPID++
					case "start":
						changed.StartUsec++
					}
					if wrong, err := ObserveNativeExitStatus(changed); err == nil || wrong.NormalZero() {
						t.Fatal("changed original identity accepted kernel completion", fault, wrong, err)
					}
				}
			}
			waitErr := child.Wait()
			if (mode == "normal" || mode == "exec-exit") && waitErr != nil {
				t.Fatal("normal fixture did not exit zero", waitErr)
			}
			if (mode == "nonzero" || mode == "signal") && waitErr == nil {
				t.Fatal("terminal failure disappeared from original wait")
			}
			if uint32(child.ProcessState.Sys().(syscall.WaitStatus)) != status.WaitStatus {
				t.Fatal("kernel snapshot differs from original later wait", status, child.ProcessState)
			}
			if after, err := ObserveNativeExitStatus(original); err == nil || after.NormalZero() {
				t.Fatal("reaped process absence fabricated a normal exit", after, err)
			}
		})
	}
}
