package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/statever"
	"github.com/poteto/noodle/internal/workspaceartifacts"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
)

type artifactCommandFixture struct {
	project, original, output, session, pause string
	host                                      hostReceipt
	calls                                     []string
}

func artifactCommandJSON(t *testing.T, path string, value any) loopruntime.SessionReadbackFileRef {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	return acceptanceFixtureJSON(t, path, value)
}

func newArtifactCommandFixture(t *testing.T) *artifactCommandFixture {
	t.Helper()
	t.Setenv("GIT_AUTHOR_NAME", "Artifact Fixture")
	t.Setenv("GIT_AUTHOR_EMAIL", "artifact@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "Artifact Fixture")
	t.Setenv("GIT_COMMITTER_EMAIL", "artifact@example.invalid")
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &artifactCommandFixture{project: filepath.Join(base, "project"), original: filepath.Join(base, "original-request.json"), output: filepath.Join(base, "continuation"), session: "original-session", pause: "original-pause"}
	if err := os.Mkdir(f.project, 0700); err != nil {
		t.Fatal(err)
	}
	acceptanceFixtureGit(t, f.project, "init")
	if err := os.WriteFile(filepath.Join(f.project, ".gitignore"), []byte(".noodle/\n.worktrees/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.project, "task.txt"), []byte("original\n"), 0644); err != nil {
		t.Fatal(err)
	}
	acceptanceFixtureGit(t, f.project, "add", ".")
	acceptanceFixtureGit(t, f.project, "-c", "commit.gpgsign=false", "commit", "-m", "fixture baseline")
	dir := filepath.Join(f.project, ".noodle", "sessions", f.session)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	execution := filepath.Join(base, "execution")
	acceptanceFixtureGit(t, f.project, "worktree", "add", "--detach", execution, "HEAD")
	b, err := workspaceartifacts.Capture(t.Context(), execution, filepath.Join(dir, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	returned := filepath.Join(dir, "returned")
	if err := os.Mkdir(returned, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".gitignore", "task.txt"} {
		raw, err := os.ReadFile(filepath.Join(b.Directory, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(returned, name), raw, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(returned, "task.txt"), []byte("accepted\n"), 0644); err != nil {
		t.Fatal(err)
	}
	r, err := workspaceartifacts.Apply(t.Context(), execution, b, returned, filepath.Join(dir, "delivery"))
	if err != nil {
		t.Fatal(err)
	}
	head, err := workspaceartifacts.Publish(t.Context(), execution, b, r, "fixture publication")
	if err != nil {
		t.Fatal(err)
	}
	acceptanceFixtureGit(t, f.project, "worktree", "remove", execution)
	acceptanceFixtureGit(t, f.project, "reset", "--hard", head)
	// Produce genuine immutable pins with the production artifact owner. Its
	// already absent copies exercise the CLI's strict reentry path; Ampere's
	// package tests cover actual deletion and interrupted surviving subsets.
	if err := workspaceartifacts.CompactRetainedPublication(t.Context(), f.project, b, r, head); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(f.project, ".worktrees", "original-worker")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "keep.txt"), []byte("worker retained"), 0600); err != nil {
		t.Fatal(err)
	}
	orderID := "original-order"
	spawn := artifactCommandJSON(t, filepath.Join(dir, "spawn.json"), map[string]any{"order_id": orderID, "session_id": f.session, "runtime": "openshell", "provider": "codex", "model": "original-model", "worktree_path": worktree, "stage_index": "0", "retry_count": 0})
	artifactCommandJSON(t, filepath.Join(dir, "meta.json"), map[string]any{"session_id": f.session, "runtime": "openshell", "provider": "codex", "model": "original-model", "status": "exited", "alive": false})
	artifactCommandJSON(t, filepath.Join(dir, "openshell-dispatch.json"), map[string]any{"Version": 1, "ContextID": "original-context", "OrderID": orderID, "SessionID": f.session, "AttemptID": "original-native-attempt", "Target": worktree, "Phase": "settled", "ProfileSHA256": strings.Repeat("a", 64), "CandidateHead": head, "Baseline": b, "Delivery": r, "Outcome": loopruntime.SessionOutcome{Status: loopruntime.StatusCompleted}, "Worker": loopruntime.OpenShellWorkerRecord{Boundary: loopruntime.OpenShellBoundaryStopped, Identity: loopruntime.OpenShellIdentity{OrderID: orderID, SessionID: f.session, AttemptID: "original-native-attempt"}}})
	candidate := artifactCommandJSON(t, filepath.Join(base, "candidate.json"), map[string]any{"schema": 1, "order_id": orderID, "session_id": f.session, "attempt_id": "canonical-attempt", "worktree": worktree, "head": head, "spawn_sha256": spawn.SHA256, "files": map[string]any{"task.txt": map[string]any{"sha256": strings.Repeat("b", 64)}}})
	merge, _ := json.Marshal(map[string]any{"candidate": candidate, "head": head, "tree": strings.Repeat("c", 40)})
	order := state.OrderNode{OrderID: orderID, Status: state.OrderCompleted, Stages: []state.StageNode{{StageIndex: 0, TaskKey: "local-task", Skill: "local-task", Provider: "codex", Model: "original-model", Runtime: "openshell", Prompt: `{"kind":"local_task","goal":"original","write_paths":["task.txt"]}`, Status: state.StageCompleted, Extra: map[string]json.RawMessage{"local_task_merge": merge}, Attempts: []state.AttemptNode{{AttemptID: "canonical-attempt", SessionID: f.session, Status: state.AttemptCompleted, WorktreeName: "original-worker"}}}}}
	evidence := artifactCommandJSON(t, filepath.Join(base, "condition.json"), map[string]any{"original": "PASS"})
	acceptance := artifactCommandJSON(t, filepath.Join(base, "acceptance.json"), loopruntime.TaskDeliveryAcceptance{Schema: 1, OrderID: orderID, SessionID: f.session, CandidateHead: head, Status: "PASS", Conditions: []loopruntime.TaskDeliveryCondition{{Condition: "original condition", Status: "PASS", Evidence: []loopruntime.SessionReadbackFileRef{evidence}}}})
	artifactCommandJSON(t, filepath.Join(f.project, ".noodle", "execution-closures", hostDigest([]byte(orderID))+".json"), map[string]any{"order": order, "reason": "original task owner accepted completed delivery", "diagnostics": map[string]loopruntime.SessionReadbackFileRef{"owner_acceptance": acceptance}})
	effectID := reducer.InitialAdmissionEffectID(orderID)
	payload, _ := json.Marshal(map[string]any{"order_id": orderID, "initial_revision": strings.Repeat("a", 32), "task_data_retention": "after_task_closure"})
	snapshot := reducer.DurableSnapshot{OrderRevision: strings.Repeat("d", 32), GeneratedAt: time.Now().UTC(), State: state.State{SchemaVersion: statever.Current, Mode: state.RunModeSupervised, LastEventID: "1", Orders: map[string]state.OrderNode{orderID: order}}, EffectLedger: []reducer.EffectLedgerRecord{{EffectID: effectID, Effect: reducer.Effect{EffectID: effectID, Type: reducer.EffectInitialAdmission, Payload: payload}, Status: reducer.EffectLedgerDone, Result: &reducer.EffectResult{EffectID: effectID, Status: reducer.EffectResultCompleted}}}}
	artifactCommandJSON(t, filepath.Join(f.project, ".noodle", "state.snapshot.json"), snapshot)
	artifactCommandJSON(t, filepath.Join(f.project, ".noodle", "status.json"), map[string]any{"loop_state": "paused"})
	artifactCommandJSON(t, filepath.Join(f.project, ".noodle", "control-ack.ndjson"), loop.ControlAck{ID: f.pause, Action: "pause", Status: "ok", DispatchState: loop.StatePaused})
	if err := os.WriteFile(filepath.Join(f.project, ".noodle", "control.lock"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(base, "original.toml")
	if err := os.WriteFile(configPath, []byte("[runtime]\nadmitted_orders=[\"original-order\"]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	configRef, err := artifactCompactionFile(configPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	artifactCommandJSON(t, filepath.Join(f.project, ".noodle", "host-config.json"), config.RuntimeSource{Path: configPath, SHA256: configRef.SHA256})
	binary := filepath.Join(base, "original-native")
	if err := os.WriteFile(binary, []byte("original binary retained"), 0700); err != nil {
		t.Fatal(err)
	}
	binaryRef, err := artifactCompactionFile(binary, nil)
	if err != nil {
		t.Fatal(err)
	}
	hostDir := filepath.Join(base, "host")
	if err := os.Mkdir(hostDir, 0700); err != nil {
		t.Fatal(err)
	}
	f.host = hostReceipt{Schema: 1, Protocol: hostProtocol, HostPID: os.Getpid(), HostInstanceID: "original-host", ContextID: "original-context", ProjectDir: f.project, ContextState: "active", Mode: "supervised", ConfigPath: configPath, ConfigSHA256: configRef.SHA256, BinaryPath: binary, BinarySHA256: binaryRef.SHA256, RequestID: "original-request", HostDir: hostDir, CleanupCompleted: true}
	artifactCommandJSON(t, filepath.Join(hostDir, "receipts.json"), []hostReceipt{f.host})
	artifactCommandJSON(t, f.original, hostRequestBinding{Schema: 1, Operation: "attach", Status: "ok", ProjectDir: f.project, RequestID: f.host.RequestID, HostDir: hostDir, BinarySHA256: binaryRef.SHA256, ConfigPath: configPath, ConfigSHA256: configRef.SHA256, Mode: f.host.Mode, EnvironmentSHA256: strings.Repeat("e", 64), CleanupCompleted: true})
	originalRead := artifactNativeRead
	t.Cleanup(func() { artifactNativeRead = originalRead })
	artifactNativeRead = func(_ context.Context, gotBinary, project, directory, request, operation string) ([]byte, error) {
		if gotBinary != binary || project != f.project || directory != hostDir || request != f.original {
			t.Fatal("readback borrowed another binary or request")
		}
		f.calls = append(f.calls, operation)
		status := "ok"
		if operation == "cleanup" {
			status = "eligible"
		}
		return json.Marshal(hostResponse{hostReceipt: f.host, Status: status})
	}
	return f
}

func TestArtifactCompactionCommandReentryPreservesOriginalOwner(t *testing.T) {
	f := newArtifactCommandFixture(t)
	ref, err := prepareArtifactCompaction(t.Context(), f.original, f.session, f.pause, f.output)
	if err != nil {
		t.Fatal(err)
	}
	var plan artifactCompactionRequest
	if _, err := artifactCompactionFile(ref.Path, &plan); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(f.project, ".noodle", "state.snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := resumeArtifactCompaction(t.Context(), ref.Path, ref.SHA256, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := resumeArtifactCompaction(t.Context(), ref.Path, ref.SHA256, true)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(f.calls) != 6 {
		t.Fatalf("readback/reentry differs: %v", f.calls)
	}
	var receipt artifactCompactionReceipt
	if _, err := artifactCompactionFile(first.Path, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.NativeReclamation != "not_performed" || receipt.OriginalContextID != f.host.ContextID || receipt.Status != "artifact_copies_compacted" {
		t.Fatal("receipt claimed native reclamation or changed context")
	}
	after, _ := os.ReadFile(filepath.Join(f.project, ".noodle", "state.snapshot.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("continuation rewrote original canonical state")
	}
	if err := artifactCompactionCheckRefs(plan.Evidence); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.project, ".worktrees", "original-worker", "keep.txt")); err != nil {
		t.Fatal("worker worktree was removed")
	}
}

func TestArtifactCompactionCommandFailsClosedAtBoundaries(t *testing.T) {
	for _, variant := range []string{"request-hash", "tool", "checkpoint-clock", "condition-drift", "lost-pin", "lock", "host-unknown", "host-changed", "resume-ack"} {
		t.Run(variant, func(t *testing.T) {
			f := newArtifactCommandFixture(t)
			ref, err := prepareArtifactCompaction(t.Context(), f.original, f.session, f.pause, f.output)
			if err != nil {
				t.Fatal(err)
			}
			expected := ref.SHA256
			switch variant {
			case "request-hash":
				expected = strings.Repeat("0", 64)
			case "tool":
				var p artifactCompactionRequest
				_, _ = artifactCompactionFile(ref.Path, &p)
				p.Tool.SHA256 = strings.Repeat("0", 64)
				ref = artifactCommandJSON(t, ref.Path, p)
				expected = ref.SHA256
			case "checkpoint-clock":
				p := filepath.Join(f.project, ".noodle", "state.snapshot.json")
				var s reducer.DurableSnapshot
				_, _ = artifactCompactionFile(p, &s)
				s.GeneratedAt = s.GeneratedAt.Add(time.Second)
				artifactCommandJSON(t, p, s)
			case "condition-drift":
				artifactCommandJSON(t, filepath.Join(filepath.Dir(f.project), "condition.json"), map[string]any{"changed": true})
			case "lost-pin":
				raw := acceptanceFixtureGit(t, f.project, "for-each-ref", "--format=%(refname)", "refs/noodle/retained/")
				acceptanceFixtureGit(t, f.project, "update-ref", "-d", strings.Split(raw, "\n")[0])
			case "lock":
				lock, err := lockfile.TryLock(filepath.Join(f.project, ".noodle", "control.lock"))
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			case "host-unknown":
				artifactNativeRead = func(context.Context, string, string, string, string, string) ([]byte, error) {
					return nil, errors.New("unknown original read")
				}
			case "host-changed":
				f.host.HostInstanceID = "replacement"
			case "resume-ack":
				artifactCommandJSON(t, filepath.Join(f.project, ".noodle", "control-ack.ndjson"), loop.ControlAck{ID: "resume", Action: "resume", Status: "ok", DispatchState: loop.StateRunning})
			}
			if _, err := resumeArtifactCompaction(t.Context(), ref.Path, expected, true); err == nil {
				t.Fatal("unsafe continuation succeeded")
			}
			if _, err := os.Lstat(filepath.Join(f.output, "completion.json")); !os.IsNotExist(err) {
				t.Fatal("refusal wrote completion")
			}
		})
	}
}

func TestArtifactCompactionCommandRegisteredAndReadOnlyCLI(t *testing.T) {
	cmd, _, err := NewRootCmd().Find([]string{"artifacts", "resume-compaction"})
	if err != nil || cmd.Name() != "resume-compaction" {
		t.Fatal("native command missing")
	}
	if _, err := artifactOriginalCLI(t.Context(), "unused", "unused", "unused", "unused", "attach"); err == nil {
		t.Fatal("original CLI admitted mutation")
	}
}

func TestArtifactCompactionPausePrecedesCleanupRearm(t *testing.T) {
	f := newArtifactCommandFixture(t)
	status := filepath.Join(f.project, ".noodle", "status.json")
	artifactCommandJSON(t, status, map[string]any{"loop_state": "running"})
	if _, err := prepareArtifactCompaction(t.Context(), f.original, f.session, f.pause, f.output); err == nil || len(f.calls) != 0 {
		t.Fatal("unpaused preparation called original cleanup rearm")
	}
	artifactCommandJSON(t, status, map[string]any{"loop_state": "paused"})
	ref, err := prepareArtifactCompaction(t.Context(), f.original, f.session, f.pause, f.output)
	if err != nil {
		t.Fatal(err)
	}
	artifactCommandJSON(t, status, map[string]any{"loop_state": "running"})
	if _, err := resumeArtifactCompaction(t.Context(), ref.Path, ref.SHA256, true); err == nil || len(f.calls) != 2 {
		t.Fatal("unpaused continuation called original cleanup rearm")
	}
}

func TestArtifactCompactionUnknownRetainedAndExplicitlyReentrant(t *testing.T) {
	f := newArtifactCommandFixture(t)
	ref, err := prepareArtifactCompaction(t.Context(), f.original, f.session, f.pause, f.output)
	if err != nil {
		t.Fatal(err)
	}
	var p artifactCompactionRequest
	if _, err := artifactCompactionFile(ref.Path, &p); err != nil {
		t.Fatal(err)
	}
	observed, err := artifactCompactionReadHost(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	unknown, err := applyArtifactCompaction(ctx, p, ref, observed)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected unresolved cancellation: %v", err)
	}
	var receipt artifactCompactionReceipt
	if _, err := artifactCompactionFile(unknown.Path, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "UNKNOWN" || receipt.NativeReclamation != "not_performed" {
		t.Fatal("unresolved continuation promoted outcome")
	}
	if _, err := resumeArtifactCompaction(t.Context(), ref.Path, ref.SHA256, true); err != nil {
		t.Fatal(err)
	}
	if _, err := artifactCompactionFile(unknown.Path, nil); err != nil {
		t.Fatal("original unknown attempt evidence was removed")
	}
}

func TestArtifactCompactionRejectsEvidenceInCopyOrWorker(t *testing.T) {
	f := newArtifactCommandFixture(t)
	ref, err := prepareArtifactCompaction(t.Context(), f.original, f.session, f.pause, f.output)
	if err != nil {
		t.Fatal(err)
	}
	var p artifactCompactionRequest
	if _, err := artifactCompactionFile(ref.Path, &p); err != nil {
		t.Fatal(err)
	}
	for _, root := range append(artifactCompactionRoots(p), p.Owner.Binding.WorktreePath) {
		evidence := artifactCommandJSON(t, filepath.Join(root, "nested-acceptance.json"), map[string]any{"PASS": true})
		if err := artifactCompactionEvidence(p, map[string]loopruntime.SessionReadbackFileRef{"nested": evidence}); err == nil {
			t.Fatal("evidence inside execution data accepted")
		}
	}
}
