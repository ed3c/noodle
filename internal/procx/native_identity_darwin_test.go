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
			if err := syscall.Exec("/bin/sleep", []string{"sleep", "30"}, os.Environ()); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := os.Stat("exit"); err == nil {
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
			for {
				q, err := ObserveNativeIdentity(c.Process.Pid)
				if errors.Is(err, ErrProcessGone) || (err == nil && q.Zombie) {
					break
				}
				if err != nil || time.Now().After(deadline) {
					t.Fatalf("exit unobserved: %+v %v", q, err)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func TestNativeIdentityRejectsExecGeneration(t *testing.T) {
	c, old := nativeFixture(t, "normal")
	if err := os.WriteFile(filepath.Join(c.Dir, "exec"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var current NativeIdentity
	for {
		var err error
		current, err = ObserveNativeIdentity(c.Process.Pid)
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
	if err := SignalNativeIdentity(old, false); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("stale token must be refused by kernel: %v", err)
	}
	q, err := ObserveNativeIdentity(c.Process.Pid)
	if err != nil || q.Zombie || !current.SameGeneration(q) {
		t.Fatalf("replacement generation affected: %+v %v", q, err)
	}
}
