package loop

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

type landedMergeResponse struct {
	Schema         int                                           `json:"schema_version"`
	Status         string                                        `json:"status"`
	Error          string                                        `json:"error"`
	HostInstanceID string                                        `json:"host_instance_id"`
	ContextID      string                                        `json:"context_id"`
	RequestID      string                                        `json:"request_id"`
	Observation    SessionReadback                               `json:"observation"`
	Sources        map[string]loopruntime.SessionReadbackFileRef `json:"sources"`
	Effects        []string                                      `json:"effects"`
}

type landedMergeObservation struct {
	Schema       int                                `json:"schema"`
	Kind         string                             `json:"kind"`
	Validation   string                             `json:"validation"`
	ExitCode     *int                               `json:"exit_code"`
	Noodle       loopruntime.SessionReadbackFileRef `json:"noodle"`
	Request      loopruntime.SessionReadbackFileRef `json:"request"`
	TaskEnvelope loopruntime.SessionReadbackFileRef `json:"task_envelope"`
	Requested    struct {
		OrderID   string `json:"order_id"`
		SessionID string `json:"session_id"`
	} `json:"requested"`
	NativeResponse json.RawMessage `json:"native_response"`
}

func validateLandedLocalMerge(project string, s reducer.DurableSnapshot, p LandedLocalMergeProof) (r LandedLocalMergeResult, err error) {
	r = LandedLocalMergeResult{Schema: 1, Owner: "Noodle stopped local merge", Status: "refused", Effects: []string{}, RetiredEffectIDs: []string{}, Sources: map[string]loopruntime.SessionReadbackFileRef{}}
	var observation landedMergeObservation
	if err = landedMergeReadRef(p.Observation, &observation); err != nil {
		return r, err
	}
	var native landedMergeResponse
	if err = landedMergeReadRef(p.Response, &native); err != nil {
		return r, err
	}
	raw, err := readAdmissionFile(p.Response.Path)
	if err != nil {
		return r, err
	}
	var original any
	var published any
	if json.Unmarshal(raw, &original) != nil || json.Unmarshal(observation.NativeResponse, &published) != nil || !reflect.DeepEqual(original, published) {
		return r, fmt.Errorf("original native response differs from verified observation")
	}
	if observation.Schema != 1 || observation.Kind != "local_task_session_readback_observation" || observation.Validation != "verified" || observation.ExitCode == nil || *observation.ExitCode != 0 || native.Schema != 1 || native.Status != "observed" || native.Error != "" || len(native.Effects) != 0 || native.HostInstanceID == "" || native.ContextID == "" || native.RequestID == "" {
		return r, fmt.Errorf("original native execution observation is not verified")
	}
	execution := native.Observation.Execution
	readback := native.Observation
	if readback.SchemaVersion != 1 || readback.Status != "observed" || readback.Runtime != "openshell" || readback.StageIndex != 0 || readback.AttemptStatus != "completed" || len(readback.Effects) != 0 || execution.NativePhase != "Stopped" || !execution.StopAcknowledged || execution.NativeExecExitCode != 0 || execution.NativeAttemptID == "" || execution.MainProcessInstanceID == "" || len(execution.CommandSHA256) != 64 || observation.Requested.OrderID != readback.OrderID || observation.Requested.SessionID != readback.SessionID {
		return r, fmt.Errorf("original OpenShell execution completion differs")
	}
	order, ok := s.State.Orders[readback.OrderID]
	if !ok || order.Status != state.OrderActive || len(order.Stages) != 1 {
		return r, fmt.Errorf("original landed order is not active single-stage custody")
	}
	stage := order.Stages[0]
	if stage.StageIndex != 0 || stage.Status != state.StageMerging || stage.Merge == nil || stage.Merge.Mode != "local" || len(stage.Attempts) != 1 || stage.Runtime != readback.Runtime || stage.Provider != readback.Provider || stage.Model != readback.Model {
		return r, fmt.Errorf("original landed stage or execution route differs")
	}
	attempt := stage.Attempts[0]
	if attempt.AttemptID != readback.AttemptID || attempt.SessionID != readback.SessionID || attempt.Status != state.AttemptCompleted || attempt.WorktreeName != stage.Merge.WorktreeName || readback.WorktreePath != filepath.Join(project, ".worktrees", attempt.WorktreeName) {
		return r, fmt.Errorf("original landed attempt/session/worktree differs")
	}
	var binding localTaskMergeBinding
	if json.Unmarshal(stage.Extra[localTaskMergeKey], &binding) != nil || !reconciliationSHA.MatchString(binding.Head) || !reconciliationSHA.MatchString(binding.Tree) || binding.Head != execution.CandidateHead {
		return r, fmt.Errorf("original landed candidate binding is missing")
	}
	candidate, err := readLocalTaskCandidate(binding.Candidate)
	if err != nil {
		return r, err
	}
	if candidate.OrderID != order.OrderID || candidate.SessionID != attempt.SessionID || candidate.AttemptID != attempt.AttemptID || candidate.Head != binding.Head || candidate.Worktree != readback.WorktreePath || candidate.SpawnSHA256 != execution.Sources["spawn.json"].SHA256 {
		return r, fmt.Errorf("landed candidate differs from original completed attempt")
	}
	var extended struct {
		TaskEnvelope    loopruntime.SessionReadbackFileRef `json:"task_envelope"`
		NativeExecution struct {
			Observation loopruntime.SessionReadbackFileRef `json:"observation"`
		} `json:"native_execution"`
	}
	if err = landedMergeReadRef(loopruntime.SessionReadbackFileRef{Path: binding.Candidate.Path, SHA256: binding.Candidate.SHA256}, &extended); err != nil {
		return r, err
	}
	if extended.TaskEnvelope != observation.TaskEnvelope || extended.NativeExecution.Observation != p.Response {
		return r, fmt.Errorf("candidate has no exact retained original native proof")
	}
	if observation.Noodle != native.Sources["client_binary"] || observation.Request != native.Sources["original_request"] {
		return r, fmt.Errorf("original native input references differ")
	}
	var request struct {
		Schema     int    `json:"schema"`
		Operation  string `json:"operation"`
		RequestID  string `json:"request_id"`
		Project    string `json:"project_dir"`
		ConfigPath string `json:"config_path"`
		ConfigSHA  string `json:"config_sha256"`
		BinarySHA  string `json:"binary_sha256"`
		Status     string `json:"status"`
	}
	if err = landedMergeReadRef(observation.Request, &request); err != nil {
		return r, err
	}
	if request.Schema != 1 || request.Operation != "attach" || request.RequestID != native.RequestID || request.Project != project || request.Status != "accepted" || request.BinarySHA != observation.Noodle.SHA256 || native.Sources["config"] != (loopruntime.SessionReadbackFileRef{Path: request.ConfigPath, SHA256: request.ConfigSHA}) {
		return r, fmt.Errorf("original native request binding differs")
	}
	for _, ref := range []loopruntime.SessionReadbackFileRef{observation.Noodle, observation.TaskEnvelope, native.Sources["config"]} {
		if err = landedMergeReadRef(ref, nil); err != nil {
			return r, err
		}
	}
	cfg, validation, err := config.Load(request.ConfigPath)
	if err != nil {
		return r, err
	}
	if len(validation.Fatals()) != 0 {
		return r, fmt.Errorf("original config is invalid")
	}
	l := &Loop{config: cfg}
	if !l.isAdmittedLocalTask(order, stage) {
		return r, fmt.Errorf("original local task admission differs")
	}
	if err = validateLandedObserver(p, binding.Candidate, observation.TaskEnvelope); err != nil {
		return r, err
	}
	// Only stable, non-secret execution artifacts are reread. The historical
	// native custody/metadata hashes remain inside the original owner-produced
	// observation; they are not exported or treated as fresh gateway observations.
	for _, name := range []string{"spawn.json", "openshell-dispatch.json", "delivery/result.json", "delivery/publish.json"} {
		ref := execution.Sources[name]
		if ref.Path != filepath.Join(project, ".noodle", "sessions", attempt.SessionID, name) {
			return r, fmt.Errorf("original local execution path differs: %s", name)
		}
		if err = landedMergeReadRef(ref, nil); err != nil {
			return r, err
		}
	}
	entries, err := os.ReadDir(filepath.Join(project, ".noodle", "sessions"))
	if err != nil {
		return r, err
	}
	if len(entries) != 1 || !entries[0].IsDir() || entries[0].Name() != attempt.SessionID {
		return r, fmt.Errorf("stopped merge has additional or missing session custody")
	}
	var integration struct {
		Branch string `json:"branch"`
	}
	data, err := readAdmissionFile(filepath.Join(project, ".noodle", "integration-branch.json"))
	if err != nil {
		return r, err
	}
	if json.Unmarshal(data, &integration) != nil || integration.Branch == "" {
		return r, fmt.Errorf("original integration branch is missing")
	}
	if err = validateLandedMergeGit(project, binding, integration.Branch); err != nil {
		return r, err
	}
	if err = validateLocalTaskFileBytes(candidate, project); err != nil {
		return r, err
	}
	mergeCount := 0
	for _, record := range s.EffectLedger {
		if record.Status == reducer.EffectLedgerDone || record.Status == reducer.EffectLedgerFailed {
			continue
		}
		var identity struct {
			OrderID      string `json:"order_id"`
			StageIndex   int    `json:"stage_index"`
			WorktreeName string `json:"worktree_name"`
			Mode         string `json:"merge_mode"`
			Branch       string `json:"merge_branch"`
			AttemptID    string `json:"attempt_id"`
		}
		if json.Unmarshal(record.Effect.Payload, &identity) != nil || record.Status != reducer.EffectLedgerPending || record.Result != nil || identity.OrderID != order.OrderID || identity.StageIndex != 0 {
			return r, fmt.Errorf("landed merge has foreign or active unresolved effect %s", record.EffectID)
		}
		switch record.Effect.Type {
		case reducer.EffectMerge:
			mergeCount++
			if identity.WorktreeName != attempt.WorktreeName || identity.Mode != "local" || identity.Branch != "" {
				return r, fmt.Errorf("pending merge identity differs")
			}
		case reducer.EffectDispatch:
			if identity.AttemptID != attempt.AttemptID {
				return r, fmt.Errorf("pending dispatch attempt differs")
			}
		case reducer.EffectAck:
		default:
			return r, fmt.Errorf("landed merge has unrelated effect %s", record.Effect.Type)
		}
		r.RetiredEffectIDs = append(r.RetiredEffectIDs, record.EffectID)
	}
	if mergeCount == 0 {
		return r, fmt.Errorf("landed merge has no original pending effects")
	}
	r.OrderID, r.StageIndex, r.AttemptID, r.SessionID = order.OrderID, 0, attempt.AttemptID, attempt.SessionID
	r.Candidate, r.Head, r.Tree = binding.Candidate, binding.Head, binding.Tree
	r.OriginalHost = landedMergeHost{native.HostInstanceID, native.ContextID, native.RequestID, observation.Noodle}
	r.Sources["execution_observation"], r.Sources["native_response"], r.Sources["observer_intent"], r.Sources["observer_receipt"] = p.Observation, p.Response, p.ObserverIntent, p.ObserverReceipt
	return r, nil
}

func validateLandedObserver(p LandedLocalMergeProof, candidate localTaskFileRef, envelope loopruntime.SessionReadbackFileRef) error {
	var intent struct {
		SourceCommit string                               `json:"source_commit"`
		Python       loopruntime.SessionReadbackFileRef   `json:"python"`
		Sources      []loopruntime.SessionReadbackFileRef `json:"sources"`
		Request      loopruntime.SessionReadbackFileRef   `json:"request"`
		Argv         []string                             `json:"argv"`
	}
	if err := landedMergeReadRef(p.ObserverIntent, &intent); err != nil {
		return err
	}
	if !reconciliationSHA.MatchString(intent.SourceCommit) || len(intent.Argv) < 7 || intent.Argv[0] != intent.Python.Path || intent.Argv[1] != "-B" || intent.Argv[3] != "loop" || intent.Argv[4] != "inspect" || intent.Argv[5] != "--request-file" || intent.Argv[6] != intent.Request.Path {
		return fmt.Errorf("original observer source invocation differs")
	}
	found := false
	entry := false
	for _, source := range intent.Sources {
		if err := landedMergeReadRef(source, nil); err != nil {
			return err
		}
		if filepath.Base(source.Path) == "task_session_readback.py" {
			found = true
		}
		if source.Path == intent.Argv[2] {
			entry = true
		}
	}
	if !found || !entry {
		return fmt.Errorf("original observer source closure is incomplete")
	}
	if err := landedMergeReadRef(intent.Python, nil); err != nil {
		return err
	}
	if err := landedMergeReadRef(intent.Request, nil); err != nil {
		return err
	}
	var receipt struct {
		ExitCode *int                               `json:"exit_code"`
		Stdout   loopruntime.SessionReadbackFileRef `json:"stdout"`
		Stderr   loopruntime.SessionReadbackFileRef `json:"stderr"`
	}
	if err := landedMergeReadRef(p.ObserverReceipt, &receipt); err != nil {
		return err
	}
	if receipt.ExitCode == nil || *receipt.ExitCode != 0 {
		return fmt.Errorf("original observer receipt is not successful")
	}
	if err := landedMergeReadRef(receipt.Stderr, nil); err != nil {
		return err
	}
	var packet struct {
		Owner   string                             `json:"owner"`
		Request loopruntime.SessionReadbackFileRef `json:"request"`
		Inner   struct {
			Candidate localTaskFileRef `json:"candidate"`
		} `json:"inner"`
		Capture struct {
			Owner        string                             `json:"owner"`
			Status       string                             `json:"status"`
			TaskEnvelope loopruntime.SessionReadbackFileRef `json:"task_envelope"`
			Gaps         []json.RawMessage                  `json:"gaps"`
			Attempts     []struct {
				NativeCompletion struct {
					Observation loopruntime.SessionReadbackFileRef `json:"observation"`
				} `json:"native_completion"`
			} `json:"attempts"`
		} `json:"capture"`
	}
	if err := landedMergeReadRef(receipt.Stdout, &packet); err != nil {
		return err
	}
	if packet.Owner != "soodles.loop" || packet.Request != intent.Request || packet.Inner.Candidate != candidate || packet.Capture.Owner != "issue-execution.local-task.capture" || packet.Capture.Status != "observed" || packet.Capture.TaskEnvelope != envelope || len(packet.Capture.Gaps) != 0 || len(packet.Capture.Attempts) != 1 || packet.Capture.Attempts[0].NativeCompletion.Observation != p.Response {
		return fmt.Errorf("original captured native completion differs")
	}
	if !strings.HasSuffix(intent.Argv[2], "soodles.py") {
		return fmt.Errorf("original observer entrypoint differs")
	}
	return nil
}
