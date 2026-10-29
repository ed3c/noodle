package runtime

import (
	"github.com/poteto/noodle/dispatcher"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The real dispatch/stop bookkeeping runs against the existing local fake
// gateway. A failed transfer precedes StartOwned, so no model exec exists.
func diagnosticFailureFixture(t *testing.T) (*OpenShellRuntime, CompletedSessionBinding, *managedFixture) {
	return diagnosticFailureInstructionFixture(t, false)
}
func diagnosticFailureInstructionFixture(t *testing.T, withInstructions bool) (*OpenShellRuntime, CompletedSessionBinding, *managedFixture) {
	t.Helper()
	r, f, target := openShellRuntimeFixture(t)
	target, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(filepath.Dir(target), "execution")
	if out, err := exec.Command("git", "-C", target, "worktree", "add", "--detach", checkout, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("fixture linked checkout: %v %s", err, out)
	}
	target = checkout
	managedMarker(t, f.root, "completion-observation")
	if err := os.WriteFile(r.config.Profile.CLI, []byte("#!/bin/sh\necho 'diagnosed upload failure' >&2\nexit 23\n"), 0700); err != nil {
		t.Fatal(err)
	}
	order := "diagnostic"
	var instructionRef *dispatcher.InstructionBundleRef
	if withInstructions {
		order = "original-order"
		instructionRef = runtimeInstructionRef(t)
		r.config.Profile.InstructionVerifier = "/sandbox/payload/noodle-instructions"
		if err := os.WriteFile(filepath.Join(r.config.Profile.PayloadDirectory, "noodle-instructions"), []byte("fixture verifier"), 0700); err != nil {
			t.Fatal(err)
		}
		r.config.Profile.PayloadSHA256, err = openShellPayloadHash(r.config.Profile.PayloadDirectory)
		if err != nil {
			t.Fatal(err)
		}
	}
	h, err := r.Dispatch(t.Context(), DispatchRequest{Name: "diagnostic", Provider: "codex", Model: "fixture", Prompt: "task", SystemPrompt: "owner", InstructionBundle: instructionRef, Runtime: "openshell", WorktreePath: target, AllowPrimaryCheckout: true, EnvVars: map[string]string{"NOODLE_ORDER_ID": order, "NOODLE_STAGE_INDEX": "0"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("diagnosed failure did not settle")
	}
	if h.Outcome().Status != StatusFailed {
		t.Fatalf("outcome: %+v", h.Outcome())
	}
	if r.config.Gateway.custody.Snapshot().Records[0].Exec != nil {
		t.Fatal("fixture submitted a model")
	}
	writeReadbackJSON(t, filepath.Join(r.config.RuntimeDir, "sessions", h.ID(), "meta.json"), map[string]any{"session_id": h.ID(), "provider": "codex", "model": "fixture", "runtime": "openshell", "status": "exited", "alive": false})
	return r, CompletedSessionBinding{OrderID: order, SessionID: h.ID(), Provider: "codex", Model: "fixture", WorktreePath: target}, f
}

func TestOpenShellDiagnosticFailureReadback(t *testing.T) {
	r, b, f := diagnosticFailureFixture(t)
	before, err := os.ReadFile(filepath.Join(f.spec.Binding.StateRoot, "owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.ReadCleanupSession(t.Context(), b, b.WorktreePath, StatusFailed)
	if err != nil {
		t.Fatal(err)
	}
	if got.OutcomeStatus != StatusFailed || got.PublishedHead == "" || got.Execution.CandidateHead != "" || got.Execution.NativeExecExitCode != -1 {
		t.Fatalf("failure promoted: %+v", got)
	}
	after, err := os.ReadFile(filepath.Join(f.spec.Binding.StateRoot, "owner.json"))
	if err != nil || string(after) != string(before) {
		t.Fatal("readback rewrote original custody")
	}
	if _, err := r.ReadCompletedSession(t.Context(), b); err == nil {
		t.Fatal("not-started accepted as completed")
	}
	if _, err := r.ReadSavedProgress(t.Context(), b); err == nil {
		t.Fatal("not-started accepted as saved progress")
	}
	if _, err := r.ReadCleanupSession(t.Context(), b, b.WorktreePath, StatusCompleted); err == nil {
		t.Fatal("failure accepted as completed cleanup")
	}
}

func failedDiagnosticAuthority(t *testing.T, order string) SessionReadbackFileRef {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	diagnostic := filepath.Join(dir, "diagnostic.json")
	writeReadbackJSON(t, diagnostic, map[string]any{"failure": "original upload failed before model submission"})
	ref, err := ReadSessionReadbackFile(diagnostic, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "task-closure.json")
	writeReadbackJSON(t, path, map[string]any{"order": map[string]any{"order_id": order, "status": "failed"}, "reason": "original owner explicitly closed diagnosed failure", "diagnostics": map[string]SessionReadbackFileRef{"original_flow_diagnostics": ref}})
	authority, err := ReadSessionReadbackFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func TestOpenShellDiagnosticFailureReclaim(t *testing.T) {
	r, b, _ := diagnosticFailureFixture(t)
	repository := filepath.Join(filepath.Dir(b.WorktreePath), "repo")
	c := r.config.Gateway.custody
	original := c.Snapshot().Records[0]
	native := &reclamationClient{OpenShellClient: c.client, id: original.Worker.Identity.Sandbox.SandboxID}
	c.client = native
	authority := failedDiagnosticAuthority(t, b.OrderID)
	dir := filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID)
	kept, err := diagnosticSessionSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := r.ReadCleanupSession(t.Context(), b, repository, StatusFailed)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got, err := r.ReclaimCleanupSession(t.Context(), b, repository, StatusFailed, "original owner closed upload failure", authority)
		if err != nil {
			t.Fatal(err)
		}
		if got.Diagnostic == nil || got.Diagnostic.ExecutionBasis != openShellNotStarted || got.Diagnostic.Outcome != before.Diagnostic.Outcome || got.Diagnostic.Observation.ExecExitCode != -1 || got.Diagnostic.Observation.ExecRequestID != "" || got.PublishedHead != before.PublishedHead || got.Retirement == nil || got.Retirement.ObservationBasis != "historical_not_started_stopped_with_fresh_native_absence" || !got.Retirement.AbsenceObserved || native.deletes != 1 {
			t.Fatalf("diagnostic closure changed failed execution or replayed: %+v", got)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "repo")); !os.IsNotExist(err) {
		t.Fatal("baseline copy retained")
	}
	if err := VerifySessionReadbackFiles(kept); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repository, "worktree", "remove", b.WorktreePath).CombinedOutput(); err != nil {
		t.Fatalf("fixture original worktree owner remove: %v %s", err, out)
	}
	if got, err := r.ReadCleanupSession(t.Context(), b, repository, StatusFailed); err != nil || got.PublishedHead != before.PublishedHead || got.OutcomeStatus != StatusFailed {
		t.Fatalf("historical baseline lost after worktree removal: %+v %v", got, err)
	}
	if got := c.Snapshot().Records[0]; got.Exec != nil || got.Retirement == nil || got.Retirement.Observation.ExecutionBasis != openShellNotStarted {
		t.Fatal("invented execution or lost tombstone")
	}
	// A new canonical observation still cannot claim successful completion.
	if _, err := r.ReadCompletedSession(t.Context(), b); err == nil {
		t.Fatal("retirement invented completion")
	}
}

func TestOpenShellDiagnosticFailureRejectsUncertainOrForeignEvidence(t *testing.T) {
	for _, kind := range []string{"create-ack", "stop-ack", "stopped", "exec-unknown", "exec-observed", "no-owner", "active", "other-active", "model", "provider", "task", "stage", "retry", "context", "profile", "meta-alive", "delivery", "progress", "baseline-dirty", "baseline-copy", "disk-only-no-exec", "unknown-dispatch", "outcome", "no-diagnosis"} {
		t.Run(kind, func(t *testing.T) {
			r, b, _ := diagnosticFailureFixture(t)
			c := r.config.Gateway.custody
			dir := filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID)
			original := c.Snapshot().Records[0]
			native := &reclamationClient{OpenShellClient: c.client, id: original.Worker.Identity.Sandbox.SandboxID}
			c.client = native
			var receipt openShellDispatchReceipt
			if _, err := ReadSessionReadbackFile(filepath.Join(dir, "openshell-dispatch.json"), &receipt); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "create-ack":
				c.state.Records[0].CreateAcknowledged = false
			case "stop-ack":
				c.state.Records[0].StopAcknowledged = false
			case "stopped":
				c.state.Records[0].Stopped = nil
			case "exec-unknown", "exec-observed", "disk-only-no-exec":
				c.state.Records[0].Exec = &OpenShellCustodyExec{AttemptID: receipt.AttemptID, Command: OpenShellCommand{RequestID: openShellRequestID}, Result: OpenShellExecResult{RequestID: openShellRequestID, StartAttempted: true, ExitObserved: kind == "exec-observed"}}
			case "no-owner":
				r.config.Gateway = nil
			case "active":
				r.active[b.SessionID] = &openShellSession{}
			case "other-active":
				r.active["another-session"] = &openShellSession{}
			case "model":
				b.Model = "foreign"
			case "provider":
				b.Provider = "pi"
			case "task":
				b.OrderID = "foreign"
			case "stage":
				b.StageIndex++
			case "retry":
				b.RetryCount++
			case "context":
				receipt.ContextID = "foreign"
			case "profile":
				receipt.ProfileSHA256 = "foreign"
			case "meta-alive":
				writeReadbackJSON(t, filepath.Join(dir, "meta.json"), map[string]any{"session_id": b.SessionID, "provider": b.Provider, "model": b.Model, "runtime": "openshell", "status": "exited", "alive": true})
			case "delivery":
				if err := os.Mkdir(filepath.Join(dir, "delivery"), 0700); err != nil {
					t.Fatal(err)
				}
			case "progress":
				receipt.ProgressHead = receipt.Baseline.Head
			case "baseline-dirty":
				if err := os.WriteFile(filepath.Join(b.WorktreePath, "untracked"), []byte("unsaved work"), 0600); err != nil {
					t.Fatal(err)
				}
			case "baseline-copy":
				if err := os.WriteFile(filepath.Join(dir, "repo", "untracked"), []byte("unsaved work"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unknown-dispatch":
				receipt.Phase = "creating"
			case "outcome":
				receipt.Outcome.Status = StatusCompleted
			case "no-diagnosis":
				receipt.Error = ""
				receipt.Outcome.Reason = ""
			}
			writeReadbackJSON(t, filepath.Join(dir, "openshell-dispatch.json"), receipt)
			if kind != "disk-only-no-exec" {
				writeReadbackJSON(t, filepath.Join(c.state.Binding.StateRoot, "owner.json"), c.state)
			}
			authority := failedDiagnosticAuthority(t, b.OrderID)
			if _, err := r.ReclaimCleanupSession(t.Context(), b, b.WorktreePath, StatusFailed, "explicit closure", authority); err == nil {
				t.Fatal("uncertain/foreign failure reclaimed")
			}
			if native.deletes != 0 {
				t.Fatal("refused failure deleted native sandbox")
			}
			if _, err := os.Stat(filepath.Join(dir, "repo")); err != nil {
				t.Fatal("refused failure removed baseline copy")
			}
		})
	}
}

func TestOpenShellDiagnosticFailureRejectsSelfDeletingAndForeignAuthority(t *testing.T) {
	for _, kind := range []string{"authority-in-copy", "diagnostic-in-worker", "foreign-order", "missing-diagnostic", "empty-reason"} {
		t.Run(kind, func(t *testing.T) {
			r, b, _ := diagnosticFailureFixture(t)
			c := r.config.Gateway.custody
			native := &reclamationClient{OpenShellClient: c.client, id: c.Snapshot().Records[0].Worker.Identity.Sandbox.SandboxID}
			c.client = native
			authority := failedDiagnosticAuthority(t, b.OrderID)
			reason := "original failure explicitly closed"
			if kind == "foreign-order" {
				authority = failedDiagnosticAuthority(t, "foreign")
			}
			if kind == "empty-reason" {
				reason = " "
			}
			if kind == "authority-in-copy" {
				raw, err := os.ReadFile(authority.Path)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID, "repo", "closure.json")
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				authority, err = ReadSessionReadbackFile(path, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			if kind == "diagnostic-in-worker" || kind == "missing-diagnostic" {
				path := filepath.Join(filepath.Dir(b.WorktreePath), "repo", ".git", "diagnostic.json")
				// Use an ignored worker-local directory so clean-baseline checking
				// passes and the authority-overlap guard itself must refuse it.
				if kind == "diagnostic-in-worker" {
					if out, err := exec.Command("git", "-C", b.WorktreePath, "config", "core.excludesFile", filepath.Join(filepath.Dir(b.WorktreePath), "worker-ignore")).CombinedOutput(); err != nil {
						t.Fatalf("ignore config: %v %s", err, out)
					}
					if err := os.WriteFile(filepath.Join(filepath.Dir(b.WorktreePath), "worker-ignore"), []byte("retained-diagnostic/\n"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(filepath.Join(b.WorktreePath, "retained-diagnostic"), 0700); err != nil {
						t.Fatal(err)
					}
					path = filepath.Join(b.WorktreePath, "retained-diagnostic", "diagnostic.json")
				}
				if err := os.WriteFile(path, []byte(`{"failure":true}`), 0600); err != nil {
					t.Fatal(err)
				}
				ref, err := ReadSessionReadbackFile(path, nil)
				if err != nil {
					t.Fatal(err)
				}
				writeReadbackJSON(t, authority.Path, map[string]any{"order": map[string]string{"order_id": b.OrderID, "status": "failed"}, "reason": "closed", "diagnostics": map[string]SessionReadbackFileRef{"original_flow_diagnostics": ref}})
				authority, err = ReadSessionReadbackFile(authority.Path, nil)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "missing-diagnostic" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := r.ReclaimCleanupSession(t.Context(), b, b.WorktreePath, StatusFailed, reason, authority); err == nil {
				t.Fatal("invalid closure reclaimed")
			}
			if native.deletes != 0 {
				t.Fatal("invalid authority deleted VM")
			}
			if _, err := os.Stat(filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID, "repo")); err != nil {
				t.Fatal("invalid authority removed copy")
			}
		})
	}
}
