package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/poteto/noodle/adapter"
	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/dispatcher"
	"github.com/poteto/noodle/internal/orderx"
	"github.com/poteto/noodle/internal/taskreg"
	"github.com/poteto/noodle/loop"
	"github.com/poteto/noodle/mise"
	"github.com/poteto/noodle/monitor"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/skill"
	"github.com/poteto/noodle/worktree"
)

type providerMise struct {
	client       *GitHubClient
	policy       Policy
	capabilities Capabilities
	lastBacklog  []adapter.BacklogItem
}

func (m *providerMise) Build(ctx context.Context, _ mise.ActiveSummary, _ []mise.HistoryItem) (mise.Brief, []string, bool, error) {
	items, diagnostics, err := Sync(ctx, m.client, m.policy, m.capabilities)
	if err != nil {
		return mise.Brief{}, nil, false, err
	}
	if len(diagnostics) != 0 {
		return mise.Brief{}, nil, false, fmt.Errorf("unexpected provider diagnostics: %#v", diagnostics)
	}
	backlog := make([]adapter.BacklogItem, 0, len(items))
	for _, item := range items {
		backlog = append(backlog, adapter.BacklogItem{ID: item.ID, Title: item.Title, Status: item.Status})
	}
	changed := !reflect.DeepEqual(backlog, m.lastBacklog)
	m.lastBacklog = append([]adapter.BacklogItem(nil), backlog...)
	return mise.Brief{Backlog: backlog}, nil, changed, nil
}

type inertAdapter struct{}

func (inertAdapter) Run(context.Context, string, string, adapter.RunOptions) (string, error) {
	return "", nil
}

type inertMonitor struct{}

func (inertMonitor) RunOnce(context.Context) ([]monitor.SessionMeta, error) { return nil, nil }

type trackingWorktree struct {
	app     *worktree.App
	created []string
}

func (w *trackingWorktree) Create(name string, opts ...worktree.CreateOpts) error {
	w.created = append(w.created, name)
	return w.app.Create(name, opts...)
}

func (w *trackingWorktree) Merge(name string, opts ...worktree.MergeOpts) error {
	return w.app.Merge(name, opts...)
}

func (w *trackingWorktree) MergeRemoteBranch(branch string) error {
	return w.app.MergeRemoteBranch(branch)
}

func (w *trackingWorktree) Cleanup(name string, opts ...worktree.CleanupOpts) error {
	return w.app.Cleanup(name, opts...)
}

func (w *trackingWorktree) HasUnmergedCommits(name string) (bool, error) {
	return w.app.HasUnmergedCommits(name)
}

type scriptedRuntime struct {
	mu             sync.Mutex
	ordersNextPath string
	item           BacklogItem
	requests       []loopruntime.DispatchRequest
	sessions       []*scriptedSession
}

func (r *scriptedRuntime) Dispatch(_ context.Context, request loopruntime.DispatchRequest) (loopruntime.SessionHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, request)
	session := newScriptedSession(request.Name)
	r.sessions = append(r.sessions, session)
	if request.Skill == "schedule" {
		orders := map[string]any{"orders": []any{map[string]any{
			"id": r.item.ID, "title": r.item.Title, "rationale": "target-authorized provider Issue",
			"stages": []any{map[string]any{"do": "execute", "with": "codex", "model": "gpt-test", "runtime": "process"}},
		}}}
		data, err := json.Marshal(orders)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(r.ordersNextPath, append(data, '\n'), 0o600); err != nil {
			return nil, err
		}
		session.complete(loopruntime.StatusCompleted)
	}
	return session, nil
}

func (r *scriptedRuntime) Terminate(handle loopruntime.SessionHandle) error {
	return handle.Terminate()
}

func (r *scriptedRuntime) ForceKill(handle loopruntime.SessionHandle) error {
	return handle.ForceKill()
}

func (r *scriptedRuntime) Recover(context.Context) ([]loopruntime.RecoveredSession, error) {
	return nil, nil
}

func (r *scriptedRuntime) sawExecute() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, request := range r.requests {
		if request.TaskKey == "execute" {
			return true
		}
	}
	return false
}

type scriptedSession struct {
	mu      sync.Mutex
	id      string
	outcome loopruntime.SessionOutcome
	done    chan struct{}
	once    sync.Once
}

func newScriptedSession(id string) *scriptedSession {
	return &scriptedSession{id: id + "-fake", done: make(chan struct{})}
}

func (s *scriptedSession) ID() string { return s.id }

func (s *scriptedSession) Outcome() loopruntime.SessionOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outcome
}

func (s *scriptedSession) Done() <-chan struct{} { return s.done }
func (s *scriptedSession) TotalCost() float64    { return 0 }
func (s *scriptedSession) VerdictPath() string   { return "" }
func (s *scriptedSession) Controller() dispatcher.AgentController {
	return dispatcher.NoopController()
}

func (s *scriptedSession) Terminate() error {
	s.complete(loopruntime.StatusCancelled)
	return nil
}

func (s *scriptedSession) ForceKill() error {
	s.complete(loopruntime.StatusKilled)
	return nil
}

func (s *scriptedSession) complete(status loopruntime.SessionStatus) {
	s.mu.Lock()
	s.outcome = loopruntime.SessionOutcome{Status: status, HasDeliverable: status == loopruntime.StatusCompleted}
	s.mu.Unlock()
	s.once.Do(func() { close(s.done) })
}

func TestNoodlesGitHubTargetConsumerCreatesOneTargetWorktree(t *testing.T) {
	body := issueBody(17)
	fake := newFakeGitHub(t, Issue{Number: 17, Title: "Authorized", State: "open", Body: body})
	payload := testPayload(17, body)
	if _, err := Receive(context.Background(), fake.client(), testPolicy(), testCapabilities(), testEvent(t, payload, testSender), testBaseSHA); err != nil {
		t.Fatal(err)
	}
	items, diagnostics, err := Sync(context.Background(), fake.client(), testPolicy(), testCapabilities())
	if err != nil || len(diagnostics) != 0 || len(items) != 1 {
		t.Fatalf("sync items=%#v diagnostics=%#v err=%v", items, diagnostics, err)
	}

	projectDir, commonDir := setupTargetGitRepository(t)
	runtimeDir := filepath.Join(projectDir, ".noodle")
	tracked := &trackingWorktree{app: &worktree.App{Root: projectDir, Quiet: true}}
	runtime := &scriptedRuntime{ordersNextPath: filepath.Join(runtimeDir, "orders-next.json"), item: items[0]}
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	prepareAgentHome(t)
	cfg, configDiagnostics, err := config.Load(filepath.Join(repositoryRoot, ".noodle.toml"))
	if err != nil || len(configDiagnostics.Fatals()) != 0 {
		t.Fatalf("load target config: diagnostics=%#v err=%v", configDiagnostics, err)
	}
	taskTypes, err := (skill.Resolver{SearchPaths: []string{filepath.Join(repositoryRoot, ".agents", "skills")}}).DiscoverTaskTypes()
	if err != nil {
		t.Fatal(err)
	}
	registry := taskreg.NewFromSkills(taskTypes)
	cfg.Mode = "auto"
	cfg.Routing.Defaults.Provider = "codex"
	cfg.Routing.Defaults.Model = "gpt-test"
	cfg.Concurrency.MaxConcurrency = 1
	noodleLoop := loop.New(projectDir, "noodle-test-binary-unused", cfg, loop.Dependencies{
		Runtimes: map[string]loopruntime.Runtime{"process": runtime}, Worktree: tracked,
		Adapter: inertAdapter{}, Mise: &providerMise{
			client: fake.client(), policy: testPolicy(), capabilities: testCapabilities(),
		}, Monitor: inertMonitor{}, Registry: registry,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	t.Cleanup(noodleLoop.Shutdown)

	for attempt := 0; attempt < 20 && !runtime.sawExecute(); attempt++ {
		if err := noodleLoop.Cycle(context.Background()); err != nil {
			t.Fatalf("Noodle cycle %d: %v", attempt, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !runtime.sawExecute() {
		runtime.mu.Lock()
		requests := append([]loopruntime.DispatchRequest(nil), runtime.requests...)
		runtime.mu.Unlock()
		orders, ordersErr := orderx.ReadOrders(filepath.Join(runtimeDir, "orders.json"))
		next, nextErr := orderx.ReadOrders(filepath.Join(runtimeDir, "orders-next.json"))
		t.Fatalf("target-local execute stage was not dispatched; requests=%#v orders=%#v ordersErr=%v next=%#v nextErr=%v state=%#v", requests, orders, ordersErr, next, nextErr, noodleLoop.State())
	}
	orders, err := orderx.ReadOrders(filepath.Join(runtimeDir, "orders.json"))
	if err != nil || len(orders.Orders) != 1 || orders.Orders[0].ID != items[0].ID {
		t.Fatalf("orders=%#v err=%v", orders, err)
	}
	if len(tracked.created) != 1 {
		t.Fatalf("created worktrees = %v, want exactly one", tracked.created)
	}
	runtime.mu.Lock()
	requestsBeforeRetry := len(runtime.requests)
	runtime.mu.Unlock()
	retry, err := Receive(context.Background(), fake.client(), testPolicy(), testCapabilities(), testEvent(t, payload, testSender), testBaseSHA)
	if err != nil || retry.Status != "reused" {
		t.Fatalf("duplicate delivery result=%#v err=%v", retry, err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := noodleLoop.Cycle(context.Background()); err != nil {
			t.Fatalf("duplicate cycle %d: %v", attempt, err)
		}
	}
	runtime.mu.Lock()
	requestsAfterRetry := len(runtime.requests)
	runtime.mu.Unlock()
	if len(tracked.created) != 1 || requestsAfterRetry != requestsBeforeRetry || fake.commentPosts != 1 || fake.issueEdits != 0 || fake.dispatchPosts != 0 || fake.modelInvocations != 0 {
		t.Fatalf("duplicate residue: worktrees=%v runtime requests=%d->%d comments=%d edits=%d dispatches=%d models=%d", tracked.created, requestsBeforeRetry, requestsAfterRetry, fake.commentPosts, fake.issueEdits, fake.dispatchPosts, fake.modelInvocations)
	}
	worktreePath := worktree.WorktreePath(projectDir, tracked.created[0])
	gotCommon := gitOutput(t, worktreePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	wantCommon, err := filepath.EvalSymlinks(commonDir)
	if err != nil {
		t.Fatal(err)
	}
	if gotCommon != wantCommon || !strings.HasSuffix(gotCommon, filepath.Join("ed3c", "noodle.git")) {
		t.Fatalf("Git common directory = %q, want %q", gotCommon, wantCommon)
	}
}

func setupTargetGitRepository(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	commonDir := filepath.Join(root, "ed3c", "noodle.git")
	if err := os.MkdirAll(filepath.Dir(commonDir), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init", "--bare", "--initial-branch=main", commonDir)
	seed := filepath.Join(root, "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "init", "--initial-branch=main")
	runGit(t, seed, "config", "user.name", "Test")
	runGit(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, ".gitignore"), []byte(".worktrees/\n.noodle/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", ".gitignore")
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "remote", "add", "origin", commonDir)
	runGit(t, seed, "push", "origin", "main")
	projectDir := filepath.Join(root, "target")
	runGit(t, root, "--git-dir="+commonDir, "worktree", "add", projectDir, "main")
	return projectDir, commonDir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func (r *scriptedRuntime) String() string {
	return fmt.Sprintf("scripted runtime with %d requests", len(r.requests))
}
