//go:build !windows

package admission

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestResidentEscapeHelper(t *testing.T) {
	mode := os.Getenv("NOODLE_TEST_ESCAPE_MODE")
	if mode == "" {
		return
	}
	if mode == "child" {
		if _, err := syscall.Setsid(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("escaped.pid", []byte(fmt.Sprint(os.Getpid())), 0600); err != nil {
			t.Fatal(err)
		}
	} else {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		child := exec.Command(binary, "-test.run=^TestResidentEscapeHelper$")
		child.Env = append(os.Environ(), "NOODLE_TEST_ESCAPE_MODE=child")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
	}
	marker := "exit"
	if mode == "child" {
		marker = "escaped.exit"
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestResidentEscapedObservedDescendantRetainsQuotaWithoutBeingSignalled(t *testing.T) {
	r, b, spec := residentFixture(t)
	spec.Sharing = "context"
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spec.Argv = []string{binary, "-test.run=^TestResidentEscapeHelper$"}
	spec.Environment = []string{"NOODLE_TEST_ESCAPE_MODE=launcher"}
	v, err := r.Acquire(spec, "a", b)
	if err != nil {
		t.Fatal(err)
	}
	var escaped int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(filepath.Join(spec.Directory, "escaped.pid"))
		escaped, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		if escaped > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if escaped <= 0 {
		t.Fatal("fixture did not produce its escaped child")
	}
	// The fixture's cooperative marker remains usable after its launcher exits.
	t.Cleanup(func() {
		if err := os.WriteFile(filepath.Join(spec.Directory, "escaped.exit"), nil, 0600); err != nil {
			t.Error(err)
			return
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			rows, readErr := ObserveResidents(context.Background())
			if readErr != nil {
				t.Error(readErr)
				return
			}
			found := false
			for _, p := range rows {
				if p.PID == escaped {
					found = true
				}
			}
			if !found {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("escaped fixture exit not observed")
	})
	if _, err = r.Release(v.ID, "a"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	var rows []Resident
	for time.Now().Before(deadline) {
		rows, err = r.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if rows[0].State == "needs_owner_readback" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rows[0].State != "needs_owner_readback" || len(rows[0].ObservedDescendants) == 0 || b.Snapshot().Allocations != 1 {
		t.Fatalf("escaped descendant lost evidence or allocation: %+v", rows)
	}
	observations, err := ObserveResidents(context.Background())
	if err != nil || !residentDescendantsPresent(&rows[0], observations) {
		t.Fatalf("escaped child was improperly stopped: %v", err)
	}
	if rows, err = r.Reconcile(context.Background()); err != nil || rows[0].State != "needs_owner_readback" {
		t.Fatalf("readback forgot live descendant: %+v %v", rows, err)
	}
	if err := os.WriteFile(filepath.Join(spec.Directory, "escaped.exit"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, err = r.Reconcile(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if rows[0].State == "exited" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rows[0].State != "exited" || b.Snapshot().Allocations != 0 {
		t.Fatalf("confirmed absence did not return quota: %+v", rows)
	}
}
