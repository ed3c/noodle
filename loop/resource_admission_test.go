package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
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

func contendedDiskPeak(t *testing.T, root string) uint64 {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("native machine identity unsupported")
	}
	cmd := exec.Command("df", "-Pk", root)
	cmd.Env = append(cmd.Environ(), "LC_ALL=C")
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	fields := strings.Fields(lines[len(lines)-1])
	if len(lines) < 2 || len(fields) < 6 {
		t.Fatalf("disk availability missing: %q", output)
	}
	availableKiB, err := strconv.ParseUint(fields[3], 10, 54)
	if err != nil || availableKiB == 0 {
		t.Fatalf("invalid disk availability %q: %v", fields[3], err)
	}
	available := availableKiB * 1024
	// One reservation fits with headroom. Two exceed available disk without writing large files.
	return available - available/3
}

func loopMachineFixture(t *testing.T, l *Loop, machine, quota, owner string, diskPeak uint64) *admission.Budget {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" { t.Skip("native machine identity unsupported") }
	b := resourceBudget(l, 9)
	if err := b.BindSharedQuota(quota, owner); err != nil { t.Fatal(err) }
	if err := b.BindMachineCapacity(machine, owner); err != nil { t.Fatal(err) }
	diskRoot, err := filepath.EvalSymlinks(l.projectDir)
	if err != nil { t.Fatal(err) }
	b.BindResourceSource(l.projectDir, "process", func(string) (admission.ResourceClaim, error) {
		return admission.ResourceClaim{Source: "isolated-original-owner-fixture", MemoryBytes: 1, CPUs: 1, DiskBytes: diskPeak, DiskPaths: []string{diskRoot}, Policy: admission.CapacityPolicy{MemoryBytes: 1, CPUs: 10, DiskSafetyBytes: 1, RecoverySamples: 2}}, nil
	})
	for i := 3; i >= 0; i-- {
		b.Observe(admission.Sample{At: time.Now().Add(-time.Duration(i)*time.Second), Pressure: admission.Normal, TotalMemoryBytes: 100, CPUs: 10}, nil)
	}
	return b
}

func TestMachineAdmissionOriginalLoopTwoHostsQueueBeforeWorktreeAndDispatch(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil { t.Fatal(err) }
	rt1, rt2 := newMockRuntime(), newMockRuntime()
	first, second := newSteerTestLoop(t, rt1), newSteerTestLoop(t, rt2)
	diskPeak := contendedDiskPeak(t, root)
	b1 := loopMachineFixture(t, first, filepath.Join(root, "machine"), filepath.Join(root, "host-a"), "host-a-old", diskPeak)
	b2 := loopMachineFixture(t, second, filepath.Join(root, "machine"), filepath.Join(root, "host-b"), "host-b-current", diskPeak)
	reserved, err := b1.TryAcquireTask(first.projectDir, "first", "process")
	if err != nil { t.Fatal(err) }
	defer reserved.Release()
	stage := Stage{TaskKey: "execute", Skill: "execute", Status: StageStatusPending}
	order := Order{ID: "42", Status: OrderStatusActive, Stages: []Stage{stage}}
	candidate := dispatchCandidate{OrderID: order.ID, Stage: stage}
	before := resourceTree(t, second.projectDir)
	err = second.spawnCook(context.Background(), candidate, order, spawnOptions{})
	if !errors.Is(err, admission.ErrDeferred) || b2.Snapshot().MachineCapacity.Reason != "disk_reservation_capacity_insufficient" ||
		len(rt2.calls) != 0 || !reflect.DeepEqual(before, resourceTree(t, second.projectDir)) || b2.Snapshot().Active != 0 {
		t.Fatal("shared disk reservation did not queue before preparation or Dispatch", err)
	}
	reserved.Release()
	if err = second.spawnCook(context.Background(), candidate, order, spawnOptions{}); err != nil { t.Fatal("safe return did not continue original Loop", err) }
	if len(rt2.calls) != 1 || b2.Snapshot().Active != 1 { t.Fatal("original Loop did not dispatch exactly once") }
	session := second.cooks.activeCooksByOrder[order.ID].session
	rt2.sessions[0].complete("completed")
	<-session.Done()
	if s := b2.Snapshot(); s.Active != 0 || s.MachineCapacity.Leases != 1 { t.Fatal("Session.Done was promoted to disk/VM release proof") }
	second.watcherWG.Wait()
}

func TestMachineAdmissionOriginalLoopSampleAndSourceDeferrals(t *testing.T) {
	for _, condition := range []string{"missing", "stale", "unsupported", "source"} {
		t.Run(condition, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil { t.Fatal(err) }
			rt := newMockRuntime()
			l := newSteerTestLoop(t, rt)
			b := loopMachineFixture(t, l, filepath.Join(root, "machine"), filepath.Join(root, "quota"), "owner", 1)
			switch condition {
			case "missing": b.Observe(admission.Sample{}, nil)
			case "stale": b.Observe(admission.Sample{At: time.Now().Add(-admission.MaxSampleAge-time.Second), Pressure: admission.Normal, TotalMemoryBytes: 100, CPUs: 10}, nil)
			case "unsupported": b.Observe(admission.Sample{At: time.Now(), Unsupported: true}, nil)
			case "source": b.Unregister(l.projectDir); b.Register(l.projectDir)
			}
			stage := Stage{TaskKey: "execute", Skill: "execute", Status: StageStatusPending}
			before := resourceTree(t, l.projectDir)
			err = l.spawnCook(context.Background(), dispatchCandidate{OrderID: "42", Stage: stage}, Order{ID: "42"}, spawnOptions{})
			if !errors.Is(err, admission.ErrDeferred) || len(rt.calls) != 0 || !reflect.DeepEqual(before, resourceTree(t, l.projectDir)) || b.Snapshot().MachineCapacity.Leases != 0 { t.Fatal("unobserved worker crossed original preparation boundary", err) }
		})
	}
}

func TestMachineAdmissionAllOriginalSpawnEntrancesPreservePreparation(t *testing.T) {
	for _, entry := range []string{"cook", "schedule", "bootstrap", "planned"} {
		t.Run(entry, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil { t.Fatal(err) }
			rt := newMockRuntime()
			l := newSteerTestLoop(t, rt)
			b := loopMachineFixture(t, l, filepath.Join(root, "machine"), filepath.Join(root, "quota"), "owner", contendedDiskPeak(t, root))
			p, err := b.TryAcquireTask(l.projectDir, "occupied", "process")
			if err != nil { t.Fatal(err) }
			defer p.Release()
			stage := Stage{TaskKey: "execute", Skill: "execute", Status: StageStatusPending}
			order := Order{ID: "42", Status: OrderStatusActive, Stages: []Stage{stage}}
			candidate := dispatchCandidate{OrderID: order.ID, Stage: stage}
			l.schedulePromoted, l.lastPromotionError, l.scheduleDispatchDigest = true, "retain", "retain"
			before := resourceTree(t, l.projectDir)
			switch entry {
			case "cook": err = l.spawnCook(context.Background(), candidate, order, spawnOptions{})
			case "schedule": err = l.spawnSchedule(context.Background(), order, 0, "")
			case "bootstrap": err = l.spawnBootstrapIfNeeded(context.Background(), order)
			case "planned": err = l.spawnPlannedCandidates(context.Background(), []dispatchCandidate{candidate}, OrdersFile{Orders: []Order{order}}, mise.Brief{})
			}
			if entry == "planned" { if err != nil { t.Fatal(err) }
			} else if !errors.Is(err, admission.ErrDeferred) { t.Fatal("resource wait did not propagate", err) }
			if b.Snapshot().MachineCapacity.Reason != "disk_reservation_capacity_insufficient" {
				t.Fatal("dispatch did not preserve the original disk contention", b.Snapshot().MachineCapacity)
			}
			if len(rt.calls) != 0 || len(l.cooks.activeCooksByOrder) != 0 || l.bootstrapAttempts != 0 || !l.schedulePromoted || l.lastPromotionError != "retain" || l.scheduleDispatchDigest != "retain" || !reflect.DeepEqual(before, resourceTree(t, l.projectDir)) { t.Fatal("machine queue changed original preparation state") }
		})
	}
}

func TestMachineAdmissionNestedPermitReplacementAndUnknownRetention(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "replacement", true: "unknown"}[unknown], func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil { t.Fatal(err) }
			rt := newMockRuntime()
			l := newSteerTestLoop(t, rt)
			b := loopMachineFixture(t, l, filepath.Join(root, "machine"), filepath.Join(root, "quota"), "owner", 1)
			runtimeName := "process"
			if unknown { runtimeName = "sprites"; rt.dispatchErr = errors.New("fixture remote ACK lost"); l.deps.Runtimes[runtimeName] = rt }
			var session loopruntime.SessionHandle
			err = l.withAdmission(context.Background(), runtimeName, "original", func(ctx context.Context) error {
				return l.withAdmission(ctx, runtimeName, "original", func(ctx context.Context) error {
					var e error
					session, _, e = l.dispatchSession(ctx, loopruntime.DispatchRequest{Name: "original", Runtime: runtimeName})
					return e
				})
			})
			if unknown {
				if !errors.Is(err, errDispatchUnknown) || b.Snapshot().Active != 1 || b.Snapshot().MachineCapacity.Leases != 1 { t.Fatal("unknown lost original quota or resource owner", err) }
				if err = l.withAdmission(context.Background(), runtimeName, "next", func(context.Context) error { t.Fatal("unknown dispatch replayed"); return nil }); !errors.Is(err, errDispatchUnknown) { t.Fatal(err) }
				return
			}
			if err != nil { t.Fatal(err) }
			b.Observe(admission.Sample{At: time.Now(), Pressure: admission.Critical}, nil)
			err = l.withReplacement(&cookHandle{session: session}, func(context.Context) error { t.Fatal("replacement reached launch before resource retirement"); return nil })
			if !errors.Is(err, admission.ErrDeferred) || b.Snapshot().Active != 1 || b.Snapshot().MachineCapacity.Leases != 1 || len(rt.calls) != 1 { t.Fatal("replacement lost original resource custody", err) }
			rt.sessions[0].complete("completed"); <-session.Done()
			if b.Snapshot().Active != 0 || b.Snapshot().MachineCapacity.Leases != 1 { t.Fatal("Done claimed GC/VM retirement") }
		})
	}
}

func TestMachineAdmissionSteerDefersBeforeKillWithoutResourceRetirement(t *testing.T) {
	for _, pressure := range []admission.Pressure{admission.Normal, admission.Critical} {
		t.Run(string(pressure), func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil { t.Fatal(err) }
			rt := newMockRuntime()
			l := newControlTestLoop(t, &fakeWorktree{}, rt)
			b := loopMachineFixture(t, l, filepath.Join(root, "machine"), filepath.Join(root, "quota"), "owner", 1)
			stage := Stage{TaskKey: "execute", Skill: "execute", Status: StageStatusPending}
			order := Order{ID: "42", Status: OrderStatusActive, Stages: []Stage{stage}}
			if err = l.spawnCook(context.Background(), dispatchCandidate{OrderID: order.ID, Stage: stage}, order, spawnOptions{}); err != nil { t.Fatal(err) }
			old := l.cooks.activeCooksByOrder[order.ID]
			b.Observe(admission.Sample{At: time.Now(), Pressure: pressure, TotalMemoryBytes: 100, CPUs: 10}, nil)
			before := resourceTree(t, l.projectDir)
			for range 3 {
				err = l.steerRespawn(old, "retain original worker and direction")
				if !errors.Is(err, admission.ErrDeferred) || b.Snapshot().MachineCapacity.Reason != "replacement_resource_retirement_required" { t.Fatal("steer reused finite resources without retirement", err) }
				select { case <-old.session.Done(): t.Fatal("deferred steer killed original worker"); default: }
				if len(rt.calls) != 1 || rt.sessions[0].status != "running" || l.cooks.activeCooksByOrder[order.ID] != old ||
					!reflect.DeepEqual(before, resourceTree(t, l.projectDir)) { t.Fatal("deferred replacement changed files, Dispatch or original worker identity") }
				if s := b.Snapshot(); s.Active != 1 || s.SharedQuota.Allocations != 1 || s.MachineCapacity.Leases != 1 { t.Fatal("deferred replacement changed original quota or resource custody") }
			}
			rt.sessions[0].complete("completed")
			<-old.session.Done()
			l.watcherWG.Wait()
			if s := b.Snapshot(); s.Active != 0 || s.SharedQuota.Allocations != 0 || s.MachineCapacity.Leases != 1 { t.Fatal("worker Done released physical resources") }
		})
	}
}

func TestMachineAdmissionSwapRecoveryBeforeWorktreeAndDispatch(t *testing.T) {
	for _, pressure := range []string{"used", "rate"} {
		t.Run(pressure, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil { t.Fatal(err) }
			rt := newMockRuntime()
			l := newSteerTestLoop(t, rt)
			b := loopMachineFixture(t, l, filepath.Join(root, "machine"), filepath.Join(root, "quota"), "owner", 1)
			now := time.Now().Add(-10 * time.Second)
			var swapouts uint64
			for range 4 {
				now = now.Add(time.Second)
				sample := admission.Sample{At: now, Pressure: admission.Normal, TotalMemoryBytes: 100, CPUs: 10}
				if pressure == "used" { sample.SwapUsedBytes = 1 } else { swapouts++ }
				sample.Swapouts = swapouts
				b.Observe(sample, nil)
			}
			stage := Stage{TaskKey: "execute", Skill: "execute", Status: StageStatusPending}
			order := Order{ID: "42", Status: OrderStatusActive, Stages: []Stage{stage}}
			before := resourceTree(t, l.projectDir)
			for i := 1; i <= 2; i++ {
				now = now.Add(time.Second)
				b.Observe(admission.Sample{At: now, Pressure: admission.Normal, TotalMemoryBytes: 100, CPUs: 10, Swapouts: swapouts}, nil)
				err = l.spawnCook(context.Background(), dispatchCandidate{OrderID: order.ID, Stage: stage}, order, spawnOptions{})
				if i == 1 {
					if !errors.Is(err, admission.ErrDeferred) || len(rt.calls) != 0 || !reflect.DeepEqual(before, resourceTree(t, l.projectDir)) { t.Fatal("Swap recovery crossed preparation before healthy sequence", err) }
					if s := b.Snapshot(); s.Active != 0 || s.MachineCapacity.Leases != 0 { t.Fatal("recovering pressure acquired resource lease") }
				} else if err != nil || len(rt.calls) != 1 { t.Fatal("healthy sequence did not resume original Loop", err) }
			}
			session := l.cooks.activeCooksByOrder[order.ID].session
			rt.sessions[0].complete("completed")
			<-session.Done()
			l.watcherWG.Wait()
		})
	}
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
			budget := resourceBudget(l, 1)
			occupied, err := budget.TryAcquire(l.projectDir)
			if err != nil {
				t.Fatal(err)
			}
			defer occupied.Release()
			stage := Stage{TaskKey: "execute", Skill: "execute", Status: StageStatusPending}
			order := Order{ID: "42", Status: OrderStatusActive, Stages: []Stage{stage}}
			cand := dispatchCandidate{OrderID: order.ID, StageIndex: 0, Stage: stage}
			l.schedulePromoted = true
			l.lastPromotionError = "retain"
			l.scheduleDispatchDigest = "retain"
			before := resourceTree(t, l.projectDir)
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
			err := l.withAdmission(context.Background(), runtimeName, "", func(ctx context.Context) error {
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

func TestResourceSteerCapOneTransfersUnderPressureAndCustodyRefusesBeforeKill(t *testing.T) {
	for _, condition := range []string{"normal", "critical", "unresolved-custody"} {
		t.Run(condition, func(t *testing.T) {
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
			if condition != "normal" {
				b.Observe(admission.Sample{At: time.Now(), Pressure: admission.Critical}, nil)
			} else {
				b.Observe(admission.Sample{At: time.Now(), Pressure: admission.Normal}, nil)
			}
			if condition == "unresolved-custody" {
				b.SetUnresolvedResidents(1)
			}
			err := l.steerRespawn(old, "keep exact original direction")
			if condition == "unresolved-custody" {
				if !errors.Is(err, admission.ErrDeferred) || rt.sessions[0].status != "running" || l.cooks.activeCooksByOrder["42"] != old || !reflect.DeepEqual(before, resourceTree(t, l.projectDir)) {
					t.Fatal("unresolved custody steer changed old owner")
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
	if !errors.Is(err, admission.ErrDeferred) || len(rt.calls) != 0 || b.Snapshot().Active != 0 || b.Snapshot().Limit != 1 || !reflect.DeepEqual(before, resourceTree(t, l.projectDir)) {
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

func TestResourceControlQueueRetainsCustodyWaitAndUnknownDispatch(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "custody_then_resume", true: "unknown_no_replay"}[unknown], func(t *testing.T) {
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
				b.Observe(admission.Sample{At: time.Now(), Pressure: admission.Critical}, nil)
				b.SetUnresolvedResidents(1)
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
				b.SetUnresolvedResidents(0)
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
			b.Observe(admission.Sample{At: time.Now(), Pressure: admission.Critical}, nil)
			b.SetUnresolvedResidents(1)
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
				b.SetUnresolvedResidents(0)
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
