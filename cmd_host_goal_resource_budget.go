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
// native sampler supplies the original observations used by the Swap policy.
func registerBudgetedGoalHostTemplate(ctx context.Context, path, profile, class string, sampler admission.Sampler) (loopruntime.SessionReadbackFileRef, error) {
	ref, err := prepareBudgetedGoalHostTemplate(ctx, path, profile, class, sampler)
	if err != nil {
		return ref, err
	}
	return registerGoalHostTemplate(ctx, path, ref.Path)
}

func prepareBudgetedGoalHostTemplate(ctx context.Context, path, profile, class string, sampler admission.Sampler, readers ...func() (*admission.MachineCapacityPolicyReadback, error)) (loopruntime.SessionReadbackFileRef, error) {
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
	if hardware.Unsupported {
		return empty, fmt.Errorf("%w: native resource class sampler unsupported", admission.ErrDeferred)
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
	boundBaseline := false
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
		observed, ok, e := loopruntime.ReadGoalResourceBudgetHardware(prior, original)
		if e != nil {
			return empty, e
		}
		if ok {
			if hardware.TotalMemoryBytes>>20 != observed.TotalMemoryBytes>>20 {
				priorClass, e := loopruntime.OpenShellResourceBudgetClass(prior)
				if e != nil {
					return empty, e
				}
				if priorClass == class {
					return ref, loopruntime.VerifyOpenShellProfile(original)
				}
				hardware.TotalMemoryBytes = observed.TotalMemoryBytes
			}
			hardware.SwapUsedBytes = observed.SwapUsedBytes
			boundBaseline = true
		}
	} else if !os.IsNotExist(readErr) {
		return empty, readErr
	}
	// Preserve access to the previous publication identity.
	identity, err := json.Marshal(struct {
		Class, Source string
		Memory        uint64
		CPUs          int
	}{class, original.SourceSHA256, hardware.TotalMemoryBytes, hardware.CPUs})
	if err != nil {
		return empty, err
	}
	legacyRoot := filepath.Join(filepath.Dir(path), "resource-templates", hostDigest(identity))
	if _, readErr := os.Lstat(legacyRoot); readErr == nil {
		if resolved, e := filepath.EvalSymlinks(legacyRoot); e != nil || resolved != legacyRoot {
			return empty, errors.New("original resource template publication is aliased")
		}
		prior, e := loopruntime.LoadOpenShellProfile(filepath.Join(legacyRoot, "openshell", "profile.json"))
		if e != nil {
			return empty, fmt.Errorf("original resource template publication unavailable: %w", e)
		}
		priorClass, e := loopruntime.OpenShellResourceBudgetClass(prior)
		if e != nil {
			return empty, fmt.Errorf("original resource template publication class unavailable: %w", e)
		}
		if priorClass != class {
			return empty, errors.New("original resource template publication class differs")
		}
		baseline, ok, e := loopruntime.GoalResourceBudgetBaseline(prior, original, hardware)
		if e != nil {
			return empty, fmt.Errorf("original resource template publication binding differs: %w", e)
		}
		if !ok {
			return empty, errors.New("original resource template publication class, source or hardware differs")
		}
		if boundBaseline && hardware.SwapUsedBytes != baseline {
			return empty, errors.New("original registered and prepared resource publication baselines conflict")
		}
		hardware.SwapUsedBytes = baseline
		boundBaseline = true
	} else if !os.IsNotExist(readErr) {
		return empty, readErr
	}
	publicationIdentity, err := json.Marshal(struct {
		Format, Class, Source, Path string
	}{"native-ram-observation-only-v1", class, original.SourceSHA256, original.SourcePath})
	if err != nil {
		return empty, err
	}
	root := filepath.Join(filepath.Dir(path), "resource-templates", hostDigest(publicationIdentity))
	previousPublication := false
	// A verified publication under the previous identity remains reusable.
	if hardware.TotalMemoryBytes != 0 {
		if _, readErr := os.Lstat(root); os.IsNotExist(readErr) {
			previousRoot, e := previousGoalResourceTemplateRoot(path, class, original.SourceSHA256, hardware.TotalMemoryBytes)
			if e != nil {
				return empty, e
			}
			if _, e = os.Lstat(filepath.Join(previousRoot, "openshell")); e == nil {
				root = previousRoot
				previousPublication = true
			} else if !os.IsNotExist(e) {
				return empty, e
			}
		} else if readErr != nil {
			return empty, readErr
		}
	}
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
	if boundBaseline || previousPublication {
		priorPath := filepath.Join(root, "openshell", "profile.json")
		if _, e := os.Lstat(priorPath); e == nil {
			prior, e := loopruntime.LoadOpenShellProfile(priorPath)
			if e != nil {
				return empty, e
			}
			observed, ok, e := loopruntime.ReadGoalResourceBudgetHardware(prior, original)
			if e != nil {
				return empty, e
			}
			if previousPublication && (!ok || observed.TotalMemoryBytes>>20 != hardware.TotalMemoryBytes>>20) {
				return empty, errors.New("original resource template publication source or hardware differs")
			}
			if boundBaseline && ok && observed.SwapUsedBytes != hardware.SwapUsedBytes {
				return empty, errors.New("original registered and prepared resource publication baselines conflict")
			}
		} else if !os.IsNotExist(e) {
			return empty, e
		}
	}
	receipt, err := loopruntime.MaterializeBudgetedGoalOpenShellProfile(profile, root, class, hardware, readers...)
	if err != nil {
		return empty, err
	}
	if err = loopruntime.VerifyOpenShellProfile(original); err != nil {
		return empty, err
	}
	return receipt.Profile, nil
}

func previousGoalResourceTemplateRoot(path, class, source string, memory uint64) (string, error) {
	identity, err := json.Marshal(struct {
		Class, Source string
		Memory        uint64
	}{class, source, memory})
	if err != nil {
		return "", err
	}
	identity, err = json.Marshal(struct {
		Format   string
		Identity json.RawMessage
	}{"native-cpu-observation-only-v1", identity})
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "resource-templates", hostDigest(identity)), nil
}
