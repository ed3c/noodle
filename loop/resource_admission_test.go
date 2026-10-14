package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/mise"
	loopruntime "github.com/poteto/noodle/runtime"
)

func resourceBudget(l *Loop, max int) *admission.Budget {
	b := admission.NewBudget(max)
	b.Register(l.projectDir)
	l.deps.Admission = b
	l.deps.AdmissionContext = l.projectDir
	return b
}

func healthyResourceBudget(b *admission.Budget) {
	now := time.Now()
	for i := 0; i < 4; i++ {
		b.Observe(admission.Sample{At: now.Add(time.Duration(i-3) * 5 * time.Second), Pressure: admission.Normal}, nil)
	}
}

func resourceTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			raw, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			out[path] = string(raw)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestResourceDeferredAllSpawnEntrancesPreservePreparation(t *testing.T) {
	for _, entry := range []string{"cook", "schedule", "bootstrap", "planned"} {
		t.Run(entry, func(t *testing.T) {
			rt := newMockRuntime()
			l := newSteerTestLoop(t, rt)
			resourceBudget(l, 1)
			stage := Stage{TaskKey: "execute", Skill: "execute", Status: StageStatusPending}
			order := Order{ID: "42", Status: OrderStatusActive, Stages: []Stage{stage}}
			cand := dispatchCandidate{OrderID: order.ID, StageIndex: 0, Stage: stage}
			l.schedulePromoted = true
			l.lastPromotionError = "retain"
			l.scheduleDispatchDigest = "retain"
			before := resourceTree(t, l.projectDir)
			var err error
			switch entry {
			case "cook":
				err = l.spawnCook(context.Background(), cand, order, spawnOptions{})
			case "schedule":
				err = l.spawnSchedule(context.Background(), order, 0, "")
			case "bootstrap":
				err = l.spawnBootstrapIfNeeded(context.Background(), order)
			case "planned":
				err = l.spawnPlannedCandidates(context.Background(), []dispatchCandidate{cand}, OrdersFile{Orders: []Order{order}}, mise.Brief{})
			}
			if entry == "planned" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, admission.ErrDeferred) {
				t.Fatalf("expected resource wait, got %v", err)
			}
			if len(rt.calls) != 0 || len(l.cooks.activeCooksByOrder) != 0 || l.bootstrapAttempts != 0 || !l.schedulePromoted || l.lastPromotionError != "retain" || l.scheduleDispatchDigest != "retain" {
				t.Fatal("defer changed dispatch state")
			}
			if !reflect.DeepEqual(before, resourceTree(t, l.projectDir)) {
				t.Fatal("defer performed preparation effects")
			}
		})
	}
}

func TestResourceSessionJoinedAndUnknownRetention(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "joined", true: "unknown_remote"}[remote], func(t *testing.T) {
			rt := newMockRuntime()
			l := newSteerTestLoop(t, rt)
			b := resourceBudget(l, 1)
			healthyResourceBudget(b)
			runtimeName := "process"
			if remote {
				runtimeName = "sprites"
				rt.dispatchErr = errors.New("remote response lost")
				l.deps.Runtimes["sprites"] = rt
			}
			var session loopruntime.SessionHandle
			err := l.withAdmission(context.Background(), runtimeName, func(ctx context.Context) error {
				var e error
				session, _, e = l.dispatchSession(ctx, loopruntime.DispatchRequest{Name: "test", Runtime: runtimeName})
				return e
			})
			if remote {
				if !errors.Is(err, errDispatchUnknown) || len(rt.calls) != 1 || b.Snapshot().Active != 1 {
					t.Fatalf("unknown dispatch replayed or released: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if b.Snapshot().Active != 1 {
				t.Fatal("active worker lost reservation")
			}
			rt.sessions[0].complete("completed")
			<-session.Done()
			if b.Snapshot().Active != 0 {
				t.Fatal("joined worker did not release")
			}
		})
	}
}

func TestResourceSteerCapOneTransfersAndPressureRefusesBeforeKill(t *testing.T) {
	for _, pressure := range []bool{false, true} {
		t.Run(map[bool]string{false: "healthy_replacement", true: "pressure_refusal"}[pressure], func(t *testing.T) {
			rt := newMockRuntime()
			l := newControlTestLoop(t, &fakeWorktree{}, rt)
			l.config.Concurrency.MaxConcurrency = 1
			b := resourceBudget(l, 1)
			healthyResourceBudget(b)
			stage := Stage{TaskKey: "execute", Skill: "execute", Status: StageStatusPending}
			order := Order{ID: "42", Status: OrderStatusActive, Stages: []Stage{stage}}
			if e := l.spawnCook(context.Background(), dispatchCandidate{OrderID: "42", Stage: stage}, order, spawnOptions{}); e != nil {
				t.Fatal(e)
			}
			old := l.cooks.activeCooksByOrder["42"]
			before := resourceTree(t, l.projectDir)
			if pressure {
				b.Observe(admission.Sample{At: time.Now(), Pressure: admission.Warning}, nil)
			} else {
				b.Observe(admission.Sample{At: time.Now(), Pressure: admission.Normal}, nil)
			}
			err := l.steerRespawn(old, "keep exact original direction")
			if pressure {
				if !errors.Is(err, admission.ErrDeferred) || rt.sessions[0].status != "running" || l.cooks.activeCooksByOrder["42"] != old || !reflect.DeepEqual(before, resourceTree(t, l.projectDir)) {
					t.Fatal("pressure steer changed old owner")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if len(rt.calls) != 2 || b.Snapshot().Active != 1 {
					t.Fatalf("cap-one replacement did not transfer: calls %d active %d", len(rt.calls), b.Snapshot().Active)
				}
			}
			for _, s := range rt.sessions {
				select {
				case <-s.Done():
				default:
					_ = s.ForceKill()
				}
			}
			l.watcherWG.Wait()
		})
	}
}

func TestResourceRecoveryDoesNotTreatClosedDoneAsFree(t *testing.T) {
	rt := newMockRuntime()
	l := newControlTestLoop(t, &fakeWorktree{}, rt)
	resourceBudget(l, 1)
	closed := make(chan struct{})
	close(closed)
	rt.recovered = []loopruntime.RecoveredSession{{SessionHandle: &mockSession{id: "unattributed-live", done: closed}, RuntimeName: "process"}}
	if err := l.recoverAdoptedSessions(context.Background()); err == nil {
		t.Fatal("unaccounted recovery was accepted")
	}
	if len(rt.calls) != 0 {
		t.Fatal("recovery started worker")
	}
}

func TestLoopEvidenceUsesBoundExecutableAndEnvironment(t *testing.T) {
	for _, sentinel := range []string{"alpha", "beta"} {
		root := t.TempDir()
		bin := filepath.Join(root, "bin")
		if e := os.Mkdir(bin, 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf '%s' \"$SENTINEL\"\n"), 0700); e != nil {
			t.Fatal(e)
		}
		e, err := execution.New(root, []string{"PATH=" + bin, "SENTINEL=" + sentinel})
		if err != nil {
			t.Fatal(err)
		}
		got, err := publicationGitWithExecution(e, root, "status")
		if err != nil || got != sentinel {
			t.Fatalf("bound publication: %q %v", got, err)
		}
		raw, err := candidateGitWithExecution(e, root, "ls-files")
		if err != nil || string(raw) != sentinel {
			t.Fatalf("bound candidate: %q %v", raw, err)
		}
		got, err = recoveryGitWithExecution(e, root, "HEAD")
		if err != nil || got != sentinel {
			t.Fatalf("bound recovery: %q %v", got, err)
		}
	}
}

type fullResourceRuntime struct{ *mockRuntime }

func (r fullResourceRuntime) CapacityAvailable() bool { return false }

func TestResourceRuntimeCapacityDefersBeforePreparation(t *testing.T) {
	rt := newMockRuntime()
	l := newSteerTestLoop(t, rt)
	l.deps.Runtimes["process"] = fullResourceRuntime{rt}
	b := resourceBudget(l, 1)
	healthyResourceBudget(b)
	before := resourceTree(t, l.projectDir)
	stage := Stage{TaskKey: "execute", Skill: "execute", Status: StageStatusPending}
	err := l.spawnCook(context.Background(), dispatchCandidate{OrderID: "42", Stage: stage}, Order{ID: "42"}, spawnOptions{})
	if !errors.Is(err, admission.ErrDeferred) || len(rt.calls) != 0 || b.Snapshot().Active != 0 || b.Snapshot().Credit != 1 || !reflect.DeepEqual(before, resourceTree(t, l.projectDir)) {
		t.Fatalf("local capacity refusal changed preparation: %v", err)
	}
}

func TestResourceScheduleBootstrapReusesOriginalPermit(t *testing.T) {
	rt := newMockRuntime()
	l := newSteerTestLoop(t, rt)
	l.registry = testRegistryWithoutSchedule()
	b := resourceBudget(l, 1)
	healthyResourceBudget(b)
	order := scheduleOrder(l.config, "")
	err := l.spawnCook(context.Background(), dispatchCandidate{OrderID: order.ID, Stage: order.Stages[0]}, order, spawnOptions{})
	if err != nil || len(rt.calls) != 1 || l.bootstrapInFlight == nil || b.Snapshot().Active != 1 {
		t.Fatalf("nested bootstrap did not share original admission: %v calls=%d active=%d", err, len(rt.calls), b.Snapshot().Active)
	}
	rt.sessions[0].complete("completed")
	<-l.bootstrapInFlight.session.Done()
	if b.Snapshot().Active != 0 {
		t.Fatal("bootstrap join did not release")
	}
}

func TestResourceRecoveryErrorFailsClosed(t *testing.T) {
	rt := newMockRuntime()
	l := newControlTestLoop(t, &fakeWorktree{}, rt)
	resourceBudget(l, 1)
	rt.recoverErr = errors.New("session readback unavailable")
	if err := l.recoverAdoptedSessions(context.Background()); !errors.Is(err, rt.recoverErr) {
		t.Fatalf("unknown recovery was ignored: %v", err)
	}
}

func TestResourceControlQueueRetainsPressureWaitAndUnknownCustody(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "pressure_then_resume", true: "unknown_no_replay"}[unknown], func(t *testing.T) {
			rt := newMockRuntime()
			l := newControlTestLoop(t, &fakeWorktree{}, rt)
			b := resourceBudget(l, 1)
			healthyResourceBudget(b)
			stage := Stage{TaskKey: "execute", Skill: "execute", Status: StageStatusPending}
			order := Order{ID: "42", Status: OrderStatusActive, Stages: []Stage{stage}}
			if err := l.spawnCook(context.Background(), dispatchCandidate{OrderID: order.ID, Stage: stage}, order, spawnOptions{}); err != nil {
				t.Fatal(err)
			}
			old := l.cooks.activeCooksByOrder[order.ID]
			if unknown {
				old.stage.Runtime = "sprites"
				l.deps.Runtimes["sprites"] = rt
				rt.dispatchErr = errors.New("remote response lost after send")
				b.Observe(admission.Sample{At: time.Now(), Pressure: admission.Normal}, nil)
			} else {
				b.Observe(admission.Sample{At: time.Now(), Pressure: admission.Warning}, nil)
			}
			command := ControlCommand{ID: "original-steer", Action: "steer", Target: old.session.ID(), Prompt: "retain exact direction"}
			raw, _ := json.Marshal(command)
			raw = append(raw, '\n')
			control, acks, _ := l.controlPaths()
			if err := os.WriteFile(control, raw, 0600); err != nil {
				t.Fatal(err)
			}
			err := l.processControlCommands()
			if unknown {
				if !errors.Is(err, errDispatchUnknown) || b.Snapshot().Active != 1 {
					t.Fatalf("unknown not quarantined: %v %+v", err, b.Snapshot())
				}
				count := len(rt.calls)
				if err = l.processControlCommands(); !errors.Is(err, errDispatchUnknown) || len(rt.calls) != count {
					t.Fatal("unknown control replayed")
				}
				if err = l.releaseIfQuiescent(); !errors.Is(err, errDispatchUnknown) {
					t.Fatal("unknown custody detached")
				}
				err = l.spawnCook(context.Background(), dispatchCandidate{OrderID: "next", Stage: stage}, Order{ID: "next"}, spawnOptions{})
				if !errors.Is(err, errDispatchUnknown) || len(rt.calls) != count {
					t.Fatal("unknown custody admitted another worker")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				pending, _ := os.ReadFile(control)
				ack, _ := os.ReadFile(acks)
				_, seen := l.cmds.processedIDs[command.ID]
				if !bytes.Equal(raw, pending) || len(ack) != 0 || seen || l.cmds.lastAppliedSeq != 0 || len(rt.calls) != 1 || rt.sessions[0].status != "running" {
					t.Fatal("deferred control consumed original request")
				}
				healthyResourceBudget(b)
				if err = l.processControlCommands(); err != nil {
					t.Fatal(err)
				}
				pending, _ = os.ReadFile(control)
				ack, _ = os.ReadFile(acks)
				if len(pending) != 0 || len(rt.calls) != 2 || !bytes.Contains(ack, []byte(`"status":"ok"`)) {
					t.Fatalf("original steer did not resume once: calls=%d pending=%s ack=%s", len(rt.calls), pending, ack)
				}
			}
			for _, session := range rt.sessions {
				select {
				case <-session.Done():
				default:
					_ = session.ForceKill()
				}
			}
			l.watcherWG.Wait()
		})
	}
}

func TestResourceControlWaitAllowsLoadReductionAndHydration(t *testing.T) {
	for _, action := range []string{"pause", "stop-all"} {
		t.Run(action, func(t *testing.T) {
			rt := newMockRuntime()
			l := newControlTestLoop(t, &fakeWorktree{}, rt)
			b := resourceBudget(l, 1)
			healthyResourceBudget(b)
			stage := Stage{TaskKey: "execute", Skill: "execute", Status: StageStatusPending}
			order := Order{ID: "42", Status: OrderStatusActive, Stages: []Stage{stage}}
			if err := l.spawnCook(context.Background(), dispatchCandidate{OrderID: order.ID, Stage: stage}, order, spawnOptions{}); err != nil {
				t.Fatal(err)
			}
			old := l.cooks.activeCooksByOrder[order.ID]
			b.Observe(admission.Sample{At: time.Now(), Pressure: admission.Warning}, nil)
			raw, _ := json.Marshal(ControlCommand{ID: "waiting-steer", Action: "steer", Target: old.session.ID(), Prompt: "original queued direction"})
			raw = append(raw, '\n')
			reduction, _ := json.Marshal(ControlCommand{ID: "reduce-load", Action: action})
			reduction = append(reduction, '\n')
			control, acks, _ := l.controlPaths()
			if err := os.WriteFile(control, append(append([]byte{}, raw...), reduction...), 0600); err != nil {
				t.Fatal(err)
			}
			if err := l.processControlCommands(); err != nil {
				t.Fatal(err)
			}
			pending, _ := os.ReadFile(control)
			ack, _ := os.ReadFile(acks)
			if !bytes.Equal(raw, pending) || !bytes.Contains(ack, []byte(`"id":"reduce-load"`)) {
				t.Fatal("load reduction blocked behind resource wait")
			}
			if _, seen := l.cmds.processedIDs["waiting-steer"]; seen {
				t.Fatal("waiting steer was deduplicated")
			}
			if l.cmds.lastAppliedSeq != 2 {
				t.Fatalf("expected original applied sequence 2, got %d", l.cmds.lastAppliedSeq)
			}
			if action == "pause" {
				if l.state != StatePaused {
					t.Fatal("pause did not run")
				}
				if err := l.writeLastAppliedSeq(); err != nil {
					t.Fatal(err)
				}
				l.cmds = cmdProcessor{processedIDs: map[string]struct{}{}}
				if err := l.hydrateProcessedCommands(); err != nil {
					t.Fatal(err)
				}
				if l.cmds.lastAppliedSeq != 2 || l.cmds.cmdSeqCounter != 2 {
					t.Fatal("original watermark was not hydrated")
				}
				stamp := time.Unix(1234, 0)
				if err := os.Chtimes(control, stamp, stamp); err != nil {
					t.Fatal(err)
				}
				if err := l.processControlCommands(); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(control)
				if err != nil {
					t.Fatal(err)
				}
				if !info.ModTime().Equal(stamp) {
					t.Fatal("unchanged waiting input rewrote control and would retrigger watcher")
				}
				healthyResourceBudget(b)
				if err := l.processControlCommands(); err != nil {
					t.Fatal(err)
				}
				pending, _ = os.ReadFile(control)
				if len(pending) != 0 || len(rt.calls) != 2 {
					t.Fatal("hydrated waiting steer was skipped or replayed")
				}
				if _, seen := l.cmds.processedIDs["waiting-steer"]; !seen {
					t.Fatal("resumed control missing dedup")
				}
			} else {
				select {
				case <-old.session.Done():
				case <-time.After(time.Second):
					t.Fatal("stop-all did not stop original worker")
				}
			}
			for _, session := range rt.sessions {
				select {
				case <-session.Done():
				default:
					_ = session.ForceKill()
				}
			}
			l.watcherWG.Wait()
		})
	}
}
