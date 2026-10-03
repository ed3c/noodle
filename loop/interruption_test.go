package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/dispatcher"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

func interruptionFixture(t *testing.T) (string, string) {
	t.Helper()
	project, old := stoppedReviewFixture(t)
	name := cookBaseName("order-1", 0, "execute")
	wt := filepath.Join(project, ".worktrees", name)
	publicationRun(t, project, "git", "worktree", "move", old, wt)
	publicationRun(t, wt, "git", "branch", "-m", name)
	dir := filepath.Join(project, ".noodle")
	s, err := reducer.ReadSnapshot(filepath.Join(dir, "state.snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	o := s.State.Orders["order-1"]
	st := &o.Stages[0]
	st.Status = state.StageRunning
	st.Provider, st.Model, st.Runtime, st.Skill = "codex", "fixture", "process", "execute"
	st.Prompt = `{"repository":"example/project","issue":7,"envelope_sha256":"` + strings.Repeat("a", 64) + `"}`
	st.Attempts[0].AttemptID = dispatchAttemptID(o.OrderID, 0, 0)
	st.Attempts[0].Status = state.AttemptRunning
	st.Attempts[0].WorktreeName = name
	s.State.Orders[o.OrderID] = o
	s.State.PendingReviews = map[string]state.PendingReviewNode{}
	payload, _ := json.Marshal(map[string]any{"order_id": o.OrderID, "stage_index": 0, "attempt_id": st.Attempts[0].AttemptID})
	s.EffectLedger = []reducer.EffectLedgerRecord{{EffectID: "dispatch-1", Effect: reducer.Effect{EffectID: "dispatch-1", Type: reducer.EffectDispatch, Payload: payload}, Status: reducer.EffectLedgerPending}}
	if err := reducer.WriteSnapshotAtomic(filepath.Join(dir, "state.snapshot.json"), s); err != nil {
		t.Fatal(err)
	}
	l := &Loop{runtimeDir: dir, canonical: s.State, canonicalLoaded: true}
	if err := l.writeProjectionState(); err != nil {
		t.Fatal(err)
	}
	if err := l.writePendingReview(); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(dir, "sessions", "session-1")
	recoveryWrite(t, filepath.Join(session, "prompt.txt"), []byte("[order:order-1] Work backlog item order-1\n\n"+st.Prompt))
	recoveryJSON(t, filepath.Join(session, "spawn.json"), map[string]any{"session_id": "session-1", "worktree_path": wt, "provider": "codex", "model": "fixture", "runtime": "process", "retry_count": 0})
	recoveryWrite(t, filepath.Join(session, "raw.ndjson"), []byte("{\"type\":\"thread.started\",\"thread_id\":\"fixture\"}\n"))
	recoveryWrite(t, filepath.Join(session, "events.ndjson"), []byte("{\"type\":\"stage_message\",\"session_id\":\"session-1\",\"payload\":{\"message\":\"feedback only\"}}\n"))
	recoveryWrite(t, filepath.Join(wt, "candidate.txt"), []byte("uncommitted writer progress\n"))
	recoveryWrite(t, filepath.Join(wt, "new.txt"), []byte("untracked writer progress\n"))
	return project, wt
}

func TestInterruptionPrepareRetainsDirtyCandidateAndHistory(t *testing.T) {
	project, wt := interruptionFixture(t)
	snapshot := filepath.Join(project, ".noodle", "state.snapshot.json")
	before, _ := os.ReadFile(snapshot)
	lockPath := filepath.Join(project, ".noodle", "noodle.lock")
	recoveryWrite(t, lockPath, []byte("original lock bytes\n"))
	events, _ := os.ReadFile(filepath.Join(project, ".noodle", "sessions", "session-1", "events.ndjson"))
	r := InspectInterruption(project, "/exact/noodle", "order-1", "example/project#7")
	if r.Status != "recoverable" || len(r.Next.Argv) != 8 {
		t.Fatalf("inspect: %+v", r)
	}
	current, _ := os.ReadFile(snapshot)
	lockBytes, _ := os.ReadFile(lockPath)
	if string(lockBytes) != "original lock bytes\n" {
		t.Fatal("inspect changed native lock bytes")
	}
	if !bytes.Equal(current, before) {
		t.Fatal("inspect wrote snapshot")
	}
	r = PrepareInterruption(project, "/exact/noodle", "order-1", "example/project#7", r.Digest)
	if r.Status != "prepared" || r.Successor != nil || len(r.Next.Argv) != 0 {
		t.Fatalf("prepare: %+v", r)
	}
	s, err := reducer.ReadSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	st := s.State.Orders["order-1"].Stages[0]
	if st.Status != state.StagePending || len(st.Attempts) != 1 || st.Attempts[0].Status != state.AttemptCancelled || st.Attempts[0].ExitCode != nil || st.Attempts[0].SessionID != "session-1" {
		t.Fatalf("attempt: %+v", st)
	}
	got, _ := os.ReadFile(filepath.Join(project, ".noodle", "sessions", "session-1", "events.ndjson"))
	if !bytes.Equal(got, events) {
		t.Fatal("owner fabricated session outcome")
	}
	assertInterruptionCandidate(t, wt)
	again := InspectInterruption(project, "/exact/noodle", "order-1", "example/project#7")
	if again.Status != "prepared" || again.Digest != r.Digest {
		t.Fatalf("readback: %+v", again)
	}
}

func assertInterruptionCandidate(t *testing.T, wt string) {
	t.Helper()
	for name, want := range map[string]string{"candidate.txt": "uncommitted writer progress\n", "new.txt": "untracked writer progress\n"} {
		got, err := os.ReadFile(filepath.Join(wt, name))
		if err != nil || string(got) != want {
			t.Fatalf("candidate %s lost: %q %v", name, got, err)
		}
	}
}

func TestInterruptionRefusalsPreserveOwner(t *testing.T) {
	for _, name := range []string{"live_lock", "live_process", "changed_file", "changed_untracked", "changed_session", "wrong_subject", "terminal", "stage_yield", "foreign_session", "unknown_effect", "unfinished_effect", "stale_digest"} {
		t.Run(name, func(t *testing.T) {
			project, wt := interruptionFixture(t)
			r := InspectInterruption(project, "/exact/noodle", "order-1", "example/project#7")
			if r.Status != "recoverable" {
				t.Fatalf("setup: %+v", r)
			}
			dir := filepath.Join(project, ".noodle")
			subject := "example/project#7"
			switch name {
			case "live_lock":
				lock, err := lockfile.TryLock(filepath.Join(dir, "noodle.lock"))
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			case "live_process":
				recoveryJSON(t, filepath.Join(dir, "sessions", "session-1", "process.json"), map[string]any{"session_id": "session-1", "pid": os.Getpid()})
			case "changed_file":
				recoveryWrite(t, filepath.Join(wt, "candidate.txt"), []byte("foreign"))
			case "changed_untracked":
				recoveryWrite(t, filepath.Join(wt, "other"), []byte("foreign"))
			case "changed_session":
				recoveryWrite(t, filepath.Join(dir, "sessions", "session-1", "prompt.txt"), []byte("foreign"))
			case "wrong_subject":
				subject = "example/project#8"
			case "stage_yield":
				recoveryWrite(t, filepath.Join(dir, "sessions", "session-1", "events.ndjson"), []byte("{\"type\":\"stage_yield\",\"payload\":{\"message\":\"done\"}}\n"))
			case "terminal":
				recoveryWrite(t, filepath.Join(dir, "sessions", "session-1", "raw.ndjson"), []byte("{\"type\":\"turn.completed\"}\n"))
			case "foreign_session":
				if err := os.MkdirAll(filepath.Join(dir, "sessions", "foreign"), 0700); err != nil {
					t.Fatal(err)
				}
				recoveryJSON(t, filepath.Join(dir, "sessions", "foreign", "meta.json"), map[string]any{"session_id": "foreign", "status": "exited", "runtime": "process"})
				recoveryJSON(t, filepath.Join(dir, "sessions", "foreign", "process.json"), map[string]any{"session_id": "foreign", "pid": publicationTestProcess(t)})
				recoveryWrite(t, filepath.Join(dir, "sessions", "foreign", "prompt.txt"), []byte("[order:foreign]"))
			case "unfinished_effect":
				recoveryWrite(t, filepath.Join(dir, "sessions", "session-1", "raw.ndjson"), []byte("{\"type\":\"item.started\",\"item\":{\"id\":\"effect-1\",\"type\":\"command_execution\"}}\n"))
			case "unknown_effect":
				p := filepath.Join(dir, "state.snapshot.json")
				s, _ := reducer.ReadSnapshot(p)
				s.EffectLedger[0].Status = reducer.EffectLedgerRunning
				if err := reducer.WriteSnapshotAtomic(p, s); err != nil {
					t.Fatal(err)
				}
			case "stale_digest":
				r.Digest = strings.Repeat("0", 64)
			}
			before, _ := os.ReadFile(filepath.Join(dir, "state.snapshot.json"))
			got := PrepareInterruption(project, "/exact/noodle", "order-1", subject, r.Digest)
			if got.Status != "refused" {
				t.Fatalf("refusal: %+v", got)
			}
			after, _ := os.ReadFile(filepath.Join(dir, "state.snapshot.json"))
			if !bytes.Equal(before, after) {
				t.Fatal("refusal changed snapshot")
			}
		})
	}
}

func TestInterruptionPreparationCrashReadback(t *testing.T) {
	for _, point := range []string{"after_intent", "after_canonical"} {
		t.Run(point, func(t *testing.T) {
			project, wt := interruptionFixture(t)
			r := InspectInterruption(project, "/exact/noodle", "order-1", "example/project#7")
			func() {
				defer func() {
					if recover() != "cut" {
						t.Error("fault not reached")
					}
				}()
				prepareInterruption(project, "/exact/noodle", "order-1", "example/project#7", r.Digest, func(at string) {
					if at == point {
						panic("cut")
					}
				})
			}()
			got := PrepareInterruption(project, "/exact/noodle", "order-1", "example/project#7", r.Digest)
			if got.Status != "prepared" {
				t.Fatalf("reentry: %+v", got)
			}
			assertInterruptionCandidate(t, wt)
		})
	}
}

func interruptionLoop(t *testing.T, project string, rt *mockRuntime) *Loop {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Mode = "manual"
	l := New(project, "noodle", cfg, Dependencies{Runtimes: map[string]loopruntime.Runtime{"process": rt}, Worktree: &fakeWorktree{}, Adapter: &fakeAdapterRunner{}, Mise: &fakeMise{}, Monitor: fakeMonitor{}, Registry: testLoopRegistry(), Now: time.Now, OrdersFile: filepath.Join(project, ".noodle", "orders.json")})
	if _, err := l.loadCanonicalSnapshot(); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestInterruptionDispatchPreservesOnceAndSupportsHeldPromptUpdate(t *testing.T) {
	project, wt := interruptionFixture(t)
	r := InspectInterruption(project, "/exact/noodle", "order-1", "example/project#7")
	r = PrepareInterruption(project, "/exact/noodle", "order-1", "example/project#7", r.Digest)
	if r.Status != "prepared" {
		t.Fatalf("prepare: %+v", r)
	}
	rt := newMockRuntime()
	l := interruptionLoop(t, project, rt)
	// The existing edit-item owner installs the selected new envelope while held.
	prompt := `{"repository":"example/project","issue":7,"envelope_sha256":"` + strings.Repeat("b", 64) + `"}`
	if err := l.controlEditItem(ControlCommand{OrderID: "order-1", Prompt: prompt}); err != nil {
		t.Fatal(err)
	}
	lock, err := lockfile.TryLock(filepath.Join(project, ".noodle", "noodle.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	read := InspectInterruption(project, "/exact/noodle", "order-1", "example/project#7")
	if read.Status != "prepared" {
		t.Fatalf("held readback: %+v", read)
	}
	orders, err := l.currentOrders()
	if err != nil {
		t.Fatal(err)
	}
	o := orders.Orders[0]
	cand := dispatchCandidate{OrderID: o.ID, StageIndex: 0, Stage: o.Stages[0]}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := l.spawnCook(ctx, cand, o, spawnOptions{attempt: 1}); err != nil {
		t.Fatal(err)
	}
	assertInterruptionCandidate(t, wt)
	read = InspectInterruption(project, "/exact/noodle", "order-1", "example/project#7")
	if read.Status != "dispatched" || !read.CandidateUnchanged || read.Successor == nil || read.Successor.AttemptID != "order-1-0-attempt-1" || read.Successor.SessionID == "session-1" {
		t.Fatalf("successor readback: %+v", read)
	}
	recoveryWrite(t, filepath.Join(wt, "candidate.txt"), []byte("successor progress"))
	drift := InspectInterruption(project, "/exact/noodle", "order-1", "example/project#7")
	if drift.Status != "dispatched" || drift.CandidateUnchanged || drift.CandidateInvalid == "" || drift.Successor == nil || *drift.Successor != *read.Successor {
		t.Fatalf("post-dispatch drift: %+v", drift)
	}
	if len(rt.calls) != 1 || !strings.Contains(rt.calls[0].Prompt, "session-1") {
		t.Fatal("missing original lineage")
	}
	if err := l.spawnCook(ctx, cand, o, spawnOptions{attempt: 1}); err == nil {
		t.Fatal("replayed interruption")
	}
	if len(rt.calls) != 1 {
		t.Fatal("second dispatch")
	}
	rt.sessions[0].ForceKill()
}

func TestInterruptionUnknownDispatchRefusesReplay(t *testing.T) {
	project, wt := interruptionFixture(t)
	r := InspectInterruption(project, "/exact/noodle", "order-1", "example/project#7")
	r = PrepareInterruption(project, "/exact/noodle", "order-1", "example/project#7", r.Digest)
	if r.Status != "prepared" {
		t.Fatalf("prepare: %+v", r)
	}
	rt := newMockRuntime()
	rt.dispatchErr = errors.New("lost launch result")
	l := interruptionLoop(t, project, rt)
	orders, _ := l.currentOrders()
	o := orders.Orders[0]
	cand := dispatchCandidate{OrderID: o.ID, StageIndex: 0, Stage: o.Stages[0]}
	if err := l.spawnCook(context.Background(), cand, o, spawnOptions{attempt: 1}); err == nil {
		t.Fatal("unknown launch accepted")
	}
	read := InspectInterruption(project, "/exact/noodle", "order-1", "example/project#7")
	if read.Status != "refused" || !strings.Contains(read.Invalid, "outcome unknown") {
		t.Fatalf("unknown readback: %+v", read)
	}
	if err := l.spawnCook(context.Background(), cand, o, spawnOptions{attempt: 1}); err == nil {
		t.Fatal("unknown launch replayed")
	}
	if len(rt.calls) != 1 {
		t.Fatalf("dispatches: %d", len(rt.calls))
	}
	assertInterruptionCandidate(t, wt)
}

func TestInterruptionOfferedLaunchRemainsUnknownBeforeResult(t *testing.T) {
	project, wt := interruptionFixture(t)
	r := InspectInterruption(project, "/exact/noodle", "order-1", "example/project#7")
	r = PrepareInterruption(project, "/exact/noodle", "order-1", "example/project#7", r.Digest)
	if r.Status != "prepared" {
		t.Fatalf("prepare: %+v", r)
	}
	rt := newMockRuntime()
	l := interruptionLoop(t, project, rt)
	orders, _ := l.currentOrders()
	o := orders.Orders[0]
	cand := dispatchCandidate{OrderID: o.ID, StageIndex: 0, Stage: o.Stages[0]}
	i, err := l.interruptionForDispatch(cand, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := offerInterruptionDispatch(project, *i, "order-1-0-attempt-1"); err != nil {
		t.Fatal(err)
	}
	for _, fn := range []func() InterruptionInspection{
		func() InterruptionInspection {
			return InspectInterruption(project, "/exact/noodle", "order-1", "example/project#7")
		},
		func() InterruptionInspection {
			return PrepareInterruption(project, "/exact/noodle", "order-1", "example/project#7", r.Digest)
		},
	} {
		got := fn()
		if got.Status != "refused" || !strings.Contains(got.Invalid, "outcome unknown") || len(got.Next.Argv) != 0 {
			t.Fatalf("unknown: %+v", got)
		}
	}
	if err := l.spawnCook(context.Background(), cand, o, spawnOptions{attempt: 1}); err == nil {
		t.Fatal("offered launch repeated")
	}
	if len(rt.calls) != 0 {
		t.Fatal("readback started writer")
	}
	node := l.canonical.Orders["order-1"]
	node.Stages[0].Status = state.StageDispatching
	l.canonical.Orders["order-1"] = node
	if err := l.reconcileStaleDispatchStages(); err == nil {
		t.Fatal("startup cleared offered launch")
	}
	if l.canonical.Orders["order-1"].Stages[0].Status != state.StageDispatching {
		t.Fatal("startup rewrote unknown attempt")
	}
	assertInterruptionCandidate(t, wt)
}

func TestInterruptionTicketOutcomeIsNotWriterTerminal(t *testing.T) {
	project, _ := interruptionFixture(t)
	path := filepath.Join(project, ".noodle", "sessions", "session-1", "events.ndjson")
	recoveryWrite(t, path, []byte("{\"type\":\"ticket_done\",\"payload\":{\"outcome\":\"resolved\"}}\n"))
	r := InspectInterruption(project, "/exact/noodle", "order-1", "example/project#7")
	if r.Status != "recoverable" {
		t.Fatalf("ticket outcome classified as writer terminal: %+v", r)
	}
}

func TestInterruptionHeldCycleUsesRealProcessDispatcher(t *testing.T) {
	project, wt := interruptionFixture(t)
	r := InspectInterruption(project, "/exact/noodle", "order-1", "example/project#7")
	r = PrepareInterruption(project, "/exact/noodle", "order-1", "example/project#7", r.Digest)
	if r.Status != "prepared" {
		t.Fatalf("prepare: %+v", r)
	}
	runtimeDir := filepath.Join(project, ".noodle")
	d := dispatcher.NewProcessDispatcher(dispatcher.ProcessDispatcherConfig{ProjectDir: project, RuntimeDir: runtimeDir, RuntimeKind: "process", RuntimeDefault: `cat >/dev/null; test "$(cat candidate.txt)" = "uncommitted writer progress" || exit 81; test "$(cat new.txt)" = "untracked writer progress" || exit 82; printf '%s\n' '{"type":"thread.started","thread_id":"fixture"}'; exec sleep 30`})
	rt := loopruntime.NewProcessRuntime(d, runtimeDir, 1)
	cfg := config.DefaultConfig()
	cfg.Mode = "manual"
	cfg.Concurrency.MaxConcurrency = 1
	l := New(project, "noodle", cfg, Dependencies{Runtimes: map[string]loopruntime.Runtime{"process": rt}, Worktree: &fakeWorktree{}, Adapter: &fakeAdapterRunner{}, Mise: &fakeMise{}, Monitor: fakeMonitor{}, Registry: testLoopRegistry(), Now: time.Now, OrdersFile: filepath.Join(runtimeDir, "orders.json"), ModeOverride: "manual"})
	if err := l.loadOrdersState(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer l.Shutdown()
	if err := l.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := l.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if len(l.cooks.activeCooksByOrder) != 0 {
		t.Fatal("held cycle started a child")
	}
	if err := l.controlMode("supervised"); err != nil {
		t.Fatal(err)
	}
	if err := l.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	cook := l.cooks.activeCooksByOrder["order-1"]
	if cook == nil {
		t.Fatalf("original order not dispatched: %+v", l.canonical.Orders)
	}
	defer cook.session.ForceKill()
	select {
	case <-cook.session.Done():
		t.Fatal("fixture process could not read retained candidate")
	default:
	}
	read := InspectInterruption(project, "/exact/noodle", "order-1", "example/project#7")
	if read.Status != "dispatched" || !read.CandidateUnchanged || read.Successor == nil || read.Successor.SessionID != cook.session.ID() {
		t.Fatalf("native readback: %+v", read)
	}
	assertInterruptionCandidate(t, wt)
	startup := filepath.Join(runtimeDir, "sessions", cook.session.ID(), "raw.ndjson")
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, _ := os.ReadFile(startup)
		if bytes.Contains(data, []byte(`"thread.started"`)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture process never confirmed candidate bytes")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestInterruptionRetainsDeclarativeScheduleDispatchHistory(t *testing.T) {
	project, _ := interruptionFixture(t)
	dir := filepath.Join(project, ".noodle")
	s, err := reducer.ReadSnapshot(filepath.Join(dir, "state.snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.State.Orders["schedule"] = state.OrderNode{OrderID: "schedule", Status: state.OrderActive, Stages: []state.StageNode{{StageIndex: 0, TaskKey: "schedule", Skill: "schedule", Status: state.StagePending, Provider: "codex", Model: "fixture", Runtime: "process"}}}
	payload, _ := json.Marshal(map[string]any{"order_id": "schedule", "stage_index": 0, "attempt_id": "schedule-0-attempt-0"})
	s.EffectLedger = append(s.EffectLedger, reducer.EffectLedgerRecord{EffectID: "schedule-dispatch", Effect: reducer.Effect{EffectID: "schedule-dispatch", Type: reducer.EffectDispatch, Payload: payload}, Status: reducer.EffectLedgerPending})
	if err := reducer.WriteSnapshotAtomic(filepath.Join(dir, "state.snapshot.json"), s); err != nil {
		t.Fatal(err)
	}
	l := &Loop{runtimeDir: dir, canonical: s.State, canonicalLoaded: true}
	if err := l.writeProjectionState(); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(dir, "sessions", "schedule-prior")
	if err := os.MkdirAll(session, 0700); err != nil {
		t.Fatal(err)
	}
	recoveryJSON(t, filepath.Join(session, "meta.json"), map[string]any{"session_id": "schedule-prior", "status": "exited", "runtime": "process"})
	recoveryJSON(t, filepath.Join(session, "process.json"), map[string]any{"session_id": "schedule-prior", "pid": publicationTestProcess(t)})
	spawn := map[string]any{"session_id": "schedule-prior", "skill": "schedule", "runtime": "process", "worktree_path": project}
	spawnPath := filepath.Join(session, "spawn.json")
	promptPath := filepath.Join(session, "prompt.txt")
	prompt := buildSchedulePrompt("schedule", "/selected/schedule", "", Order{}, "", dir, "", nil, nil)
	recoveryJSON(t, spawnPath, spawn)
	recoveryWrite(t, promptPath, []byte(prompt))
	for _, field := range []string{"session_id", "skill", "runtime", "worktree_path"} {
		t.Run("foreign_"+field, func(t *testing.T) {
			original := spawn[field]
			spawn[field] = "foreign"
			recoveryJSON(t, spawnPath, spawn)
			read := InspectInterruption(project, "/exact/noodle", "order-1", "example/project#7")
			if read.Status != "refused" || !strings.Contains(read.Invalid, "foreign session schedule-prior") {
				t.Fatalf("foreign schedule spawn accepted: %+v", read)
			}
			spawn[field] = original
			recoveryJSON(t, spawnPath, spawn)
		})
	}
	t.Run("missing_spawn", func(t *testing.T) {
		if err := os.Remove(spawnPath); err != nil {
			t.Fatal(err)
		}
		read := InspectInterruption(project, "/exact/noodle", "order-1", "example/project#7")
		if read.Status != "refused" {
			t.Fatalf("missing spawn accepted: %+v", read)
		}
		recoveryJSON(t, spawnPath, spawn)
	})
	t.Run("foreign_explicit_order", func(t *testing.T) {
		recoveryWrite(t, promptPath, []byte("[order:foreign]\n"+prompt))
		read := InspectInterruption(project, "/exact/noodle", "order-1", "example/project#7")
		if read.Status != "refused" {
			t.Fatalf("foreign order accepted: %+v", read)
		}
		recoveryWrite(t, promptPath, []byte(prompt))
	})
	read := InspectInterruption(project, "/exact/noodle", "order-1", "example/project#7")
	if read.Status != "recoverable" {
		t.Fatalf("declarative history rejected: %+v", read)
	}
	prepared := PrepareInterruption(project, "/exact/noodle", "order-1", "example/project#7", read.Digest)
	if prepared.Status != "prepared" {
		t.Fatalf("prepare: %+v", prepared)
	}
	after, err := reducer.ReadSnapshot(filepath.Join(dir, "state.snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	beforeLedger, _ := json.Marshal(s.EffectLedger)
	afterLedger, _ := json.Marshal(after.EffectLedger)
	if !bytes.Equal(beforeLedger, afterLedger) {
		t.Fatal("rewrote declarative effect history")
	}
}
