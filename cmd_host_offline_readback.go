package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
)

// Offline evidence is one original-owner observation. It cannot release a
// lifetime, authorize dispatch, or replace a native round continuation.
type hostOfflineReadback struct {
	Schema                     int                                           `json:"schema"`
	Owner                      string                                        `json:"owner"`
	Status                     string                                        `json:"status"`
	Scope                      string                                        `json:"scope"`
	Request                    *loopruntime.SessionReadbackFileRef           `json:"request,omitempty"`
	RequestID                  string                                        `json:"request_id,omitempty"`
	HostInstanceID             string                                        `json:"host_instance_id,omitempty"`
	ContextID                  string                                        `json:"context_id,omitempty"`
	Sources                    map[string]loopruntime.SessionReadbackFileRef `json:"sources,omitempty"`
	OriginalProcessGroupAbsent *bool                                         `json:"original_process_group_absent,omitempty"`
	OrderID                    string                                        `json:"order_id,omitempty"`
	StageReadback              string                                        `json:"stage_readback,omitempty"`
	ResidentPreparation        *admission.PreparationObservation             `json:"resident_preparation,omitempty"`
	TaskPreparation            *admission.PreparationObservation             `json:"task_preparation,omitempty"`
	Missing                    []string                                      `json:"missing"`
	Next                       *string                                       `json:"next"`
	Effects                    []string                                      `json:"effects"`
	AuthorizesDispatch         bool                                          `json:"authorizes_dispatch"`
	Error                      string                                        `json:"error,omitempty"`
}

func observeOfflineHostRequest(project, requestFile string) hostOfflineReadback {
	dir, err := admission.DefaultMachineCapacityDirectory()
	if err != nil {
		return offlineHostInputRequired(err)
	}
	return readOfflineHostRequest(project, requestFile, dir)
}

func offlineHostInputRequired(err error) hostOfflineReadback {
	return hostOfflineReadback{Schema: 1, Owner: "Noodle", Status: "input_required", Scope: "offline_original_request",
		Missing: []string{"original_request_readback"}, Effects: []string{}, Error: err.Error()}
}

func readOfflineHostRequest(project, requestFile, machineDir string) hostOfflineReadback {
	owner, err := readHostOwnerBinding(requestFile)
	if err != nil {
		return offlineHostInputRequired(err)
	}
	project, err = filepath.EvalSymlinks(project)
	if err != nil || project != owner.Current.ProjectDir {
		return offlineHostInputRequired(errors.New("original request project differs"))
	}
	var binding hostRequestBinding
	ref, err := loopruntime.ReadSessionReadbackFile(requestFile, &binding)
	if err != nil || ref != owner.Request || binding.Status != "accepted" {
		return offlineHostInputRequired(errors.New("original accepted request source differs"))
	}
	pool, err := readHostPool(owner.HostRoot)
	taskOwner := pool.Owners[binding.RequestID]
	g := pool.Generations[owner.Current.HostInstanceID]
	if err != nil || taskOwner.State != "owned" || owner.Original.ContextID != owner.Current.ContextID ||
		owner.Original.HostInstanceID != g.ID || owner.Current.ContextState != "active" ||
		g.Process.PID != owner.Current.HostPID || g.Process.SessionID != g.ID || g.Process.StartedAt.IsZero() ||
		g.SourceService != nil || g.Preparation == nil {
		return offlineHostInputRequired(errors.New("original accepted generation is not an offline task subject"))
	}
	result := hostOfflineReadback{Schema: 1, Owner: "Noodle", Status: "unknown_effect", Scope: "offline_original_request",
		Request: &ref, RequestID: binding.RequestID, HostInstanceID: g.ID, ContextID: owner.Current.ContextID,
		Sources: owner.Sources, Missing: []string{"original_native_round_handoff"}, Effects: []string{}}
	absent, err := g.Process.GroupAbsent()
	if err != nil || !absent {
		return offlineHostInputRequired(errors.New("original Host process group is running or unknown"))
	}
	result.OriginalProcessGroupAbsent = &absent

	controlPath := filepath.Join(filepath.Dir(project), "control.json")
	var control worktree.TaskControl
	controlRef, err := loopruntime.ReadSessionReadbackFile(controlPath, &control)
	if err != nil || controlRef.Path != controlPath || control.Schema != 2 || control.Status != "prepared" ||
		control.ControlRoot != project || filepath.Dir(filepath.Dir(project)) != filepath.Join(owner.HostRoot, "controls") {
		return offlineHostInputRequired(errors.New("original task preparation control differs"))
	}
	result.Sources["task_control"] = controlRef
	taskRef, err := loopruntime.ReadSessionReadbackFile(control.Task.Path, nil)
	if err != nil || taskRef.Path != control.Task.Path || taskRef.SHA256 != control.Task.SHA256 {
		return offlineHostInputRequired(errors.New("original task preparation source changed"))
	}
	result.Sources["task"] = taskRef

	cfgPath := binding.ConfigPath
	if cfgPath == "" {
		cfgPath = filepath.Join(project, config.DefaultConfigPath)
	}
	cfgRef, err := loopruntime.ReadSessionReadbackFile(cfgPath, nil)
	data, readErr := readHostRequestConfig(hostRequest{ProjectDir: project, ConfigPath: binding.ConfigPath})
	if err != nil || readErr != nil || cfgRef.SHA256 != binding.ConfigSHA256 || hostDigest(data) != cfgRef.SHA256 {
		return offlineHostInputRequired(errors.New("original task configuration changed"))
	}
	cfg, err := config.Parse(data)
	if err != nil || len(cfg.Runtime.AdmittedOrders) != 1 || cfg.Runtime.OpenShell.Profile != filepath.Join(owner.HostRoot, "openshell", "profile.json") {
		return offlineHostInputRequired(errors.New("original OpenShell task order is unavailable"))
	}
	result.Sources["configuration"] = cfgRef
	result.OrderID = cfg.Runtime.AdmittedOrders[0]
	capacity, err := loopruntime.ReadSessionReadbackFile(filepath.Join(machineDir, "capacity.json"), nil)
	if err != nil {
		return offlineHostInputRequired(errors.New("original machine capacity ledger unavailable"))
	}
	rows, err := admission.ReadMachinePreparations(machineDir, *g.Preparation, control.Preparation)
	if err != nil {
		return offlineHostInputRequired(err)
	}
	resident, task := rows[*g.Preparation], rows[control.Preparation]
	if resident.Source.Purpose != "host-resident" || resident.Owner != owner.HostRoot+"/resident" || resident.ContextID != "" ||
		resident.Source.Bindings["host_root"] != owner.HostRoot ||
		resident.Source.Bindings["binary_path"] != g.Binary || resident.Source.Bindings["binary_sha256"] != g.SHA ||
		task.Owner != hostMachineGenerationOwner(owner.HostRoot, g.ID) || task.ContextID != result.ContextID ||
		task.OrderID != result.OrderID || task.Runtime != "openshell" ||
		task.Source.Purpose != "task-preparation" || task.Source.Bindings["host_root"] != owner.HostRoot ||
		task.Source.Bindings["control_root"] != project || task.Source.Bindings["task_path"] != taskRef.Path ||
		task.Source.Bindings["task_sha256"] != taskRef.SHA256 || task.Source.Bindings["execution_binary"] != g.Binary+"/"+g.SHA {
		return offlineHostInputRequired(errors.New("original offline preparation sources differ"))
	}
	if err := result.readWorkerHistory(project, task); err != nil {
		result.Missing = append(result.Missing, "original_worker_history")
		result.Error = err.Error()
	}
	if witness, witnessErr := readResidentExitWitness(owner.HostRoot, g); witnessErr == nil {
		result.Sources["resident_exit"] = witness
	} else {
		result.Missing = append(result.Missing, "original_resident_exit")
	}
	if !resident.Closed {
		result.Missing = append(result.Missing, "original_resident_settlement")
	}
	if !task.Closed {
		result.Missing = append(result.Missing, "original_task_preparation_settlement")
	}
	result.Sources["machine_capacity"] = capacity
	result.ResidentPreparation, result.TaskPreparation = &resident, &task
	if err = loopruntime.VerifySessionReadbackFiles(result.Sources); err != nil {
		return offlineHostInputRequired(err)
	}
	return result
}

func (result *hostOfflineReadback) readWorkerHistory(project string, task admission.PreparationObservation) error {
	if task.Promoted || task.SessionBound {
		return errors.New("original task preparation has worker effects")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	plan, err := planHostStorage(ctx, hostRequest{ProjectDir: project, Environment: os.Environ()}, "", "")
	raw, encodeErr := json.Marshal(plan)
	if err != nil || encodeErr != nil || plan.Source.StorageRoot == "" || hostDigest(raw) != task.Source.Bindings["storage"] {
		return errors.New("original task runtime storage plan differs")
	}
	var source, paired hostStorageSource
	sourceRef, err := loopruntime.ReadSessionReadbackFile(filepath.Join(plan.OwnerDir, "source.json"), &source)
	pairedRef, pairedErr := loopruntime.ReadSessionReadbackFile(filepath.Join(plan.Source.StorageRoot, "source.json"), &paired)
	if err != nil || pairedErr != nil || sourceRef.SHA256 != pairedRef.SHA256 || source != plan.Source || paired != source {
		return errors.New("original task runtime storage receipts differ")
	}
	for _, name := range []string{".noodle", ".worktrees"} {
		logical := filepath.Join(project, name)
		info, err := os.Lstat(logical)
		target, targetErr := filepath.EvalSymlinks(logical)
		if err != nil || info.Mode()&os.ModeSymlink == 0 || targetErr != nil || target != filepath.Join(plan.Source.StorageRoot, name) {
			return errors.New("original task storage alias differs: " + name)
		}
	}
	result.Sources["storage_owner"] = sourceRef
	result.Sources["storage_source"] = pairedRef
	physical := filepath.Join(plan.Source.StorageRoot, ".noodle")
	history, err := unstartedEvidence(physical, result.OrderID)
	if err != nil {
		return err
	}
	snapshot, err := loopruntime.ReadSessionReadbackFile(filepath.Join(physical, "state.snapshot.json"), nil)
	if err != nil || snapshot.SHA256 != history["state.snapshot.json"] {
		return errors.New("original task state snapshot changed during readback")
	}
	result.Sources["state_snapshot"] = snapshot
	result.StageReadback = "pending_no_worker_observed"
	return nil
}
