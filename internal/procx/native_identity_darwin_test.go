//go:build darwin && cgo

package procx

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestNativeIdentityHelper(t *testing.T) {
	mode := os.Getenv("NOODLE_NATIVE_IDENTITY_HELPER")
	if mode == "" {
		return
	}
	if mode == "ignore" {
		signal.Ignore(syscall.SIGTERM)
	}
	if err := os.WriteFile("ready", nil, 0600); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat("exec"); err == nil {
			if mode == "exec-empty-argv" {
				if err := syscall.Exec("/bin/sleep", []string{"", "30"}, os.Environ()); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "exec-exit" {
				if err := syscall.Exec(os.Args[0], []string{os.Args[0], "-test.run=^TestNativeIdentityReplacementHelper$"}, os.Environ()); err != nil {
					t.Fatal(err)
				}
			}
			if err := syscall.Exec("/bin/sleep", []string{"sleep", "30"}, os.Environ()); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := os.Stat("exit"); err == nil {
			if mode == "nonzero" {
				os.Exit(23)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func nativeFixture(t *testing.T, mode string) (*exec.Cmd, NativeIdentity) {
	t.Helper()
	c := exec.Command(os.Args[0], "-test.run=^TestNativeIdentityHelper$")
	c.Dir = t.TempDir()
	c.Env = append(os.Environ(), "NOODLE_NATIVE_IDENTITY_HELPER="+mode)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	// This is the test's unreaped original child; this cleanup never targets historical processes.
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(c.Dir, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native fixture not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	p, err := ObserveNativeIdentity(c.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	return c, p
}

func TestNativeIdentityGracefulAndForce(t *testing.T) {
	for _, mode := range []string{"normal", "ignore"} {
		t.Run(mode, func(t *testing.T) {
			c, p := nativeFixture(t, mode)
			if err := SignalNativeIdentity(p, mode == "ignore"); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(2 * time.Second)
			var visibilityError error
			for {
				q, err := ObserveNativeIdentity(c.Process.Pid)
				if errors.Is(err, ErrProcessGone) || (err == nil && q.Zombie) {
					break
				}
				if errors.Is(err, syscall.EACCES) && time.Now().Before(deadline) {
					if visibilityError == nil {
						t.Logf("kernel exit transition temporarily unobservable: %v", err)
					}
					visibilityError = err
					time.Sleep(10 * time.Millisecond)
					continue
				}
				if err != nil || time.Now().After(deadline) {
					t.Fatalf("exit unobserved: %+v %v", q, err)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func TestNativeIdentityPauseAndResumePreserveTheOriginalGeneration(t *testing.T) {
	c, original := nativeFixture(t, "normal")
	if original.Stopped {
		t.Fatal("new fixture already stopped")
	}
	if err := SetNativeSuspended(original, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		current, err := ObserveNativeIdentity(original.PID)
		if err != nil || !original.SameGeneration(current) {
			t.Fatal("pause changed original generation", current, err)
		}
		if current.Stopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("kernel stop not observed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if gone, err := ObserveNativeExit(original); gone || err != nil {
		t.Fatal("stopped process was treated as retired", gone, err)
	}
	if err := os.WriteFile(filepath.Join(c.Dir, "exit"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if current, err := ObserveNativeIdentity(original.PID); err != nil || !current.Stopped || current.Zombie {
		t.Fatal("stopped fixture progressed", current, err)
	}
	if err := SetNativeSuspended(original, false); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	var visibilityError error
	for {
		gone, err := ObserveNativeExit(original)
		if errors.Is(err, syscall.EACCES) && time.Now().Before(deadline) {
			if visibilityError == nil {
				t.Logf("resumed fixture exit temporarily unobservable: %v", err)
			}
			visibilityError = err
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("resumed fixture did not consume its exit marker")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNativeIdentityRejectsExecGeneration(t *testing.T) {
	c, old := nativeFixture(t, "normal")
	if err := os.WriteFile(filepath.Join(c.Dir, "exec"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var current NativeIdentity
	var visibilityError error
	for {
		var err error
		current, err = ObserveNativeIdentity(c.Process.Pid)
		if errors.Is(err, syscall.EACCES) && time.Now().Before(deadline) {
			if visibilityError == nil {
				t.Logf("kernel exec transition temporarily unobservable: %v", err)
			}
			visibilityError = err
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if current.Version != old.Version {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("exec generation did not change")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if gone, err := ObserveNativeExit(old); gone || err == nil {
		t.Fatalf("exec replacement must remain unknown, not settled: gone=%v err=%v", gone, err)
	}
	if err := SignalNativeIdentity(old, false); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("stale token must be refused by kernel: %v", err)
	}
	q, err := ObserveNativeIdentity(c.Process.Pid)
	if err != nil || q.Zombie || !current.SameGeneration(q) {
		t.Fatalf("replacement generation affected: %+v %v", q, err)
	}
	for _, paused := range []bool{true, false} {
		if err := SetNativeSuspended(old, paused); err == nil {
			t.Fatal("stale generation changed pause state of its exec replacement")
		}
	}
	q, err = ObserveNativeIdentity(c.Process.Pid)
	if err != nil || q.Zombie || q.Stopped || !current.SameGeneration(q) {
		t.Fatalf("stale pause control affected the replacement: %+v %v", q, err)
	}
}
