package loop

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/mise"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestPromotedScheduleTerminatesWriter(t *testing.T) {
	projectDir := t.TempDir()
	runtimeDir := filepath.Join(projectDir, ".noodle")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ordersPath := filepath.Join(runtimeDir, "orders.json")
	if err := writeOrdersAtomic(ordersPath, OrdersFile{}); err != nil {
		t.Fatalf("write orders: %v", err)
	}

	l := New(projectDir, "noodle", config.DefaultConfig(), Dependencies{
		Runtimes:       map[string]loopruntime.Runtime{"process": newMockRuntime()},
		Worktree:       &fakeWorktree{},
		Adapter:        &fakeAdapterRunner{},
		Mise:           &fakeMise{},
		Monitor:        fakeMonitor{},
		Registry:       testLoopRegistry(),
		Now:            time.Now,
		OrdersFile:     ordersPath,
		OrdersNextFile: filepath.Join(runtimeDir, "orders-next.json"),
	})
	scheduler := &mockSession{id: "schedule-session", status: "running", done: make(chan struct{})}
	l.cooks.activeCooksByOrder[scheduleOrderID] = &cookHandle{
		cookIdentity: cookIdentity{
			orderID:    scheduleOrderID,
			stageIndex: 0,
			stage:      Stage{TaskKey: scheduleOrderID, Skill: scheduleOrderID, Status: StageStatusActive},
		},
		session: scheduler,
	}
	nonScheduler := &mockSession{id: "worker-session", status: "running", done: make(chan struct{})}
	l.cooks.activeCooksByOrder["42"] = &cookHandle{
		cookIdentity: cookIdentity{
			orderID:    "42",
			stageIndex: 0,
			stage:      Stage{TaskKey: "execute", Skill: "execute", Status: StageStatusActive},
		},
		session: nonScheduler,
	}
	promoted := OrdersFile{Orders: []Order{{
		ID: "42", Status: OrderStatusActive,
		Stages: []Stage{{TaskKey: "execute", Skill: "execute", Status: StageStatusPending}},
	}}}

	if err := l.handlePromotionResult(mergeResult{Orders: promoted, Promoted: true}, mise.Brief{}, nil); err != nil {
		t.Fatalf("handle promotion: %v", err)
	}
	if got := scheduler.Status(); got != "killed" {
		t.Fatalf("promoted scheduler status = %q, want killed", got)
	}
	if got := nonScheduler.Status(); got != "running" {
		t.Fatalf("non-scheduler status = %q, want running", got)
	}
	if _, ok := l.cooks.activeCooksByOrder[scheduleOrderID]; !ok {
		t.Fatal("scheduler must remain tracked until its watcher observes exit")
	}
	readback, err := readOrders(ordersPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(readback.Orders) != 1 || readback.Orders[0].ID != "42" {
		t.Fatalf("promoted order readback = %#v", readback.Orders)
	}

	unpromoted := &mockSession{id: "unpromoted-schedule", status: "running", done: make(chan struct{})}
	l.cooks.activeCooksByOrder[scheduleOrderID] = &cookHandle{
		cookIdentity: cookIdentity{
			orderID: scheduleOrderID,
			stage:   Stage{TaskKey: scheduleOrderID, Skill: scheduleOrderID, Status: StageStatusActive},
		},
		session: unpromoted,
	}
	if err := l.handlePromotionResult(mergeResult{}, mise.Brief{}, nil); err != nil {
		t.Fatalf("handle absent promotion: %v", err)
	}
	if got := unpromoted.Status(); got != "running" {
		t.Fatalf("unpromoted scheduler status = %q, want running", got)
	}

	empty := &mockSession{id: "empty-schedule", status: "running", done: make(chan struct{})}
	l.cooks.activeCooksByOrder[scheduleOrderID] = &cookHandle{
		cookIdentity: cookIdentity{
			orderID: scheduleOrderID,
			stage:   Stage{TaskKey: scheduleOrderID, Skill: scheduleOrderID, Status: StageStatusActive},
		},
		session: empty,
	}
	l.scheduleDispatchDigest = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := l.handlePromotionResult(mergeResult{Orders: OrdersFile{}, Promoted: true, EmptyPromotion: true}, mise.Brief{}, nil); err != nil {
		t.Fatalf("handle empty promotion: %v", err)
	}
	if got := empty.Status(); got != "killed" {
		t.Fatalf("empty promoted scheduler status = %q, want killed", got)
	}
}

func TestPromotionPreservesActiveAdoptedCook(t *testing.T) {
	projectDir := t.TempDir()
	runtimeDir := filepath.Join(projectDir, ".noodle")
	ordersPath := filepath.Join(runtimeDir, "orders.json")
	if err := writeOrdersAtomic(ordersPath, OrdersFile{}); err != nil {
		t.Fatalf("write orders: %v", err)
	}

	worktree := &fakeWorktree{}
	l := New(projectDir, "noodle", config.DefaultConfig(), Dependencies{
		Runtimes:       map[string]loopruntime.Runtime{"process": newMockRuntime()},
		Worktree:       worktree,
		Adapter:        &fakeAdapterRunner{},
		Mise:           &fakeMise{},
		Monitor:        fakeMonitor{},
		Registry:       testLoopRegistry(),
		Now:            time.Now,
		OrdersFile:     ordersPath,
		OrdersNextFile: filepath.Join(runtimeDir, "orders-next.json"),
	})

	const orderID = "ed3c/noodles#395"
	const sessionID = "adopted-execute-session"
	worktreePath := filepath.Join(projectDir, ".worktrees", "395-0-execute")
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatalf("create adopted worktree: %v", err)
	}
	markerPath := filepath.Join(worktreePath, "candidate.txt")
	if err := os.WriteFile(markerPath, []byte("candidate bytes"), 0o644); err != nil {
		t.Fatalf("write adopted worktree marker: %v", err)
	}

	session := &mockSession{id: sessionID, status: "running", done: make(chan struct{})}
	l.cooks.activeCooksByOrder[orderID] = &cookHandle{
		cookIdentity: cookIdentity{
			orderID:    orderID,
			stageIndex: 0,
			stage:      Stage{TaskKey: "execute", Skill: "noodles-issue-execute", Status: StageStatusActive},
		},
		session:      session,
		worktreeName: "395-0-execute",
		worktreePath: worktreePath,
	}
	l.cooks.adoptedTargets[orderID] = sessionID
	l.cooks.adoptedSessions = append(l.cooks.adoptedSessions, sessionID)

	// The schedule can publish while the adopted cook is still running and omit
	// its subject because the adopted-target set already owns that exact session.
	if err := l.handlePromotionResult(mergeResult{Orders: OrdersFile{}, Promoted: true, EmptyPromotion: true}, mise.Brief{}, nil); err != nil {
		t.Fatalf("handle empty promotion: %v", err)
	}

	if got := session.Status(); got != "running" {
		t.Fatalf("active adopted cook was force-killed: status = %q", got)
	}
	if _, ok := l.cooks.activeCooksByOrder[orderID]; !ok {
		t.Fatal("active adopted cook tracking entry was removed")
	}
	if len(worktree.cleaned) != 0 {
		t.Fatalf("active adopted cook worktree was cleaned: %v", worktree.cleaned)
	}
	if data, err := os.ReadFile(markerPath); err != nil || string(data) != "candidate bytes" {
		t.Fatalf("active adopted cook worktree bytes are not readable: data=%q err=%v", data, err)
	}
}

func TestCancelSupersededActiveCooksCancelsChangedStage(t *testing.T) {
	projectDir := t.TempDir()
	runtimeDir := filepath.Join(projectDir, ".noodle")
	ordersPath := filepath.Join(runtimeDir, "orders.json")
	if err := writeOrdersAtomic(ordersPath, OrdersFile{}); err != nil {
		t.Fatalf("write orders: %v", err)
	}

	worktree := &fakeWorktree{}
	l := New(projectDir, "noodle", config.DefaultConfig(), Dependencies{
		Runtimes:   map[string]loopruntime.Runtime{"process": newMockRuntime()},
		Worktree:   worktree,
		Adapter:    &fakeAdapterRunner{},
		Mise:       &fakeMise{},
		Monitor:    fakeMonitor{},
		Registry:   testLoopRegistry(),
		Now:        time.Now,
		OrdersFile: ordersPath,
	})

	session := &mockSession{id: "sess-97", status: "running", done: make(chan struct{})}
	l.cooks.activeCooksByOrder["97"] = &cookHandle{
		cookIdentity: cookIdentity{
			orderID:    "97",
			stageIndex: 0,
			stage: Stage{
				TaskKey:  "execute",
				Prompt:   "old prompt",
				Provider: "codex",
				Model:    "gpt-5.4",
				Status:   StageStatusActive,
			},
		},
		session:      session,
		worktreeName: "97-0-execute",
		worktreePath: filepath.Join(projectDir, ".worktrees", "97-0-execute"),
	}
	// Stale ownership for the same order must not protect a different session.
	l.cooks.adoptedTargets["97"] = "older-session"

	orders := OrdersFile{
		Orders: []Order{{
			ID:     "97",
			Status: OrderStatusActive,
			Stages: []Stage{
				{TaskKey: "execute", Prompt: "new prompt", Provider: "codex", Model: "gpt-5.4", Status: StageStatusPending},
				{TaskKey: "adversarial-review", Provider: "claude", Model: "claude-opus-4-6", Status: StageStatusPending},
			},
		}},
	}

	l.cancelSupersededActiveCooks(orders)

	if _, ok := l.cooks.activeCooksByOrder["97"]; ok {
		t.Fatal("expected superseded active cook to be removed")
	}
	if got := session.Status(); got != "killed" {
		t.Fatalf("session status = %q, want killed", got)
	}
	if len(worktree.cleaned) != 1 || worktree.cleaned[0] != "97-0-execute" {
		t.Fatalf("cleaned worktrees = %v, want [97-0-execute]", worktree.cleaned)
	}
}

func TestCancelSupersededActiveCooksKeepsMatchingStage(t *testing.T) {
	projectDir := t.TempDir()
	runtimeDir := filepath.Join(projectDir, ".noodle")
	ordersPath := filepath.Join(runtimeDir, "orders.json")
	if err := writeOrdersAtomic(ordersPath, OrdersFile{}); err != nil {
		t.Fatalf("write orders: %v", err)
	}

	worktree := &fakeWorktree{}
	l := New(projectDir, "noodle", config.DefaultConfig(), Dependencies{
		Runtimes:   map[string]loopruntime.Runtime{"process": newMockRuntime()},
		Worktree:   worktree,
		Adapter:    &fakeAdapterRunner{},
		Mise:       &fakeMise{},
		Monitor:    fakeMonitor{},
		Registry:   testLoopRegistry(),
		Now:        time.Now,
		OrdersFile: ordersPath,
	})

	session := &mockSession{id: "sess-97", status: "running", done: make(chan struct{})}
	l.cooks.activeCooksByOrder["97"] = &cookHandle{
		cookIdentity: cookIdentity{
			orderID:    "97",
			stageIndex: 0,
			stage: Stage{
				TaskKey:  "execute",
				Prompt:   "steady prompt",
				Provider: "codex",
				Model:    "gpt-5.4",
				Status:   StageStatusActive,
			},
		},
		session:      session,
		worktreeName: "97-0-execute",
		worktreePath: filepath.Join(projectDir, ".worktrees", "97-0-execute"),
	}

	orders := OrdersFile{
		Orders: []Order{{
			ID:     "97",
			Status: OrderStatusActive,
			Stages: []Stage{
				{TaskKey: "execute", Prompt: "steady prompt", Provider: "codex", Model: "gpt-5.4", Status: StageStatusPending},
				{TaskKey: "adversarial-review", Provider: "claude", Model: "claude-opus-4-6", Status: StageStatusPending},
			},
		}},
	}

	l.cancelSupersededActiveCooks(orders)

	if _, ok := l.cooks.activeCooksByOrder["97"]; !ok {
		t.Fatal("expected matching active cook to stay running")
	}
	if got := session.Status(); got != "running" {
		t.Fatalf("session status = %q, want running", got)
	}
	if len(worktree.cleaned) != 0 {
		t.Fatalf("expected no worktree cleanup, got %v", worktree.cleaned)
	}
}

func TestCancelSupersededActiveCooksKeepsSchedulerUntilSessionExit(t *testing.T) {
	projectDir := t.TempDir()
	runtimeDir := filepath.Join(projectDir, ".noodle")
	ordersPath := filepath.Join(runtimeDir, "orders.json")
	if err := writeOrdersAtomic(ordersPath, OrdersFile{}); err != nil {
		t.Fatalf("write orders: %v", err)
	}

	worktree := &fakeWorktree{}
	l := New(projectDir, "noodle", config.DefaultConfig(), Dependencies{
		Runtimes:   map[string]loopruntime.Runtime{"process": newMockRuntime()},
		Worktree:   worktree,
		Adapter:    &fakeAdapterRunner{},
		Mise:       &fakeMise{},
		Monitor:    fakeMonitor{},
		Registry:   testLoopRegistry(),
		Now:        time.Now,
		OrdersFile: ordersPath,
	})

	session := &mockSession{id: "schedule-session", status: "running", done: make(chan struct{})}
	l.cooks.activeCooksByOrder[scheduleOrderID] = &cookHandle{
		cookIdentity: cookIdentity{
			orderID:    scheduleOrderID,
			stageIndex: 0,
			stage: Stage{
				TaskKey: scheduleOrderID,
				Skill:   scheduleOrderID,
				Status:  StageStatusActive,
			},
		},
		session:      session,
		worktreePath: projectDir,
	}

	// A promoted scheduler proposal contains product orders, not the synthetic
	// schedule order that produced it.
	promotedOrders := OrdersFile{Orders: []Order{{
		ID:     "product-order",
		Status: OrderStatusActive,
		Stages: []Stage{{TaskKey: "execute", Status: StageStatusPending}},
	}}}
	l.cancelSupersededActiveCooks(promotedOrders)

	if _, ok := l.cooks.activeCooksByOrder[scheduleOrderID]; !ok {
		t.Fatal("expected active scheduler to remain tracked until its session exits")
	}
	if got := session.Status(); got != "running" {
		t.Fatalf("scheduler session status = %q, want running", got)
	}
	if len(worktree.cleaned) != 0 {
		t.Fatalf("scheduler promotion must not clean a worktree, got %v", worktree.cleaned)
	}

	if _, err := l.ensureScheduleIfNeeded(mise.Brief{}, &promotedOrders, true); err != nil {
		t.Fatalf("ensure schedule: %v", err)
	}
	if hasScheduleOrder(promotedOrders) {
		t.Fatal("must not dispatch a second scheduler while the promoted scheduler session is still active")
	}
}
