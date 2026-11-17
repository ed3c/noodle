package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
	loopruntime "github.com/poteto/noodle/runtime"
)

// The existing template command is the only policy registration entry. Its
// native sampler supplies physical capacity once, not per-task estimates.
func registerBudgetedGoalHostTemplate(ctx context.Context, path, profile, class string, sampler admission.Sampler) (loopruntime.SessionReadbackFileRef, error) {
	ref, err := prepareBudgetedGoalHostTemplate(ctx, path, profile, class, sampler)
	if err != nil {
		return ref, err
	}
	return registerGoalHostTemplate(ctx, path, ref.Path)
}

func prepareBudgetedGoalHostTemplate(ctx context.Context, path, profile, class string, sampler admission.Sampler) (loopruntime.SessionReadbackFileRef, error) {
	var empty loopruntime.SessionReadbackFileRef
	if (class != loopruntime.ConservativeGoalResourceBudget && class != loopruntime.QualificationGoalResourceBudget) || sampler == nil || !filepath.IsAbs(path) || !filepath.IsAbs(profile) {
		return empty, errors.New("unsupported Goal resource budget or original source")
	}
	source, err := loopruntime.ReadSessionReadbackFile(profile, nil)
	if err != nil {
		return empty, err
	}
	profile = source.Path
	original, err := loopruntime.LoadOpenShellProfile(profile)
	if err != nil {
		return empty, err
	}
	readCtx, cancel := context.WithTimeout(ctx, admission.SampleTimeout)
	defer cancel()
	hardware, err := sampler.Read(readCtx)
	if err != nil {
		return empty, fmt.Errorf("%w: native resource class hardware: %v", admission.ErrDeferred, err)
	}
	if hardware.Unsupported || hardware.TotalMemoryBytes == 0 || hardware.CPUs <= 0 {
		return empty, fmt.Errorf("%w: native resource class hardware unavailable", admission.ErrDeferred)
	}
	if err = loopruntime.VerifyOpenShellProfile(original); err != nil {
		return empty, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return empty, err
	}
	publication, err := lockfile.Acquire(ctx, path+".resource.lock")
	if err != nil {
		return empty, err
	}
	defer publication.Close()
	// The original registered template fixes the first observed stock. A
	// second class uses that same policy; retries cannot ratchet it upward.
	if raw, readErr := os.ReadFile(path); readErr == nil {
		var installed goalHostTemplateRegistration
		if err = json.Unmarshal(raw, &installed); err != nil {
			return empty, err
		}
		if installed.Schema != 1 {
			return empty, errors.New("original template registration schema differs")
		}
		prior, e := loopruntime.LoadOpenShellProfile(installed.Profile.Path)
		if e != nil {
			return empty, e
		}
		ref, e := loopruntime.ReadSessionReadbackFile(installed.Profile.Path, nil)
		if e != nil || ref != installed.Profile || prior.SourceSHA256 != installed.PolicySHA256 {
			return empty, errors.New("original registered resource template changed")
		}
		baseline, ok, e := loopruntime.GoalResourceBudgetBaseline(prior, original, hardware)
		if e != nil {
			return empty, e
		}
		if ok {
			hardware.SwapUsedBytes = baseline
		}
	} else if !os.IsNotExist(readErr) {
		return empty, readErr
	}
	// Capacity, never instantaneous free space/RSS/pressure, fixes the class.
	identity, err := json.Marshal(struct {
		Class, Source string
		Memory        uint64
		CPUs          int
	}{class, original.SourceSHA256, hardware.TotalMemoryBytes, hardware.CPUs})
	if err != nil {
		return empty, err
	}
	root := filepath.Join(filepath.Dir(path), "resource-templates", hostDigest(identity))
	if err = os.MkdirAll(root, 0700); err != nil {
		return empty, err
	}
	if resolved, e := filepath.EvalSymlinks(root); e != nil || resolved != root {
		return empty, errors.New("Goal resource template destination is aliased")
	}
	lock, err := lockfile.Acquire(ctx, root+".lock")
	if err != nil {
		return empty, err
	}
	defer lock.Close()
	receipt, err := loopruntime.MaterializeBudgetedGoalOpenShellProfile(profile, root, class, hardware)
	if err != nil {
		return empty, err
	}
	if err = loopruntime.VerifyOpenShellProfile(original); err != nil {
		return empty, err
	}
	return receipt.Profile, nil
}
