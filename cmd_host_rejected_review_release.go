package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/workspaceartifacts"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
)

const rejectedReviewReleaseHold = "rejected-review-release:"

type rejectedReviewReleasePlan struct {
	Schema      int                                `json:"schema"`
	Request     loopruntime.SessionReadbackFileRef `json:"request"`
	Successor   loopruntime.SessionReadbackFileRef `json:"successor"`
	Order       string                             `json:"order_id"`
	RuntimeDir  string                             `json:"runtime_dir"`
	OwnerBefore hostTaskOwner                      `json:"owner_before"`
	Generation  hostGeneration                     `json:"generation"`
	Host        hostOperatorActor                  `json:"host"`
	Gateway     hostOperatorActor                  `json:"gateway"`
	Resident    admission.Resident                 `json:"resident"`
	Review      string                             `json:"canonical_review_sha256"`
	Events      loopruntime.SessionReadbackFileRef `json:"rejected_review_events"`
	Profile     loopruntime.SessionReadbackFileRef `json:"profile"`
	LoopMode    string                             `json:"loop_mode"`
	Attempts    []hostRejectedReviewAttempt        `json:"attempts"`
}

type rejectedReviewRoundRelease struct {
	Schema             int                                 `json:"schema"`
	Owner              string                              `json:"owner"`
	Status             string                              `json:"status"`
	Error              string                              `json:"error,omitempty"`
	Plan               string                              `json:"plan,omitempty"`
	Request            loopruntime.SessionReadbackFileRef  `json:"request"`
	RequestID          string                              `json:"request_id"`
	HostInstanceID     string                              `json:"host_instance_id"`
	ContextID          string                              `json:"context_id"`
	OrderID            string                              `json:"order_id"`
	Intent             *loopruntime.SessionReadbackFileRef `json:"intent,omitempty"`
	RoundState         *loop.HostContinuation              `json:"round_state,omitempty"`
	GatewayExit        *loopruntime.SessionReadbackFileRef `json:"gateway_exit,omitempty"`
	SourceRelease      *loopruntime.SessionReadbackFileRef `json:"source_release,omitempty"`
	Maintenance        *loopruntime.SessionReadbackFileRef `json:"maintenance_receipt,omitempty"`
	SourceExit         *loopruntime.SessionReadbackFileRef `json:"source_exit,omitempty"`
	SuccessorHost      string                              `json:"successor_host_instance_id,omitempty"`
	SuccessorContext   string                              `json:"successor_context_id,omitempty"`
	SourceProcessCount *int                                `json:"source_process_count,omitempty"`
	SourceRSS          *int64                              `json:"source_rss_bytes,omitempty"`
	SourceDisk         *int64                              `json:"source_generated_disk_bytes,omitempty"`
	Storage            *loopruntime.SessionReadbackFileRef `json:"storage_receipt,omitempty"`
	HostZero           string                              `json:"host_zero_occupancy"`
	Attempts           []hostRejectedReviewAttempt         `json:"attempts"`
	Retention          string                              `json:"retention,omitempty"`
	Effects            []string                            `json:"effects"`
	AuthorizesDispatch bool                                `json:"authorizes_dispatch"`
	Next               *rejectedReviewReleaseNext          `json:"next"`
}

type rejectedReviewReleaseNext struct {
	Kind      string   `json:"kind"`
	Owner     string   `json:"owner"`
	Operation string   `json:"operation,omitempty"`
	Argv      []string `json:"argv,omitempty"`
	Required  []string `json:"required,omitempty"`
}

func (p rejectedReviewReleasePlan) next(mode, digest string) *rejectedReviewReleaseNext {
	operation := "original_native_round_release"
	switch mode {
	case "reconcile":
		operation = "original_native_round_release_readback"
	case "retire":
		operation = "original_native_retained_review_retirement"
	case "finish":
		operation = "original_native_retired_source_switch"
	case "adopt":
		operation = "original_native_host_transfer"
	}
	return &rejectedReviewReleaseNext{Kind: "executable", Owner: "Noodle", Operation: operation,
		Argv: []string{p.Successor.Path, "--project-dir", p.OwnerBefore.Current.ProjectDir, "host", "rejected-review-handoff",
			"--request-file", p.Request.Path, "--order-id", p.Order, "--mode", mode, "--expected-plan", digest}}
}

func runRejectedReviewRoundRelease(ctx context.Context, project, requestFile, order, mode, digest string) (rejectedReviewRoundRelease, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	result := rejectedReviewRoundRelease{Schema: 1, Owner: "Noodle", Status: "refused", OrderID: order, Effects: []string{}, HostZero: "NOT_RUN"}
	switch mode {
	case "plan":
		if digest != "" {
			return result, errors.New("release planning takes no expected-plan")
		}
	case "release", "reconcile", "rollback", "retire", "finish", "adopt":
		if raw, err := hex.DecodeString(digest); err != nil || len(raw) != 32 {
			return result, errors.New("original release plan digest required")
		}
	default:
		return result, errors.New("unknown rejected review release mode")
	}
	var request hostRequestBinding
	ref, err := loopruntime.ReadSessionReadbackFile(requestFile, &request)
	if err != nil {
		return result, err
	}
	project, err = filepath.EvalSymlinks(project)
	if err != nil || project != request.ProjectDir || request.Status != "accepted" || request.Operation != "attach" || request.RequestID == "" {
		return result, errors.New("original accepted source request or project differs")
	}
	exe, err := os.Executable()
	if err != nil {
		return result, err
	}
	binary, err := hostReadbackBinaryRef(exe)
	if err != nil {
		return result, err
	}
	runtimeDir, err := filepath.EvalSymlinks(filepath.Join(project, ".noodle"))
	if err != nil {
		return result, err
	}
	intentPath := filepath.Join(runtimeDir, "rejected-review-round-release-"+hostDigest([]byte(request.RequestID))+".json")
	var plan rejectedReviewReleasePlan
	var intent loopruntime.SessionReadbackFileRef
	if mode != "plan" {
		intent, err = loopruntime.ReadSessionReadbackFile(intentPath, &plan)
		if err == nil {
			if intent.SHA256 != digest || plan.Schema != 1 || plan.Request != ref || plan.Order != order || plan.RuntimeDir != runtimeDir ||
				plan.OwnerBefore.Current.RequestID != request.RequestID || plan.OwnerBefore.Current.ProjectDir != project ||
				plan.OwnerBefore.Current.HostDir != request.HostDir || plan.Successor != binary {
				return result, errors.New("original rejected review release intent differs")
			}
		} else if !os.IsNotExist(err) || mode != "release" {
			return result, err
		}
	}
	if mode == "release" || mode == "rollback" || mode == "retire" || mode == "finish" || mode == "adopt" {
		guard, err := lockfile.TryLock(filepath.Join(runtimeDir, "rejected-review-round-release.lock"))
		if err != nil {
			return result, err
		}
		defer guard.Close()
	}
	if intent.Path == "" {
		var raw []byte
		plan, raw, err = readRejectedReviewReleasePlan(ctx, project, runtimeDir, requestFile, order, binary)
		if err != nil {
			return result, err
		}
		result = projectRejectedReviewRelease(plan, hostDigest(raw), nil)
		if mode == "plan" {
			result.Status, result.Next = "release_eligible", plan.next("release", result.Plan)
			return result, nil
		}
		if result.Plan != digest {
			return result, errors.New("original rejected review release plan changed")
		}
		// No write precedes an explicit apply and a freshly matching plan.
		if err = saveHostStorageBytes(intentPath, raw); err != nil {
			return result, err
		}
		intent = loopruntime.SessionReadbackFileRef{Path: intentPath, SHA256: digest}
		result.Status, result.Next = "source_pause_pending", plan.next("reconcile", digest)
		result.Effects = append(result.Effects, "original_round_release_intent")
	} else {
		result = projectRejectedReviewRelease(plan, digest, &intent)
	}
	result.Intent = &intent
	if mode == "reconcile" {
		return reconcileRejectedReviewHandoff(ctx, plan, intent, result)
	}
	if err = loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"release_intent": intent}); err != nil {
		return result, err
	}
	if mode == "rollback" {
		return rollbackRejectedReviewRoundRelease(ctx, plan, intent, result)
	}
	if mode == "retire" {
		return retireReleasedRejectedReview(ctx, plan, intent, result)
	}
	if mode == "finish" {
		return finishRejectedReviewSource(ctx, plan, intent, result)
	}
	if mode == "adopt" {
		return adoptRejectedReviewSuccessor(ctx, plan, intent, result)
	}
	result, err = applyRejectedReviewRoundRelease(ctx, plan, intent, result)
	if err == nil && result.Status == "source_round_released" {
		result.SourceRelease, err = saveRejectedReviewSourceRelease(intent, result)
	}
	return result, err
}

func projectRejectedReviewRelease(p rejectedReviewReleasePlan, digest string, intent *loopruntime.SessionReadbackFileRef) rejectedReviewRoundRelease {
	owner := p.OwnerBefore.Current
	return rejectedReviewRoundRelease{Schema: 1, Owner: "Noodle", Status: "refused", Plan: digest, Request: p.Request,
		RequestID: owner.RequestID, HostInstanceID: owner.HostInstanceID, ContextID: owner.ContextID, OrderID: p.Order,
		Effects: []string{}, Intent: intent, Attempts: p.Attempts, Retention: reducer.TaskDataRetention, HostZero: "NOT_RUN"}
}

func readRejectedReviewReleasePlan(ctx context.Context, project, runtimeDir, requestFile, order string, binary loopruntime.SessionReadbackFileRef) (rejectedReviewReleasePlan, []byte, error) {
	var p rejectedReviewReleasePlan
	readback, err := readHostRejectedReviewHandoff(ctx, project, requestFile, order)
	if err != nil {
		return p, nil, err
	}
	if readback.observer == nil || readback.WorkerLeaseReadbackError != "" {
		return p, nil, errors.New("original selected stopped attempts or machine lease readback unavailable")
	}
	r := readback.observer.After
	p = rejectedReviewReleasePlan{Schema: 1, Request: readback.Request, Order: order, RuntimeDir: runtimeDir,
		Successor: binary, Host: r.Host, Gateway: r.Gateway, Resident: r.Resident, Events: readback.Sources["rejected_review_events"],
		Profile: readback.Sources["profile"]}
	root := r.Status.HostDir
	pool, err := readHostPool(root)
	if err != nil {
		return p, nil, err
	}
	p.Generation = pool.Generations[readback.HostInstanceID]
	p.OwnerBefore = pool.Owners[readback.RequestID]
	liveReceipt := r.Status.hostReceipt
	liveReceipt.OwnerBinding = nil // The status CLI also returns the independently validated pool projection.
	if p.OwnerBefore.Current != liveReceipt || p.OwnerBefore.State != "owned" || p.OwnerBefore.ControllerOnly ||
		p.OwnerBefore.MaintenanceHold != "" || p.OwnerBefore.RoundState != nil || p.Generation.Preparation == nil ||
		p.Generation.State != "active" || p.Generation.SourceService != nil || r.Host.Process.PID != p.Generation.Process.PID {
		return p, nil, errors.New("original active execution owner is not an unfenced managed round")
	}
	if err = validateRejectedReviewReleasePool(pool, p); err != nil {
		return p, nil, err
	}
	var frame sessionObserverResources
	if err = json.Unmarshal(r.Resources, &frame); err != nil || frame.Resources == nil || frame.Resources.Active == nil ||
		*frame.Resources.Active != 0 || frame.Resources.Quota == nil || frame.Resources.Quota.Allocations != 1 ||
		frame.Resources.Quota.Residents != 1 || frame.Resources.Quota.Owned != 1 ||
		r.Resident.ForceAfterGrace || len(r.Resident.Contexts) != 1 || r.Resident.Contexts[0] != readback.ContextID ||
		len(r.Resident.ConsumerLeases) != 0 {
		return p, nil, errors.New("original release requires one exclusive guarded Gateway and zero active workers")
	}
	rawStatus, err := os.ReadFile(filepath.Join(runtimeDir, "status.json"))
	if err != nil {
		return p, nil, err
	}
	p.LoopMode, _, err = readMaintenanceStatus(rawStatus)
	if err != nil || (p.LoopMode != "manual" && p.LoopMode != "supervised") {
		return p, nil, errors.New("original Loop effective review mode differs")
	}
	p.Attempts = readback.Attempts
	if err := verifyRejectedReviewCustodyBeforeRelease(p); err != nil {
		return p, nil, err
	}
	p.Review, err = rejectedReviewReleaseHistory(p)
	if err != nil {
		return p, nil, err
	}
	raw, err := json.Marshal(p)
	return p, raw, err
}

func validateRejectedReviewReleasePool(pool hostPool, p rejectedReviewReleasePlan) error {
	if pool.Active != p.Generation.ID || len(pool.Owners) != 1 ||
		!reflect.DeepEqual(pool.Generations[p.Generation.ID], p.Generation) {
		return errors.New("original release pool generation or owner scope changed")
	}
	for id, other := range pool.Generations {
		if id != p.Generation.ID && generationLive(other) {
			return errors.New("original release has another live generation")
		}
	}
	for _, pending := range pool.Queue {
		if pending.State == "queued" || pending.State == "starting" || pending.State == "blocked" {
			return errors.New("original release has a competing version request")
		}
	}
	return nil
}

func rejectedReviewReleaseHistory(p rejectedReviewReleasePlan) (string, error) {
	history, err := loop.ReadRejectedReviewHistory(p.RuntimeDir, p.Order)
	if err != nil {
		return "", err
	}
	if len(history.Attempts) != len(p.Attempts) || len(p.Attempts) == 0 ||
		history.Head != p.Attempts[len(p.Attempts)-1].CandidateHead || history.Sources["rejected_review_events"] != p.Events {
		return "", errors.New("original rejected review history changed")
	}
	for i, a := range p.Attempts {
		if (loop.RejectedReviewAttempt{StageIndex: a.StageIndex, SessionID: a.SessionID, AttemptID: a.AttemptID}) != history.Attempts[i] {
			return "", errors.New("original rejected review attempt changed")
		}
	}
	checkpoint := history.Sources["canonical_snapshot"]
	var snapshot reducer.DurableSnapshot
	ref, err := loopruntime.ReadSessionReadbackFile(checkpoint.Path, &snapshot)
	if err != nil || ref != checkpoint || len(snapshot.State.PendingReviews) != 1 || len(snapshot.State.ActionNeeded) != 0 {
		return "", errors.New("original rejected review is not the sole pending owner work")
	}
	for id, other := range snapshot.State.Orders {
		if id == p.Order {
			continue
		}
		if id != "schedule" {
			return "", errors.New("another accepted order belongs to the original context")
		}
		for _, stage := range other.Stages {
			if stage.Status != state.StagePending || len(stage.Attempts) != 0 {
				return "", errors.New("original scheduler has effects")
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join(p.RuntimeDir, "sessions"))
	if err != nil || len(entries) != len(p.Attempts) {
		return "", errors.New("original session directory differs from canonical rejected history")
	}
	for _, entry := range entries {
		if !entry.IsDir() || !slices.ContainsFunc(p.Attempts, func(a hostRejectedReviewAttempt) bool { return a.SessionID == entry.Name() }) {
			return "", errors.New("unqualified session belongs to the original context")
		}
	}
	retained, err := json.Marshal(struct {
		Order   state.OrderNode
		Review  state.PendingReviewNode
		Effects []reducer.EffectLedgerRecord
	}{snapshot.State.Orders[p.Order], snapshot.State.PendingReviews[p.Order], snapshot.EffectLedger})
	if err != nil {
		return "", err
	}
	return hostDigest(retained), nil
}

// The original actor retains the Gateway child handle. The only socket effect
// here is its pre-existing cooperative round release, offered once.
func applyRejectedReviewRoundRelease(ctx context.Context, p rejectedReviewReleasePlan, intent loopruntime.SessionReadbackFileRef,
	result rejectedReviewRoundRelease) (rejectedReviewRoundRelease, error) {
	result.Status, result.Next = "source_pause_pending", p.next("reconcile", intent.SHA256)
	offerPath := intent.Path + ".offer"
	offered, err := releaseMarker(offerPath, intent.SHA256)
	if err != nil {
		return result, err
	}
	if offered {
		return readRejectedReviewRoundRelease(ctx, p, intent, result)
	}
	rolledBack, err := releaseMarker(intent.Path+".rollback", intent.SHA256)
	if err != nil || rolledBack {
		return result, errors.New("original rejected review release was withdrawn or remains unknown")
	}
	if err := verifyRejectedReviewReleaseSource(ctx, p, intent.SHA256); err != nil {
		return result, err
	}
	pausePath := intent.Path + ".pause"
	_, priorPause := os.Lstat(pausePath)
	paused, err := legacyPauseCheckpoint(ctx, p.RuntimeDir, pausePath)
	if err != nil {
		return result, err
	}
	if os.IsNotExist(priorPause) {
		result.Effects = append(result.Effects, "original_native_pause_offered")
	}
	if !paused {
		result.Status, result.Next = "source_pause_pending", p.next("reconcile", intent.SHA256)
		return result, nil
	}
	if err := verifyRejectedReviewReleasePause(p.RuntimeDir, pausePath); err != nil {
		return result, err
	}
	if err := verifyRejectedReviewReleaseSource(ctx, p, intent.SHA256); err != nil {
		return result, err
	}
	hold := rejectedReviewReleaseHold + intent.SHA256
	before := p.OwnerBefore
	fenced := before
	fenced.ControllerOnly, fenced.MaintenanceHold = true, hold
	root := before.Current.HostDir
	if err := updateHostPool(root, func(pool *hostPool) error {
		if err := validateRejectedReviewReleasePool(*pool, p); err != nil {
			return err
		}
		current := pool.Owners[before.Current.RequestID]
		if reflect.DeepEqual(current, fenced) {
			return nil
		}
		if !reflect.DeepEqual(current, before) {
			return errors.New("original rejected review owner changed before authority transition")
		}
		pool.Owners[before.Current.RequestID] = fenced
		return nil
	}); err != nil {
		return result, err
	}
	result.Effects = append(result.Effects, "original_round_release_fence")
	if err := verifyRejectedReviewReleasePause(p.RuntimeDir, pausePath); err != nil {
		return result, err
	}
	if err := saveHostStorageBytes(offerPath, []byte(intent.SHA256)); err != nil {
		return result, err
	}
	result.Status, result.Next = "source_release_pending", p.next("reconcile", intent.SHA256)
	result.Effects = append(result.Effects, "original_round_release_offered")
	r := before.Original
	request := hostRequest{Operation: "round_prepare", ProjectDir: r.ProjectDir, RequestID: r.RequestID,
		ConfigPath: r.ConfigPath, ConfigSHA256: r.ConfigSHA256, Mode: r.Mode, CleanupCompleted: r.CleanupCompleted,
		BinarySHA256: p.Generation.SHA, AdmissionSHA: r.BinarySHA256, AdoptCurrentProfile: true}
	raw, err := nativeHostRPC(ctx, p.Generation.Directory, request)
	if err != nil {
		return result, fmt.Errorf("original cooperative round release outcome needs readback: %w", err)
	}
	var response hostResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return result, err
	}
	if response.Status != "ok" || !hostReceiptMatches(request, response.hostReceipt) || response.HostInstanceID != before.Current.HostInstanceID ||
		response.ContextID != before.Current.ContextID || response.ContextState != "detached" || response.TerminalCause != loop.TerminalMigrationDetach {
		return result, fmt.Errorf("original cooperative round release needs readback: %s %s", response.Status, response.Error)
	}
	return readRejectedReviewRoundRelease(ctx, p, intent, result)
}

func releaseMarker(path, digest string) (bool, error) {
	ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil || ref.SHA256 != hostDigest([]byte(digest)) {
		return false, errors.New("original rejected review release marker differs")
	}
	return true, nil
}

func verifyRejectedReviewReleasePause(runtimeDir, pausePath string) error {
	var pause legacyModeIntent
	if _, err := loopruntime.ReadSessionReadbackFile(pausePath, &pause); err != nil {
		return err
	}
	ack, err := archiveRecoveryPaused(runtimeDir)
	if err != nil || ack.PauseCommandID != pause.Command.ID {
		return errors.New("original release native pause acknowledgement differs")
	}
	return nil
}

func verifyRejectedReviewReleaseSource(ctx context.Context, p rejectedReviewReleasePlan, digest string) error {
	root := p.OwnerBefore.Current.HostDir
	pool, err := readHostPool(root)
	if err != nil {
		return err
	}
	if err = validateRejectedReviewReleasePool(pool, p); err != nil {
		return err
	}
	expected := p.OwnerBefore
	current := pool.Owners[expected.Current.RequestID]
	if current.MaintenanceHold == rejectedReviewReleaseHold+digest {
		expected.ControllerOnly, expected.MaintenanceHold = true, current.MaintenanceHold
	}
	if !reflect.DeepEqual(current, expected) {
		return errors.New("original active rejected review owner changed")
	}
	if err := verifyRejectedReviewReleaseActors(p, true); err != nil {
		return err
	}
	if err := verifyRejectedReviewRetainedCheckout(ctx, p); err != nil {
		return err
	}
	for _, a := range p.Attempts {
		sources := maps.Clone(a.NativeOpenShellStoppedReadback.Sources)
		delete(sources, "canonical_snapshot") // Its accepted semantics were checked above after the native pause.
		if err := loopruntime.VerifySessionReadbackFiles(sources); err != nil {
			return err
		}
	}
	return loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"original_request": p.Request, "profile": p.Profile})
}

func verifyRejectedReviewRetainedCheckout(ctx context.Context, p rejectedReviewReleasePlan) error {
	review, err := rejectedReviewReleaseHistory(p)
	if err != nil || review != p.Review {
		return errors.New("original canonical rejected review or checkout binding changed")
	}
	checkpoint, err := reducer.ReadSnapshot(filepath.Join(p.RuntimeDir, "state.snapshot.json"))
	if err != nil {
		return err
	}
	canonicalReview := checkpoint.State.PendingReviews[p.Order]
	id, err := workspaceartifacts.ReadCleanIdentity(ctx, canonicalReview.WorktreePath)
	if err != nil || id.Head != p.Attempts[len(p.Attempts)-1].CandidateHead {
		return errors.New("original retained checkout is unreadable or changed")
	}
	return nil
}

func verifyRejectedReviewReleaseActors(p rejectedReviewReleasePlan, gatewayLive bool) error {
	host, err := observeSessionActor(p.Host.Process.PID, p.Host.Binary)
	if err != nil || !p.Host.Process.SameGeneration(host.Process) {
		return errors.New("original cooperative source Host generation changed")
	}
	if gatewayLive {
		gateway, err := observeSessionActor(p.Gateway.Process.PID, p.Gateway.Binary)
		if err != nil || !p.Gateway.Process.SameGeneration(gateway.Process) || gateway.Process.ParentPID != host.Process.PID ||
			gateway.Process.Group != p.Gateway.Process.Group {
			return errors.New("original cooperative source Gateway generation changed")
		}
	}
	return nil
}

func readRejectedReviewRoundRelease(ctx context.Context, p rejectedReviewReleasePlan, intent loopruntime.SessionReadbackFileRef,
	result rejectedReviewRoundRelease) (rejectedReviewRoundRelease, error) {
	rolledBack, err := releaseMarker(intent.Path+".rollback", intent.SHA256)
	if err != nil {
		return result, err
	}
	if rolledBack {
		pool, err := readHostPool(p.OwnerBefore.Current.HostDir)
		if err != nil {
			return result, err
		}
		if reflect.DeepEqual(pool.Owners[result.RequestID], p.OwnerBefore) {
			result.Status = "source_release_withdrawn"
			return result, nil
		}
		result.Status, result.Next = "source_release_withdrawal_pending", p.next("rollback", intent.SHA256)
		return result, nil
	}
	offered, err := releaseMarker(intent.Path+".offer", intent.SHA256)
	if err != nil {
		return result, err
	}
	if !offered {
		result.Status, result.Next = "source_pause_pending", p.next("reconcile", intent.SHA256)
		if _, err := os.Lstat(intent.Path + ".pause"); os.IsNotExist(err) {
			result.Status, result.Next = "release_eligible", p.next("release", intent.SHA256)
		} else if err := verifyRejectedReviewReleasePause(p.RuntimeDir, intent.Path+".pause"); err == nil {
			result.Status, result.Next = "source_release_eligible", p.next("release", intent.SHA256)
		}
		return result, nil
	}
	result.Status, result.Next = "source_release_pending", p.next("reconcile", intent.SHA256)
	hold := rejectedReviewReleaseHold + intent.SHA256
	owner, err := readHostOwnerBindingWithHold(p.Request.Path, hold)
	if err != nil {
		return result, err
	}
	pool, err := readHostPool(owner.HostRoot)
	if err != nil {
		return result, err
	}
	if err = validateRejectedReviewReleasePool(pool, p); err != nil {
		return result, err
	}
	current := pool.Owners[result.RequestID]
	if current.State != "ready" {
		return result, nil
	}
	expected := p.OwnerBefore
	expected.State, expected.ControllerOnly, expected.MaintenanceHold = "ready", true, hold
	expected.Current.ContextState, expected.Current.TerminalCause = "detached", loop.TerminalMigrationDetach
	expected.RoundState = &loop.HostContinuation{Mode: p.LoopMode, DispatchState: loop.StatePaused}
	if !reflect.DeepEqual(current, expected) || owner.Request != p.Request || owner.Current != expected.Current {
		return result, errors.New("original native round ownership, release state or pause differs")
	}
	if err := verifyRejectedReviewReleaseActors(p, false); err != nil {
		return result, err
	}
	if err := verifyRejectedReviewRetainedCheckout(ctx, p); err != nil {
		return result, err
	}
	normal, err := readRejectedReviewGatewayExit(ctx, p)
	if err != nil {
		return result, err
	}
	result.Status, result.GatewayExit, result.RoundState = "source_round_released", &normal, current.RoundState
	result.Next = &rejectedReviewReleaseNext{Kind: "input", Owner: "Noodle", Required: []string{"original_native_host_transfer_readback"}}
	return result, nil
}

func readRejectedReviewGatewayExit(ctx context.Context, p rejectedReviewReleasePlan) (loopruntime.SessionReadbackFileRef, error) {
	var rows map[string]admission.Resident
	ref, err := loopruntime.ReadSessionReadbackFile(filepath.Join(p.Generation.Directory, "residents.json"), &rows)
	if err != nil {
		return ref, err
	}
	if len(rows) != 1 {
		return ref, errors.New("original resident inventory changed at gateway release")
	}
	final := rows[p.Resident.ID]
	if final.StopRequested == "" {
		return ref, errors.New("original guarded gateway has no cooperative stop request")
	}
	if err := admission.VerifyResidentNormalExit(p.Resident, final); err != nil {
		return ref, err
	}
	gone, err := procx.ObserveNativeExit(p.Gateway.Process)
	if err != nil || !gone {
		return ref, errors.New("original gateway process group absence remains unknown")
	}
	if absent, err := procx.ProcessGroupAbsent(p.Gateway.Process.Group); err != nil || !absent {
		return ref, errors.New("original gateway process group remains occupied or unknown")
	}
	_, identity, err := residentRetirementResources(ctx, p.OwnerBefore.Current.HostDir, p.Generation, nativeEmptyHostRetirement(), true)
	if err != nil || !p.Host.Process.SameGeneration(identity) {
		return ref, fmt.Errorf("original source Host zero-context and shared-quota readback unavailable: %v", err)
	}
	return ref, loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"original_gateway_normal_wait": ref})
}

type rejectedReviewSourceRelease struct {
	Schema      int                                `json:"schema"`
	Intent      loopruntime.SessionReadbackFileRef `json:"intent"`
	RoundState  loop.HostContinuation              `json:"round_state"`
	GatewayExit loopruntime.SessionReadbackFileRef `json:"gateway_exit"`
}

func saveRejectedReviewSourceRelease(intent loopruntime.SessionReadbackFileRef, result rejectedReviewRoundRelease) (*loopruntime.SessionReadbackFileRef, error) {
	if result.Status != "source_round_released" || result.RoundState == nil || result.RoundState.DispatchState != loop.StatePaused || result.GatewayExit == nil {
		return nil, errors.New("original cooperative source round release has no normal-exit proof")
	}
	release := rejectedReviewSourceRelease{1, intent, *result.RoundState, *result.GatewayExit}
	raw, err := json.Marshal(release)
	if err != nil {
		return nil, err
	}
	path := intent.Path + ".source-release"
	if err := saveHostStorageBytes(path, raw); err != nil {
		return nil, err
	}
	ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
	if err != nil || hostDigest(raw) != ref.SHA256 {
		return nil, errors.New("original cooperative source round readback changed")
	}
	return &ref, nil
}

func rollbackRejectedReviewRoundRelease(ctx context.Context, p rejectedReviewReleasePlan, intent loopruntime.SessionReadbackFileRef,
	result rejectedReviewRoundRelease) (rejectedReviewRoundRelease, error) {
	offered, err := releaseMarker(intent.Path+".offer", intent.SHA256)
	if err != nil || offered {
		return result, errors.New("original round release was offered or remains unknown; use readback")
	}
	if err := verifyRejectedReviewReleaseSource(ctx, p, intent.SHA256); err != nil {
		return result, err
	}
	fenced := p.OwnerBefore
	fenced.ControllerOnly, fenced.MaintenanceHold = true, rejectedReviewReleaseHold+intent.SHA256
	root := p.OwnerBefore.Current.HostDir
	if err := saveHostStorageBytes(intent.Path+".rollback", []byte(intent.SHA256)); err != nil {
		return result, err
	}
	if err := updateHostPool(root, func(pool *hostPool) error {
		if err := validateRejectedReviewReleasePool(*pool, p); err != nil {
			return err
		}
		current := pool.Owners[result.RequestID]
		if reflect.DeepEqual(current, p.OwnerBefore) {
			return nil
		}
		if !reflect.DeepEqual(current, fenced) {
			return errors.New("original release fence changed before withdrawal")
		}
		pool.Owners[result.RequestID] = p.OwnerBefore
		return nil
	}); err != nil {
		return result, err
	}
	result.Status, result.Effects = "source_release_withdrawn", []string{"original_round_release_withdrawn"}
	return result, nil
}
