package loop

import (
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

func notStartedCleanupReadback(b loopruntime.CompletedSessionBinding, ref loopruntime.SessionReadbackFileRef) loopruntime.CleanupSessionReadback {
	id := loopruntime.OpenShellIdentity{OrderID: b.OrderID, SessionID: b.SessionID, AttemptID: "original-native-attempt", Sandbox: loopruntime.OpenShellSandboxRef{Gateway: "original-gateway", Workspace: "original-workspace", SandboxID: "original-worker"}, SandboxName: "original-worker-name", ConfigSource: "original-config"}
	observation := loopruntime.OpenShellCompletedObservation{ExecutionBasis: "not_started", Identity: id, Task: loopruntime.OpenShellTask{OrderID: id.OrderID, SessionID: id.SessionID, AttemptID: id.AttemptID}, ExecExitCode: -1, MainProcessInstanceID: "original-main", Phase: "Stopped", CreateAcknowledged: true, StopAcknowledged: true}
	return loopruntime.CleanupSessionReadback{OutcomeStatus: loopruntime.StatusFailed, PublishedHead: strings.Repeat("a", 40), Execution: loopruntime.CompletedSessionReadback{ProfileSHA256: strings.Repeat("b", 64), NativeAttemptID: id.AttemptID, NativeExecExitCode: -1, NativePhase: "Stopped", StopAcknowledged: true, MainProcessInstanceID: observation.MainProcessInstanceID, Sources: map[string]loopruntime.SessionReadbackFileRef{"native_custody": ref}}, Diagnostic: &loopruntime.DiagnosticFailureReadback{ExecutionBasis: "not_started", Outcome: loopruntime.SessionOutcome{Status: loopruntime.StatusFailed, ExitCode: -1, Reason: "original upload failed before Exec"}, Observation: observation}}
}

func TestCleanupNotStartedFailureRequiresExplicitClosureAndPreservesExit(t *testing.T) {
	l, cook, request := failedClosureFixture(t)
	rt := l.deps.Runtimes["openshell"].(*cleanupReadbackRuntime)
	rt.read = func(b loopruntime.CompletedSessionBinding, _ string, _ loopruntime.SessionStatus) (loopruntime.CleanupSessionReadback, error) {
		return notStartedCleanupReadback(b, request.Diagnostics), nil
	}
	if err := l.checkCompletedTasks(t.Context()); err == nil {
		t.Fatal("unclosed failure became eligible")
	}
	if err := l.closeFailedTask(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if err := l.cleanupCompleted(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if l.canonical.Orders[cook.orderID].Status != state.OrderFailed {
		t.Fatal("diagnostic failure promoted")
	}
	if len(rt.calls) != 0 {
		t.Fatal("diagnostic inspection dispatched work")
	}
	got, _ := rt.read(loopruntime.CompletedSessionBinding{OrderID: cook.orderID, SessionID: request.SessionID}, "", loopruntime.StatusFailed)
	if got.Execution.NativeExecExitCode != -1 || got.Diagnostic.Outcome.ExitCode != -1 {
		t.Fatal("unsubmitted execution relabelled successful")
	}
}

func TestCleanupRejectsMalformedNotStartedDiagnostic(t *testing.T) {
	l, _, request := failedClosureFixture(t)
	rt := l.deps.Runtimes["openshell"].(*cleanupReadbackRuntime)
	for _, tc := range []struct {
		name   string
		change func(*loopruntime.CleanupSessionReadback)
	}{
		{"missing-diagnostic", func(v *loopruntime.CleanupSessionReadback) { v.Diagnostic = nil }},
		{"unknown-basis", func(v *loopruntime.CleanupSessionReadback) { v.Diagnostic.ExecutionBasis = "unknown" }},
		{"fake-zero-exit", func(v *loopruntime.CleanupSessionReadback) { v.Execution.NativeExecExitCode = 0 }},
		{"failed-exec", func(v *loopruntime.CleanupSessionReadback) { v.Execution.NativeExecExitCode = 23 }},
		{"successful-outcome", func(v *loopruntime.CleanupSessionReadback) { v.Diagnostic.Outcome.Status = loopruntime.StatusCompleted }},
		{"successful-diagnostic-exit", func(v *loopruntime.CleanupSessionReadback) { v.Diagnostic.Outcome.ExitCode = 0 }},
		{"missing-reason", func(v *loopruntime.CleanupSessionReadback) { v.Diagnostic.Outcome.Reason = " " }},
		{"create-unacknowledged", func(v *loopruntime.CleanupSessionReadback) { v.Diagnostic.Observation.CreateAcknowledged = false }},
		{"stop-unacknowledged", func(v *loopruntime.CleanupSessionReadback) { v.Diagnostic.Observation.StopAcknowledged = false }},
		{"wrong-phase", func(v *loopruntime.CleanupSessionReadback) { v.Diagnostic.Observation.Phase = "Ready" }},
		{"wrong-order", func(v *loopruntime.CleanupSessionReadback) { v.Diagnostic.Observation.Identity.OrderID = "foreign" }},
		{"wrong-session", func(v *loopruntime.CleanupSessionReadback) { v.Diagnostic.Observation.Identity.SessionID = "foreign" }},
		{"wrong-native-attempt", func(v *loopruntime.CleanupSessionReadback) { v.Diagnostic.Observation.Identity.AttemptID = "foreign" }},
		{"wrong-task", func(v *loopruntime.CleanupSessionReadback) { v.Diagnostic.Observation.Task.SessionID = "foreign" }},
		{"missing-main", func(v *loopruntime.CleanupSessionReadback) {
			v.Diagnostic.Observation.MainProcessInstanceID = ""
			v.Execution.MainProcessInstanceID = ""
		}},
		{"changed-main", func(v *loopruntime.CleanupSessionReadback) {
			v.Diagnostic.Observation.MainProcessInstanceID = "another-main"
		}},
		{"missing-native-owner", func(v *loopruntime.CleanupSessionReadback) { delete(v.Execution.Sources, "native_custody") }},
		{"observation-basis", func(v *loopruntime.CleanupSessionReadback) { v.Diagnostic.Observation.ExecutionBasis = "" }},
		{"observation-exit", func(v *loopruntime.CleanupSessionReadback) { v.Diagnostic.Observation.ExecExitCode = 0 }},
		{"exec-request", func(v *loopruntime.CleanupSessionReadback) {
			v.Diagnostic.Observation.ExecRequestID = "unknown-submission"
		}},
		{"exec-attempt", func(v *loopruntime.CleanupSessionReadback) {
			v.Diagnostic.Observation.ExecAttemptID = "unknown-submission"
		}},
		{"exec-command", func(v *loopruntime.CleanupSessionReadback) { v.Diagnostic.Observation.CommandSHA256 = "executed" }},
		{"progress-stop", func(v *loopruntime.CleanupSessionReadback) {
			v.Diagnostic.Observation.ProgressStopRequestID = "unknown-control"
		}},
		{"progress-stop-command", func(v *loopruntime.CleanupSessionReadback) {
			v.Diagnostic.Observation.ProgressStopCommandSHA256 = "executed-control"
		}},
		{"delivery-command", func(v *loopruntime.CleanupSessionReadback) { v.Execution.CommandSHA256 = "executed" }},
		{"candidate", func(v *loopruntime.CleanupSessionReadback) { v.Execution.CandidateHead = "published-candidate" }},
		{"uploaded-instructions", func(v *loopruntime.CleanupSessionReadback) {
			v.Execution.Instructions = &loopruntime.CompletedInstructionEvidence{}
		}},
		{"source-drift", func(v *loopruntime.CleanupSessionReadback) {
			v.Execution.Sources["native_custody"] = loopruntime.SessionReadbackFileRef{Path: request.Diagnostics.Path, SHA256: strings.Repeat("0", 64)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt.read = func(b loopruntime.CompletedSessionBinding, _ string, _ loopruntime.SessionStatus) (loopruntime.CleanupSessionReadback, error) {
				v := notStartedCleanupReadback(b, request.Diagnostics)
				tc.change(&v)
				return v, nil
			}
			if err := l.observeCleanupSessions(t.Context()); err == nil {
				t.Fatal("malformed diagnostic accepted")
			}
		})
	}
}
