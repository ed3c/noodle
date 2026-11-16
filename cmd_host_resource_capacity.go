package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

func standalonePreparationAdmission() error {
	return fmt.Errorf("%w: standalone runtime preparation unsupported; use the original Goal Host admission", admission.ErrDeferred)
}

func (h *noodleHost) bindMachineCapacity() {
	directory, err := admission.DefaultMachineCapacityDirectory()
	if err != nil {
		_ = h.budget.BindMachineCapacity("", "")
		return
	}
	root := h.controlRoot()
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || canonical != root {
		_ = h.budget.BindMachineCapacity("", "")
		return
	}
	// No task or Host environment override can select a separate machine pool.
	_ = h.budget.BindMachineCapacity(directory, root+"/generation/"+h.instanceID)
}

func (h *noodleHost) bindWorkerResourceSource(id, project, storage string, profile *loopruntime.OpenShellProfile, preparedRoots ...string) {
	if h.budget == nil {
		return
	}
	var selected loopruntime.OpenShellProfile
	if profile != nil {
		selected = *profile
	}
	roots := []string{project, h.controlRoot(), os.TempDir()}
	if storage != "" {
		roots = append(roots, storage)
	}
	roots = append(roots, preparedRoots...)
	for i, root := range roots {
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			roots[i] = resolved
		}
	}
	generationGuard := h.machineGenerationGuard()
	h.budget.BindResourceSource(id, "openshell", func(runtime string) (admission.ResourceClaim, error) {
		if runtime != "openshell" || profile == nil {
			return admission.ResourceClaim{}, errors.New("worker_resource_runtime_unsupported: " + runtime)
		}
		if err := generationGuard(); err != nil {
			return admission.ResourceClaim{}, err
		}
		if len(preparedRoots) != 0 {
			return loopruntime.OpenShellPreparedWorkerResourceClaim(selected, roots...)
		}
		return loopruntime.OpenShellWorkerResourceClaim(selected, roots...)
	})
}

func verifyMachineRollingTarget(ctx context.Context, binary, digest string) error {
	info, err := os.Lstat(binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return errors.New("machine_policy_reader_binary_unavailable")
	}
	data, err := os.ReadFile(binary)
	if err != nil || hostDigest(data) != digest {
		return errors.New("machine_policy_reader_binary_changed")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	raw, err := hostCapabilityCommand(ctx, binary).Output()
	var capabilities struct {
		Admission int    `json:"machine_worker_admission"`
		Policy    string `json:"machine_admission_policy"`
	}
	if err != nil || json.Unmarshal(raw, &capabilities) != nil || capabilities.Admission != 1 || capabilities.Policy != admission.MachineAdmissionPolicy {
		return errors.New("generation_machine_policy_unsupported_or_unknown")
	}
	return nil
}

func (h *noodleHost) machineGenerationGuard() func() error {
	if h.rolling == nil {
		return func() error { return nil }
	}
	root := h.rolling.root
	known := map[string]bool{h.binarySHA: true}
	return func() error {
		pool, err := readHostPool(root)
		if err != nil {
			return errors.New("generation_machine_policy_readback_unavailable")
		}
		for _, generation := range pool.Generations {
			if generation.ID == h.instanceID || generation.State == "exited" {
				continue
			}
			ready, seen := known[generation.SHA]
			if !seen {
				ready = verifyMachineRollingTarget(context.Background(), generation.Binary, generation.SHA) == nil
				known[generation.SHA] = ready
			}
			if !ready {
				return errors.New("generation_machine_policy_unknown_original_owner_retained")
			}
		}
		return nil
	}
}
