package main

import (
	"errors"
	"io"
	"path/filepath"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
)

// The original Host supplies this callback; task JSON cannot assert readiness.
// Loop calls it only after startup is complete without a native worker attempt.
func (h *noodleHost) preparationReady(control worktree.TaskControl, plan hostTaskPreparationPlan, order string) func() (io.Closer, error) {
	return func() (io.Closer, error) {
		if err := verifyTaskPreparationPlan(plan); err != nil {
			return nil, err
		}
		if err := h.budget.VerifyPreparationSource(control.Preparation, plan.Source, plan.Claim); err != nil {
			return nil, err
		}
		files := make(map[string]admission.MachineSettlementFile)
		for role, ref := range plan.Sources {
			files[role] = admission.MachineSettlementFile{Path: ref.Path, SHA256: ref.SHA256}
		}
		for path, ref := range loopruntime.OpenShellProfileReadbackFiles(plan.Profile) {
			files["profile_source:"+path] = admission.MachineSettlementFile{Path: ref.Path, SHA256: ref.SHA256}
			if path == plan.Profile.SourcePath {
				files["profile"] = files["profile_source:"+path]
			}
		}
		var current worktree.TaskControl
		ref, err := loopruntime.ReadSessionReadbackFile(filepath.Join(filepath.Dir(control.ControlRoot), "control.json"), &current)
		if err != nil {
			return nil, err
		}
		expected := control
		if plan.OriginalControlPreparation.ID != "" {
			expected.Preparation = plan.OriginalControlPreparation
			if ref != plan.Sources["original_control"] {
				return nil, errors.New("original rejected review control evidence changed before readiness")
			}
		}
		if current != expected {
			return nil, errors.New("original preparation control changed before readiness")
		}
		files["control"] = admission.MachineSettlementFile{Path: ref.Path, SHA256: ref.SHA256}
		return h.budget.BeginPreparationWait(control.Preparation, control.Preparation.ID, order, files)
	}
}
