package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

type openShellReaderFunc func(context.Context, OpenShellSandboxRef) (OpenShellSandboxObservation, error)

func (f openShellReaderFunc) ReadSandbox(ctx context.Context, ref OpenShellSandboxRef) (OpenShellSandboxObservation, error) {
	return f(ctx, ref)
}

type openShellStopperFunc func(context.Context, OpenShellSandboxRef) error

func (f openShellStopperFunc) StopSandbox(ctx context.Context, ref OpenShellSandboxRef) error {
	return f(ctx, ref)
}

func openShellTestIdentity() OpenShellIdentity {
	return OpenShellIdentity{
		OrderID: "order-1", AttemptID: "attempt-2", SessionID: "session-3",
		Sandbox:     OpenShellSandboxRef{Gateway: "https://original-gateway", Workspace: "original-workspace", SandboxID: "native-vm-4"},
		SandboxName: "reusable-name", ConfigSource: "/retained/config.toml#openshell",
	}
}

func newOpenShellTestWorker(t *testing.T) *OpenShellWorker {
	t.Helper()
	w, err := NewOpenShellWorker(openShellTestIdentity())
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func openShellPhase(phase string) openShellReaderFunc {
	return func(_ context.Context, ref OpenShellSandboxRef) (OpenShellSandboxObservation, error) {
		return OpenShellSandboxObservation{Sandbox: ref, Phase: phase}, nil
	}
}

func assertOpenShellBoundary(t *testing.T, w *OpenShellWorker, want OpenShellBoundaryState) {
	t.Helper()
	if got := w.Record().Boundary; got != want {
		t.Fatalf("boundary = %q, want %q", got, want)
	}
	select {
	case <-w.Done():
		if want != OpenShellBoundaryStopped {
			t.Fatal("unconfirmed boundary released admission")
		}
	default:
		if want == OpenShellBoundaryStopped {
			t.Fatal("confirmed stop did not release admission")
		}
	}
}

func TestOpenShellIdentityRequiresOriginalBinding(t *testing.T) {
	fields := map[string]func(*OpenShellIdentity){
		"order ID":             func(i *OpenShellIdentity) { i.OrderID = "" },
		"attempt ID":           func(i *OpenShellIdentity) { i.AttemptID = "" },
		"session ID":           func(i *OpenShellIdentity) { i.SessionID = "" },
		"gateway":              func(i *OpenShellIdentity) { i.Sandbox.Gateway = "" },
		"workspace":            func(i *OpenShellIdentity) { i.Sandbox.Workspace = " " },
		"native sandbox ID":    func(i *OpenShellIdentity) { i.Sandbox.SandboxID = "" },
		"configuration source": func(i *OpenShellIdentity) { i.ConfigSource = "" },
	}
	for field, remove := range fields {
		t.Run(field, func(t *testing.T) {
			identity := openShellTestIdentity()
			remove(&identity)
			w, err := NewOpenShellWorker(identity)
			if w != nil || err == nil || !strings.Contains(err.Error(), field+" missing") {
				t.Fatalf("missing binding accepted: worker=%v error=%v", w, err)
			}
		})
	}
	w := newOpenShellTestWorker(t)
	snapshot := w.Record()
	snapshot.Identity.Sandbox.SandboxID = "local-launch-ID"
	if w.Record().Identity != openShellTestIdentity() {
		t.Fatal("snapshot changed the original binding")
	}
	assertOpenShellBoundary(t, w, OpenShellBoundaryUnknown)
}

func TestOpenShellModelAndExecCompletionDoNotProveVMExit(t *testing.T) {
	for _, event := range []string{"CLI exit zero", "model completed", "native exec Exit", "exec response lost after model completed"} {
		t.Run(event, func(t *testing.T) {
			w := newOpenShellTestWorker(t)
			switch event {
			case "CLI exit zero":
				w.RecordExecResult(OpenShellExecResult{CLIExited: true, CLIExitCode: 0})
			case "model completed", "exec response lost after model completed":
				w.RecordModelOutcome(SessionOutcome{Status: StatusCompleted, HasDeliverable: true})
			case "native exec Exit":
				w.RecordExecResult(OpenShellExecResult{ExitObserved: true, ExitCode: 0})
			}
			if event == "exec response lost after model completed" {
				w.RecordExecResult(OpenShellExecResult{Reason: "exec relay disconnected"})
			}
			if err := w.Readback(t.Context(), openShellPhase("Ready")); err != nil {
				t.Fatal(err)
			}
			assertOpenShellBoundary(t, w, OpenShellBoundaryReady)
			if event == "CLI exit zero" && (!w.Record().ExecResult.CLIExited || w.Record().ExecResult.ExitObserved) {
				t.Fatal("CLI exit was lost or promoted to native exec Exit")
			}
			if event == "exec response lost after model completed" {
				record := w.Record()
				if record.ExecResult.ExitObserved || record.ExecResult.Reason != "exec relay disconnected" || record.ModelOutcome.Status != StatusCompleted {
					t.Fatalf("exec uncertainty was conflated with model result: %+v", record)
				}
			}
		})
	}
}

func TestOpenShellOnlyFreshOriginalStoppedReleasesBoundary(t *testing.T) {
	for _, phase := range []string{"", "Ready", "Unknown", "Provisioning", "Starting", "Stopping", "Error", "Deleting", "Completed", "stopped", "Stopped"} {
		t.Run(phase, func(t *testing.T) {
			w := newOpenShellTestWorker(t)
			if err := w.Readback(t.Context(), openShellPhase(phase)); err != nil {
				t.Fatal(err)
			}
			want := OpenShellBoundaryUnknown
			if phase == "Ready" {
				want = OpenShellBoundaryReady
			} else if phase == "Stopped" {
				want = OpenShellBoundaryStopped
			}
			assertOpenShellBoundary(t, w, want)
			if w.Record().ModelOutcome != (SessionOutcome{}) || w.Record().ExecResult.ExitObserved {
				t.Fatal("VM observation fabricated an execution or model result")
			}
		})
	}
	for _, mismatch := range []string{"same name different ID", "gateway", "workspace"} {
		t.Run(mismatch, func(t *testing.T) {
			w := newOpenShellTestWorker(t)
			reader := openShellReaderFunc(func(_ context.Context, ref OpenShellSandboxRef) (OpenShellSandboxObservation, error) {
				switch mismatch {
				case "same name different ID":
					ref.SandboxID = "replacement-native-vm"
				case "gateway":
					ref.Gateway = "https://replacement-gateway"
				case "workspace":
					ref.Workspace = "replacement-workspace"
				}
				return OpenShellSandboxObservation{Sandbox: ref, Phase: "Stopped"}, nil
			})
			if err := w.Readback(t.Context(), reader); err == nil || !strings.Contains(err.Error(), "identity conflicts") {
				t.Fatalf("foreign Stopped accepted: %v", err)
			}
			assertOpenShellBoundary(t, w, OpenShellBoundaryUnknown)
		})
	}
}

func TestOpenShellQueryFailureRetainsUnknown(t *testing.T) {
	for _, reason := range []string{"gateway disappeared", "resource not found", "query failed"} {
		t.Run(reason, func(t *testing.T) {
			w := newOpenShellTestWorker(t)
			if err := w.Readback(t.Context(), openShellPhase("Ready")); err != nil {
				t.Fatal(err)
			}
			cause := errors.New(reason)
			reader := openShellReaderFunc(func(_ context.Context, ref OpenShellSandboxRef) (OpenShellSandboxObservation, error) {
				// An error invalidates even an accompanying Stopped payload.
				return OpenShellSandboxObservation{Sandbox: ref, Phase: "Stopped"}, cause
			})
			if err := w.Readback(t.Context(), reader); !errors.Is(err, cause) {
				t.Fatalf("query diagnostic lost: %v", err)
			}
			assertOpenShellBoundary(t, w, OpenShellBoundaryUnknown)
		})
	}
}

func TestOpenShellCancelledQueryCannotCertifyStopped(t *testing.T) {
	w := newOpenShellTestWorker(t)
	ctx, cancel := context.WithCancel(t.Context())
	reader := openShellReaderFunc(func(_ context.Context, ref OpenShellSandboxRef) (OpenShellSandboxObservation, error) {
		cancel()
		return OpenShellSandboxObservation{Sandbox: ref, Phase: "Stopped"}, nil
	})
	if err := w.Readback(ctx, reader); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read acknowledged Stopped: %v", err)
	}
	assertOpenShellBoundary(t, w, OpenShellBoundaryUnknown)
}

func TestOpenShellStopRequiresIndependentOriginalReadback(t *testing.T) {
	for _, phase := range []string{"Ready", "Unknown", "Stopped", "query failed", "stop failed"} {
		t.Run(phase, func(t *testing.T) {
			w := newOpenShellTestWorker(t)
			reads, stops := 0, 0
			stopper := openShellStopperFunc(func(_ context.Context, ref OpenShellSandboxRef) error {
				stops++
				if ref != openShellTestIdentity().Sandbox {
					t.Fatalf("stop targeted a replacement: %+v", ref)
				}
				assertOpenShellBoundary(t, w, OpenShellBoundaryUnknown)
				if phase == "stop failed" {
					return errors.New("stop failed")
				}
				return nil
			})
			reader := openShellReaderFunc(func(ctx context.Context, ref OpenShellSandboxRef) (OpenShellSandboxObservation, error) {
				reads++
				if phase == "query failed" {
					return OpenShellSandboxObservation{}, errors.New("query failed")
				}
				return openShellPhase(phase)(ctx, ref)
			})
			err := w.Stop(t.Context(), stopper, reader)
			wantError := phase == "query failed" || phase == "stop failed"
			if (err != nil) != wantError || stops != 1 || (phase == "stop failed" && reads != 0) || (phase != "stop failed" && reads != 1) {
				t.Fatalf("stop/read: err=%v stops=%d reads=%d", err, stops, reads)
			}
			want := OpenShellBoundaryUnknown
			if phase == "Ready" {
				want = OpenShellBoundaryReady
			} else if phase == "Stopped" {
				want = OpenShellBoundaryStopped
			}
			assertOpenShellBoundary(t, w, want)
		})
	}
}

// The existing runtime capacity watcher only consumes Done. This adapter lets
// the evidence gate exercise that real watcher without adding a dispatch route.
type openShellGateHandle struct {
	SessionHandle
	worker *OpenShellWorker
}

func (s openShellGateHandle) Done() <-chan struct{} { return s.worker.Done() }

func TestOpenShellUnknownRetainsRuntimeCapacityUntilStopped(t *testing.T) {
	w := newOpenShellTestWorker(t)
	rt := NewDispatcherRuntime("test-evidence", nil, t.TempDir())
	rt.SetMaxConcurrent(1)
	rt.active = 1
	joined := make(chan struct{})
	go rt.watchDone(openShellGateHandle{worker: w}, joined)
	t.Cleanup(func() {
		if err := w.Readback(context.Background(), openShellPhase("Stopped")); err != nil {
			t.Error(err)
		}
		<-joined
	})
	w.RecordModelOutcome(SessionOutcome{Status: StatusCompleted})
	w.RecordExecResult(OpenShellExecResult{Reason: "lost exec response"})
	if err := w.Readback(t.Context(), openShellPhase("Ready")); err != nil {
		t.Fatal(err)
	}
	if rt.CapacityAvailable() {
		t.Fatal("CLI/model completion or unknown exec released quota")
	}
	if err := w.Readback(t.Context(), openShellPhase("Stopped")); err != nil {
		t.Fatal(err)
	}
	<-joined
	if !rt.CapacityAvailable() {
		t.Fatal("original Stopped did not release quota")
	}
	// Repeated and later observations cannot release a second permit.
	for _, phase := range []string{"Stopped", "Ready", "Unknown", "Stopped"} {
		if err := w.Readback(t.Context(), openShellPhase(phase)); err != nil {
			t.Fatal(err)
		}
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.active != 0 {
		t.Fatalf("duplicate observations changed active count: %d", rt.active)
	}
}

func TestOpenShellReadbackRejectsOutOfOrderEvidence(t *testing.T) {
	for _, newerPhase := range []string{"Ready", "Unknown", "Stopped", "query failed", "exec response lost", "stop failed"} {
		t.Run(newerPhase, func(t *testing.T) {
			w := newOpenShellTestWorker(t)
			entered, release := make(chan struct{}), make(chan struct{})
			oldResult := make(chan error, 1)
			oldPhase := "Stopped"
			if newerPhase == "Stopped" {
				oldPhase = "Ready"
			}
			go func() {
				oldResult <- w.Readback(t.Context(), openShellReaderFunc(func(_ context.Context, ref OpenShellSandboxRef) (OpenShellSandboxObservation, error) {
					close(entered)
					<-release
					return OpenShellSandboxObservation{Sandbox: ref, Phase: oldPhase}, nil
				}))
			}()
			<-entered
			switch newerPhase {
			case "exec response lost":
				w.RecordExecResult(OpenShellExecResult{Reason: "response lost"})
			case "stop failed":
				if err := w.Stop(t.Context(), openShellStopperFunc(func(context.Context, OpenShellSandboxRef) error {
					return errors.New("stop failed")
				}), openShellPhase("Stopped")); err == nil {
					t.Error("stop error lost")
				}
			case "query failed":
				if err := w.Readback(t.Context(), openShellReaderFunc(func(context.Context, OpenShellSandboxRef) (OpenShellSandboxObservation, error) {
					return OpenShellSandboxObservation{}, errors.New("read failed")
				})); err == nil {
					t.Error("query error lost")
				}
			default:
				if err := w.Readback(t.Context(), openShellPhase(newerPhase)); err != nil {
					t.Error(err)
				}
			}
			close(release)
			err := <-oldResult
			if newerPhase != "Stopped" && (err == nil || !strings.Contains(err.Error(), "superseded")) {
				t.Fatalf("old Stopped was accepted: %v", err)
			}
			want := OpenShellBoundaryUnknown
			if newerPhase == "Ready" {
				want = OpenShellBoundaryReady
			} else if newerPhase == "Stopped" {
				want = OpenShellBoundaryStopped
			}
			assertOpenShellBoundary(t, w, want)
		})
	}
}

func TestOpenShellConcurrentDuplicateStopped(t *testing.T) {
	w := newOpenShellTestWorker(t)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			w.RecordModelOutcome(SessionOutcome{Status: StatusCompleted})
			w.RecordExecResult(OpenShellExecResult{ExitObserved: true})
			if err := w.Readback(t.Context(), openShellPhase("Stopped")); err != nil && !strings.Contains(err.Error(), "superseded") {
				t.Error(err)
			}
			_ = w.Record()
		})
	}
	wg.Wait()
	if err := w.Readback(t.Context(), openShellPhase("Stopped")); err != nil {
		t.Fatal(err)
	}
	assertOpenShellBoundary(t, w, OpenShellBoundaryStopped)
}

func TestOpenShellRecoveryReadsOriginalWithoutTrustingSavedStop(t *testing.T) {
	original := openShellTestIdentity()
	retained := OpenShellWorkerRecord{
		Identity: original, Boundary: OpenShellBoundaryStopped,
		ModelOutcome: SessionOutcome{Status: StatusCompleted},
		ExecResult:   OpenShellExecResult{Reason: "original exec response lost"},
	}
	// Exercise the actual retained record format, including unknown execution.
	data, err := json.Marshal(retained)
	if err != nil {
		t.Fatal(err)
	}
	var saved OpenShellWorkerRecord
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"Ready", "Unknown", "Stopped", "not found"} {
		t.Run(phase, func(t *testing.T) {
			reads := 0
			reader := openShellReaderFunc(func(_ context.Context, ref OpenShellSandboxRef) (OpenShellSandboxObservation, error) {
				reads++
				if ref != original.Sandbox {
					t.Fatalf("recovery changed gateway/workspace/native ID: %+v", ref)
				}
				if phase == "not found" {
					return OpenShellSandboxObservation{}, errors.New("not found")
				}
				return OpenShellSandboxObservation{Sandbox: ref, Phase: phase}, nil
			})
			w, err := RecoverOpenShellWorker(t.Context(), original, saved, reader)
			if w == nil || (err != nil) != (phase == "not found") || reads != 1 {
				t.Fatalf("recovery lost unresolved owner or replayed effects: worker=%v err=%v reads=%d", w, err, reads)
			}
			want := OpenShellBoundaryUnknown
			if phase == "Ready" {
				want = OpenShellBoundaryReady
			} else if phase == "Stopped" {
				want = OpenShellBoundaryStopped
			}
			assertOpenShellBoundary(t, w, want)
			got := w.Record()
			if got.Identity != original || got.ModelOutcome != saved.ModelOutcome || got.ExecResult != saved.ExecResult || retained != saved {
				t.Fatalf("recovery altered original binding/results: %+v", got)
			}
		})
	}
}

func TestOpenShellRecoveryRejectsAnotherAttemptOrIdentity(t *testing.T) {
	for _, field := range []string{"order", "attempt", "session", "sandbox ID", "gateway", "workspace", "config source"} {
		t.Run(field, func(t *testing.T) {
			original := openShellTestIdentity()
			saved := OpenShellWorkerRecord{Identity: original, Boundary: OpenShellBoundaryStopped}
			switch field {
			case "order":
				saved.Identity.OrderID = "other-order"
			case "attempt":
				saved.Identity.AttemptID = "prior-attempt"
			case "session":
				saved.Identity.SessionID = "other-session"
			case "sandbox ID":
				saved.Identity.Sandbox.SandboxID = "same-name-different-ID"
			case "gateway":
				saved.Identity.Sandbox.Gateway = "https://new-gateway"
			case "workspace":
				saved.Identity.Sandbox.Workspace = "new-workspace"
			case "config source":
				saved.Identity.ConfigSource = "ambient-default"
			}
			reader := openShellReaderFunc(func(context.Context, OpenShellSandboxRef) (OpenShellSandboxObservation, error) {
				t.Fatal("recovery queried before checking original binding")
				return OpenShellSandboxObservation{}, nil
			})
			if w, err := RecoverOpenShellWorker(t.Context(), original, saved, reader); w != nil || err == nil {
				t.Fatalf("foreign attempt accepted: worker=%v err=%v", w, err)
			}
		})
	}
}

func TestOpenShellRecoveryNameIsNotAuthoritative(t *testing.T) {
	original := openShellTestIdentity()
	saved := OpenShellWorkerRecord{Identity: original}
	saved.Identity.SandboxName = "old-diagnostic-name"
	w, err := RecoverOpenShellWorker(t.Context(), original, saved, openShellPhase("Stopped"))
	if err != nil {
		t.Fatal(err)
	}
	assertOpenShellBoundary(t, w, OpenShellBoundaryStopped)
}
