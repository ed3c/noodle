package loop

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/mise"
)

func initialLoopAt(t *testing.T, root string) *testLoopContext {
	t.Helper()
	tc := initialLoop(t)
	deps := tc.loop.deps
	deps.OrdersFile = filepath.Join(root, ".noodle", "orders.json")
	deps.OrdersNextFile = filepath.Join(root, ".noodle", "orders-next.json")
	if err := os.MkdirAll(filepath.Dir(deps.OrdersFile), 0700); err != nil {
		t.Fatal(err)
	}
	tc.loop = New(root, "noodle", tc.loop.config, deps)
	tc.projectDir, tc.runtimeDir = root, filepath.Join(root, ".noodle")
	if err := tc.loop.loadOrdersState(); err != nil {
		t.Fatal(err)
	}
	if err := tc.loop.loadOrBootstrapCanonical(); err != nil {
		t.Fatal(err)
	}
	return tc
}

func TestInitialAdmissionCrashChild(t *testing.T) {
	root := os.Getenv("NOODLE_INITIAL_CRASH_DIR")
	if root == "" {
		return
	}
	tc := initialLoopAt(t, root)
	l := tc.loop
	initialProposal(t, l, ownerRevision(t, l), "soodles-18")
	l.TestInitialAdmissionBarrier = func() {
		fmt.Println("INITIAL_ADMISSION_DURABLE")
		// Parent kills this process while the promoted proposal still exists.
		for {
			time.Sleep(time.Second)
		}
	}
	promoteInitial(t, l)
}

func TestInitialAdmissionPhysicalCrashBeforeProposalRemoval(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestInitialAdmissionCrashChild$")
	cmd.Env = append(os.Environ(), "NOODLE_INITIAL_CRASH_DIR="+root)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "INITIAL_ADMISSION_DURABLE" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("child exited before durable checkpoint")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child did not reach durable checkpoint")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("child was not interrupted")
	}
	tc := initialLoopAt(t, root)
	refusedInitial(t, tc, "initial_revision")
	orders, err := tc.loop.currentOrders()
	if err != nil {
		t.Fatal(err)
	}
	if len(orders.Orders) != 1 || orders.Orders[0].ID != "soodles-18" {
		t.Fatal("restart did not preserve promoted order")
	}
}

func initialLoop(t *testing.T) *testLoopContext {
	t.Helper()
	logger, _ := newTestLogger()
	tc := newTestLoop(t, logger)
	if err := tc.loop.loadOrBootstrapCanonical(); err != nil {
		t.Fatal(err)
	}
	return tc
}

func ownerRevision(t *testing.T, l *Loop) string {
	t.Helper()
	data, err := os.ReadFile(l.canonicalSnapshotPath())
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	revision, _ := snapshot["order_revision"].(string)
	if len(revision) != 32 {
		t.Fatalf("missing owner order_revision: %q", revision)
	}
	return revision
}

func initialProposal(t *testing.T, l *Loop, revision, id string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"initial_revision": revision,
		"orders": []any{map[string]any{"id": id, "stages": []any{map[string]any{
			"do": "execute", "with": "codex", "model": "gpt-5.6-sol",
		}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.deps.OrdersNextFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	return data
}

func promoteInitial(t *testing.T, l *Loop) {
	t.Helper()
	r, err := l.mergeOrdersNext()
	if err != nil {
		t.Fatal(err)
	}
	if !r.Promoted {
		t.Fatal("initial proposal was not promoted")
	}
	if err := l.handlePromotionResult(r, mise.Brief{}, nil); err != nil {
		t.Fatal(err)
	}
	if l.lastPromotionError != "" {
		t.Fatal(l.lastPromotionError)
	}
	if _, err := os.Stat(l.deps.OrdersNextFile); !os.IsNotExist(err) {
		t.Fatal("promoted proposal remains")
	}
}

func refusedInitial(t *testing.T, tc *testLoopContext, field string) {
	t.Helper()
	before, err := os.ReadFile(tc.loop.canonicalSnapshotPath())
	if err != nil {
		t.Fatal(err)
	}
	r, err := tc.loop.mergeOrdersNext()
	if err == nil || !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "owner: Noodle") {
		t.Fatalf("expected actionable %s refusal, got %#v, %v", field, r, err)
	}
	if r.Promoted {
		t.Fatal("refusal promoted work")
	}
	after, err := os.ReadFile(tc.loop.canonicalSnapshotPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || len(tc.runtime.calls) != 0 {
		t.Fatal("refusal changed owner or dispatched worker")
	}
}

func TestInitialAdmissionReplayAcrossOwnershipLifecycle(t *testing.T) {
	tc := initialLoop(t)
	l := tc.loop
	originalRevision := ownerRevision(t, l)
	proposal := initialProposal(t, l, originalRevision, "soodles-18")
	promoteInitial(t, l)
	admittedRevision := ownerRevision(t, l)
	if admittedRevision == originalRevision {
		t.Fatal("admission did not change revision")
	}
	// A different ID is not permission to use a stale absence observation.
	initialProposal(t, l, originalRevision, "soodles-19")
	refusedInitial(t, tc, "initial_revision")
	orders, err := l.currentOrders()
	if err != nil {
		t.Fatal(err)
	}
	failed, err := failStage(orders, "soodles-18", "interrupted")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.writeOrdersState(failed); err != nil {
		t.Fatal(err)
	}
	if ownerRevision(t, l) != admittedRevision {
		t.Fatal("ordinary progress changed ownership revision")
	}
	if err := os.WriteFile(l.deps.OrdersNextFile, proposal, 0600); err != nil {
		t.Fatal(err)
	}
	refusedInitial(t, tc, "initial_revision")
	initialProposal(t, l, admittedRevision, "soodles-18")
	refusedInitial(t, tc, "order.id")
	if err := l.writeOrdersState(OrdersFile{}); err != nil {
		t.Fatal(err)
	}
	if ownerRevision(t, l) == originalRevision {
		t.Fatal("empty ownership resurrected old revision")
	}
	if err := os.WriteFile(l.deps.OrdersNextFile, proposal, 0600); err != nil {
		t.Fatal(err)
	}
	refusedInitial(t, tc, "initial_revision")
	initialProposal(t, l, ownerRevision(t, l), "soodles-18")
	refusedInitial(t, tc, "order.id")
	initialProposal(t, l, ownerRevision(t, l), "soodles-19")
	promoteInitial(t, l)
	current, err := l.currentOrders()
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Orders) != 1 || current.Orders[0].ID != "soodles-19" {
		t.Fatal("independent initial work was not admitted exactly once")
	}
}

func TestInitialAdmissionRecoveryBeforeProposalRemoval(t *testing.T) {
	tc := initialLoop(t)
	l := tc.loop
	proposal := initialProposal(t, l, ownerRevision(t, l), "soodles-18")
	r, err := l.mergeOrdersNext()
	if err != nil {
		t.Fatal(err)
	}
	// Real durable checkpoint and projection write; interrupt before proposal deletion.
	if err := l.recordInitialAdmissions(r.InitialIDs); err != nil {
		t.Fatal(err)
	}
	if err := l.writeOrdersState(r.Orders); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(l.deps.OrdersNextFile); err != nil || string(data) != string(proposal) {
		t.Fatal("proposal changed before interruption")
	}
	oldRevision := ownerRevision(t, l)
	restarted := New(tc.projectDir, "noodle", l.config, l.deps)
	if err := restarted.loadOrdersState(); err != nil {
		t.Fatal(err)
	}
	if err := restarted.loadOrBootstrapCanonical(); err != nil {
		t.Fatal(err)
	}
	tc.loop = restarted
	if ownerRevision(t, restarted) != oldRevision {
		t.Fatal("restart changed durable identity")
	}
	refusedInitial(t, tc, "initial_revision")
	// Even a current revision must not amend a running/owned order.
	initialProposal(t, restarted, oldRevision, "soodles-18")
	refusedInitial(t, tc, "order.id")
	// A stale projection cannot erase canonical ownership during new admission.
	restarted.orders = OrdersFile{}
	initialProposal(t, restarted, oldRevision, "soodles-19")
	refusedInitial(t, tc, "orders projection")
}

func TestInitialAdmissionLegacySnapshotAndScheduleNoncase(t *testing.T) {
	tc := initialLoop(t)
	l := tc.loop
	revision := ownerRevision(t, l)
	if err := l.writeOrdersState(bootstrapScheduleOrder(l.config)); err != nil {
		t.Fatal(err)
	}
	if ownerRevision(t, l) != revision {
		t.Fatal("schedule activity invalidated an unrelated initial proposal")
	}
	snapshot, err := reducer.ReadSnapshot(l.canonicalSnapshotPath())
	if err != nil {
		t.Fatal(err)
	}
	// A legacy snapshot remains readable but has no initial-admission binding.
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "order_revision")
	data, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.canonicalSnapshotPath(), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.loadCanonicalSnapshot(); err != nil {
		t.Fatal(err)
	}
	initialProposal(t, l, revision, "soodles-18")
	refusedInitial(t, tc, "initial_revision")
	if err := l.persistCanonicalCheckpoint(); err != nil {
		t.Fatal(err)
	}
	initialProposal(t, l, ownerRevision(t, l), "soodles-18")
	promoteInitial(t, l)
}

func TestInitialAdmissionMalformedRevision(t *testing.T) {
	for _, value := range []string{`null`, `""`, `"bad"`, `"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"`, `42`} {
		t.Run(value, func(t *testing.T) {
			tc := initialLoop(t)
			data := []byte(`{"initial_revision":` + value + `,"orders":[]}`)
			if err := os.WriteFile(tc.loop.deps.OrdersNextFile, data, 0600); err != nil {
				t.Fatal(err)
			}
			_, err := tc.loop.mergeOrdersNext()
			if err == nil || !strings.Contains(err.Error(), "initial_revision") {
				t.Fatalf("malformed revision accepted: %v", err)
			}
		})
	}
}

func TestInitialAdmissionFailedCheckpointDoesNotPublishRevision(t *testing.T) {
	tc := initialLoop(t)
	l := tc.loop
	revision := ownerRevision(t, l)
	initialProposal(t, l, revision, "soodles-18")
	snapshotPath := l.canonicalSnapshotPath()
	backup := filepath.Join(tc.runtimeDir, "before.json")
	if err := os.Rename(snapshotPath, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(snapshotPath, 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.prepareOrdersForCycle(mise.Brief{}, nil, false); err == nil {
		t.Fatal("failed initial checkpoint did not stop the cycle before dispatch")
	}
	if l.orderRevision != revision || len(tc.runtime.calls) != 0 {
		t.Fatal("failed persistence published a revision or dispatched work")
	}
}
