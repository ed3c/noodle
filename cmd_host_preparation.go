package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/execution"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
	"github.com/poteto/noodle/worktree/archive"
)

type hostTaskPreparationPlan struct {
	Source  admission.PreparationSource
	Claim   admission.ResourceClaim
	Storage hostStoragePlan
	Archive hostArchivePlan
	Profile loopruntime.OpenShellProfile
	Sources map[string]loopruntime.SessionReadbackFileRef
}

type hostPreparationReader func(context.Context, hostRequest, []byte) (worktree.TaskControl, hostTaskPreparationPlan, error)

func taskPreparationPlan(ctx context.Context, hostRoot string, control worktree.TaskControl, environment []string, binary loopruntime.SessionReadbackFileRef) (hostTaskPreparationPlan, error) {
	var result hostTaskPreparationPlan
	if _, err := archive.FileIdentity(control.Repository); err != nil {
		return result, fmt.Errorf("%w: original archive metadata unsupported: %v", admission.ErrDeferred, err)
	}
	profile, err := loopruntime.LoadOpenShellProfile(filepath.Join(hostRoot, "openshell", "profile.json"))
	if err != nil {
		return result, err
	}
	if profile.Gateway.Binding.StateRoot != filepath.Join(hostRoot, "openshell", "custody") {
		return result, errors.New("original task preparation profile belongs to another Host")
	}
	var task struct {
		GoalSource string `json:"goal_source"`
	}
	ref, err := loopruntime.ReadSessionReadbackFile(control.Task.Path, &task)
	if err != nil || ref.Path != control.Task.Path || ref.SHA256 != control.Task.SHA256 {
		return result, errors.New("original task preparation source changed")
	}
	goal := ref
	if task.GoalSource != "" {
		goal, err = loopruntime.ReadSessionReadbackFile(task.GoalSource, nil)
		if err != nil {
			return result, err
		}
	}
	result.Profile = profile
	result.Sources = map[string]loopruntime.SessionReadbackFileRef{"task": ref, "goal": goal, "execution_binary": binary}
	result.Storage, err = planHostStorage(ctx, hostRequest{ProjectDir: control.ControlRoot, Environment: environment}, "", control.Repository)
	if err != nil || result.Storage.Source.StorageRoot == "" {
		return result, fmt.Errorf("original task preparation storage plan missing: %w", err)
	}
	e, err := execution.New(control.ControlRoot, environment)
	if err != nil {
		return result, err
	}
	result.Archive, err = planHostCheckoutArchive(ctx, e, filepath.Join(result.Storage.Source.StorageRoot, ".worktrees"))
	if err != nil {
		return result, err
	}
	temporary, err := hostPlannedPath(filepath.Clean(os.TempDir()))
	if err != nil {
		return result, err
	}
	roots := []string{hostRoot, control.ControlRoot, result.Storage.Source.CommonDir, result.Storage.Source.StorageRoot, temporary}
	roots = append(roots, result.Archive.diskRoots()...)
	result.Claim, err = loopruntime.OpenShellLifecycleResourceClaim(profile, "task-preparation", roots...)
	if err != nil {
		return result, fmt.Errorf("%w: %v", admission.ErrDeferred, err)
	}
	storageRaw, err := json.Marshal(result.Storage)
	if err != nil {
		return result, err
	}
	archiveRaw, err := json.Marshal(result.Archive)
	if err != nil {
		return result, err
	}
	var archiveSource string
	if result.Archive.Bound != "" {
		ref, err := loopruntime.ReadSessionReadbackFile(result.Archive.Bound, nil)
		if err != nil {
			return result, err
		}
		archiveSource = ref.Path + "/" + ref.SHA256
		result.Sources["archive_policy"] = ref
	} else {
		archiveSource = "native-default/" + hostDigest(archiveRaw)
	}
	codecPath, err := filepath.EvalSymlinks(result.Archive.Config.Codec)
	if err != nil {
		return result, err
	}
	codec, err := loopruntime.ReadSessionReadbackFile(codecPath, nil)
	if err != nil {
		return result, err
	}
	result.Sources["archive_codec"] = codec
	rootRaw, err := json.Marshal(roots)
	if err != nil {
		return result, err
	}
	result.Source = admission.PreparationSource{Purpose: "task-preparation", Bindings: map[string]string{
		"task_path": control.Task.Path, "task_sha256": control.Task.SHA256,
		"goal_path": goal.Path, "goal_sha256": goal.SHA256,
		"repository": control.Repository, "head": control.Head, "control_root": control.ControlRoot,
		"host_root": hostRoot, "profile": profile.SourceSHA256, "execution_binary": binary.Path + "/" + binary.SHA256,
		"storage": hostDigest(storageRaw), "archive": archiveSource, "codec": codec.Path + "/" + codec.SHA256,
		"planned_roots": string(rootRaw),
	}}
	return result, nil
}

func preparationBudget(owner string) (*admission.Budget, error) {
	directory, err := admission.DefaultMachineCapacityDirectory()
	if err != nil {
		return nil, err
	}
	budget := admission.NewBudget(admission.DefaultWorkerLimit)
	if err = budget.BindMachineCapacity(directory, owner); err != nil {
		return nil, err
	}
	return budget, nil
}

func reserveTaskControlPreparation(ctx context.Context, hostRoot string, control worktree.TaskControl) (admission.PreparationRef, error) {
	pool, err := readHostPool(hostRoot)
	if err != nil {
		return admission.PreparationRef{}, fmt.Errorf("%w: original preparation Host readback unavailable: %v", admission.ErrDeferred, err)
	}
	generation, exists := pool.Generations[pool.Active]
	if !exists || generation.State != "active" {
		return admission.PreparationRef{}, fmt.Errorf("%w: original active Host generation unavailable", admission.ErrDeferred)
	}
	binary, err := hostReadbackBinaryRef(generation.Binary)
	if err != nil || binary.SHA256 != generation.SHA {
		return admission.PreparationRef{}, errors.New("original preparation execution binary changed")
	}
	plan, err := taskPreparationPlan(ctx, hostRoot, control, os.Environ(), binary)
	if err != nil {
		return admission.PreparationRef{}, err
	}
	budget, err := preparationBudget(filepath.Dir(control.ControlRoot))
	if err != nil {
		return admission.PreparationRef{}, err
	}
	ref, err := budget.PrepareMachineResources(ctx, plan.Source, plan.Claim, admission.NewSampler())
	if err != nil {
		return ref, err
	}
	if err = verifyTaskPreparationPlan(plan); err != nil {
		return ref, errors.Join(err, budget.CancelUnstartedPreparation(ref))
	}
	return ref, budget.StartPreparationEffects(ref)
}

func verifyTaskPreparationPlan(plan hostTaskPreparationPlan) error {
	codec, err := filepath.EvalSymlinks(plan.Archive.Config.Codec)
	if err != nil || codec != plan.Sources["archive_codec"].Path {
		return errors.New("original archive codec alias changed")
	}
	if err := loopruntime.VerifySessionReadbackFiles(plan.Sources); err != nil {
		return err
	}
	return loopruntime.VerifyOpenShellProfile(plan.Profile)
}

// The Host consumes the original native control journal, rather than accepting
// an arbitrary preparation ID or authority flag from a request.
func (h *noodleHost) taskPreparation(ctx context.Context, req hostRequest, data []byte) (worktree.TaskControl, hostTaskPreparationPlan, error) {
	var control worktree.TaskControl
	var plan hostTaskPreparationPlan
	cfg, err := config.Parse(data)
	if err != nil {
		return control, plan, err
	}
	if h.budget == nil || len(cfg.Runtime.AdmittedOrders) != 1 || cfg.Runtime.OpenShell.Profile != filepath.Join(h.controlRoot(), "openshell", "profile.json") {
		return control, plan, fmt.Errorf("%w: original task preparation unsupported for this context", admission.ErrDeferred)
	}
	if _, err = loopruntime.ReadSessionReadbackFile(filepath.Join(filepath.Dir(req.ProjectDir), "control.json"), &control); err != nil {
		return control, plan, err
	}
	if control.Schema != 2 || control.Status != "prepared" || control.ControlRoot != req.ProjectDir || filepath.Dir(filepath.Dir(req.ProjectDir)) != filepath.Join(h.controlRoot(), "controls") {
		return control, plan, fmt.Errorf("%w: original task control preparation missing", admission.ErrDeferred)
	}
	plan, err = taskPreparationPlan(ctx, h.controlRoot(), control, req.Environment, loopruntime.SessionReadbackFileRef{Path: h.binaryPath, SHA256: h.binarySHA})
	if err != nil {
		return control, plan, err
	}
	actualStorage, err := planHostStorage(ctx, req, "", "")
	if err != nil || actualStorage != plan.Storage {
		return control, plan, errors.New("original control Git owner or planned storage changed")
	}
	if err = h.budget.VerifyPreparationSource(control.Preparation, plan.Source, plan.Claim); err != nil {
		return control, plan, err
	}
	if err = verifyTaskPreparationPlan(plan); err != nil {
		return control, plan, err
	}
	if err = h.budget.BindPreparation(control.Preparation, control.Preparation.ID, cfg.Runtime.AdmittedOrders[0], "openshell"); err != nil {
		return control, plan, err
	}
	return control, plan, nil
}
