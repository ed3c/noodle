package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
)

type rejectedReviewSuccessorStart struct {
	Schema      int                                `json:"schema"`
	Intent      loopruntime.SessionReadbackFileRef `json:"intent"`
	SourceZero  loopruntime.SessionReadbackFileRef `json:"source_zero"`
	Binary      loopruntime.SessionReadbackFileRef `json:"successor"`
	Preparation admission.PreparationRef           `json:"preparation"`
}

type rejectedReviewSuccessorPreparationIntent struct {
	Schema     int                                `json:"schema"`
	Intent     loopruntime.SessionReadbackFileRef `json:"intent"`
	SourceZero loopruntime.SessionReadbackFileRef `json:"source_zero"`
	Binary     loopruntime.SessionReadbackFileRef `json:"successor"`
}

func adoptRejectedReviewSuccessor(ctx context.Context, p rejectedReviewReleasePlan, intent loopruntime.SessionReadbackFileRef,
	result rejectedReviewRoundRelease) (rejectedReviewRoundRelease, error) {
	startPath := intent.Path + ".successor-start"
	start, err := readRejectedReviewSuccessorStart(p, intent)
	if os.IsNotExist(err) {
		preparation, err := readRejectedReviewSuccessorPreparationIntent(p, intent)
		if os.IsNotExist(err) {
			current, err := reconcileRejectedReviewHandoff(ctx, p, intent, result)
			if err != nil || current.Status != "source_host_zero" || current.Storage == nil {
				return current, errors.Join(err, errors.New("original source Host ZERO is required before successor admission"))
			}
			result = current
			preparation = rejectedReviewSuccessorPreparationIntent{1, intent, *current.Storage, p.Successor}
			raw, err := json.Marshal(preparation)
			if err != nil {
				return current, err
			}
			if err := saveHostStorageBytes(intent.Path+".successor-prepare", raw); err != nil {
				return current, err
			}
		} else if err != nil {
			return result, err
		}
		result.Status, result.Next = "successor_admission_pending", p.next("reconcile", intent.SHA256)
		// The original machine producer returns the same preparation nonce if
		// the first reservation persisted before the local start receipt.
		start, err = prepareRejectedReviewSuccessor(ctx, p, intent, preparation.SourceZero)
		if err != nil {
			return result, err
		}
		raw, err := json.Marshal(start)
		if err != nil {
			return result, err
		}
		if err := saveHostStorageBytes(startPath, raw); err != nil {
			return result, err
		}
	} else if err != nil {
		return result, err
	}
	result.Status, result.Next = "successor_admission_pending", p.next("reconcile", intent.SHA256)
	successor, err := ensureRejectedReviewSuccessor(ctx, p, intent, start)
	if err != nil {
		return result, err
	}
	result.SuccessorHost = successor.ID
	observation, err := readRejectedReviewSuccessor(ctx, p, intent, result)
	if err != nil {
		return observation, err
	}
	if observation.SuccessorContext != "" {
		return acceptRejectedReviewSuccessor(p, intent, observation)
	}
	offer := intent.Path + ".successor-attach"
	if _, err := os.Lstat(offer); err == nil {
		return observation, nil // Existing attach offer can only be read back.
	} else if !os.IsNotExist(err) {
		return observation, err
	}
	root, project := p.OwnerBefore.Current.HostDir, p.OwnerBefore.Current.ProjectDir
	environment, err := p.OwnerBefore.ContextInputs.rebuild(project, os.Environ())
	if err != nil {
		return result, err
	}
	var control worktree.TaskControl
	controlRef, err := loopruntime.ReadSessionReadbackFile(filepath.Join(filepath.Dir(project), "control.json"), &control)
	if err != nil {
		return result, err
	}
	var lease *admission.MachineSessionLease
	for _, a := range p.Attempts {
		if a.WorkerLease != nil && a.WorkerLease.ID == control.Preparation.ID {
			lease = a.WorkerLease
		}
	}
	if lease == nil {
		return result, errors.New("original task control has no exact settled preparation binding")
	}
	release, err := loopruntime.ReadSessionReadbackFile(intent.Path+".source-release", nil)
	if err != nil {
		return result, err
	}
	ref, err := reserveRejectedReviewTaskPreparation(ctx, root, environment, rejectedReviewTaskPreparationSource{
		Request: p.Request, Control: controlRef, SourceZero: start.SourceZero, SourceRelease: release, Successor: p.Successor, Lease: *lease,
	})
	if err != nil {
		return result, err
	}
	raw, err := json.Marshal(ref)
	if err != nil {
		return result, err
	}
	if err := saveHostStorageBytes(offer, raw); err != nil {
		return result, err
	}
	original := p.OwnerBefore.Original
	request := hostRequest{Operation: "round_rebuild", ProjectDir: original.ProjectDir, RequestID: original.RequestID,
		ConfigPath: original.ConfigPath, ConfigSHA256: original.ConfigSHA256, Mode: original.Mode, CleanupCompleted: original.CleanupCompleted,
		BinarySHA256: successor.SHA, AdmissionSHA: original.BinarySHA256, RejectedReviewAdoption: &ref, Environment: environment}
	result.Effects = append(result.Effects, "original_paused_successor_admission_offered")
	reply, err := nativeHostRPC(ctx, successor.Directory, request)
	if err != nil {
		return result, fmt.Errorf("original paused successor admission outcome unknown: %w", err)
	}
	var response hostResponse
	if err := json.Unmarshal(reply, &response); err != nil {
		return result, err
	}
	if response.Status != "ok" || !hostReceiptMatches(request, response.hostReceipt) || response.HostInstanceID != successor.ID ||
		response.ContextState != "active" || response.ContextID == "" {
		return result, fmt.Errorf("original paused successor admission requires readback: %s %s", response.Status, response.Error)
	}
	observation, err = readRejectedReviewSuccessor(ctx, p, intent, result)
	if err != nil {
		return observation, err
	}
	return acceptRejectedReviewSuccessor(p, intent, observation)
}

func readRejectedReviewSuccessorPreparationIntent(p rejectedReviewReleasePlan, intent loopruntime.SessionReadbackFileRef) (rejectedReviewSuccessorPreparationIntent, error) {
	var saved rejectedReviewSuccessorPreparationIntent
	if _, err := loopruntime.ReadSessionReadbackFile(intent.Path+".successor-prepare", &saved); err != nil {
		return saved, err
	}
	if saved.Schema != 1 || saved.Intent != intent || saved.Binary != p.Successor {
		return saved, errors.New("original successor preparation intent differs")
	}
	return saved, verifyRejectedReviewZeroRef(p, intent, saved.SourceZero)
}

func readRejectedReviewSuccessorStart(p rejectedReviewReleasePlan, intent loopruntime.SessionReadbackFileRef) (rejectedReviewSuccessorStart, error) {
	var start rejectedReviewSuccessorStart
	if _, err := loopruntime.ReadSessionReadbackFile(intent.Path+".successor-start", &start); err != nil {
		return start, err
	}
	if start.Schema != 1 || start.Intent != intent || start.Binary != p.Successor || start.Preparation.ID == "" ||
		start.SourceZero.Path != intent.Path+".storage-zero" {
		return start, errors.New("original successor resident preparation intent differs")
	}
	return start, verifyRejectedReviewZeroRef(p, intent, start.SourceZero)
}

func verifyRejectedReviewZeroRef(p rejectedReviewReleasePlan, intent, ref loopruntime.SessionReadbackFileRef) error {
	var storage rejectedReviewStorageReceipt
	actual, err := loopruntime.ReadSessionReadbackFile(ref.Path, &storage)
	if err != nil || actual != ref || ref.Path != intent.Path+".storage-zero" || storage.Schema != 1 ||
		storage.Intent != intent || !storage.SourceOnly || storage.Successor != "" ||
		storage.SourceExit.Path != filepath.Join(p.Generation.Directory, "resource-resident-exit.json") ||
		storage.Maintenance.Path != intent.Path+".maintenance-exit" {
		return errors.New("original source-only Host ZERO receipt differs")
	}
	return loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{
		"original_release_intent": intent, "source_exit": storage.SourceExit, "maintenance_exit": storage.Maintenance,
	})
}

func prepareRejectedReviewSuccessor(ctx context.Context, p rejectedReviewReleasePlan, intent, zero loopruntime.SessionReadbackFileRef) (rejectedReviewSuccessorStart, error) {
	result := rejectedReviewSuccessorStart{Schema: 1, Intent: intent, SourceZero: zero, Binary: p.Successor}
	if p.OwnerBefore.ContextInputs == nil {
		return result, errors.New("original execution environment unavailable for successor preparation")
	}
	if err := verifyRollingTarget(ctx, p.Successor.Path, p.Successor.SHA256); err != nil {
		return result, err
	}
	if err := verifyMachineRollingTarget(ctx, p.Successor.Path, p.Successor.SHA256); err != nil {
		return result, err
	}
	directory, err := admission.DefaultMachineCapacityDirectory()
	if err != nil {
		return result, err
	}
	old, err := admission.ReadMachinePreparations(directory, *p.Generation.Preparation)
	preparation := old[*p.Generation.Preparation]
	root := p.OwnerBefore.Current.HostDir
	source := preparation.Source
	if err != nil || !preparation.Closed || source.Purpose != "host-resident" ||
		source.Bindings["host_root"] != root || source.Bindings["binary_sha256"] != p.Host.Binary.SHA256 {
		return result, errors.New("original closed resident preparation lineage differs")
	}
	var roots []string
	if err := json.Unmarshal([]byte(source.Bindings["planned_roots"]), &roots); err != nil || !slices.Contains(roots, root) {
		return result, errors.New("original resident disk scope unavailable")
	}
	originalGoal := loopruntime.SessionReadbackFileRef{Path: source.Bindings["goal_path"], SHA256: source.Bindings["goal_sha256"]}
	template := loopruntime.SessionReadbackFileRef{Path: source.Bindings["template_path"], SHA256: source.Bindings["template_sha256"]}
	if err := loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"goal": originalGoal, "template": template, "source_zero": zero}); err != nil {
		return result, err
	}
	profile, err := loopruntime.LoadOpenShellProfile(template.Path)
	if err != nil || profile.SourceSHA256 != source.Bindings["template_policy"] {
		return result, errors.New("original resident template profile changed")
	}
	claim, err := loopruntime.OpenShellLifecycleResourceClaim(profile, "host-resident", roots...)
	if err != nil {
		return result, err
	}
	source.Bindings["binary_path"], source.Bindings["binary_sha256"] = p.Successor.Path, p.Successor.SHA256
	source.Bindings["handoff_source_zero"] = zero.SHA256
	budget, err := preparationBudget(root + "/resident")
	if err != nil {
		return result, err
	}
	result.Preparation, err = budget.PrepareMachineResources(ctx, source, claim, admission.NewSampler())
	return result, err
}

func findRejectedReviewSuccessor(pool hostPool, p rejectedReviewReleasePlan, start rejectedReviewSuccessorStart) (hostGeneration, error) {
	var successor hostGeneration
	for id, g := range pool.Generations {
		if id == p.Generation.ID || !generationLive(g) {
			continue
		}
		if successor.ID != "" || g.Preparation == nil || *g.Preparation != start.Preparation ||
			g.Binary != p.Successor.Path || g.SHA != p.Successor.SHA256 {
			return successor, errors.New("original source has a competing successor generation")
		}
		successor = g
	}
	return successor, nil
}

func ensureRejectedReviewSuccessor(ctx context.Context, p rejectedReviewReleasePlan, intent loopruntime.SessionReadbackFileRef,
	start rejectedReviewSuccessorStart) (hostGeneration, error) {
	root := p.OwnerBefore.Current.HostDir
	pool, err := readHostPool(root)
	if err != nil {
		return hostGeneration{}, err
	}
	successor, err := findRejectedReviewSuccessor(pool, p, start)
	if err != nil {
		return successor, err
	}
	if successor.ID == "" {
		offered, err := releaseMarker(intent.Path+".successor-launch", intent.SHA256)
		if err != nil || offered {
			return successor, errors.New("original successor launch outcome remains unknown; no spawn replay")
		}
		if pool.Active != "" || pool.Generations[p.Generation.ID].State != "exited" {
			return successor, errors.New("original source is not fully retired")
		}
		budget, err := preparationBudget(root + "/resident")
		if err != nil {
			return successor, err
		}
		if err := budget.StartPreparationEffects(start.Preparation); err != nil {
			return successor, err
		}
		if err := saveHostStorageBytes(intent.Path+".successor-launch", []byte(intent.SHA256)); err != nil {
			return successor, err
		}
		ctx := context.WithValue(ctx, goalPreparationContextKey{}, goalResidentAdmission{root, p.Successor, start.Preparation})
		if err := ensureRollingHost(ctx, p.Successor.Path, root); err != nil {
			return successor, err
		}
		pool, err = readHostPool(root)
		if err != nil {
			return successor, err
		}
		successor, err = findRejectedReviewSuccessor(pool, p, start)
		if err != nil {
			return successor, err
		}
	}
	if successor.ID != "" && pool.Active == "" && successor.State == "starting" {
		if _, _, err := residentRetirementResources(ctx, root, successor, nativeEmptyHostRetirement(), true); err != nil {
			return successor, err
		}
		if err := updateHostPool(root, func(pool *hostPool) error {
			if pool.Active != "" || !reflect.DeepEqual(pool.Generations[successor.ID], successor) {
				return errors.New("original prepared successor changed before selection")
			}
			successor.State = "active"
			pool.Active, pool.Generations[successor.ID] = successor.ID, successor
			return nil
		}); err != nil {
			return successor, err
		}
		pool.Active = successor.ID
	}
	if successor.ID == "" || pool.Active != successor.ID || successor.State != "active" {
		return successor, errors.New("original successor Host has no exact active readback")
	}
	return successor, repairPoolSocket(root, successor.ID)
}

func readRejectedReviewSuccessor(ctx context.Context, p rejectedReviewReleasePlan, intent loopruntime.SessionReadbackFileRef,
	result rejectedReviewRoundRelease) (rejectedReviewRoundRelease, error) {
	start, err := readRejectedReviewSuccessorStart(p, intent)
	if err != nil {
		return result, err
	}
	result.Status, result.Next = "successor_admission_pending", p.next("adopt", intent.SHA256)
	root := p.OwnerBefore.Current.HostDir
	pool, err := readHostPool(root)
	if err != nil {
		return result, err
	}
	successor, err := findRejectedReviewSuccessor(pool, p, start)
	if err != nil {
		return result, err
	}
	result.SuccessorHost = successor.ID
	current := pool.Owners[p.OwnerBefore.Current.RequestID]
	if successor.ID == "" || current.Current.HostInstanceID != successor.ID {
		launchOffered, _ := releaseMarker(intent.Path+".successor-launch", intent.SHA256)
		if _, err := os.Lstat(intent.Path + ".successor-attach"); err == nil || successor.ID == "" && launchOffered {
			result.Next = p.next("reconcile", intent.SHA256)
		}
		return result, nil
	}
	expectedOld := p.OwnerBefore.Current
	expectedOld.ContextState, expectedOld.TerminalCause = "detached", loop.TerminalMigrationDetach
	if current.State != "owned" || current.Current.ContextState != "active" || current.Current.ContextID == "" ||
		current.Original != p.OwnerBefore.Original || len(current.Previous) != len(p.OwnerBefore.Previous)+1 ||
		!slices.Equal(current.Previous[:len(p.OwnerBefore.Previous)], p.OwnerBefore.Previous) ||
		current.Previous[len(current.Previous)-1] != expectedOld || pool.Active != successor.ID ||
		(current.MaintenanceHold != "" && current.MaintenanceHold != rejectedReviewReleaseHold+intent.SHA256) {
		return result, errors.New("original request or successor native lineage differs")
	}
	owner, err := readHostOwnerBindingWithHold(p.Request.Path, current.MaintenanceHold)
	if err != nil || owner.Request != p.Request || owner.ExecutionBinary != p.Successor || owner.Current != current.Current {
		return result, errors.New("original request has no independently confirmed successor context")
	}
	var fresh rejectedReviewTaskPreparation
	preparation, err := loopruntime.ReadSessionReadbackFile(start.SourceZero.Path+".task-preparation", &fresh)
	if err != nil || fresh.Schema != 1 || fresh.Source.SourceZero != start.SourceZero || fresh.Source.Request != p.Request ||
		fresh.Source.Successor != p.Successor || fresh.Preparation.ID != owner.Current.ContextID {
		return result, errors.New("original successor context does not consume its selected fresh preparation")
	}
	var offered loopruntime.SessionReadbackFileRef
	if _, err := loopruntime.ReadSessionReadbackFile(intent.Path+".successor-attach", &offered); err != nil || offered != preparation {
		return result, errors.New("original successor admission offer differs from its selected preparation")
	}
	if _, err := observeSessionActor(owner.Current.HostPID, p.Successor); err != nil {
		return result, err
	}
	if err := verifyRejectedReviewRetainedCheckout(ctx, p); err != nil {
		return result, err
	}
	status, err := os.ReadFile(filepath.Join(p.RuntimeDir, "status.json"))
	if err != nil {
		return result, err
	}
	mode, dispatch, err := readMaintenanceStatus(status)
	if err != nil || mode != p.LoopMode || dispatch != "paused" {
		return result, errors.New("original successor native Loop is not verified paused")
	}
	result.SuccessorContext = owner.Current.ContextID
	result.RoundState = &loop.HostContinuation{Mode: mode, DispatchState: loop.StatePaused}
	result.HostZero, result.Storage = "ZERO", &start.SourceZero
	var storage rejectedReviewStorageReceipt
	if _, err := loopruntime.ReadSessionReadbackFile(start.SourceZero.Path, &storage); err != nil {
		return result, err
	}
	var release rejectedReviewSourceRelease
	releaseRef, err := loopruntime.ReadSessionReadbackFile(intent.Path+".source-release", &release)
	if err != nil || release.Intent != intent || release.RoundState != *result.RoundState {
		return result, errors.New("original paused source release evidence differs")
	}
	result.SourceExit, result.SourceRelease, result.Maintenance, result.GatewayExit = &storage.SourceExit, &releaseRef, &storage.Maintenance, &release.GatewayExit
	zeroCount, zeroBytes := 0, int64(0)
	result.SourceProcessCount, result.SourceRSS, result.SourceDisk = &zeroCount, &zeroBytes, &zeroBytes
	if current.MaintenanceHold == "" {
		result.Status = "successor_round_adopted"
		result.Next = &rejectedReviewReleaseNext{Kind: "input", Owner: "Main", Required: []string{"original_paused_review_continuation"}}
	}
	return result, nil
}

func acceptRejectedReviewSuccessor(p rejectedReviewReleasePlan, intent loopruntime.SessionReadbackFileRef, result rejectedReviewRoundRelease) (rejectedReviewRoundRelease, error) {
	if result.Status == "successor_round_adopted" {
		return result, nil
	}
	if result.SuccessorContext == "" {
		return result, errors.New("original paused successor context requires readback")
	}
	root := p.OwnerBefore.Current.HostDir
	if err := updateHostPool(root, func(pool *hostPool) error {
		owner := pool.Owners[p.OwnerBefore.Current.RequestID]
		generation := pool.Generations[result.SuccessorHost]
		if pool.Active != result.SuccessorHost || generation.State != "active" || generation.Binary != p.Successor.Path ||
			generation.SHA != p.Successor.SHA256 || owner.Original != p.OwnerBefore.Original || owner.State != "owned" ||
			owner.Current.ContextState != "active" || owner.Current.ContextID != result.SuccessorContext ||
			owner.Current.HostInstanceID != result.SuccessorHost || owner.MaintenanceHold != rejectedReviewReleaseHold+intent.SHA256 ||
			owner.RoundState == nil || *owner.RoundState != (loop.HostContinuation{Mode: p.LoopMode, DispatchState: loop.StatePaused}) {
			return errors.New("original paused successor authority changed before hold release")
		}
		owner.MaintenanceHold = ""
		pool.Owners[p.OwnerBefore.Current.RequestID] = owner
		return nil
	}); err != nil {
		return result, err
	}
	result.Effects = append(result.Effects, "original_same_request_paused_successor_adopted")
	result.Status = "successor_round_adopted"
	result.Next = &rejectedReviewReleaseNext{Kind: "input", Owner: "Main", Required: []string{"original_paused_review_continuation"}}
	return result, nil
}

// Called only by the selected Host's original round_rebuild turn. This
// authorization never changes the ordinary attach and rebuild paths.
func (h *noodleHost) adoptRetiredRejectedReview(ctx context.Context, req hostRequest) hostResponse {
	refuse := func(err error) hostResponse { return hostError("refused", err) }
	if h.rolling == nil || req.RequestID == "" || req.RejectedReviewAdoption == nil || req.BinarySHA256 != h.binarySHA {
		return refuse(errors.New("original selected successor adoption binding is unavailable"))
	}
	runtimeDir, err := filepath.EvalSymlinks(filepath.Join(req.ProjectDir, ".noodle"))
	if err != nil {
		return refuse(err)
	}
	path := filepath.Join(runtimeDir, "rejected-review-round-release-"+hostDigest([]byte(req.RequestID))+".json")
	var p rejectedReviewReleasePlan
	intent, err := loopruntime.ReadSessionReadbackFile(path, &p)
	if err != nil || p.Schema != 1 || p.RuntimeDir != runtimeDir || p.OwnerBefore.Current.HostDir != h.controlRoot() ||
		p.Successor != (loopruntime.SessionReadbackFileRef{Path: h.binaryPath, SHA256: h.binarySHA}) ||
		!hostReceiptMatches(req, p.OwnerBefore.Original) || req.AdmissionSHA != p.OwnerBefore.Original.BinarySHA256 {
		return refuse(errors.New("original successor adoption intent or request differs"))
	}
	var offer loopruntime.SessionReadbackFileRef
	if _, err := loopruntime.ReadSessionReadbackFile(path+".successor-attach", &offer); err != nil ||
		offer != *req.RejectedReviewAdoption || offer.Path != path+".storage-zero.task-preparation" {
		return refuse(errors.New("original successor task preparation was not offered by the same handoff"))
	}
	if err := loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"successor_adoption": offer}); err != nil {
		return refuse(err)
	}
	start, err := readRejectedReviewSuccessorStart(p, intent)
	if err != nil {
		return refuse(err)
	}
	pool, err := readHostPool(h.controlRoot())
	if err != nil {
		return refuse(err)
	}
	successor, err := findRejectedReviewSuccessor(pool, p, start)
	if err != nil || successor.ID != h.instanceID || pool.Active != h.instanceID ||
		successor.State != "active" || pool.Generations[p.Generation.ID].State != "exited" {
		return refuse(errors.New("original successor generation, source exit or current routing differs"))
	}
	owner := pool.Owners[req.RequestID]
	if owner.State == "owned" && owner.Current.HostInstanceID == h.instanceID {
		return h.reconcileRollingRound(req)
	}
	hold := rejectedReviewReleaseHold + intent.SHA256
	expected := p.OwnerBefore
	expected.State, expected.ControllerOnly, expected.MaintenanceHold = "ready", true, hold
	expected.Current.ContextState, expected.Current.TerminalCause = "detached", loop.TerminalMigrationDetach
	expected.RoundState = &loop.HostContinuation{Mode: p.LoopMode, DispatchState: loop.StatePaused}
	if !reflect.DeepEqual(owner, expected) {
		return refuse(errors.New("original held paused source changed; no second successor admission offered"))
	}
	if err := verifyRejectedReviewRetainedCheckout(ctx, p); err != nil {
		return refuse(err)
	}
	if err := verifyRejectedReviewReleasePause(p.RuntimeDir, intent.Path+".pause"); err != nil {
		return refuse(err)
	}
	req.Operation, req.RoundState = "attach", owner.RoundState
	return h.attachRollingFromHold(ctx, req, req.RoundState, hold)
}
