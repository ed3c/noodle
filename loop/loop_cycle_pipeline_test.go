package loop

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/mise"
	loopruntime "github.com/poteto/noodle/runtime"
)

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
