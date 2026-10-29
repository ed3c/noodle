package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	loopruntime "github.com/poteto/noodle/runtime"
)

type successorRetirement struct {
	Schema                          int
	Plan, Request, Closure, Failure loopruntime.SessionReadbackFileRef
}

type closedSuccessor struct {
	loopruntime.OriginalHostLifetime
	Schema       int                                `json:"schema"`
	HostInstance string                             `json:"host_instance_id"`
	HostPID      int                                `json:"host_pid"`
	Resources    loopruntime.SessionReadbackFileRef `json:"resources_before_shutdown"`
	Shutdown     loopruntime.SessionReadbackFileRef `json:"shutdown_intent"`
	Receipts     loopruntime.SessionReadbackFileRef `json:"receipts_after_exit"`
}

func readRetirementSource(ref loopruntime.SessionReadbackFileRef, value any) error {
	actual, err := loopruntime.ReadSessionReadbackFile(ref.Path, value)
	if err != nil {
		return err
	}
	if actual != ref {
		return fmt.Errorf("retirement source changed: %s", ref.Path)
	}
	return nil
}

// Retiring resource debt does not turn a failed task or native request into a
// successful one. The immutable native response remains its outcome evidence.
func validateSuccessorRetirement(proof successorRetirement) (loopruntime.ProgressMaintenance, closedSuccessor, hostReceipt, []map[string]any, error) {
	var plan loopruntime.ProgressMaintenance
	var closed closedSuccessor
	var failure hostResponse
	var request, original hostRequestBinding
	fail := func(err error) (loopruntime.ProgressMaintenance, closedSuccessor, hostReceipt, []map[string]any, error) {
		return plan, closed, failure.hostReceipt, nil, err
	}
	if proof.Schema != 1 {
		return fail(fmt.Errorf("successor retirement schema unsupported"))
	}
	for _, source := range []struct {
		ref   loopruntime.SessionReadbackFileRef
		value any
	}{{proof.Plan, &plan}, {proof.Closure, &closed}, {proof.Request, &request}, {proof.Failure, &failure}} {
		if err := readRetirementSource(source.ref, source.value); err != nil {
			return fail(err)
		}
	}
	if err := loopruntime.VerifyProgressMaintenanceHandoff(proof.Plan); err != nil {
		return fail(err)
	}
	if err := readRetirementSource(plan.Request, &original); err != nil {
		return fail(err)
	}
	r := failure.hostReceipt
	if closed.Schema != 1 || failure.Status != "failed" || failure.Error == "" || r.Schema != 1 || r.Protocol != hostProtocol || r.ContextState != "failed" || r.TerminalCause != "" || r.ContextID == "" || filepath.Base(r.ContextID) != r.ContextID || r.HostPID <= 1 || r.HostPID != closed.HostPID || r.HostInstanceID != closed.HostInstance || r.HostInstanceID == plan.HostInstance {
		return fail(fmt.Errorf("retirement is not the original failed Host successor"))
	}
	if request.Schema != 1 || request.Operation != "attach" || request.RequiredHostInstanceID != closed.HostInstance || request.RequestID != r.RequestID || request.BinarySHA256 != r.BinarySHA256 || request.HostDir != plan.HostDir || r.HostDir != plan.HostDir || request.ProjectDir != original.ProjectDir || r.ProjectDir != original.ProjectDir || request.ConfigPath != original.ConfigPath || r.ConfigPath != original.ConfigPath || request.ConfigSHA256 != original.ConfigSHA256 || r.ConfigSHA256 != original.ConfigSHA256 || request.Mode != "manual" || r.Mode != "manual" || !request.CleanupCompleted || !r.CleanupCompleted {
		return fail(fmt.Errorf("retirement changed the original request or configuration binding"))
	}
	config, err := os.ReadFile(request.ConfigPath)
	if err != nil {
		return fail(err)
	}
	if hostDigest(config) != request.ConfigSHA256 {
		return fail(fmt.Errorf("retirement configuration source changed"))
	}
	if err := validateClosedSuccessor(closed, r, plan); err != nil {
		return fail(err)
	}
	var handoff loopruntime.ProgressMaintenanceHostHandoff
	if _, err := loopruntime.ReadSessionReadbackFile(filepath.Join(filepath.Dir(proof.Plan.Path), "host-handoff.json"), &handoff); err != nil {
		return fail(err)
	}
	var baseline, before []map[string]any
	var failedRow map[string]any
	if err := readRetirementSource(handoff.HostReceiptsAfter, &baseline); err != nil {
		return fail(err)
	}
	if err := readRetirementSource(closed.Receipts, &before); err != nil {
		return fail(err)
	}
	if err := readRetirementSource(proof.Failure, &failedRow); err != nil {
		return fail(err)
	}
	delete(failedRow, "status")
	delete(failedRow, "error")
	// Fix the complete reviewed history: the maintained predecessor followed by
	// exactly this failed startup response, preserving unknown-field differences.
	want := append(append([]map[string]any(nil), baseline...), failedRow)
	if !reflect.DeepEqual(before, want) {
		return fail(fmt.Errorf("retirement history differs from the original handoff and startup response"))
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		return fail(err)
	}
	var roundtrip map[string]any
	if err := json.Unmarshal(encoded, &roundtrip); err != nil {
		return fail(err)
	}
	if !reflect.DeepEqual(roundtrip, failedRow) {
		return fail(fmt.Errorf("retirement receipt has fields unsupported by Host persistence"))
	}
	if err := validateRetirementHistory(before); err != nil {
		return fail(err)
	}
	return plan, closed, r, before, nil
}

func validateRetirementHistory(rows []map[string]any) error {
	seenRequest, seenContext := map[string]bool{}, map[string]bool{}
	for _, row := range rows {
		request, _ := row["request_id"].(string)
		context, _ := row["context_id"].(string)
		if request == "" || context == "" || seenRequest[request] || seenContext[context] {
			return fmt.Errorf("retirement Host history has missing or duplicate identity")
		}
		seenRequest[request], seenContext[context] = true, true
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	var typed []hostReceipt
	if err := json.Unmarshal(encoded, &typed); err != nil {
		return err
	}
	encoded, err = json.Marshal(typed)
	if err != nil {
		return err
	}
	var persisted []map[string]any
	if err := json.Unmarshal(encoded, &persisted); err != nil {
		return err
	}
	if !reflect.DeepEqual(rows, persisted) {
		return fmt.Errorf("retirement history has fields unsupported by Host persistence")
	}
	return nil
}

func successorRetirementPath(directory, contextID string) string {
	return filepath.Join(directory, "retired-successors", contextID+".json")
}

func requireRetirementHistoryPrefix(live, before []map[string]any) error {
	if err := validateRetirementHistory(live); err != nil {
		return err
	}
	if len(live) < len(before) || !reflect.DeepEqual(live[:len(before)], before) {
		return fmt.Errorf("retired successor history was removed, replaced or reordered")
	}
	return nil
}

// Later legitimate admissions append rows. Every baseline row remains fixed;
// deleting the retired row cannot make its published proof disappear.
func verifySuccessorRetirements(directory string, receipts []hostReceipt) (map[string]bool, error) {
	entries, err := os.ReadDir(filepath.Join(directory, "retired-successors"))
	if os.IsNotExist(err) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	var live []map[string]any
	if _, err := loopruntime.ReadSessionReadbackFile(filepath.Join(directory, "receipts.json"), &live); err != nil {
		return nil, err
	}
	seenRequest, seenContext := map[string]bool{}, map[string]bool{}
	for _, r := range receipts {
		if seenRequest[r.RequestID] || seenContext[r.ContextID] {
			return nil, fmt.Errorf("retirement Host history has duplicate request or context identity")
		}
		seenRequest[r.RequestID], seenContext[r.ContextID] = true, true
	}
	retired := map[string]bool{}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			return nil, fmt.Errorf("retirement marker is not a regular file")
		}
		var proof successorRetirement
		marker := filepath.Join(directory, "retired-successors", entry.Name())
		if _, err := loopruntime.ReadSessionReadbackFile(marker, &proof); err != nil {
			return nil, err
		}
		plan, _, r, before, err := validateSuccessorRetirement(proof)
		if err != nil {
			return nil, err
		}
		if plan.HostDir != directory || marker != successorRetirementPath(directory, r.ContextID) {
			return nil, fmt.Errorf("retired successor marker belongs to a different Host or context")
		}
		if err := requireRetirementHistoryPrefix(live, before); err != nil {
			return nil, err
		}
		retired[r.ContextID] = true
	}
	return retired, nil
}
