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
	"github.com/poteto/noodle/internal/filex"
	loopruntime "github.com/poteto/noodle/runtime"
)

func verifyGenerationPreparation(root, binary, digest string, ref admission.PreparationRef) error {
	budget, err := preparationBudget(root + "/resident")
	if err != nil {
		return err
	}
	source, _, effects, err := budget.ReadPreparation(ref)
	if err != nil {
		return err
	}
	if !effects || source.Purpose != "host-resident" || source.Bindings["host_root"] != root || source.Bindings["binary_path"] != binary || source.Bindings["binary_sha256"] != digest {
		return errors.New("original resident preparation source or execution binary differs")
	}
	if _, err = os.Lstat(filepath.Join(root, "openshell", "materialization.json")); err != nil {
		return err
	}
	reservation, err := loopruntime.ReadGoalOpenShellReservation(root)
	if err != nil {
		return err
	}
	if reservation.Template.Path != source.Bindings["template_path"] || reservation.Template.SHA256 != source.Bindings["template_sha256"] || reservation.TemplatePolicySHA256 != source.Bindings["template_policy"] {
		return errors.New("original resident profile materialization differs")
	}
	return nil
}

// This runs before hostDirectory can create anything. Only the generation
// started by the original native pool may consume its exact lifetime.
func verifyServingResident(ctx context.Context) error {
	root, id := os.Getenv("NOODLE_HOST_POOL_ROOT"), os.Getenv("NOODLE_HOST_GENERATION")
	if !filepath.IsAbs(root) || !validPoolID(id) {
		return fmt.Errorf("%w: standalone or unpooled resident preparation unsupported", admission.ErrDeferred)
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return err
	}
	ref, err := hostReadbackBinaryRef(binary)
	if err != nil {
		return err
	}
	// The parent saves its returned PID immediately after Start. Read that same
	// starting operation; do not manufacture another generation on a lost ACK.
	ctx, cancel := context.WithTimeout(ctx, admission.PollInterval)
	defer cancel()
	for {
		pool, err := readHostPool(root)
		if err != nil {
			return err
		}
		g, exists := pool.Generations[id]
		if !exists || g.Preparation == nil || g.Binary != ref.Path || g.SHA != ref.SHA256 || g.Directory != filepath.Join(root, "g", id) {
			return errors.New("original serving resident generation differs")
		}
		if g.Process.PID == os.Getpid() && g.Process.SessionID == id && !g.Process.StartedAt.IsZero() {
			return verifyGenerationPreparation(root, ref.Path, ref.SHA256, *g.Preparation)
		}
		if g.Process.PID != 0 || g.State != "starting" {
			return errors.New("original resident process identity differs")
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%w: original resident PID readback pending", admission.ErrDeferred)
		case <-timer.C:
		}
	}
}

type residentExitWitness struct {
	Schema      int                      `json:"schema"`
	Root        string                   `json:"host_root"`
	Generation  string                   `json:"generation"`
	PID         int                      `json:"host_pid"`
	Preparation admission.PreparationRef `json:"machine_preparation"`
	Residents   []admission.Resident     `json:"residents"`
}

// The original actor calls this only at its already-validated shutdown/retire
// boundary. The login Source is protected and never participates in this GC.
func (h *noodleHost) saveResidentExitWitness() error {
	if h.rolling == nil || h.prepareContext != nil || h.sourceService != nil {
		return nil
	}
	pool, err := readHostPool(h.controlRoot())
	if err != nil {
		return err
	}
	g, exists := pool.Generations[h.instanceID]
	if !exists || g.Preparation == nil || g.Process.PID != os.Getpid() {
		return errors.New("original resident exit generation missing")
	}
	rows, err := h.residents.Snapshot()
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.State != "exited" || len(row.Contexts) != 0 {
			return errors.New("original resident exit users or effects unresolved")
		}
	}
	witness := residentExitWitness{1, h.controlRoot(), h.instanceID, os.Getpid(), *g.Preparation, rows}
	raw, err := json.Marshal(witness)
	if err != nil {
		return err
	}
	path := filepath.Join(h.dir, "resource-resident-exit.json")
	if prior, err := os.ReadFile(path); err == nil {
		if string(prior) != string(raw) {
			return errors.New("original resident exit witness changed")
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if err = filex.WriteFileAtomicDurable(path, raw); err != nil {
		return err
	}
	ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
	if err != nil {
		return err
	}
	return updateHostPool(h.controlRoot(), func(pool *hostPool) error {
		current := pool.Generations[h.instanceID]
		if current.Preparation == nil || *current.Preparation != *g.Preparation || current.Process != g.Process {
			return errors.New("original resident exit owner changed")
		}
		if current.ResidentExit != nil && *current.ResidentExit != ref {
			return errors.New("original resident exit reference changed")
		}
		current.ResidentExit = &ref
		pool.Generations[h.instanceID] = current
		return nil
	})
}

func returnHostResidentResources(root string, g hostGeneration) error {
	if g.Preparation == nil {
		return errors.New("legacy resident is not participating; original resources retained")
	}
	if g.SourceService != nil {
		return errors.New("login Source resident is protected")
	}
	budget, err := preparationBudget(root + "/resident")
	if err != nil {
		return err
	}
	return returnHostResidentResourcesWithBudget(root, g, budget)
}

func reclaimGoalResident(root string) error {
	pool, err := readHostPool(root)
	if err != nil {
		return err
	}
	g, exists := pool.Generations[pool.Active]
	if !exists {
		return errors.New("original Goal resident generation missing")
	}
	// An old unparticipating generation grants no capacity return authority.
	if g.Preparation == nil {
		return nil
	}
	return returnHostResidentResources(root, g)
}

func returnHostResidentResourcesWithBudget(root string, g hostGeneration, budget *admission.Budget) error {
	if g.Preparation == nil || g.SourceService != nil {
		return errors.New("original resident lifetime unavailable or protected")
	}
	return budget.CloseResidentPreparation(*g.Preparation, func(source admission.PreparationSource) (map[string]admission.MachineSettlementFile, error) {
		if source.Bindings["host_root"] != root || source.Bindings["binary_path"] != g.Binary || source.Bindings["binary_sha256"] != g.SHA {
			return nil, errors.New("original resident execution source differs")
		}
		if g.ResidentExit == nil || g.ResidentExit.Path != filepath.Join(g.Directory, "resource-resident-exit.json") || g.Process.PID <= 0 || g.Process.SessionID != g.ID || g.Process.StartedAt.IsZero() {
			return nil, errors.New("original resident exit proof missing")
		}
		var witness residentExitWitness
		ref, err := loopruntime.ReadSessionReadbackFile(g.ResidentExit.Path, &witness)
		if err != nil || ref != *g.ResidentExit {
			return nil, errors.New("original resident exit proof changed")
		}
		if witness.Schema != 1 || witness.Root != root || witness.Generation != g.ID || witness.PID != g.Process.PID || witness.Preparation != *g.Preparation || witness.Residents == nil {
			return nil, errors.New("original resident exit subject differs")
		}
		for _, row := range witness.Residents {
			if row.State != "exited" || len(row.Contexts) != 0 {
				return nil, errors.New("original resident effects unresolved")
			}
		}
		absent, err := g.Process.GroupAbsent()
		if err != nil || !absent {
			return nil, errors.New("original resident process group is running or unknown")
		}
		return map[string]admission.MachineSettlementFile{"resident_exit": {Path: ref.Path, SHA256: ref.SHA256}}, nil
	})
}
