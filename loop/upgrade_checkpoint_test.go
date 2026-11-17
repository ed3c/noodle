package loop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/ingest"
	"github.com/poteto/noodle/internal/reducer"
	loopruntime "github.com/poteto/noodle/runtime"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func returnHandoffTestLoop(t *testing.T, endpoint string) (*Loop, *cookHandle) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(dir, ".noodle")
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeOrdersAtomic(filepath.Join(runtimeDir, "orders.json"), OrdersFile{Orders: []Order{{ID: "42", Status: OrderStatusActive, Stages: []Stage{{TaskKey: "execute", Skill: "execute", Status: StageStatusActive}}}}}); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Runtime.AdmittedOrders = []string{"42"}
	save := func(name string, v any) config.RuntimeSource {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		return config.RuntimeSource{Path: path, SHA256: hex.EncodeToString(sum[:])}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	producer := config.RuntimeSource{Path: exe, SHA256: hex.EncodeToString(sum[:])}
	binding := save("binding.json", map[string]any{"schema": 1, "request": config.RuntimeSource{Path: "/fixture/request", SHA256: hex.EncodeToString(sum[:])}, "goal": "fixture", "main": map[string]string{"threadId": "fixture-Main", "hostId": "local"}, "connection_instance": "fixture", "consumer_revision": "v1", "producer": producer, "host_instance_id": "fixture-host"})
	if endpoint == "" {
		endpoint = filepath.Join(dir, "absent.sock")
	}
	cfg.Runtime.ReturnHandoff = save("descriptor.json", returnDescriptor{Schema: 1, Binding: binding, Endpoint: endpoint, Producer: producer, HostInstanceID: "fixture-host"})
	l := New(dir, "noodle", cfg, Dependencies{Runtimes: map[string]loopruntime.Runtime{"process": newMockRuntime()}, Worktree: &fakeWorktree{}, Adapter: &fakeAdapterRunner{}, Mise: &fakeMise{}, Monitor: fakeMonitor{}, Registry: testLoopRegistry(), Now: time.Now, ReturnHostInstance: "fixture-host"})
	cook := &cookHandle{cookIdentity: cookIdentity{orderID: "42", stage: Stage{TaskKey: "execute", Skill: "execute"}}, session: &adoptedSession{id: "old-worker", status: "completed"}, worktreeName: "42", worktreePath: filepath.Join(dir, "worktree")}
	if err := l.ensureCanonicalOrderFromOrders(cook.orderID); err != nil {
		t.Fatal(err)
	}
	if err := l.emitEventChecked(ingest.EventDispatchCompleted, map[string]any{"order_id": cook.orderID, "stage_index": 0, "attempt_id": dispatchAttemptID(cook.orderID, 0, 0), "session_id": cook.session.ID(), "worktree_name": cook.worktreeName}); err != nil {
		t.Fatal(err)
	}
	return l, cook
}

func TestUpgradeReturnCheckpointAndReentry(t *testing.T) {
	l, cook := returnHandoffTestLoop(t, "")
	if err := l.parkPendingReview(cook, ""); err != nil {
		t.Fatal(err)
	}
	saved, err := reducer.ReadSnapshot(l.canonicalSnapshotPath())
	if err != nil {
		t.Fatal(err)
	}
	intent := saved.State.PendingReviews["42"].ReturnHandoff
	if intent == nil || intent.EventID == "" || intent.Status != "unknown" {
		t.Fatal("checkpoint missing original unknown", intent)
	}
	for _, worker := range []string{"new-opt-in", "legacy-opt-out"} {
		if worker == "legacy-opt-out" {
			l.config.Runtime.ReturnHandoff = config.RuntimeSource{}
		}
		if err := l.parkPendingReview(cook, ""); err != nil {
			t.Fatal("native repark refused", worker, err)
		}
		after, err := reducer.ReadSnapshot(l.canonicalSnapshotPath())
		if err != nil || *after.State.PendingReviews["42"].ReturnHandoff != *intent {
			t.Fatal("repark replaced original callback intent", worker, err)
		}
	}
}
