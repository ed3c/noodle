//go:build !windows

package residentchild

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResidentLifetimeHelper(t *testing.T) {
	switch os.Getenv("NOODLE_RESIDENT_LIFETIME_HELPER") {
	case "parent":
		cmd := exec.Command(os.Args[0], "-test.run=^TestResidentLifetimeHelper$")
		cmd.Env = append(os.Environ(), "NOODLE_RESIDENT_LIFETIME_HELPER=child")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		waitForFile(t, "child.pid")
	case "child":
		if err := os.WriteFile("child.pid", []byte(fmt.Sprint(os.Getpid())), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Wait(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("child.done", []byte("original parent exited"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func waitForFile(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(name); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fixture evidence %s not observed", name)
}

func TestResidentHelperExitsWhenOriginalParentEnds(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Cleanup(func() {
		if err := os.WriteFile(filepath.Join(dir, "exit"), nil, 0600); err != nil {
			t.Error(err)
		}
	})
	cmd := exec.Command(os.Args[0], "-test.run=^TestResidentLifetimeHelper$")
	cmd.Env = append(os.Environ(), "NOODLE_RESIDENT_LIFETIME_HELPER=parent")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture parent: %v %s", err, output)
	}
	waitForFile(t, "child.done")
	pid, err := os.ReadFile("child.pid")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		output, err := exec.Command("ps", "-p", string(pid), "-o", "stat=").CombinedOutput()
		if err != nil && strings.TrimSpace(string(output)) == "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("fixture helper survived its original parent")
}
