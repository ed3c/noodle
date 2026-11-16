package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

type goalHostPreparation func(context.Context, string, goalHostSelection, loopruntime.OpenShellProfile) (goalHostSelection, error)
type goalPreparationContextKey struct{}
type goalResidentAdmission struct {
	Root        string
	Binary      loopruntime.SessionReadbackFileRef
	Preparation admission.PreparationRef
}

func prepareGoalHostResources(ctx context.Context, root string, selected goalHostSelection, profile loopruntime.OpenShellProfile) (goalHostSelection, error) {
	bindings := map[string]string{
		"goal_path": selected.Goal.Path, "goal_sha256": selected.Goal.SHA256,
		"template_path": selected.Template.Path, "template_sha256": selected.Template.SHA256,
		"template_policy": profile.SourceSHA256,
		"binary_path":     selected.Noodle.Path, "binary_sha256": selected.Noodle.SHA256,
		"native_root": root,
	}
	if selected.Operation != nil {
		bindings["operation_path"], bindings["operation_sha256"] = selected.Operation.Path, selected.Operation.SHA256
	}
	if len(selected.Previous) != 0 {
		bindings["previous_host"] = selected.Previous[len(selected.Previous)-1].Root
	}
	identity, err := json.Marshal(bindings)
	if err != nil {
		return selected, err
	}
	selected.Root = filepath.Join(root, "h", hostDigest(identity)[:16])
	bindings["host_root"] = selected.Root
	temporary, err := hostPlannedPath(filepath.Clean(os.TempDir()))
	if err != nil {
		return selected, err
	}
	roots := []string{selected.Root, filepath.Join(root, "goals"), temporary}
	claim, err := loopruntime.OpenShellLifecycleResourceClaim(profile, "host-resident", roots...)
	if err != nil {
		return selected, fmt.Errorf("%w: %v", admission.ErrDeferred, err)
	}
	rootData, err := json.Marshal(roots)
	if err != nil {
		return selected, err
	}
	bindings["planned_roots"] = string(rootData)
	budget, err := preparationBudget(selected.Root + "/resident")
	if err != nil {
		return selected, err
	}
	ref, err := budget.PrepareMachineResources(ctx, admission.PreparationSource{Purpose: "host-resident", Bindings: bindings}, claim, admission.NewSampler())
	if err != nil {
		return selected, err
	}
	selected.Preparation = ref
	refs := map[string]loopruntime.SessionReadbackFileRef{"goal": selected.Goal, "template": selected.Template, "binary": selected.Noodle}
	if selected.Operation != nil {
		refs["operation"] = *selected.Operation
	}
	err = loopruntime.VerifySessionReadbackFiles(refs)
	if err == nil {
		err = loopruntime.VerifyOpenShellProfile(profile)
	}
	if err != nil {
		return selected, errors.Join(err, budget.CancelUnstartedPreparation(ref))
	}
	if err = budget.StartPreparationEffects(ref); err != nil {
		return selected, err
	}
	return selected, nil
}

func verifyGoalResidentPreparation(selected goalHostSelection) error {
	budget, err := preparationBudget(selected.Root + "/resident")
	if err != nil {
		return err
	}
	source, _, effects, err := budget.ReadPreparation(selected.Preparation)
	if err != nil {
		return err
	}
	if !effects || source.Purpose != "host-resident" || source.Bindings["host_root"] != selected.Root || source.Bindings["goal_path"] != selected.Goal.Path || source.Bindings["goal_sha256"] != selected.Goal.SHA256 || source.Bindings["binary_path"] != selected.Noodle.Path || source.Bindings["binary_sha256"] != selected.Noodle.SHA256 || source.Bindings["template_path"] != selected.Template.Path || source.Bindings["template_sha256"] != selected.Template.SHA256 {
		return errors.New("original Goal resident source differs")
	}
	profile, err := loopruntime.LoadOpenShellProfile(selected.Template.Path)
	if err != nil {
		return err
	}
	if source.Bindings["template_policy"] != profile.SourceSHA256 {
		return errors.New("original Goal resident policy changed")
	}
	return loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"goal": selected.Goal, "template": selected.Template, "binary": selected.Noodle})
}
