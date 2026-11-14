package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
)

type returnRecoveryPlan struct {
	Schema     int                       `json:"schema"`
	Project    string                    `json:"project"`
	Controller stoppedMergeRef           `json:"actual_controller"`
	Host       hostOwnerBinding          `json:"original_host"`
	Event      loop.ReturnRecoveryEvent  `json:"original_event"`
	Frame      loop.ReturnRecoveryPrefix `json:"original_frame_prefix"`
	Log        loop.ReturnRecoveryPrefix `json:"normal_log_prefix"`
	Before     stoppedMergeRef           `json:"canonical_before"`
	History    stoppedMergeRef           `json:"history_before"`
	Config     stoppedMergeRef           `json:"original_config"`
}
type returnRecoveryIntent struct {
	Schema     int                        `json:"schema"`
	Plan       stoppedMergeRef            `json:"plan"`
	Subject    loop.ReturnRecoverySubject `json:"subject"`
	Controller stoppedMergeRef            `json:"actual_controller"`
	Readback   loop.ReturnCustodyReadback  `json:"qualified_absent"`
}
type returnRecoveryReceipt struct {
	Intent stoppedMergeRef `json:"intent"`
	Status string          `json:"status"`
}
type returnRecoveryFrame struct {
	Owner   string               `json:"owner"`
	Request config.RuntimeSource `json:"request"`
	Inner struct {
		Native struct {
			Owner *hostOwnerBinding `json:"owner_binding"`
		} `json:"native_host"`
		Order   state.OrderNode                     `json:"order"`
		Reviews map[string]state.PendingReviewNode `json:"pending_reviews"`
	} `json:"inner"`
}
func returnRecoveryActual() (stoppedMergeRef, error) {
	exe, err := os.Executable()
	if err != nil {
		return stoppedMergeRef{}, err
	}
	return hostReadbackBinaryRef(exe)
}
func returnRecoverySource(ref stoppedMergeRef) config.RuntimeSource {
	return config.RuntimeSource{Path: ref.Path, SHA256: ref.SHA256}
}
func returnRecoveryDirectory(project string, subject loop.ReturnRecoverySubject) string {
	raw, _ := json.Marshal(subject.Packet)
	runtimeDir, err := filepath.EvalSymlinks(filepath.Join(project, ".noodle"))
	if err != nil {
		return ""
	}
	return filepath.Join(runtimeDir, "return-handoff", hostDigest(raw))
}
func prepareReturnRecovery(project, framePath string) (returnRecoveryResult, error) {
	r := returnRecoveryBase()
	frame, frameRaw, err := loop.ReadReturnRecoveryPrefix(framePath, nil)
	if err != nil {
		return r, err
	}
	var input returnRecoveryFrame
	if json.Unmarshal(frameRaw, &input) != nil || input.Owner != "soodles.loop" || input.Inner.Native.Owner == nil || len(input.Inner.Order.Stages) != 1 || input.Inner.Order.Stages[0].StageIndex != 0 {
		return r, fmt.Errorf("original return readback unavailable")
	}
	requestPath, orderID := input.Inner.Native.Owner.Request.Path, input.Inner.Order.OrderID
	eventID, count := "", 0
	if review, current := input.Inner.Reviews[orderID]; current {
		if review.OrderID != orderID || review.StageIndex != 0 || review.SessionID == "" || review.WorktreeName == "" || review.WorktreePath == "" || review.ReturnHandoff == nil || review.ReturnHandoff.Status != "unknown" || input.Inner.Order.Stages[0].Status != state.StageReview {
			return r, fmt.Errorf("original current return review differs")
		}
		for _, attempt := range input.Inner.Order.Stages[0].Attempts {
			if attempt.ReturnHandoff != nil && attempt.ReturnHandoff.EventID == review.ReturnHandoff.EventID && attempt.SessionID != review.SessionID {
				return r, fmt.Errorf("original current return review event/session differs")
			}
			if attempt.SessionID != review.SessionID {
				continue
			}
			if attempt.AttemptID == "" || attempt.WorktreeName != review.WorktreeName || attempt.ReturnHandoffUnattempted || attempt.ReturnHandoff == nil || *attempt.ReturnHandoff != *review.ReturnHandoff {
				return r, fmt.Errorf("original current return review/attempt differs")
			}
			switch attempt.Status {
			case state.AttemptCompleted, state.AttemptFailed, state.AttemptCancelled:
			default:
				return r, fmt.Errorf("original current return review attempt is not terminal")
			}
			eventID, count = attempt.ReturnHandoff.EventID, count+1
		}
		if count != 1 || eventID == "" {
			return r, fmt.Errorf("original current return review attempt missing or ambiguous")
		}
	} else {
		for _, attempt := range input.Inner.Order.Stages[0].Attempts {
			if attempt.ReturnHandoff != nil && attempt.ReturnHandoff.Status == "unknown" {
				eventID, count = attempt.ReturnHandoff.EventID, count+1
			}
		}
	}
	if count != 1 || eventID == "" || orderID == "" {
		return r, fmt.Errorf("original return readback event missing or ambiguous")
	}
	project, err = filepath.EvalSymlinks(project)
	if err != nil {
		return r, err
	}
	owner, err := readHostOwnerBinding(requestPath)
	if err != nil {
		return r, err
	}
	if owner.Current.ProjectDir != project || owner.Current.ContextState != "active" {
		return r, fmt.Errorf("original return Host context differs")
	}
	controller, err := returnRecoveryActual()
	if err != nil {
		return r, err
	}
	cfgRef := stoppedMergeRef{Path: owner.Current.ConfigPath, SHA256: owner.Current.ConfigSHA256}
	if err = stoppedMergeRead(cfgRef, nil); err != nil {
		return r, err
	}
	cfg, diagnostics, err := config.Load(cfgRef.Path)
	if err != nil || !diagnostics.CanSpawn() || len(cfg.Runtime.AdmittedOrders) != 1 || cfg.Runtime.AdmittedOrders[0] != orderID {
		return r, fmt.Errorf("original return admitted config unavailable: %v", err)
	}
	snapshot, err := returnRecoverySnapshot(project)
	if err != nil {
		return r, err
	}
	history, err := os.ReadFile(filepath.Join(project, ".noodle", "loop-events.ndjson"))
	if err != nil && !os.IsNotExist(err) {
		return r, err
	}
	// Request is derived from the original Frozen binding, never supplied as a boolean or tuple.
	var descriptor struct { Binding config.RuntimeSource `json:"binding"` }
	if err = stoppedMergeRead(stoppedMergeRef{Path: cfg.Runtime.ReturnHandoff.Path, SHA256: cfg.Runtime.ReturnHandoff.SHA256}, nil); err != nil {
		return r, err
	}
	raw, err := config.ReadExplicitSource(cfg.Runtime.ReturnHandoff.Path)
	if err != nil || json.Unmarshal(raw, &descriptor) != nil {
		return r, fmt.Errorf("original return descriptor unavailable")
	}
	var binding struct { Request config.RuntimeSource `json:"request"` }
	if err = stoppedMergeRead(stoppedMergeRef{Path: descriptor.Binding.Path, SHA256: descriptor.Binding.SHA256}, &binding); err != nil {
		return r, err
	}
	original, err := loop.MaterializeReturnRecovery(snapshot, history, orderID, eventID, cfg.Runtime.ReturnHandoff, binding.Request, owner.Current.HostInstanceID, returnRecoverySource(owner.ExecutionBinary))
	if err != nil {
		return r, err
	}
	if err = stoppedMergeRead(stoppedMergeRef{Path: binding.Request.Path, SHA256: binding.Request.SHA256}, nil); err != nil {
		return r, err
	}
	if err = validateReturnRecoveryFrame(frameRaw, original, *owner); err != nil {
		return r, err
	}
	logPrefix, logRaw, err := loop.ReadReturnRecoveryPrefix(filepath.Join(owner.GenerationDir, "host.log"), nil)
	if err != nil {
		return r, err
	}
	if err = validateReturnRecoveryDial(*owner, original, controller, frameRaw, logRaw); err != nil {
		return r, err
	}
	if _, _, err = loop.ReadReturnRecoveryPrefix(frame.Path, &frame); err != nil {
		return r, err
	}
	if _, _, err = loop.ReadReturnRecoveryPrefix(logPrefix.Path, &logPrefix); err != nil {
		return r, err
	}
	dir := returnRecoveryDirectory(project, original.Subject)
	if dir == "" {
		return r, fmt.Errorf("original return runtime location unavailable")
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return r, err
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil || canonical != dir {
		return r, fmt.Errorf("original return recovery directory changed")
	}
	guard, err := lockfile.TryLock(filepath.Join(dir, "recovery.lock"))
	if err != nil {
		return r, err
	}
	defer guard.Close()
	planPath := filepath.Join(dir, "plan.json")
	if _, err = os.Lstat(planPath); err == nil {
		return inspectPreparedReturnRecovery(project, planPath)
	} else if !os.IsNotExist(err) {
		return r, err
	}
	before, err := stoppedMergeArchive(filepath.Join(dir, "before.json"), snapshot)
	if err != nil {
		return r, err
	}
	historyRef, err := stoppedMergeArchive(filepath.Join(dir, "history.ndjson"), history)
	if err != nil {
		return r, err
	}
	p := returnRecoveryPlan{1, project, controller, *owner, original, frame, logPrefix, before, historyRef, cfgRef}
	if err = validateReturnRecoveryPlan(p); err != nil {
		return r, err
	}
	if err = validateReturnRecoveryCurrent(p); err != nil {
		return r, err
	}
	ref, err := stoppedMergeSave(planPath, p)
	if err != nil {
		return r, err
	}
	for _, directory := range []string{dir, filepath.Dir(dir), filepath.Dir(filepath.Dir(dir))} {
		if err = filex.SyncDir(directory); err != nil {
			return r, err
		}
	}
	r = returnRecoveryFor(p, ref)
	r.Status = "prepared"
	return returnRecoveryNext(r, p, ref), nil
}

func validateReturnRecoveryFrame(raw []byte, original loop.ReturnRecoveryEvent, owner hostOwnerBinding) error {
	var frame returnRecoveryFrame
	if json.Unmarshal(raw, &frame) != nil || frame.Owner != "soodles.loop" || frame.Request != original.Subject.Request || frame.Inner.Native.Owner == nil {
		return fmt.Errorf("original native event frame unavailable")
	}
	b := frame.Inner.Native.Owner
	if b.Schema != 1 || b.Request != owner.Request || b.ExecutionBinary != owner.ExecutionBinary || b.GenerationDir != owner.GenerationDir || b.HostRoot != owner.HostRoot || b.Current.ProjectDir != owner.Current.ProjectDir || b.Current.HostInstanceID != owner.Current.HostInstanceID || b.Current.ContextID != owner.Current.ContextID || b.Current.RequestID != owner.Current.RequestID || b.Current.BinaryPath != owner.ExecutionBinary.Path || b.Current.BinarySHA256 != owner.ExecutionBinary.SHA256 {
		return fmt.Errorf("original frame Host/executor differs")
	}
	p := original.Subject.Packet
	if frame.Inner.Order.OrderID != p.OrderID || len(frame.Inner.Order.Stages) != 1 {
		return fmt.Errorf("original event frame order differs")
	}
	count, index := 0, -1
	for i, a := range frame.Inner.Order.Stages[0].Attempts {
		if a.AttemptID == p.AttemptID && a.SessionID == p.SessionID && !a.ReturnHandoffUnattempted && a.ReturnHandoff != nil && a.ReturnHandoff.EventID == p.EventID && *a.ReturnHandoff == *original.Attempt.ReturnHandoff && a.CompletedAt.Equal(original.Attempt.CompletedAt) && a.StartedAt.Equal(original.Attempt.StartedAt) {
			count++
			index = i
		}
	}
	review, ok := frame.Inner.Reviews[p.OrderID]
	if count != 1 {
		return fmt.Errorf("original unique event frame differs")
	}
	if !ok || review.SessionID != p.SessionID {
		var archive struct {
			Binding struct {
				Attempt      int    `json:"attempt"`
				AttemptID    string `json:"attempt_id"`
				SessionID    string `json:"session_id"`
				Reason       string `json:"reason"`
				WorktreeName string `json:"worktree_name"`
				WorktreePath string `json:"worktree_path"`
			} `json:"binding"`
			Review state.PendingReviewNode `json:"review"`
		}
		stage := frame.Inner.Order.Stages[0]
		framed := stage.Attempts[index]
		if json.Unmarshal(stage.Extra["request_changes_requeued"], &archive) != nil || framed.Status != state.AttemptFailed || framed.Error != original.Attempt.Error || archive.Binding.Attempt != index || archive.Binding.AttemptID != p.AttemptID || archive.Binding.SessionID != p.SessionID || archive.Binding.Reason != original.Attempt.Error || archive.Binding.WorktreeName != original.Attempt.WorktreeName || archive.Binding.WorktreePath != original.Review.WorktreePath || index+1 >= len(stage.Attempts) {
			return fmt.Errorf("original archived event frame differs")
		}
		review = archive.Review
	}
	if review.SessionID != p.SessionID || review.OrderID != p.OrderID || review.StageIndex != 0 || review.ReturnHandoff == nil || *review.ReturnHandoff != *original.Attempt.ReturnHandoff || review.WorktreeName != original.Review.WorktreeName || review.WorktreePath != original.Review.WorktreePath {
		return fmt.Errorf("original unique event/review frame differs")
	}
	return nil
}
func validateReturnRecoveryDial(owner hostOwnerBinding, original loop.ReturnRecoveryEvent, controller stoppedMergeRef, frameRaw, logRaw []byte) error {
	legacy, err := loop.QualifyReturnDialPrefix(logRaw, original, returnRecoverySource(controller))
	if err != nil || !legacy {
		return err
	}
	var frame returnRecoveryFrame
	if err = json.Unmarshal(frameRaw, &frame); err != nil {
		return err
	}
	returns := 0
	for _, attempt := range frame.Inner.Order.Stages[0].Attempts {
		if attempt.ReturnHandoff != nil {
			returns++
		}
	}
	if returns != 1 {
		return fmt.Errorf("legacy return frame has ambiguous events")
	}
	pool, err := readHostPool(owner.HostRoot)
	if err != nil {
		return err
	}
	for id, other := range pool.Owners {
		if id == owner.Current.RequestID {
			continue
		}
		for _, receipt := range append([]hostReceipt{other.Original, other.Current}, other.Previous...) {
			if receipt.HostInstanceID == owner.Current.HostInstanceID {
				ref := stoppedMergeRef{Path: receipt.ConfigPath, SHA256: receipt.ConfigSHA256}
				if err = stoppedMergeRead(ref, nil); err != nil {
					return err
				}
				cfg, diagnostics, loadErr := config.Load(ref.Path)
				if loadErr != nil || !diagnostics.CanSpawn() {
					return fmt.Errorf("legacy return producer config unavailable")
				}
				if cfg.Runtime.ReturnHandoff.Path == "" && cfg.Runtime.ReturnHandoff.SHA256 == "" {
					continue
				}
				var target struct {
					Schema   int                  `json:"schema"`
					Binding  config.RuntimeSource `json:"binding"`
					Endpoint string               `json:"endpoint"`
					Producer config.RuntimeSource `json:"producer"`
					Host     string               `json:"host_instance_id"`
				}
				if err = stoppedMergeRead(stoppedMergeRef{Path: cfg.Runtime.ReturnHandoff.Path, SHA256: cfg.Runtime.ReturnHandoff.SHA256}, &target); err != nil {
					return err
				}
				if target.Schema != 1 || target.Host != receipt.HostInstanceID || target.Producer.Path != receipt.BinaryPath || target.Producer.SHA256 != receipt.BinarySHA256 || !filepath.IsAbs(target.Endpoint) || target.Endpoint == original.Endpoint {
					return fmt.Errorf("legacy return log has ambiguous endpoint producers")
				}
				if err = stoppedMergeRead(stoppedMergeRef{Path: target.Binding.Path, SHA256: target.Binding.SHA256}, nil); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func validateReturnRecoveryPlan(p returnRecoveryPlan) error {
	if p.Schema != 1 || p.Project != p.Host.Current.ProjectDir || p.Event.Subject.Packet.Producer != returnRecoverySource(p.Host.ExecutionBinary) || p.Log.Path != filepath.Join(p.Host.GenerationDir, "host.log") {
		return fmt.Errorf("original return plan identity differs")
	}
	for _, ref := range []stoppedMergeRef{p.Before, p.History, p.Config, p.Host.Request, p.Host.ExecutionBinary, {Path: p.Event.Subject.Request.Path, SHA256: p.Event.Subject.Request.SHA256}} {
		if err := stoppedMergeRead(ref, nil); err != nil {
			return err
		}
	}
	cfg, _, err := config.Load(p.Config.Path)
	if err != nil || cfg.Runtime.ReturnHandoff != p.Event.Subject.Target.Descriptor || len(cfg.Runtime.AdmittedOrders) != 1 || cfg.Runtime.AdmittedOrders[0] != p.Event.Subject.Packet.OrderID {
		return fmt.Errorf("original return config changed")
	}
	before, err := os.ReadFile(p.Before.Path)
	if err != nil {
		return err
	}
	history, err := os.ReadFile(p.History.Path)
	if err != nil {
		return err
	}
	event, err := loop.MaterializeReturnRecovery(before, history, p.Event.Subject.Packet.OrderID, p.Event.Subject.Packet.EventID, p.Event.Subject.Target.Descriptor, p.Event.Subject.Request, p.Host.Current.HostInstanceID, returnRecoverySource(p.Host.ExecutionBinary))
	if err != nil || !reflect.DeepEqual(event, p.Event) {
		return fmt.Errorf("original return before-image differs: %v", err)
	}
	_, frame, err := loop.ReadReturnRecoveryPrefix(p.Frame.Path, &p.Frame)
	if err != nil {
		return err
	}
	if err = validateReturnRecoveryFrame(frame, event, p.Host); err != nil {
		return err
	}
	_, log, err := loop.ReadReturnRecoveryPrefix(p.Log.Path, &p.Log)
	if err != nil {
		return err
	}
	if err = validateReturnRecoveryDial(p.Host, event, p.Controller, frame, log); err != nil {
		return err
	}
	owner, err := readHostOwnerBinding(p.Host.Request.Path)
	if err != nil {
		return err
	}
	if owner.Request != p.Host.Request || owner.ExecutionBinary != p.Host.ExecutionBinary || owner.GenerationDir != p.Host.GenerationDir || owner.Current.ContextID != p.Host.Current.ContextID || owner.Current.HostInstanceID != p.Host.Current.HostInstanceID || owner.Current.ProjectDir != p.Project {
		return fmt.Errorf("original return Host changed")
	}
	return nil
}
func validateReturnRecoveryCurrent(p returnRecoveryPlan) error {
	event := p.Event
	current, err := returnRecoverySnapshot(p.Project)
	if err != nil {
		return err
	}
	currentHistory, err := os.ReadFile(filepath.Join(p.Project, ".noodle", "loop-events.ndjson"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	fresh, err := loop.MaterializeReturnRecovery(current, currentHistory, event.Subject.Packet.OrderID, event.Subject.Packet.EventID, event.Subject.Target.Descriptor, event.Subject.Request, event.Subject.Packet.HostInstanceID, event.Subject.Packet.Producer)
	if err != nil || fresh.Subject != event.Subject || fresh.Endpoint != event.Endpoint || !fresh.Attempt.CompletedAt.Equal(event.Attempt.CompletedAt) {
		return fmt.Errorf("original historical return custody changed: %v", err)
	}
	return nil
}
func inspectPreparedReturnRecovery(project, planPath string) (returnRecoveryResult, error) {
	r, p, ref, err := readReturnRecoveryPlan(project, planPath)
	if err != nil {
		return r, err
	}
	for _, name := range []string{"intent.json", "receipt.json"} {
		if _, statErr := os.Lstat(filepath.Join(filepath.Dir(ref.Path), name)); statErr == nil {
			return r, nil
		} else if !os.IsNotExist(statErr) {
			return r, statErr
		}
	}
	r.Status = "prepared"
	return returnRecoveryNext(r, p, ref), nil
}
func readReturnRecoveryPlan(project, planPath string) (returnRecoveryResult, returnRecoveryPlan, stoppedMergeRef, error) {
	r := returnRecoveryBase()
	var p returnRecoveryPlan
	ref, err := loopruntime.ReadSessionReadbackFile(planPath, &p)
	if err != nil {
		return r, p, ref, err
	}
	project, err = filepath.EvalSymlinks(project)
	if err != nil {
		return r, p, ref, err
	}
	actual, err := returnRecoveryActual()
	if err != nil || actual != p.Controller {
		return r, p, ref, fmt.Errorf("actual return controller differs from plan")
	}
	if _, _, err = config.LoadRuntime(project); err != nil {
		return r, p, ref, err
	}
	dir := returnRecoveryDirectory(project, p.Event.Subject)
	canonical, dirErr := filepath.EvalSymlinks(dir)
	if p.Project != project || ref.Path != filepath.Join(dir, "plan.json") || dirErr != nil || canonical != dir || p.Before.Path != filepath.Join(dir, "before.json") || p.History.Path != filepath.Join(dir, "history.ndjson") {
		return r, p, ref, fmt.Errorf("original event recovery location differs")
	}
	if err = validateReturnRecoveryPlan(p); err != nil {
		return r, p, ref, err
	}
	return returnRecoveryFor(p, ref), p, ref, nil
}

func operateReturnRecovery(project, planPath, expected, operation string, barrier func(string) error) (returnRecoveryResult, error) {
	r, p, ref, err := readReturnRecoveryPlan(project, planPath)
	if err != nil {
		return r, err
	}
	if operation != "inspect" && operation != "continue" {
		return r, fmt.Errorf("return recovery operation unsupported")
	}
	if operation == "continue" && expected != ref.SHA256 {
		return r, fmt.Errorf("original return expected version differs")
	}
	dir := filepath.Dir(ref.Path)
	guard, err := lockfile.TryLock(filepath.Join(dir, "recovery.lock"))
	if err != nil {
		return r, err
	}
	defer guard.Close()
	if err = validateReturnRecoveryPlan(p); err != nil {
		return r, err
	}
	intentPath, receiptPath := filepath.Join(dir, "intent.json"), filepath.Join(dir, "receipt.json")
	var prior returnRecoveryIntent
	var intentRef stoppedMergeRef
	intentExists := false
	if _, err = os.Lstat(intentPath); err == nil {
		intentExists = true
		if intentRef, err = loopruntime.ReadSessionReadbackFile(intentPath, &prior); err != nil {
			return r, err
		}
		if prior.Schema != 1 || prior.Plan != ref || prior.Subject != p.Event.Subject || prior.Controller != p.Controller || prior.Readback.Status != "absent" || loop.ValidateReturnCustody(prior.Readback, prior.Subject, returnRecoverySource(p.Controller)) != nil {
			return r, fmt.Errorf("original return continuation intent differs")
		}
		if operation == "continue" {
			if err = syncStoppedMergeFile(intentPath); err != nil {
				return r, err
			}
		}
	} else if !os.IsNotExist(err) {
		return r, err
	}
	if _, err = os.Lstat(receiptPath); err == nil {
		if !intentExists {
			return r, fmt.Errorf("original return receipt lacks intent")
		}
		var receipt returnRecoveryReceipt
		if _, err = loopruntime.ReadSessionReadbackFile(receiptPath, &receipt); err != nil {
			return r, err
		}
		if receipt.Intent != intentRef || receipt.Status != "acknowledged" {
			return r, fmt.Errorf("original return receipt differs")
		}
		if operation == "continue" {
			if err = syncStoppedMergeFile(receiptPath); err != nil {
				return r, err
			}
		}
	} else if !os.IsNotExist(err) {
		return r, err
	}
	readback, err := loop.ReadReturnCustody(p.Event.Endpoint, p.Event.Subject, returnRecoverySource(p.Controller))
	if err != nil {
		return r, err
	}
	r.Readback = &readback
	if readback.Status == "custodied" {
		if operation == "continue" {
			r.CustodyEvidence, err = stoppedMergeSave(filepath.Join(dir,"custody-"+readback.Custody.Registry.SHA256+".json"), readback.Custody)
			if err != nil {
				return r, err
			}
		}
		r.Status = "custodied"
		return r, nil
	}
	if intentExists || readback.Status != "absent" {
		return r, nil
	}
	if operation == "inspect" {
		r.Status = "qualified_absent"
		return returnRecoveryNext(r, p, ref), nil
	}
	if err = validateReturnRecoveryPlan(p); err != nil {
		return r, err
	}
	if err = validateReturnRecoveryCurrent(p); err != nil {
		return r, err
	}
	for _, source := range []stoppedMergeRef{ref, p.Before, p.History} {
		if err = stoppedMergeRead(source, nil); err != nil {
			return r, err
		}
		if err = syncStoppedMergeFile(source.Path); err != nil {
			return r, err
		}
		if err = stoppedMergeRead(source, nil); err != nil {
			return r, err
		}
	}
	for _, directory := range []string{dir, filepath.Dir(dir), filepath.Dir(filepath.Dir(dir))} {
		if err = filex.SyncDir(directory); err != nil {
			return r, err
		}
	}
	actual, err := returnRecoveryActual()
	if err != nil || actual != p.Controller {
		return r, fmt.Errorf("actual return controller changed before intent")
	}
	intent := returnRecoveryIntent{1, ref, p.Event.Subject, p.Controller, readback}
	if barrier != nil {
		if err = barrier("before_intent"); err != nil {
			return r, err
		}
	}
	if intentRef, err = stoppedMergeSave(intentPath, intent); err != nil {
		return r, err
	}
	if barrier != nil {
		if err = barrier("after_intent"); err != nil {
			return r, err
		}
	}
	r.Effects = append(r.Effects, "same_event_bounded_packet_attempt")
	if err = loop.ContinueReturnPacket(p.Event.Endpoint, p.Event.Subject.Packet); err != nil {
		return r, err
	}
	if barrier != nil {
		if err = barrier("after_ack"); err != nil {
			return r, err
		}
	}
	if _, err = stoppedMergeSave(receiptPath, returnRecoveryReceipt{intentRef, "acknowledged"}); err != nil {
		return r, err
	}
	if barrier != nil {
		if err = barrier("after_receipt"); err != nil {
			return r, err
		}
	}
	r.Status = "acknowledged"
	return r, nil
}


func returnRecoverySnapshot(project string) ([]byte, error) {
	if _, _, err := config.LoadRuntime(project); err != nil {
		return nil, err
	}
	path, err := filepath.EvalSymlinks(filepath.Join(project, ".noodle", "state.snapshot.json"))
	if err != nil {
		return nil, err
	}
	return config.ReadExplicitSource(path)
}
