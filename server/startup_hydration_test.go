package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/snapshot"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/taskreg"
	"github.com/poteto/noodle/loop"
	"github.com/poteto/noodle/mise"
	"github.com/poteto/noodle/monitor"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/skill"
)

const startupIncompleteMessage = "Noodle startup initialization incomplete; retry snapshot"

type startupBarrierRuntime struct {
	entered chan struct{}
	release chan struct{}
	rel     sync.Once
}

func newStartupBarrierRuntime() *startupBarrierRuntime {
	return &startupBarrierRuntime{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (r *startupBarrierRuntime) Dispatch(context.Context, loopruntime.DispatchRequest) (loopruntime.SessionHandle, error) {
	return nil, errors.New("startup hydration test dispatched unexpectedly")
}

func (r *startupBarrierRuntime) Terminate(loopruntime.SessionHandle) error { return nil }
func (r *startupBarrierRuntime) ForceKill(loopruntime.SessionHandle) error { return nil }

func (r *startupBarrierRuntime) Recover(ctx context.Context) ([]loopruntime.RecoveredSession, error) {
	close(r.entered)
	select {
	case <-r.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *startupBarrierRuntime) releaseRecovery() {
	r.rel.Do(func() { close(r.release) })
}

type startupMise struct {
	err error
}

func (m startupMise) Build(context.Context, mise.ActiveSummary, []mise.HistoryItem) (mise.Brief, []string, bool, error) {
	return mise.Brief{}, nil, false, m.err
}

type startupMonitor struct{}

func (startupMonitor) RunOnce(context.Context) ([]monitor.SessionMeta, error) { return nil, nil }

// Regression: HTTP 200 with an empty projection before startup recovery must
// never mask durable orders and reviews that the current daemon has not loaded.
func TestSnapshotStartupHydrationBarrier(t *testing.T) {
	projectDir := t.TempDir()
	runtimeDir := writeStartupRuntime(t, projectDir, loop.OrdersFile{Orders: []loop.Order{
		{
			ID:     "fixture/order-1",
			Title:  "durable review",
			Status: loop.OrderStatusActive,
			Stages: []loop.Stage{{
				TaskKey: "execute",
				Skill:   "execute",
				Runtime: "process",
				Status:  loop.StageStatusActive,
			}},
		},
	}}, []loop.PendingReviewItem{
		{
			OrderID:      "fixture/order-1",
			StageIndex:   0,
			TaskKey:      "execute",
			Skill:        "execute",
			Runtime:      "process",
			WorktreeName: "fixture-order-1-0-execute",
			WorktreePath: filepath.Join(projectDir, ".worktrees", "fixture-order-1-0-execute"),
			SessionID:    "session-1",
		},
	}, `{"id":"pause-on-startup","action":"pause"}`)

	runtime := newStartupBarrierRuntime()
	noodleLoop := newStartupLoop(projectDir, runtime, startupMise{})
	api := newStartupHTTPServer(runtimeDir, noodleLoop)
	defer api.Close()

	run := startStartupLoop(t, noodleLoop, runtime)

	waitForStartupSignal(t, runtime.entered, "runtime recovery barrier")
	status, body := getStartupSnapshot(t, api.URL)
	assertStartupIncomplete(t, status, body)

	runtime.releaseRecovery()
	snap := waitForHydratedSnapshot(t, api.URL)
	if snap.LoopState != "paused" {
		t.Fatalf("loop_state = %q, want paused", snap.LoopState)
	}
	if len(snap.Orders) != 1 || snap.Orders[0].ID != "fixture/order-1" {
		t.Fatalf("hydrated orders = %#v, want fixture/order-1", snap.Orders)
	}
	if snap.PendingReviewCount != 1 || len(snap.PendingReviews) != 1 || snap.PendingReviews[0].OrderID != "fixture/order-1" {
		t.Fatalf("hydrated pending reviews = %#v (count %d), want fixture/order-1", snap.PendingReviews, snap.PendingReviewCount)
	}

	if err := run.stopAndWait(t); err != nil {
		t.Fatalf("loop exit: %v", err)
	}
}

func TestSnapshotStartupHydrationAllowsInitializedEmptyQueue(t *testing.T) {
	projectDir := t.TempDir()
	runtimeDir := writeStartupRuntime(t, projectDir, loop.OrdersFile{}, nil, "")
	runtime := newStartupBarrierRuntime()
	noodleLoop := newStartupLoop(projectDir, runtime, startupMise{})
	api := newStartupHTTPServer(runtimeDir, noodleLoop)
	defer api.Close()

	run := startStartupLoop(t, noodleLoop, runtime)

	waitForStartupSignal(t, runtime.entered, "runtime recovery barrier")
	status, body := getStartupSnapshot(t, api.URL)
	assertStartupIncomplete(t, status, body)

	runtime.releaseRecovery()
	snap := waitForHydratedSnapshot(t, api.URL)
	if snap.LoopState != "idle" {
		t.Fatalf("loop_state = %q, want idle", snap.LoopState)
	}
	if len(snap.Orders) != 0 {
		t.Fatalf("orders = %#v, want initialized empty queue", snap.Orders)
	}

	if err := run.stopAndWait(t); err != nil {
		t.Fatalf("loop exit: %v", err)
	}
}

func TestSnapshotStartupHydrationFirstCycleFailureNeverBecomesReady(t *testing.T) {
	projectDir := t.TempDir()
	runtimeDir := writeStartupRuntime(t, projectDir, loop.OrdersFile{}, nil, "")
	runtime := newStartupBarrierRuntime()
	wantFailure := errors.New("fixture mise build failed")
	noodleLoop := newStartupLoop(projectDir, runtime, startupMise{err: wantFailure})
	api := newStartupHTTPServer(runtimeDir, noodleLoop)
	defer api.Close()

	run := startStartupLoop(t, noodleLoop, runtime)

	waitForStartupSignal(t, runtime.entered, "runtime recovery barrier")
	runtime.releaseRecovery()
	err := run.wait(t)
	if err == nil || !strings.Contains(err.Error(), wantFailure.Error()) {
		t.Fatalf("loop error = %v, want first-cycle failure containing %q", err, wantFailure)
	}
	status, body := getStartupSnapshot(t, api.URL)
	assertStartupIncomplete(t, status, body)
}

func newStartupLoop(projectDir string, runtime loopruntime.Runtime, builder loop.MiseBuilder) *loop.Loop {
	cfg := config.DefaultConfig()
	return loop.New(projectDir, "noodle", cfg, loop.Dependencies{
		Runtimes:     map[string]loopruntime.Runtime{"process": runtime},
		Mise:         builder,
		Monitor:      startupMonitor{},
		Registry:     startupRegistry(),
		ModeOverride: state.RunModeManual,
		Now: func() time.Time {
			return time.Date(2026, 9, 13, 8, 0, 0, 0, time.UTC)
		},
	})
}

func startupRegistry() taskreg.Registry {
	return taskreg.NewFromSkills([]skill.SkillMeta{
		{
			Name:        "schedule",
			Path:        "/skills/schedule",
			Frontmatter: skill.Frontmatter{Schedule: "when the queue is empty"},
		},
		{
			Name:        "execute",
			Path:        "/skills/execute",
			Frontmatter: skill.Frontmatter{Schedule: "when work is ready"},
		},
	})
}

func writeStartupRuntime(t *testing.T, projectDir string, orders loop.OrdersFile, reviews []loop.PendingReviewItem, control string) string {
	t.Helper()
	runtimeDir := filepath.Join(projectDir, ".noodle")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("create runtime directory: %v", err)
	}
	writeStartupJSON(t, filepath.Join(runtimeDir, "orders.json"), orders)
	writeStartupJSON(t, filepath.Join(runtimeDir, "pending-review.json"), struct {
		Items []loop.PendingReviewItem `json:"items"`
	}{Items: reviews})
	writeStartupJSON(t, filepath.Join(runtimeDir, "schedule-empty-decision.json"), map[string]any{
		"version":         1,
		"decision_digest": strings.Repeat("0", 64),
	})
	if strings.TrimSpace(control) != "" {
		if err := os.WriteFile(filepath.Join(runtimeDir, "control.ndjson"), []byte(control+"\n"), 0o644); err != nil {
			t.Fatalf("write startup control: %v", err)
		}
	}
	return runtimeDir
}

func writeStartupJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %s: %v", filepath.Base(path), err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write %s: %v", filepath.Base(path), err)
	}
}

func newStartupHTTPServer(runtimeDir string, provider LoopStateProvider) *httptest.Server {
	s := New(Options{
		RuntimeDir:        runtimeDir,
		LoopStateProvider: provider,
	})
	return httptest.NewServer(s.httpServer.Handler)
}

type startupLoopRun struct {
	cancel  context.CancelFunc
	runtime *startupBarrierRuntime
	done    chan struct{}
	err     error
}

func startStartupLoop(t *testing.T, noodleLoop *loop.Loop, runtime *startupBarrierRuntime) *startupLoopRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	run := &startupLoopRun{
		cancel:  cancel,
		runtime: runtime,
		done:    make(chan struct{}),
	}
	go func() {
		run.err = noodleLoop.Run(ctx)
		close(run.done)
	}()
	t.Cleanup(func() {
		run.cancel()
		run.runtime.releaseRecovery()
		_ = run.wait(t)
		noodleLoop.Shutdown()
	})
	return run
}

func (r *startupLoopRun) stopAndWait(t *testing.T) error {
	t.Helper()
	r.cancel()
	r.runtime.releaseRecovery()
	return r.wait(t)
}

func (r *startupLoopRun) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-r.done:
		return r.err
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for loop exit")
		return nil
	}
}

func getStartupSnapshot(t *testing.T, baseURL string) (int, []byte) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(baseURL + "/api/snapshot")
	if err != nil {
		t.Fatalf("GET /api/snapshot: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /api/snapshot: %v", err)
	}
	return resp.StatusCode, body
}

func assertStartupIncomplete(t *testing.T, status int, body []byte) {
	t.Helper()
	if status != http.StatusServiceUnavailable {
		t.Fatalf("pre-hydration status = %d, want 503; body=%s", status, body)
	}
	if message := strings.TrimSpace(string(body)); message != startupIncompleteMessage {
		t.Fatalf("pre-hydration message = %q, want %q", message, startupIncompleteMessage)
	}
	if len(body) > 128 {
		t.Fatalf("pre-hydration response is unbounded: %d bytes", len(body))
	}
}

func waitForHydratedSnapshot(t *testing.T, baseURL string) snapshot.Snapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, body := getStartupSnapshot(t, baseURL)
		if status == http.StatusOK {
			if strings.Contains(string(body), "startup_ready") {
				t.Fatalf("startup readiness leaked into public JSON: %s", body)
			}
			var snap snapshot.Snapshot
			if err := json.Unmarshal(body, &snap); err != nil {
				t.Fatalf("decode hydrated snapshot: %v", err)
			}
			return snap
		}
		if status != http.StatusServiceUnavailable {
			t.Fatalf("startup status = %d, want 503 or 200; body=%s", status, body)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for hydrated snapshot")
	return snapshot.Snapshot{}
}

func waitForStartupSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}
