package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
)

func verifyQualificationHostGoal(root string, goal loopruntime.SessionReadbackFileRef) error {
	pool, err := readHostPool(root)
	if err != nil {
		return err
	}
	generation, exists := pool.Generations[pool.Active]
	if !exists || generation.Preparation == nil {
		return fmt.Errorf("%w: original qualification Host Goal lifetime missing", admission.ErrDeferred)
	}
	budget, err := preparationBudget(root + "/resident")
	if err != nil {
		return err
	}
	source, _, effects, err := budget.ReadPreparation(*generation.Preparation)
	if err != nil {
		return err
	}
	if !effects || source.Purpose != "host-resident" || source.Bindings["host_root"] != root || source.Bindings["goal_path"] != goal.Path || source.Bindings["goal_sha256"] != goal.SHA256 {
		return fmt.Errorf("%w: task belongs to another original qualification Goal", admission.ErrDeferred)
	}
	return nil
}

func verifyQualificationPreparation(ctx context.Context, profile loopruntime.OpenShellProfile, plan *hostTaskPreparationPlan) error {
	if plan == nil || plan.Source.Purpose != "task-preparation" {
		return fmt.Errorf("%w: original qualification task preparation missing", admission.ErrDeferred)
	}
	goal, exists := plan.Sources["goal"]
	if !exists {
		return errors.New("original qualification Goal source missing")
	}
	if _, err := loopruntime.ReadOpenShellGoalExecutionConstraints(profile, goal); err != nil {
		return err
	}
	task, exists := plan.Sources["task"]
	if !exists {
		return errors.New("original qualification task source missing")
	}
	if err := loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"task": task}); err != nil {
		return err
	}
	binding := plan.Source.Bindings
	return worktree.VerifySmallQualificationRepository(ctx, worktree.TaskControl{Repository: binding["repository"], Head: binding["head"], ControlRoot: binding["control_root"]})
}
