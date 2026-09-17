package loop

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/orderx"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

func recoveryFixture(t *testing.T) (*testLoopContext, []byte) {
	t.Helper()
	tc := initialLoopAt(t, t.TempDir())
	if err := tc.loop.writeProjectionState(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tc.runtimeDir, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	proposal := initialProposal(t, tc.loop, strings.Repeat("0", 32), "B")
	return tc, proposal
}
func recoveryWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func recoveryJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	recoveryWrite(t, path, data)
}
func recoverySnapshot(t *testing.T, tc *testLoopContext, change func(*reducer.DurableSnapshot)) {
	t.Helper()
	s, err := reducer.ReadSnapshot(tc.loop.canonicalSnapshotPath())
	if err != nil {
		t.Fatal(err)
	}
	change(&s)
	if err := reducer.WriteSnapshotAtomic(tc.loop.canonicalSnapshotPath(), s); err != nil {
		t.Fatal(err)
	}
}
func recoverySession(t *testing.T, tc *testLoopContext, status string, pid int) {
	t.Helper()
	dir := filepath.Join(tc.runtimeDir, "sessions", "worker")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	recoveryJSON(t, filepath.Join(dir, "meta.json"), map[string]any{"session_id": "worker", "status": status, "alive": false})
	recoveryJSON(t, filepath.Join(dir, "process.json"), map[string]any{"session_id": "worker", "pid": pid})
}

func TestAdmissionRecoveryRetiresExactBytes(t *testing.T) {
	tc, proposal := recoveryFixture(t)
	before, err := os.ReadFile(tc.loop.canonicalSnapshotPath())
	if err != nil {
		t.Fatal(err)
	}
	ordersBefore, err := os.ReadFile(tc.loop.deps.OrdersFile)
	if err != nil {
		t.Fatal(err)
	}
	r := InspectAdmission(tc.projectDir, "/exact/noodle")
	if r.Status != "recoverable" || len(r.Next.Argv) != 7 || r.Next.Argv[5] != r.Subject.SHA256 || r.Next.Argv[6] != r.Revision {
		t.Fatalf("inspection: %+v", r)
	}
	r = RetireAdmission(tc.projectDir, "/exact/noodle", r.Subject.SHA256, r.Revision)
	if r.Status != "retired" {
		t.Fatalf("retirement: %+v", r)
	}
	receipt, err := readAdmissionReceipt(tc.runtimeDir, r.Subject.SHA256, r.Revision)
	if err != nil || receipt == nil || !receipt.Retired || !bytes.Equal(receipt.Proposal, proposal) || !strings.Contains(receipt.Reason, "initial_revision") {
		t.Fatalf("archive: %+v %v", receipt, err)
	}
	after, _ := os.ReadFile(tc.loop.canonicalSnapshotPath())
	ordersAfter, _ := os.ReadFile(tc.loop.deps.OrdersFile)
	if !bytes.Equal(before, after) || !bytes.Equal(ordersBefore, ordersAfter) || len(tc.runtime.calls) != 0 {
		t.Fatal("retirement changed canonical/projection or dispatched")
	}
	if _, err := os.Stat(tc.loop.deps.OrdersNextFile); !os.IsNotExist(err) {
		t.Fatal("mailbox remains")
	}
	if again := RetireAdmission(tc.projectDir, "/exact/noodle", r.Subject.SHA256, r.Revision); again.Status != "retired" {
		t.Fatalf("replay: %+v", again)
	}
	later := initialProposal(t, tc.loop, r.Revision, "C")
	if again := RetireAdmission(tc.projectDir, "/exact/noodle", r.Subject.SHA256, r.Revision); again.Status != "retired" {
		t.Fatalf("replay with later mailbox: %+v", again)
	}
	got, _ := os.ReadFile(tc.loop.deps.OrdersNextFile)
	if !bytes.Equal(got, later) {
		t.Fatal("later mailbox overwritten")
	}
}

func TestAdmissionRecoveryRefusalGates(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testing.T, *testLoopContext)
	}{
		{"missing_checkpoint", func(t *testing.T, tc *testLoopContext) {
			if err := os.Remove(tc.loop.canonicalSnapshotPath()); err != nil {
				t.Fatal(err)
			}
		}},
		{"corrupt_checkpoint", func(t *testing.T, tc *testLoopContext) {
			recoveryWrite(t, tc.loop.canonicalSnapshotPath(), []byte("{"))
		}},
		{"incomplete_checkpoint", func(t *testing.T, tc *testLoopContext) {
			recoverySnapshot(t, tc, func(s *reducer.DurableSnapshot) { s.EffectLedger = nil })
		}},
		{"unknown_schema", func(t *testing.T, tc *testLoopContext) {
			recoverySnapshot(t, tc, func(s *reducer.DurableSnapshot) { s.State.SchemaVersion = 99 })
		}},
		{"missing_orders", func(t *testing.T, tc *testLoopContext) {
			if err := os.Remove(tc.loop.deps.OrdersFile); err != nil {
				t.Fatal(err)
			}
		}},
		{"corrupt_orders", func(t *testing.T, tc *testLoopContext) { recoveryWrite(t, tc.loop.deps.OrdersFile, []byte("{")) }},
		{"missing_sessions", func(t *testing.T, tc *testLoopContext) {
			if err := os.Remove(filepath.Join(tc.runtimeDir, "sessions")); err != nil {
				t.Fatal(err)
			}
		}},
		{"valid_initial", func(t *testing.T, tc *testLoopContext) { initialProposal(t, tc.loop, ownerRevision(t, tc.loop), "B") }},
		{"noninitial", func(t *testing.T, tc *testLoopContext) {
			recoveryWrite(t, tc.loop.deps.OrdersNextFile, []byte(`{"orders":[{"id":"B","stages":[{"do":"execute"}]}]}`))
		}},
		{"corrupt_proposal", func(t *testing.T, tc *testLoopContext) { recoveryWrite(t, tc.loop.deps.OrdersNextFile, []byte("{")) }},
		{"owned", func(t *testing.T, tc *testLoopContext) {
			recoverySnapshot(t, tc, func(s *reducer.DurableSnapshot) {
				s.State.Orders["B"] = state.OrderNode{OrderID: "B", Status: state.OrderCompleted, Stages: []state.StageNode{{StageIndex: 0, Status: state.StageCompleted}}}
			})
		}},
		{"admitted", func(t *testing.T, tc *testLoopContext) {
			if err := tc.loop.recordInitialAdmissions([]string{"B"}); err != nil {
				t.Fatal(err)
			}
			if err := tc.loop.persistCanonicalCheckpoint(); err != nil {
				t.Fatal(err)
			}
		}},
		{"malformed_ledger", func(t *testing.T, tc *testLoopContext) {
			recoverySnapshot(t, tc, func(s *reducer.DurableSnapshot) {
				s.EffectLedger = append(s.EffectLedger, reducer.EffectLedgerRecord{EffectID: "unknown"})
			})
		}},
		{"projection_mismatch", func(t *testing.T, tc *testLoopContext) {
			if err := orderx.WriteOrdersAtomic(tc.loop.deps.OrdersFile, OrdersFile{Orders: []Order{{ID: "unknown", Status: OrderStatusActive}}}); err != nil {
				t.Fatal(err)
			}
		}},
		{"active_session", func(t *testing.T, tc *testLoopContext) { recoverySession(t, tc, "running", os.Getpid()) }},
		{"live_process", func(t *testing.T, tc *testLoopContext) { recoverySession(t, tc, "exited", os.Getpid()) }},
		{"ambiguous_session", func(t *testing.T, tc *testLoopContext) {
			recoverySession(t, tc, "exited", 99999999)
			recoveryWrite(t, filepath.Join(tc.runtimeDir, "sessions", "worker", "process.json"), []byte(`{}`))
		}},
		{"corrupt_session", func(t *testing.T, tc *testLoopContext) {
			recoverySession(t, tc, "exited", 99999999)
			recoveryWrite(t, filepath.Join(tc.runtimeDir, "sessions", "worker", "meta.json"), []byte(`{`))
		}},
		{"symlink_proposal", func(t *testing.T, tc *testLoopContext) {
			path := tc.loop.deps.OrdersNextFile
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			recoveryWrite(t, path+".target", data)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(path+".target", path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tc, _ := recoveryFixture(t)
			test.change(t, tc)
			before, _ := os.ReadFile(tc.loop.deps.OrdersNextFile)
			snapshotBefore, snapshotErr := os.ReadFile(tc.loop.canonicalSnapshotPath())
			digest := fmt.Sprintf("%x", sha256.Sum256(before))
			r := InspectAdmission(tc.projectDir, "noodle")
			if r.Status != "refused" || r.Invalid == "" || len(r.Next.Argv) != 0 {
				t.Fatalf("unsafe inspection: %+v", r)
			}
			revision := tc.loop.orderRevision
			r = RetireAdmission(tc.projectDir, "noodle", digest, revision)
			if r.Status != "refused" {
				t.Fatalf("unsafe retirement: %+v", r)
			}
			after, _ := os.ReadFile(tc.loop.deps.OrdersNextFile)
			snapshotAfter, afterErr := os.ReadFile(tc.loop.canonicalSnapshotPath())
			if !bytes.Equal(before, after) || !bytes.Equal(snapshotBefore, snapshotAfter) || (os.IsNotExist(snapshotErr) != os.IsNotExist(afterErr)) {
				t.Fatal("refusal mutated evidence")
			}
		})
	}
}

func TestAdmissionRecoveryBindingsAndLock(t *testing.T) {
	for _, name := range []string{"digest", "revision", "lock", "corrupt_receipt"} {
		t.Run(name, func(t *testing.T) {
			tc, original := recoveryFixture(t)
			r := InspectAdmission(tc.projectDir, "noodle")
			if r.Status != "recoverable" {
				t.Fatalf("fixture: %+v", r)
			}
			switch name {
			case "digest":
				r.Subject.SHA256 = strings.Repeat("0", 64)
			case "revision":
				r.Revision = strings.Repeat("0", 32)
			case "lock":
				lock, err := lockfile.TryLock(filepath.Join(tc.runtimeDir, "noodle.lock"))
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				if got := InspectAdmission(tc.projectDir, "noodle"); got.Status != "refused" {
					t.Fatal("inspection ignored instance lock")
				}
			case "corrupt_receipt":
				path := admissionReceiptPath(tc.runtimeDir, r.Subject.SHA256, r.Revision)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				recoveryWrite(t, path, []byte("{"))
			}
			got := RetireAdmission(tc.projectDir, "noodle", r.Subject.SHA256, r.Revision)
			if got.Status != "refused" {
				t.Fatalf("unsafe retirement: %+v", got)
			}
			data, _ := os.ReadFile(tc.loop.deps.OrdersNextFile)
			if !bytes.Equal(data, original) {
				t.Fatal("refusal changed proposal")
			}
		})
	}
}

func TestAdmissionRecoveryLegalNoncases(t *testing.T) {
	t.Run("valid_initial_remains_admissible", func(t *testing.T) {
		tc, _ := recoveryFixture(t)
		initialProposal(t, tc.loop, ownerRevision(t, tc.loop), "B")
		r := InspectAdmission(tc.projectDir, "noodle")
		if r.Status != "refused" || !strings.Contains(r.Invalid, "currently valid") {
			t.Fatalf("%+v", r)
		}
		promoteInitial(t, tc.loop)
	})
	t.Run("no_proposal", func(t *testing.T) {
		tc, _ := recoveryFixture(t)
		if err := os.Remove(tc.loop.deps.OrdersNextFile); err != nil {
			t.Fatal(err)
		}
		r := InspectAdmission(tc.projectDir, "noodle")
		if r.Status != "no_proposal" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("dead_terminal_session", func(t *testing.T) {
		tc, _ := recoveryFixture(t)
		recoverySession(t, tc, "exited", 99999999)
		r := InspectAdmission(tc.projectDir, "noodle")
		if r.Status != "recoverable" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("completed_A_stale_B", func(t *testing.T) {
		tc, _ := recoveryFixture(t)
		initialProposal(t, tc.loop, ownerRevision(t, tc.loop), "A")
		promoteInitial(t, tc.loop)
		node := tc.loop.canonical.Orders["A"]
		node.Status = state.OrderCompleted
		node.Stages[0].Status = state.StageCompleted
		tc.loop.canonical.Orders["A"] = node
		if err := tc.loop.persistCanonicalCheckpoint(); err != nil {
			t.Fatal(err)
		}
		if err := tc.loop.writeProjectionState(); err != nil {
			t.Fatal(err)
		}
		initialProposal(t, tc.loop, strings.Repeat("0", 32), "B")
		r := InspectAdmission(tc.projectDir, "noodle")
		if r.Status != "recoverable" {
			t.Fatalf("%+v", r)
		}
		r = RetireAdmission(tc.projectDir, "noodle", r.Subject.SHA256, r.Revision)
		if r.Status != "retired" {
			t.Fatalf("%+v", r)
		}
	})
}

func TestAdmissionRetirementCrashChild(t *testing.T) {
	root := os.Getenv("TEST_ADMISSION_ROOT")
	if root == "" {
		return
	}
	phase := os.Getenv("TEST_ADMISSION_PHASE")
	r := InspectAdmission(root, "noodle")
	recoverAdmission(root, "noodle", r.Subject.SHA256, r.Revision, func(at string) {
		if at == phase {
			fmt.Println("RETIREMENT_BARRIER")
			for {
				time.Sleep(time.Second)
			}
		}
	})
	t.Fatal("child missed barrier")
}

func TestAdmissionRetirementPhysicalCrash(t *testing.T) {
	for _, phase := range []string{"before_intent", "after_intent", "after_remove"} {
		t.Run(phase, func(t *testing.T) {
			tc, original := recoveryFixture(t)
			r := InspectAdmission(tc.projectDir, "noodle")
			cmd := exec.Command(os.Args[0], "-test.run=^TestAdmissionRetirementCrashChild$")
			cmd.Env = append(os.Environ(), "TEST_ADMISSION_ROOT="+tc.projectDir, "TEST_ADMISSION_PHASE="+phase)
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			ready := make(chan bool, 1)
			go func() {
				scanner := bufio.NewScanner(out)
				for scanner.Scan() {
					if scanner.Text() == "RETIREMENT_BARRIER" {
						ready <- true
						return
					}
				}
				ready <- false
			}()
			select {
			case ok := <-ready:
				if !ok {
					t.Fatal("child exited before barrier")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("child timeout")
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			if err == nil {
				t.Fatal("child not interrupted")
			}
			t.Logf("actual child pid=%d phase=%s exit=%v", cmd.Process.Pid, phase, err)
			receipt, err := readAdmissionReceipt(tc.runtimeDir, r.Subject.SHA256, r.Revision)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "before_intent" {
				if receipt != nil {
					t.Fatal("intent existed before persistence")
				}
			} else if receipt == nil || !bytes.Equal(receipt.Proposal, original) {
				t.Fatal("durable original bytes absent")
			}
			resumed := InspectAdmission(tc.projectDir, "noodle")
			if resumed.Status != "recoverable" {
				t.Fatalf("readback cannot resume: %+v", resumed)
			}
			got := RetireAdmission(tc.projectDir, "noodle", resumed.Next.Argv[5], resumed.Next.Argv[6])
			if got.Status != "retired" {
				t.Fatalf("restart: %+v", got)
			}
			if _, err := os.Stat(tc.loop.deps.OrdersNextFile); !os.IsNotExist(err) {
				t.Fatal("mailbox remains")
			}
		})
	}
}
