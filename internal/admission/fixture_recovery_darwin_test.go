//go:build darwin && cgo

package admission

import (
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

func TestFixtureRecoveryLauncher(t *testing.T) {
	dir := os.Getenv("NOODLE_RECOVERY_LAUNCHER_DIR")
	if dir == "" {
		return
	}
	logPath := filepath.Join(dir, "resident-recovery-probe.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	helper := "-test.run=^TestResidentChildHelper$"
	if os.Getenv("NOODLE_RECOVERY_IGNORE_TERM") == "1" {
		helper = "-test.run=^TestResidentUnresponsiveHelper$"
	}
	child := exec.Command(os.Args[0], helper)
	child.Dir = dir
	child.Stdout = log
	child.Stderr = log
	child.Env = append(os.Environ(), "NOODLE_RESIDENT_TEST_CHILD=1", "NOODLE_RECOVERY_LEGACY_PROBE=1")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err = os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = os.WriteFile(filepath.Join(dir, "exit"), nil, 0600)
			t.Fatal("legacy probe not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	native, err := procx.ObserveNativeIdentity(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := procx.ObserveNativeIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	c := FixtureCandidate{Origin: "registered", PID: native.PID, UID: os.Getuid(), Started: fixtureStarted(native), Argv: []string{os.Args[0], helper}, Directory: dir, LogPath: logPath, Test: "TestFixtureRecoveryLegacyOrphan", ResidentID: "probe", Native: &native, Owner: &owner}
	journal := filepath.Join(dir, "owner")
	if err = os.Mkdir(journal, 0700); err != nil {
		t.Fatal(err)
	}
	m, err := OpenFixtureRecovery(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Register(FixtureManifest{Schema: 1, EvidenceSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Candidates: []FixtureCandidate{c}}, "original-request/context"); err != nil {
		t.Fatal(err)
	}
	m.RunPending(context.Background())
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "candidate.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	for time.Now().Before(deadline) {
		if _, err = os.Stat(filepath.Join(dir, "release-owner")); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = os.WriteFile(filepath.Join(dir, "exit"), nil, 0600)
	t.Fatal("controller failed to release launcher")
}

func TestFixtureRecoveryLegacyOrphan(t *testing.T) {
	for _, mode := range []string{"normal", "ignore"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			t.Cleanup(func() {
				if err := os.WriteFile(filepath.Join(dir, "exit"), nil, 0600); err != nil {
					t.Error(err)
				}
			})
			launcher := exec.Command(os.Args[0], "-test.run=^TestFixtureRecoveryLauncher$")
			launcher.Env = append(os.Environ(), "NOODLE_RECOVERY_LAUNCHER_DIR="+dir)
			if mode == "ignore" {
				launcher.Env = append(launcher.Env, "NOODLE_RECOVERY_IGNORE_TERM=1")
			}
			if err := launcher.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			t.Cleanup(func() {
				if !waited {
					_ = os.WriteFile(filepath.Join(dir, "release-owner"), nil, 0600)
					_ = launcher.Wait()
				}
			})
			var c FixtureCandidate
			deadline := time.Now().Add(2 * time.Second)
			for {
				data, err := os.ReadFile(filepath.Join(dir, "candidate.json"))
				if err == nil && json.Unmarshal(data, &c) == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("candidate evidence unavailable")
				}
				time.Sleep(10 * time.Millisecond)
			}
			m, err := OpenFixtureRecovery(filepath.Join(dir, "owner"))
			if err != nil {
				t.Fatal(err)
			}
			manifest := FixtureManifest{Schema: 1, EvidenceSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Candidates: []FixtureCandidate{c}}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			for {
				m, err = OpenFixtureRecovery(filepath.Join(dir, "owner"))
				if err != nil {
					t.Fatal(err)
				}
				rows, err := m.Snapshot()
				if err != nil {
					t.Fatal(err)
				}
				if rows[0].State == "waiting_owner" {
					if len(rows[0].Actions) != 0 {
						t.Fatal(rows)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("active owner not preserved", rows)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err = os.WriteFile(filepath.Join(dir, "release-owner"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err = launcher.Wait(); err != nil {
				t.Fatal(err)
			}
			waited = true
			deadline = time.Now().Add(5 * time.Second)
			var final FixtureReclamation
			for {
				m.RunPending(ctx)
				rows, err := m.Snapshot()
				if err != nil {
					t.Fatal(err)
				}
				final = rows[0]
				m.mu.Lock()
				running := m.running
				m.mu.Unlock()
				if final.State == "exited" && !running {
					break
				}
				if final.State == "unknown" || time.Now().After(deadline) {
					t.Fatalf("legacy fixture recovery failed: %+v", final)
				}
				time.Sleep(20 * time.Millisecond)
			}
			want := 1
			if mode == "ignore" {
				want = 2
			}
			if len(final.Actions) != want || final.Evidence == nil {
				t.Fatalf("recovery evidence/actions: %+v", final)
			}
			for _, a := range final.Actions {
				if a.Result != "sent" || a.IntentAt == "" || a.ResultAt == "" {
					t.Fatal(a)
				}
				if a.Evidence == nil {
					t.Fatal("per-action evidence was lost", a)
				}
				observed, _ := time.Parse(time.RFC3339Nano, a.Evidence.ObservedAt)
				intent, _ := time.Parse(time.RFC3339Nano, a.IntentAt)
				if observed.After(intent) {
					t.Fatal("action evidence must precede its control intent", a)
				}
			}
			if want == 2 {
				term, _ := time.Parse(time.RFC3339Nano, final.Actions[0].ResultAt)
				kill, _ := time.Parse(time.RFC3339Nano, final.Actions[1].IntentAt)
				if kill.Sub(term) < residentStopGrace {
					t.Fatal("force before grace elapsed", final.Actions)
				}
			}
			if err = m.Register(manifest, "new-binding-must-not-repeat"); err != nil {
				t.Fatal(err)
			}
			m.RunPending(ctx)
			rows, err := m.Snapshot()
			if err != nil || len(rows[0].Actions) != want {
				t.Fatalf("duplicate recovery: %+v %v", rows, err)
			}
		})
	}
}
