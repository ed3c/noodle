package loop

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/orderx"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
	"github.com/poteto/noodle/worktree/archive"
)

type archiveReclaimRuntimeFixture struct {
	*readbackRuntime
	checks, retireCalls int
	startupRead         func(string, string, string) (bool, error)
	release             func() error
	observeDrain        func()
}

func (f *archiveReclaimRuntimeFixture) ArchiveRecoveryFrozenSession(sessionID, orderID, attemptID string) (bool, error) {
	return f.startupRead(sessionID, orderID, attemptID)
}

func (f *archiveReclaimRuntimeFixture) VerifyArchiveRecoveryVMForReclamation(context.Context, loopruntime.SessionReadbackFileRef) error {
	f.checks++
	return nil
}

func (f *archiveReclaimRuntimeFixture) RetireArchiveRecoveryVM(_ context.Context, _ loopruntime.SessionReadbackFileRef, proof archive.CheckoutReclamationEvidence) (loopruntime.ArchiveRecoveryVMRetirementReadback, error) {
	f.retireCalls++
	if f.observeDrain != nil {
		f.observeDrain()
	}
	if proof.Packet.Path == "" || proof.Removal.Path == "" || proof.VerificationRelease.Path == "" {
		return loopruntime.ArchiveRecoveryVMRetirementReadback{}, errors.New("test attempted runtime retirement without a signed archive restoration")
	}
	return loopruntime.ArchiveRecoveryVMRetirementReadback{}, errors.New("original native disk deletion acknowledgement is unresolved")
}

func (f *archiveReclaimRuntimeFixture) ReleaseArchiveRecoveryHandle(context.Context, loopruntime.SessionReadbackFileRef) error {
	if f.release != nil {
		return f.release()
	}
	return errors.New("test stops before native retirement")
}

func TestArchiveRecoveryReclamationGuardsAndReplaysOnlyNativeReadback(t *testing.T) {
	l, cook, _, native := openShellLocalTaskFixture(t, "codex")
	localTaskRealWorktree(t, l)
	completedCheckoutLoopPolicy(t, l, cook)
	l.deps.CleanupCompleted = true
	l.state = StatePaused
	root, err := canonicalDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	machine := filepath.Join(root, "archive-machine")
	budget := bindCompletedResourceFixture(t, l, cook, machine, filepath.Join(root, "archive-quota"), "original-archive-host", "openshell", 1)
	project, err := canonicalDirectory(l.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	leases, err := budget.MachineSessionLeases(l.deps.AdmissionContext, project)
	if err != nil || len(leases) != 1 {
		t.Fatal(leases, err)
	}
	runtime := &archiveReclaimRuntimeFixture{readbackRuntime: native}
	l.deps.Runtimes["openshell"] = runtime
	app := l.deps.Worktree.(*worktree.App)
	checkout, err := app.ReadCheckoutIdentity(leases[0].Binding.WorktreePath)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := recoveryGit(cook.worktreePath, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	_, ack, _ := l.controlPaths()
	if err := appendAcks(ack, []ControlAck{{ID: "original-archive-pause", Action: "pause", Status: "ok", DispatchState: StatePaused}}); err != nil {
		t.Fatal(err)
	}
	disposition := filepath.Join(l.runtimeDir, "sessions", leases[0].Binding.SessionID, "archive-recovery-cleanup-disposition.json")
	recoveryWriteJSON(t, disposition, map[string]any{"original_return": "UNKNOWN"})
	ref, err := loopruntime.ReadSessionReadbackFile(disposition, nil)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := loopruntime.ReadSessionReadbackFile(ack, nil)
	if err != nil {
		t.Fatal(err)
	}
	input := ArchiveRecoveryReclamation{Disposition: ref, Lease: leases[0], Checkout: checkout, Head: checkout.Head, Tree: tree,
		Hold: loopruntime.ArchiveRecoveryDispatchHold{Acknowledgements: paused, PauseCommandID: "original-archive-pause"}}
	runtime.observeDrain = func() {
		if _, err := ReadArchiveRecoveryWorkerDrain(ref, leases[0]); err != nil {
			t.Fatalf("native retirement started without the durable original worker drain: %v", err)
		}
	}

	changed := input
	changed.Checkout.Incarnation = strings.Repeat("b", 64)
	if _, err = l.reclaimArchiveRecovery(t.Context(), changed); err == nil || !strings.Contains(err.Error(), "incarnation") {
		t.Fatalf("changed checkout may be reclaimed: %v", err)
	}
	if _, err = os.Stat(checkout.Path); err != nil || runtime.retireCalls != 0 {
		t.Fatal("incarnation refusal changed resources", err)
	}

	ledgerPath := filepath.Join(machine, "capacity.json")
	ledger := recoveryRead(t, ledgerPath)
	var ledgerValue map[string]json.RawMessage
	if err = json.Unmarshal(ledger, &ledgerValue); err != nil {
		t.Fatal(err)
	}
	var ledgerLeases map[string]json.RawMessage
	if err = json.Unmarshal(ledgerValue["leases"], &ledgerLeases); err != nil {
		t.Fatal(err)
	}
	ledgerLeases["fixture-duplicate-nonce"] = ledgerLeases[leases[0].ID]
	ledgerValue["leases"], err = json.Marshal(ledgerLeases)
	if err != nil {
		t.Fatal(err)
	}
	recoveryWriteJSON(t, ledgerPath, ledgerValue)
	if _, err = l.reclaimArchiveRecovery(t.Context(), input); err == nil ||
		!strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous lease may reclaim checkout: %v", err)
	}
	if _, err = os.Stat(checkout.Path); err != nil || runtime.retireCalls != 0 {
		t.Fatal("lease refusal changed resources", err)
	}
	if err = os.WriteFile(ledgerPath, ledger, 0600); err != nil {
		t.Fatal(err)
	}

	before, _ := json.Marshal(l.canonical)
	first, err := l.reclaimArchiveRecovery(t.Context(), input)
	if err == nil || first.Checkout == nil || first.Checkout.Identity != checkout ||
		first.WorkerDrain == nil || first.OriginalReturn != "UNKNOWN" || first.Status != "archive_recovery_reclamation_waiting" {
		t.Fatalf("checkout proof or pending native readback differs: %+v %v", first, err)
	}
	if _, err = os.Stat(checkout.Path); !os.IsNotExist(err) {
		t.Fatal("original checkout was not removed after archive verification", err)
	}
	wrongLease := leases[0]
	wrongLease.ID = "foreign-nonce"
	if _, err = ReadArchiveRecoveryWorkerDrain(ref, wrongLease); err == nil {
		t.Fatal("a foreign lease inherited the original worker drain")
	}
	drainPath := first.WorkerDrain.Path
	drainBytes := recoveryRead(t, drainPath)
	withExtra := strings.Replace(string(drainBytes), "\n}\n", ",\n  \"next\": null\n}\n", 1)
	if err = os.WriteFile(drainPath, []byte(withExtra), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadArchiveRecoveryWorkerDrain(ref, leases[0]); err == nil {
		t.Fatal("a worker drain with extra continuation fields was accepted")
	}
	recoveryWriteJSON(t, drainPath, archiveRecoveryWorkerDrain(ref, wrongLease))
	if _, err = l.reclaimArchiveRecovery(t.Context(), input); err == nil || runtime.retireCalls != 1 {
		t.Fatalf("altered worker drain repeated native retirement: %v", err)
	}
	if err = os.WriteFile(drainPath, drainBytes, 0600); err != nil {
		t.Fatal(err)
	}
	second, err := l.reclaimArchiveRecovery(t.Context(), input)
	if err == nil || second.Checkout == nil || *first.Checkout != *second.Checkout ||
		second.WorkerDrain == nil || *second.WorkerDrain != *first.WorkerDrain ||
		runtime.checks != 2 || runtime.retireCalls != 2 {
		t.Fatalf("replay dropped the original checkout proof or replayed the checkout stage: %+v %v %v", second, err, runtime)
	}
	// Before worker drain receipts existed, the owner could record signed
	// checkout removal and reach native retirement. Keep that replay valid.
	if err = os.Remove(drainPath); err != nil {
		t.Fatal(err)
	}
	runtime.observeDrain = nil
	legacy, err := l.reclaimArchiveRecovery(t.Context(), input)
	if err == nil || legacy.WorkerDrain != nil || legacy.Checkout == nil ||
		*legacy.Checkout != *first.Checkout || runtime.checks != 2 || runtime.retireCalls != 3 {
		t.Fatalf("missing historical drain blocked signed checkout replay or invented evidence: %+v %v", legacy, err)
	}
	if _, err = os.Stat(drainPath); !os.IsNotExist(err) {
		t.Fatalf("legacy replay recreated a pre-checkout receipt: %v", err)
	}
	after, _ := json.Marshal(l.canonical)
	if string(before) != string(after) {
		t.Fatal("resource reclamation changed historical worker disposition")
	}
	retained, err := budget.MachineSessionLeases(l.deps.AdmissionContext, project)
	if err != nil || len(retained) != 1 || retained[0].ID != leases[0].ID {
		t.Fatal("unknown native retirement released the original lease", retained, err)
	}
}

func TestArchiveRecoveryTerminalProofReadsExactSealedSettlement(t *testing.T) {
	dir, err := canonicalDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, value any) loopruntime.SessionReadbackFileRef {
		t.Helper()
		path := filepath.Join(dir, name)
		recoveryWriteJSON(t, path, value)
		ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	binding := admission.MachineSessionBinding{ContextID: "original-context", ProjectDir: dir, OrderID: "original-order",
		AttemptID: "original-attempt", SessionID: "original-session", Runtime: "openshell", WorktreePath: filepath.Join(dir, "checkout")}
	lease := admission.MachineSessionLease{ID: "original-nonce", Owner: "original-owner", Source: "original-source",
		ContextID: binding.ContextID, State: "held", Binding: &binding}
	identity := archive.CheckoutIdentity{Path: binding.WorktreePath, Incarnation: "checkout-incarnation"}
	stop := write("original-stop.json", "original Stop observed")
	landed := write("original-landing.json", "exact candidate landed")
	disposition := write("archive-recovery-cleanup-disposition.json", map[string]any{
		"schema": 1, "kind": "archive_recovery_cleanup_disposition", "original_return": "UNKNOWN",
		"lease": lease, "checkout": identity, "landing_proof": landed, "stop": stop,
		"native_attempt_id": "native-original", "sandbox_id": "vm-original",
	})
	custody := write("custody.json", "old native custody with verified disk absence")
	sources := map[string]admission.MachineSettlementFile{
		"archive_recovery_disposition":  admission.MachineSettlementFile(disposition),
		"archive_recovery_landing":      admission.MachineSettlementFile(landed),
		"archive_recovery_stop":         admission.MachineSettlementFile(stop),
		"native_custody":                admission.MachineSettlementFile(custody),
		"checkout_packet":               admission.MachineSettlementFile(write("packet.json", "signed packet")),
		"checkout_removal":              admission.MachineSettlementFile(write("removed.json", "signed removal")),
		"checkout_verification_release": admission.MachineSettlementFile(write("release.json", "signed restoration release")),
	}
	fact := admission.MachineReclamation{Binding: binding, CheckoutOperation: "archive-recovery-checkout/" + disposition.SHA256,
		Sources: sources, RuntimeState: loopruntime.CleanupSessionRetired, NativeAttemptID: "native-original",
		SandboxID: "vm-original", NativeDeletionOutcome: "DELETION_OUTCOME_COMPLETED"}
	scope := ArchiveRecoveryReclamation{Disposition: disposition, Lease: lease, Checkout: identity}
	result := ArchiveRecoveryReclamationResult{Status: "archive_recovery_reclamation_waiting", Effects: []string{}}
	if err = sealArchiveRecoveryReclamation(&result, scope, nil); err == nil || result.Reclamation != nil {
		t.Fatalf("terminal was invented without an original ledger settlement: %+v %v", result, err)
	}
	if err = prepareArchiveRecoveryReclamation(disposition, lease, &fact); err != nil {
		t.Fatal("verified receipt could not be made durable before machine settlement", err)
	}
	preparedPath := filepath.Join(dir, ArchiveRecoveryReclamationFile)
	prepared, err := loopruntime.ReadSessionReadbackFile(preparedPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ReadArchiveRecoveryReclamation(disposition, lease, nil); err == nil {
		t.Fatal("prepared receipt independently authorized terminal before the ledger confirmed")
	}
	fresher := write("custody.json", "the same retired native row and unrelated changes before commit")
	fact.Sources["native_custody"] = admission.MachineSettlementFile(fresher)
	if err = prepareArchiveRecoveryReclamation(disposition, lease, &fact); err != nil ||
		fact.Sources[archiveRecoveryReclamationSource] != admission.MachineSettlementFile(prepared) {
		t.Fatalf("retry from a same-path custody update did not retain prepared receipt: %v", err)
	}
	if err = sealArchiveRecoveryReclamation(&result, scope, &fact); err != nil ||
		result.Status != "archive_recovery_reclaimed" || result.Reclamation == nil || *result.Reclamation != prepared {
		t.Fatalf("exact original settlement was not persisted: %+v %v", result, err)
	}
	ref := *result.Reclamation
	write("custody.json", "later unrelated native custody updates")
	replayed := ArchiveRecoveryReclamationResult{Effects: []string{}}
	if err = sealArchiveRecoveryReclamation(&replayed, scope, &fact); err != nil ||
		replayed.Reclamation == nil || *replayed.Reclamation != ref || len(replayed.Effects) != 0 || replayed.VM != nil {
		t.Fatalf("settled replay changed receipt or emitted another effect: %+v %v", replayed, err)
	}
	foreign := fact
	foreign.SandboxID = "foreign-vm"
	if _, err = ReadArchiveRecoveryReclamation(disposition, lease, &foreign); err == nil {
		t.Fatal("foreign VM inherited the original terminal result")
	}
	write("release.json", "changed after settlement")
	if _, err = ReadArchiveRecoveryReclamation(disposition, lease, &fact); err == nil {
		t.Fatal("changed restoration evidence remained terminal")
	}
	write("release.json", "signed restoration release")
	write(ArchiveRecoveryReclamationFile, map[string]any{"kind": "archive_recovery_reclamation", "original_return": "UNKNOWN"})
	if _, err = ReadArchiveRecoveryReclamation(disposition, lease, &fact); err == nil ||
		!strings.Contains(err.Error(), "not sealed") {
		t.Fatalf("changed terminal receipt inherited the original ledger seal: %v", err)
	}
}

func TestArchiveRecoveryDrainPreservesHistoryAndRequiresQuotaReturn(t *testing.T) {
	l, cook, _, native := openShellLocalTaskFixture(t, "codex")
	localTaskRealWorktree(t, l)
	completedCheckoutLoopPolicy(t, l, cook)
	l.deps.CleanupCompleted = true
	l.state = StatePaused
	root, err := canonicalDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project, err := canonicalDirectory(l.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	checkout, err := canonicalDirectory(cook.worktreePath)
	if err != nil {
		t.Fatal(err)
	}
	quota := filepath.Join(root, "quota")
	budget := loopMachineFixture(t, l, filepath.Join(root, "machine"), quota, "original-host", 1)
	budget.Unregister(l.deps.AdmissionContext)
	budget.Register(l.deps.AdmissionContext)
	budget.BindResourceSource(l.deps.AdmissionContext, "openshell", func(string) (admission.ResourceClaim, error) {
		return admission.ResourceClaim{Source: "isolated-original-owner-fixture", MemoryBytes: 1, CPUs: 1,
			DiskBytes: 1, DiskPaths: []string{project}, Policy: admission.CapacityPolicy{MemoryBytes: 1, CPUs: 10,
				DiskSafetyBytes: 1, RecoverySamples: 2}}, nil
	})
	settlementHealthy(budget)
	permit, err := budget.TryAcquireTask(l.deps.AdmissionContext, cook.orderID, "openshell")
	if err != nil {
		t.Fatal(err)
	}
	defer permit.Release()
	if err = permit.StartResourceEffects(); err != nil {
		t.Fatal(err)
	}
	stage := l.canonical.Orders[cook.orderID].Stages[0]
	attempt := stage.Attempts[len(stage.Attempts)-1]
	bound := admission.MachineSessionBinding{ContextID: l.deps.AdmissionContext, ProjectDir: project,
		OrderID: cook.orderID, AttemptID: attempt.AttemptID, SessionID: attempt.SessionID, Runtime: "openshell", WorktreePath: checkout}
	if err = permit.BindSession(bound); err != nil {
		t.Fatal(err)
	}
	leases, err := budget.MachineSessionLeases(bound.ContextID, project)
	if err != nil || len(leases) != 1 {
		t.Fatal(leases, err)
	}
	lease := leases[0]
	dispositionFile := filepath.Join(root, "original.json")
	recoveryWriteJSON(t, dispositionFile, map[string]any{"original_return": "UNKNOWN"})
	dispositionRef, err := loopruntime.ReadSessionReadbackFile(dispositionFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	disposition := admission.MachineSettlementFile(dispositionRef)
	raw := &mockSession{id: attempt.SessionID, status: "failed", done: make(chan struct{})}
	cook.session = l.retainDispatched(context.WithValue(t.Context(), admissionKey{}, &launchAdmission{permit: permit}), raw)
	cook.dispatchedRuntime = bound.Runtime
	cook.generation = 42
	l.cooks.activeCooksByOrder[cook.orderID] = cook
	l.trackCookStarted(cook)
	got, owned, err := l.archiveRecoveryOriginalCook(bound)
	if err != nil || got != cook || owned != permit {
		t.Fatalf("exact retained actor was not identified: %v %v %v", got, owned, err)
	}
	if _, err = budget.ReadArchiveRecoverySettlementForCleanup(lease, disposition, owned); err != nil {
		t.Fatal("matching inert permit must retain its own archive-only preflight", err)
	}
	if _, err = budget.ReadArchiveRecoverySettlementForCleanup(lease, disposition, &admission.Permit{}); err == nil {
		t.Fatal("foreign permit inherited original reclamation")
	}
	if _, err = budget.ReadArchiveRecoverySettlement(lease, disposition); err == nil {
		t.Fatal("ordinary settlement bypassed the retained permit")
	}
	original, _ := json.Marshal(l.canonical)
	history := len(l.recentHistory)
	pending := len(l.cooks.pendingReview)
	worktreeOwner := l.deps.Worktree.(*worktree.App)
	checkoutIdentity, err := worktreeOwner.ReadCheckoutIdentity(checkout)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := recoveryGit(checkout, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	_, ack, _ := l.controlPaths()
	if err = appendAcks(ack, []ControlAck{{ID: "original-pause", Action: "pause", Status: "ok", DispatchState: StatePaused}}); err != nil {
		t.Fatal(err)
	}
	ackRef, err := loopruntime.ReadSessionReadbackFile(ack, nil)
	if err != nil {
		t.Fatal(err)
	}
	var lock *lockfile.Lock
	runtime := &archiveReclaimRuntimeFixture{readbackRuntime: native, release: func() error {
		var err error
		lock, err = lockfile.TryLock(filepath.Join(quota, "quota.lock"))
		if err != nil {
			return err
		}
		raw.complete("failed")
		return nil
	}}
	l.deps.Runtimes["openshell"] = runtime

	// A joined wrapper alone is insufficient if the original quota ledger
	// could not record the return. Its owner later retries this same nonce.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	input := ArchiveRecoveryReclamation{
		Disposition: dispositionRef, Lease: lease, Checkout: checkoutIdentity, Head: checkoutIdentity.Head, Tree: tree,
		Hold: loopruntime.ArchiveRecoveryDispatchHold{Acknowledgements: ackRef, PauseCommandID: "original-pause"},
	}
	result, drainErr := l.reclaimArchiveRecovery(ctx, input)
	if lock != nil {
		lock.Close()
	}
	if drainErr == nil || !strings.Contains(drainErr.Error(), "shared quota lock") ||
		result.WorkerDrain != nil || result.Checkout != nil || result.VM != nil || runtime.retireCalls != 0 {
		t.Fatalf("original permit must be joined and its quota observed before native resource effects: %+v %v", result, drainErr)
	}
	if _, err = ReadArchiveRecoveryWorkerDrain(dispositionRef, lease); !os.IsNotExist(err) {
		t.Fatalf("unconfirmed quota return left a durable worker drain: %v", err)
	}
	if _, err = os.Stat(checkout); err != nil {
		t.Fatal("checkout was removed before quota readback", err)
	}
	if l.cooks.activeCooksByOrder[cook.orderID] != nil {
		t.Fatal("joined original cook still in the active map")
	}
	if err = l.applyStageResult(t.Context(), StageResult{OrderID: cook.orderID,
		SessionID: attempt.SessionID, Generation: cook.generation, Status: StageResultFailed}); err != nil {
		t.Fatal(err)
	}
	updated, _ := json.Marshal(l.canonical)
	if string(original) != string(updated) || len(l.recentHistory) != history ||
		len(l.cooks.pendingReview) != pending || l.activeSummary.Total != 0 {
		t.Fatal("archive release rewrote history, recreated review, or left an active cook")
	}
	if _, err = budget.ReadArchiveRecoverySettlement(lease, disposition); err == nil || !strings.Contains(err.Error(), "quota has not returned") {
		t.Fatalf("unseen quota return counted as final: %v", err)
	}
	budget.Observe(admission.Sample{At: time.Now(), Pressure: admission.Normal, TotalMemoryBytes: 100, CPUs: 10}, nil)
	if proof, err := budget.ReadArchiveRecoverySettlement(lease, disposition); err != nil || proof != nil {
		t.Fatalf("observed original slot return should leave only the unsettled physical lease: %+v %v", proof, err)
	}
	runtime.release = func() error { t.Fatal("retry repeated the original worker release"); return nil }
	result, drainErr = l.reclaimArchiveRecovery(t.Context(), input)
	if drainErr == nil || result.WorkerDrain == nil || result.OriginalReturn != "UNKNOWN" || runtime.retireCalls != 1 {
		t.Fatalf("retry did not preserve the worker drain before waiting for VM evidence: %+v %v", result, drainErr)
	}
}

func TestArchiveRecoveryRestartPreservesOnlyTheExactArchivedAttempt(t *testing.T) {
	l, cook, _, native := openShellLocalTaskFixture(t, "codex")
	order := l.canonical.Orders[cook.orderID]
	order.Status = state.OrderActive
	stage := order.Stages[0]
	stage.Status = state.StageRunning
	stage.Attempts[0].Status = state.AttemptRunning
	attempt := stage.Attempts[0]
	order.Stages[0] = stage
	l.canonical.Orders[cook.orderID] = order

	path := filepath.Join(l.runtimeDir, "sessions", attempt.SessionID, "archive-recovery-cleanup-disposition.json")
	recoveryWriteJSON(t, path, map[string]any{"fixture": "native reader still independently checks this source"})
	frozen := false
	probes := 0
	runtime := &archiveReclaimRuntimeFixture{readbackRuntime: native, startupRead: func(session, order, logicalAttempt string) (bool, error) {
		probes++
		if session != attempt.SessionID || order != cook.orderID || logicalAttempt != attempt.AttemptID {
			t.Fatalf("wrong original restart scope: %s %s %s", session, order, logicalAttempt)
		}
		return frozen, nil
	}}
	l.deps.Runtimes["openshell"] = runtime
	if err := l.reconcileStaleDispatchStages(); err == nil || !strings.Contains(err.Error(), "exact restart ownership") {
		t.Fatalf("sidecar alone authorized historical state change: %v", err)
	}
	if got := l.canonical.Orders[cook.orderID].Stages[0].Attempts[0]; got.Status != state.AttemptRunning {
		t.Fatal("missing owner proof reclassified the original worker")
	}

	frozen = true
	other := "unrelated-order"
	l.canonical.Orders[other] = state.OrderNode{OrderID: other, Status: state.OrderActive,
		Stages: []state.StageNode{{Runtime: "process", Skill: "execute", TaskKey: "execute", Status: state.StageRunning,
			Attempts: []state.AttemptNode{{SessionID: "other-session", AttemptID: "other-attempt", Status: state.AttemptRunning}}}}}
	legacy := OrdersFile{Orders: []Order{
		{ID: cook.orderID, Status: OrderStatusActive, Stages: []Stage{{Status: StageStatusActive}}},
		{ID: other, Status: OrderStatusActive, Stages: []Stage{{Status: StageStatusActive}}},
	}}
	l.setOrdersState(legacy)
	before, _ := json.Marshal(l.canonical.Orders[cook.orderID])
	if err := l.reconcileStaleDispatchStages(); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(l.canonical.Orders[cook.orderID])
	orders, err := l.currentOrders()
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]orderx.StageStatus{}
	for _, projected := range orders.Orders {
		status[projected.ID] = projected.Stages[0].Status
	}
	if string(before) != string(after) || probes != 2 || status[cook.orderID] != StageStatusActive ||
		l.canonical.Orders[other].Stages[0].Status != state.StagePending || status[other] != StageStatusPending {
		t.Fatal("restart did not preserve only the owned original attempt and leave ordinary recovery intact")
	}
}
