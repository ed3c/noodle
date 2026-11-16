package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
)

func bindCompletedResourceFixture(t *testing.T, l *Loop, cook *cookHandle, machine, quota, owner, runtime string) *admission.Budget {
	t.Helper()
	b := loopMachineFixture(t, l, machine, quota, owner)
	// The same original profile handler supplies this runtime's finite peak.
	if runtime != "process" {
		root, err := canonicalDirectory(l.projectDir)
		if err != nil {
			t.Fatal(err)
		}
		b.Unregister(l.deps.AdmissionContext)
		b.Register(l.deps.AdmissionContext)
		b.BindResourceSource(l.deps.AdmissionContext, runtime, func(string) (admission.ResourceClaim, error) {
			return admission.ResourceClaim{Source: "isolated-original-owner-fixture", MemoryBytes: 1, CPUs: 1, DiskBytes: 1, DiskPaths: []string{root}, Policy: admission.CapacityPolicy{MemoryBytes: 1, CPUs: 10, DiskSafetyBytes: 1, RecoverySamples: 2}}, nil
		})
		settlementHealthy(b)
	}
	permit, err := b.TryAcquireTask(l.deps.AdmissionContext, cook.orderID, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err := permit.StartResourceEffects(); err != nil {
		t.Fatal(err)
	}
	stage := l.canonical.Orders[cook.orderID].Stages[0]
	attempt := stage.Attempts[len(stage.Attempts)-1]
	project, err := canonicalDirectory(l.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := canonicalDirectory(cook.worktreePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := permit.BindSession(admission.MachineSessionBinding{ContextID: l.deps.AdmissionContext,
		ProjectDir: project, OrderID: cook.orderID, StageIndex: 0, AttemptID: attempt.AttemptID,
		SessionID: attempt.SessionID, Runtime: runtime, WorktreePath: worktree}); err != nil {
		t.Fatal(err)
	}
	permit.Release() // Quota return alone deliberately leaves the physical lease.
	return b
}

func settlementHealthy(b *admission.Budget) {
	for i := 3; i >= 0; i-- {
		b.Observe(admission.Sample{At: time.Now().Add(-time.Duration(i) * time.Second), Pressure: admission.Normal, TotalMemoryBytes: 100, CPUs: 10}, nil)
	}
}

func approveSettlementFixture(t *testing.T, l *Loop, cmd ControlCommand) {
	t.Helper()
	cmd.Action = "retain-local-task"
	if err := l.dispatchControlCommand(cmd); err != nil {
		t.Fatal(err)
	}
	_, ack, _ := l.controlPaths()
	if err := appendAcks(ack, []ControlAck{{ID: cmd.ID, Action: cmd.Action, Status: "ok"}}); err != nil {
		t.Fatal(err)
	}
}

type settlementCreatingWorktree struct {
	WorktreeManager
	path func(string) string
}

func (w settlementCreatingWorktree) Create(name string, options ...worktree.CreateOpts) error {
	if err := w.WorktreeManager.Create(name, options...); err != nil {
		return err
	}
	return os.MkdirAll(w.path(name), 0700)
}

func TestMachineSettlementOriginalCleanupContinuesQueuedOtherHost(t *testing.T) {
	root, err := canonicalDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, cook, cmd := localTaskReviewFixture(t)
	localTaskRealWorktree(t, first)
	completedCheckoutLoopPolicy(t, first, cook)
	machine := filepath.Join(root, "machine")
	b1 := bindCompletedResourceFixture(t, first, cook, machine, filepath.Join(root, "host-a"), "original-host-a", "process")
	rt := newMockRuntime()
	second := newSteerTestLoop(t, rt)
	second.deps.Worktree = settlementCreatingWorktree{WorktreeManager: second.deps.Worktree, path: second.worktreePath}
	b2 := loopMachineFixture(t, second, machine, filepath.Join(root, "host-b"), "original-host-b")
	stage := Stage{TaskKey: "execute", Skill: "execute", Status: StageStatusPending}
	order := Order{ID: "42", Status: OrderStatusActive, Stages: []Stage{stage}}
	candidate := dispatchCandidate{OrderID: order.ID, Stage: stage}
	before := resourceTree(t, second.projectDir)
	if err := second.spawnCook(context.Background(), candidate, order, spawnOptions{}); !errors.Is(err, admission.ErrDeferred) || len(rt.calls) != 0 || !reflect.DeepEqual(before, resourceTree(t, second.projectDir)) {
		t.Fatal("resource queue crossed its original dispatch boundary", err)
	}
	if err := first.settleCompletedMachineSessions(false); err == nil || b1.Snapshot().MachineCapacity.Leases != 1 {
		t.Fatal("unaccepted candidate returned physical resources", err)
	}
	approveSettlementFixture(t, first, cmd)
	if err := first.cleanupCompleted(false); err != nil {
		t.Fatal(err)
	}
	if err := first.settleCompletedMachineSessions(false); err == nil || b1.Snapshot().MachineCapacity.Leases != 1 {
		t.Fatal("readiness without restoration/removal returned resources", err)
	}
	if err := first.cleanupCompleted(true); err != nil {
		t.Fatal("original applying cleanup did not settle its own lease", err)
	}
	if _, err := os.Stat(cook.worktreePath); !os.IsNotExist(err) || b1.Snapshot().MachineCapacity.Leases != 0 {
		t.Fatal("lease return preceded actual checkout reclamation", err)
	}
	if err := first.settleCompletedMachineSessions(false); err != nil {
		t.Fatal("settled readback replayed work", err)
	}
	settlementHealthy(b2)
	if err := second.spawnCook(context.Background(), candidate, order, spawnOptions{}); err != nil || len(rt.calls) != 1 {
		t.Fatal("original queued dispatch did not resume exactly once", err)
	}
	for _, b := range []*admission.Budget{b1, b2} {
		if s := b.Snapshot(); s.Maximum != 9 || s.SharedQuota.Maximum != 9 {
			t.Fatal("settlement changed per-Host nine", s)
		}
	}
	session := second.cooks.activeCooksByOrder[order.ID].session
	project, err := canonicalDirectory(second.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	leases, err := b2.MachineSessionLeases(second.deps.AdmissionContext, project)
	if err != nil || len(leases) != 1 || leases[0].Binding == nil || leases[0].Binding.SessionID != session.ID() {
		t.Fatal("original dispatch did not persist the native session binding", leases, err)
	}
	rt.sessions[0].complete("completed")
	<-session.Done()
	second.watcherWG.Wait()
}

type settlementRuntime struct {
	*readbackRuntime
	readCleanup func(loopruntime.CompletedSessionBinding, string, loopruntime.SessionStatus) (loopruntime.CleanupSessionReadback, error)
	retire      func(loopruntime.CompletedSessionBinding, string, loopruntime.SessionStatus) (loopruntime.CleanupSessionRetirement, error)
}

func (r *settlementRuntime) ReadCleanupSession(_ context.Context, b loopruntime.CompletedSessionBinding, project string, expected loopruntime.SessionStatus) (loopruntime.CleanupSessionReadback, error) {
	return r.readCleanup(b, project, expected)
}
func (r *settlementRuntime) RetireCleanupSession(_ context.Context, b loopruntime.CompletedSessionBinding, project string, expected loopruntime.SessionStatus) (loopruntime.CleanupSessionRetirement, error) {
	return r.retire(b, project, expected)
}

func TestMachineSettlementOriginalVMUnknownAndStartupReadback(t *testing.T) {
	for _, kind := range []string{"unimplemented", "lost-ack", "pending-disk", "foreign-session", "missing-source", "delayed-disk"} {
		t.Run(kind, func(t *testing.T) {
			root, err := canonicalDirectory(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			l, cook, cmd, original := openShellLocalTaskFixture(t, "codex")
			localTaskRealWorktree(t, l)
			completedCheckoutLoopPolicy(t, l, cook)
			machine, quota := filepath.Join(root, "machine"), filepath.Join(root, "host-quota")
			b := bindCompletedResourceFixture(t, l, cook, machine, quota, "original-host", "openshell")
			custody := filepath.Join(l.runtimeDir, "original-native-custody.json")
			recoveryWriteJSON(t, custody, map[string]any{"state": "stopped"})
			retired, pending, retireCalls, deleteCalls := false, false, 0, 0
			disk := filepath.Join(root, "native-disk", "original-native-uuid")
			if err := os.MkdirAll(disk, 0700); err != nil {
				t.Fatal(err)
			}
			rt := &settlementRuntime{readbackRuntime: original}
			rt.readCleanup = func(binding loopruntime.CompletedSessionBinding, project string, status loopruntime.SessionStatus) (loopruntime.CleanupSessionReadback, error) {
				if binding.OrderID != cook.orderID || binding.SessionID != cook.session.ID() || binding.Provider != "codex" || binding.Model != cook.stage.Model || status != loopruntime.StatusCompleted {
					t.Fatal("original VM reader binding differs", binding, status)
				}
				ref, err := loopruntime.ReadSessionReadbackFile(custody, nil)
				if err != nil {
					return loopruntime.CleanupSessionReadback{}, err
				}
				e := loopruntime.CompletedSessionReadback{NativeAttemptID: "original-native-attempt", CandidateHead: "original-head", NativeExecExitCode: 0, NativePhase: "Stopped", StopAcknowledged: true, Sources: map[string]loopruntime.SessionReadbackFileRef{"native_custody": ref}}
				if retired || pending {
					e.NativePhase = "Retired"
					e.Retirement = &loopruntime.CleanupSessionRetirement{OutcomeStatus: status, SessionID: binding.SessionID, NativeAttemptID: e.NativeAttemptID, SandboxID: "original-native-uuid", State: loopruntime.CleanupSessionRetired, NativeDeletionOutcome: "unknown"}
					if !retired {
						e.NativePhase, e.Retirement.State, e.Retirement.NativeDeletionOutcome = "Retiring", "pending", "DELETION_OUTCOME_COMPLETED"
					}
					if kind == "foreign-session" {
						e.Retirement.SessionID = "foreign-session"
					}
					if kind == "missing-source" {
						delete(e.Sources, "native_custody")
					}
				}
				return loopruntime.CleanupSessionReadback{OutcomeStatus: status, PublishedHead: "original-head", Execution: e}, nil
			}
			rt.retire = func(binding loopruntime.CompletedSessionBinding, project string, expected loopruntime.SessionStatus) (loopruntime.CleanupSessionRetirement, error) {
				retireCalls++
				if kind == "unimplemented" {
					return loopruntime.CleanupSessionRetirement{}, errors.New("native stopped deletion UNIMPLEMENTED")
				}
				if kind == "pending-disk" || kind == "delayed-disk" {
					if !pending {
						pending = true
						deleteCalls++
					}
					if _, err := os.Stat(disk); err == nil {
						return loopruntime.CleanupSessionRetirement{State: "pending"}, errors.New("original VM disk remains")
					}
					retired = true
					recoveryWriteJSON(t, custody, map[string]any{"state": "retired", "uuid": "original-native-uuid"})
					return loopruntime.CleanupSessionRetirement{OutcomeStatus: expected, SessionID: binding.SessionID, NativeAttemptID: "original-native-attempt", SandboxID: "original-native-uuid", State: loopruntime.CleanupSessionRetired, NativeDeletionOutcome: "DELETION_OUTCOME_COMPLETED"}, nil
				}
				retired = true
				recoveryWriteJSON(t, custody, map[string]any{"state": "retired", "uuid": "original-native-uuid"})
				return loopruntime.CleanupSessionRetirement{}, errors.New("lost original native deletion acknowledgement")
			}
			l.deps.Runtimes["openshell"] = rt
			approveSettlementFixture(t, l, cmd)
			if err := l.cleanupOwnedCheckouts(true); err != nil {
				t.Fatal("original checkout preservation/removal", err)
			}
			if err := l.settleCompletedMachineSessions(false); err == nil || retireCalls != 0 || b.Snapshot().MachineCapacity.Leases != 1 {
				t.Fatal("startup initiated deletion or returned an unretired lease", retireCalls, err)
			}
			if kind == "delayed-disk" {
				if err := l.cleanupCompleted(true); err == nil || retireCalls != 1 || b.Snapshot().MachineCapacity.Leases != 1 {
					t.Fatal("ACK with existing disk was treated as release", retireCalls, err)
				}
				if err := l.cleanupCompleted(false); err != nil {
					t.Fatal("known Retiring intent could not reach original readback", err)
				}
				if err := os.RemoveAll(disk); err != nil {
					t.Fatal(err)
				}
				if err := l.cleanupCompleted(true); err != nil || retireCalls != 2 || deleteCalls != 1 || b.Snapshot().MachineCapacity.Leases != 0 {
					t.Fatal("original false→true retirement did not settle without replay", retireCalls, deleteCalls, err)
				}
				return
			}
			if err := l.settleCompletedMachineSessions(true); err == nil || retireCalls != 1 || b.Snapshot().MachineCapacity.Leases != 1 {
				t.Fatal("unknown native result returned resources or replayed", retireCalls, err)
			}
			// The original process/context restarts with the same generation and
			// source. It reads the durable nonce, never reconstructs an allocation.
			b = loopMachineFixture(t, l, machine, quota, "original-host")
			b.Unregister(l.deps.AdmissionContext)
			b.Register(l.deps.AdmissionContext)
			b.BindResourceSource(l.deps.AdmissionContext, "openshell", func(string) (admission.ResourceClaim, error) {
				project, err := canonicalDirectory(l.projectDir)
				return admission.ResourceClaim{Source: "isolated-original-owner-fixture", MemoryBytes: 1, CPUs: 1, DiskBytes: 1, DiskPaths: []string{project}, Policy: admission.CapacityPolicy{MemoryBytes: 1, CPUs: 10, DiskSafetyBytes: 1, RecoverySamples: 2}}, err
			})
			err = l.settleCompletedMachineSessions(false)
			if kind == "lost-ack" {
				if err != nil || b.Snapshot().MachineCapacity.Leases != 0 {
					t.Fatal("original retired readback failed to settle the same nonce", err)
				}
			} else if err == nil || b.Snapshot().MachineCapacity.Leases != 1 {
				t.Fatal("unknown/foreign retirement released original resources", err)
			}
			if retireCalls != 1 {
				t.Fatal("startup reissued native deletion", retireCalls)
			}
		})
	}
}
