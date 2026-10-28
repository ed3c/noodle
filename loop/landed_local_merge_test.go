package loop

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

func landedMergeFixture(t *testing.T) (*Loop, LandedLocalMergeProof, loopruntime.SessionReadbackFileRef) {
	t.Helper()
	tempRoot, tempErr := filepath.EvalSymlinks(os.TempDir())
	if tempErr != nil {
		t.Fatal(tempErr)
	}
	t.Setenv("TMPDIR", tempRoot)
	l, cook, cmd, _ := openShellLocalTaskFixture(t, "pi")
	localTaskRealWorktree(t, l)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	put := func(name string, v any) loopruntime.SessionReadbackFileRef {
		path := filepath.Join(root, name)
		recoveryWriteJSON(t, path, v)
		ref, e := loopruntime.ReadSessionReadbackFile(path, nil)
		if e != nil {
			t.Fatal(e)
		}
		return ref
	}
	ref := func(path string) loopruntime.SessionReadbackFileRef {
		v, e := loopruntime.ReadSessionReadbackFile(path, nil)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	binary := put("noodle", map[string]string{"fixture": "not executable"})
	configPath := filepath.Join(root, "noodle.toml")
	requestChangesWrite(t, configPath, []byte("[runtime]\nadmitted_orders = [\""+cook.orderID+"\"]\n"))
	configRef := ref(configPath)
	request := put("host-request.json", map[string]any{"schema": 1, "operation": "attach", "request_id": "original-request", "project_dir": l.projectDir, "config_path": configPath, "config_sha256": configRef.SHA256, "binary_sha256": binary.SHA256, "status": "accepted"})
	envelope := put("task-envelope.json", map[string]string{"fixture": "original envelope"})
	order := l.canonical.Orders[cook.orderID]
	stage := order.Stages[0]
	attempt := stage.Attempts[0]
	head, e := recoveryGit(cook.worktreePath, "rev-parse", "HEAD")
	if e != nil {
		t.Fatal(e)
	}
	tree, e := recoveryGit(cook.worktreePath, "rev-parse", "HEAD^{tree}")
	if e != nil {
		t.Fatal(e)
	}
	sources := map[string]loopruntime.SessionReadbackFileRef{}
	for _, name := range []string{"spawn.json", "openshell-dispatch.json", "delivery/result.json", "delivery/changes.patch", "delivery/publish.json"} {
		sources[name] = ref(filepath.Join(l.runtimeDir, "sessions", attempt.SessionID, name))
	}
	for _, name := range []string{"instruction_mapping", "instruction_manifest", "instruction_input"} {
		sources[name] = put(name+".json", map[string]string{"fixture": name})
	}
	instructions := &loopruntime.CompletedInstructionEvidence{Directory: filepath.Join(root, "instructions"), GuestDirectory: "/sandbox/instructions-fixture", Manifest: sources["instruction_manifest"]}
	nativeSources := make(map[string]loopruntime.SessionReadbackFileRef, len(sources)+3)
	for name, source := range sources {
		nativeSources[name] = source
	}
	for _, name := range []string{"meta.json", "native_custody", "canonical_snapshot"} {
		nativeSources[name] = loopruntime.SessionReadbackFileRef{Path: filepath.Join(root, name), SHA256: strings.Repeat("e", 64)}
	}
	native := landedMergeResponse{Schema: 1, Status: "observed", HostInstanceID: "original-host", ContextID: "original-context", RequestID: "original-request", Effects: []string{}, Sources: map[string]loopruntime.SessionReadbackFileRef{"client_binary": binary, "original_request": request, "config": configRef}, Observation: SessionReadback{SchemaVersion: 1, Status: "observed", OrderID: order.OrderID, StageIndex: 0, AttemptID: attempt.AttemptID, SessionID: attempt.SessionID, OrderStatus: "active", StageStatus: "review", AttemptStatus: "completed", Runtime: stage.Runtime, Provider: stage.Provider, Model: stage.Model, WorktreePath: cook.worktreePath, Effects: []string{}, Execution: loopruntime.CompletedSessionReadback{CandidateHead: head, CommandSHA256: strings.Repeat("c", 64), NativeAttemptID: "native-attempt", NativePhase: "Stopped", StopAcknowledged: true, MainProcessInstanceID: "original-main", Sources: nativeSources, Instructions: instructions}}}
	response := put("response.json", native)
	observation := put("observation.json", map[string]any{"schema": 1, "kind": "local_task_session_readback_observation", "validation": "verified", "exit_code": 0, "noodle": binary, "request": request, "task_envelope": envelope, "requested": map[string]string{"order_id": order.OrderID, "session_id": attempt.SessionID}, "native_response": native})
	var candidate map[string]any
	if json.Unmarshal(recoveryRead(t, cmd.Value), &candidate) != nil {
		t.Fatal("candidate fixture")
	}
	candidate["task_envelope"] = envelope
	candidate["native_execution"] = map[string]any{
		"native_attempt_id": "native-attempt", "command_sha256": strings.Repeat("c", 64),
		"candidate_head": head, "native_exec_exit_code": 0, "native_phase": "Stopped",
		"stop_acknowledged": true, "main_process_instance_id": "original-main", "sources": sources, "instructions": instructions,
	}
	candidateRef := put("candidate.json", candidate)
	binding := localTaskMergeBinding{Candidate: localTaskFileRef{Path: candidateRef.Path, SHA256: candidateRef.SHA256}, Head: head, Tree: tree}
	stage.Status = state.StageMerging
	stage.Merge = &state.MergeRecoveryNode{WorktreeName: attempt.WorktreeName, Mode: "local"}
	if stage.Extra == nil {
		stage.Extra = map[string]json.RawMessage{}
	}
	stage.Extra[localTaskMergeKey], err = json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	order.Stages[0] = stage
	l.canonical.Orders[order.OrderID] = order
	review := l.canonical.PendingReviews[order.OrderID]
	review.Reason = "merge failed: exit status 1"
	l.canonical.PendingReviews[order.OrderID] = review
	for _, id := range []string{"event-8-effect-0", "event-12-effect-0"} {
		payload, _ := json.Marshal(map[string]any{"order_id": order.OrderID, "stage_index": 0, "worktree_name": attempt.WorktreeName, "merge_mode": "local", "merge_branch": ""})
		l.effectLedger.Record(reducer.Effect{EffectID: id, Type: reducer.EffectMerge, Payload: payload})
	}
	dispatchPayload, _ := json.Marshal(map[string]any{"order_id": order.OrderID, "stage_index": 0, "attempt_id": attempt.AttemptID})
	l.effectLedger.Record(reducer.Effect{EffectID: "event-4-effect-0", Type: reducer.EffectDispatch, Payload: dispatchPayload})
	ackPayload, _ := json.Marshal(map[string]any{"order_id": order.OrderID, "stage_index": 0})
	l.effectLedger.Record(reducer.Effect{EffectID: "event-9-effect-0", Type: reducer.EffectAck, Payload: ackPayload})
	if err = l.persistCanonicalCheckpoint(); err != nil {
		t.Fatal(err)
	}
	recoveryWriteJSON(t, filepath.Join(l.runtimeDir, "loop-events.ndjson"), map[string]any{"type": "merge_failed", "error": "exit status 1"})
	recoveryWriteJSON(t, filepath.Join(l.runtimeDir, "control-ack.ndjson"), map[string]any{"id": "original-failed-merge", "status": "error", "error": "ambiguous merge effect"})
	runGitInRepo(t, l.projectDir, "merge", "--ff-only", head)
	script := put("soodles.py", map[string]string{"fixture": "not executable"})
	observer := put("task_session_readback.py", map[string]string{"fixture": "pinned source"})
	python := put("python", map[string]string{"fixture": "not executable"})
	taskRequest := put("task-request.json", map[string]string{"fixture": "same original request"})
	intent := put("observer-intent.json", map[string]any{"source_commit": strings.Repeat("a", 40), "python": python, "sources": []loopruntime.SessionReadbackFileRef{script, observer}, "request": taskRequest, "argv": []string{python.Path, "-B", script.Path, "loop", "inspect", "--request-file", taskRequest.Path, "--capture-dir", root}})
	captureExecution := map[string]any{}
	for key, value := range candidate["native_execution"].(map[string]any) {
		captureExecution[key] = value
	}
	captureExecution["observation"] = response
	stdout := put("observer-stdout.json", map[string]any{"owner": "soodles.loop", "request": taskRequest, "inner": map[string]any{"candidate": candidateRef}, "capture": map[string]any{"owner": "issue-execution.local-task.capture", "status": "observed", "task_envelope": envelope, "gaps": []string{}, "attempts": []any{map[string]any{"native_completion": captureExecution}}}})
	stderr := put("observer-stderr.json", map[string]any{"fixture": true})
	receipt := put("observer-receipt.json", map[string]any{"exit_code": 0, "stdout": stdout, "stderr": stderr})
	proof := LandedLocalMergeProof{1, observation, response, intent, receipt}
	return l, proof, put("proof.json", proof)
}

func TestLandedLocalMergeRepairsOnlyStoppedProjectionAndPreservesHistory(t *testing.T) {
	l, _, proof := landedMergeFixture(t)
	before := recoveryRead(t, filepath.Join(l.runtimeDir, "state.snapshot.json"))
	inspection, err := InspectLandedLocalMerge(l.projectDir, proof.Path, proof.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status != "observed" || len(inspection.Effects) != 0 || len(inspection.RetiredEffectIDs) != 4 {
		t.Fatalf("inspection %+v", inspection)
	}
	if string(recoveryRead(t, filepath.Join(l.runtimeDir, "state.snapshot.json"))) != string(before) {
		t.Fatal("inspect modified canonical state")
	}
	result, err := ReconcileLandedLocalMerge(l.projectDir, proof.Path, proof.SHA256, inspection.Version)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "reconciled" {
		t.Fatal(result.Status)
	}
	after, err := reducer.ReadSnapshot(filepath.Join(l.runtimeDir, "state.snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	var prior reducer.DurableSnapshot
	if json.Unmarshal(before, &prior) != nil {
		t.Fatal("before")
	}
	old := prior.State.Orders[result.OrderID]
	now := after.State.Orders[result.OrderID]
	if now.Status != state.OrderCompleted || now.Stages[0].Status != state.StageCompleted || !reflect.DeepEqual(old.Stages[0].Attempts, now.Stages[0].Attempts) {
		t.Fatal("original attempt history changed")
	}
	if string(recoveryRead(t, result.Sources["canonical_before"].Path)) != string(before) {
		t.Fatal("original failure snapshot not retained byte-for-byte")
	}
	if prior.State.PendingReviews[result.OrderID].Reason != "merge failed: exit status 1" {
		t.Fatal("original error missing")
	}
	for i, r := range after.EffectLedger {
		if r.Effect.Type == reducer.EffectMerge || r.Effect.Type == reducer.EffectDispatch || r.Effect.Type == reducer.EffectAck {
			if r.Result == nil || r.Result.Status != reducer.EffectResultCancelled || r.Result.Timestamp.IsZero() {
				t.Fatal("historical merge execution fabricated")
			}
			if !reflect.DeepEqual(r.Effect, prior.EffectLedger[i].Effect) {
				t.Fatal("original effect changed")
			}
		} else if !reflect.DeepEqual(r, prior.EffectLedger[i]) {
			t.Fatal("unrelated history changed")
		}
	}
	readback, readErr := InspectLandedLocalMerge(l.projectDir, proof.Path, proof.SHA256)
	if readErr != nil || readback.Status != "reconciled" || len(readback.Effects) != 0 || readback.Sources["receipt"] != result.Sources["receipt"] {
		t.Fatalf("completed inspect: %+v %v", readback, readErr)
	}
	again, err := ReconcileLandedLocalMerge(l.projectDir, proof.Path, proof.SHA256, inspection.Version)
	if err != nil || !reflect.DeepEqual(result, again) {
		t.Fatalf("explicit same-intent readback: %+v %v", again, err)
	}
}

func TestLandedLocalMergeRefusesActiveOwnerAndStaleSources(t *testing.T) {
	for _, condition := range []string{"owner-active", "wrong-version", "dirty-control", "different-head", "running-effect", "foreign-effect", "proof-drift", "different-session", "missing-stop"} {
		t.Run(condition, func(t *testing.T) {
			l, p, proof := landedMergeFixture(t)
			inspection, err := InspectLandedLocalMerge(l.projectDir, proof.Path, proof.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			var held *lockfile.Lock
			switch condition {
			case "owner-active":
				held, err = lockfile.TryLock(filepath.Join(l.runtimeDir, "noodle.lock"))
				if err != nil {
					t.Fatal(err)
				}
				defer held.Close()
			case "wrong-version":
				inspection.Version = strings.Repeat("f", 64)
			case "dirty-control":
				requestChangesWrite(t, filepath.Join(l.projectDir, "candidate.txt"), []byte("changed"))
			case "different-head":
				runGitInRepo(t, l.projectDir, "commit", "--allow-empty", "-m", "unrelated advance")
			case "running-effect":
				if err = l.effectLedger.MarkRunning("event-8-effect-0"); err != nil {
					t.Fatal(err)
				}
				if err = l.persistCanonicalCheckpoint(); err != nil {
					t.Fatal(err)
				}
			case "foreign-effect":
				l.effectLedger.Record(reducer.Effect{EffectID: "foreign", Type: reducer.EffectMerge, Payload: []byte(`{"order_id":"foreign","stage_index":0}`)})
				if err = l.persistCanonicalCheckpoint(); err != nil {
					t.Fatal(err)
				}
			case "proof-drift":
				requestChangesWrite(t, p.ObserverIntent.Path, []byte("changed"))
			case "different-session", "missing-stop":
				var response landedMergeResponse
				if err = landedMergeReadRef(p.Response, &response); err != nil {
					t.Fatal(err)
				}
				if condition == "different-session" {
					response.Observation.SessionID = "foreign"
				} else {
					response.Observation.Execution.StopAcknowledged = false
				}
				recoveryWriteJSON(t, p.Response.Path, response)
			}
			before := recoveryRead(t, filepath.Join(l.runtimeDir, "state.snapshot.json"))
			if _, err = ReconcileLandedLocalMerge(l.projectDir, proof.Path, proof.SHA256, inspection.Version); err == nil {
				t.Fatal("changed authority allowed repair")
			}
			if string(recoveryRead(t, filepath.Join(l.runtimeDir, "state.snapshot.json"))) != string(before) {
				t.Fatal("refusal changed canonical state")
			}
		})
	}
}

func TestLandedLocalMergeInterruptedIntentRequiresExplicitSameVersion(t *testing.T) {
	for _, point := range []string{"after_intent", "after_canonical"} {
		t.Run(point, func(t *testing.T) {
			l, _, proof := landedMergeFixture(t)
			inspect, err := InspectLandedLocalMerge(l.projectDir, proof.Path, proof.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			r, err := reconcileLandedLocalMerge(l.projectDir, proof.Path, proof.SHA256, inspect.Version, func(at string) error {
				if at == point {
					return errors.New("fixture interruption")
				}
				return nil
			})
			if err == nil || r.Status != "unknown" {
				t.Fatalf("interruption hidden: %+v %v", r, err)
			}
			if _, err = os.Stat(filepath.Join(l.runtimeDir, "landed-local-merge", proof.SHA256, "receipt.json")); !os.IsNotExist(err) {
				t.Fatal("interrupted repair declared complete")
			}
			done, err := ReconcileLandedLocalMerge(l.projectDir, proof.Path, proof.SHA256, inspect.Version)
			if err != nil || done.Status != "reconciled" {
				t.Fatalf("explicit same owner repair: %+v %v", done, err)
			}
		})
	}
}

func TestLandedLocalMergeObservationRunsNoGitHooksAndIgnoresSelectionEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX hook fixture")
	}
	l, _, proof := landedMergeFixture(t)
	marker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	hook := filepath.Join(t.TempDir(), "monitor-hook")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf ran > "+strconv.Quote(marker)+"\nprintf '\\0'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	runGitInRepo(t, l.projectDir, "config", "core.fsmonitor", hook)
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "foreign.git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "foreign-index"))
	observed, err := InspectLandedLocalMerge(l.projectDir, proof.Path, proof.SHA256)
	if err != nil || observed.Status != "observed" {
		t.Fatalf("safe observation: %+v %v", observed, err)
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("read-only observation ran project hook")
	}
}

func TestLandedLocalMergeRejectsChangedCandidateExecutionProjection(t *testing.T) {
	for _, condition := range []string{"command", "instruction", "stable-source", "capture-only-reference"} {
		t.Run(condition, func(t *testing.T) {
			l, _, proof := landedMergeFixture(t)
			var snapshot reducer.DurableSnapshot
			if err := json.Unmarshal(recoveryRead(t, filepath.Join(l.runtimeDir, "state.snapshot.json")), &snapshot); err != nil {
				t.Fatal(err)
			}
			for id, order := range snapshot.State.Orders {
				stage := order.Stages[0]
				var binding localTaskMergeBinding
				if err := json.Unmarshal(stage.Extra[localTaskMergeKey], &binding); err != nil {
					t.Fatal(err)
				}
				var candidate map[string]any
				if err := json.Unmarshal(recoveryRead(t, binding.Candidate.Path), &candidate); err != nil {
					t.Fatal(err)
				}
				execution := candidate["native_execution"].(map[string]any)
				switch condition {
				case "command":
					execution["command_sha256"] = strings.Repeat("f", 64)
				case "instruction":
					execution["instructions"].(map[string]any)["guest_directory"] = "/sandbox/foreign"
				case "stable-source":
					execution["sources"].(map[string]any)["delivery/changes.patch"].(map[string]any)["sha256"] = strings.Repeat("f", 64)
				case "capture-only-reference":
					execution["observation"] = proof
				}
				recoveryWriteJSON(t, binding.Candidate.Path, candidate)
				binding.Candidate.SHA256 = recoveryDigest(recoveryRead(t, binding.Candidate.Path))
				stage.Extra[localTaskMergeKey], _ = json.Marshal(binding)
				order.Stages[0] = stage
				snapshot.State.Orders[id] = order
			}
			recoveryWriteJSON(t, filepath.Join(l.runtimeDir, "state.snapshot.json"), snapshot)
			before := recoveryRead(t, filepath.Join(l.runtimeDir, "state.snapshot.json"))
			_, err := InspectLandedLocalMerge(l.projectDir, proof.Path, proof.SHA256)
			if err == nil || !strings.Contains(err.Error(), "candidate differs from original native execution projection") {
				t.Fatalf("changed projection: %v", err)
			}
			if !reflect.DeepEqual(before, recoveryRead(t, filepath.Join(l.runtimeDir, "state.snapshot.json"))) {
				t.Fatal("inspection mutated canonical")
			}
		})
	}
}

func TestLandedLocalMergeRejectsCaptureExecutionDifferentFromRawObservation(t *testing.T) {
	l, proof, _ := landedMergeFixture(t)
	var receipt map[string]any
	if err := json.Unmarshal(recoveryRead(t, proof.ObserverReceipt.Path), &receipt); err != nil {
		t.Fatal(err)
	}
	stdout := receipt["stdout"].(map[string]any)
	var packet map[string]any
	path := stdout["path"].(string)
	if err := json.Unmarshal(recoveryRead(t, path), &packet); err != nil {
		t.Fatal(err)
	}
	captured := packet["capture"].(map[string]any)["attempts"].([]any)[0].(map[string]any)["native_completion"].(map[string]any)
	captured["native_attempt_id"] = "foreign-attempt"
	recoveryWriteJSON(t, path, packet)
	stdout["sha256"] = recoveryDigest(recoveryRead(t, path))
	recoveryWriteJSON(t, proof.ObserverReceipt.Path, receipt)
	proof.ObserverReceipt.SHA256 = recoveryDigest(recoveryRead(t, proof.ObserverReceipt.Path))
	path = filepath.Join(filepath.Dir(path), "changed-proof.json")
	recoveryWriteJSON(t, path, proof)
	_, err := InspectLandedLocalMerge(l.projectDir, path, recoveryDigest(recoveryRead(t, path)))
	if err == nil || !strings.Contains(err.Error(), "capture completion differs from native execution projection") {
		t.Fatalf("changed capture: %v", err)
	}
}
