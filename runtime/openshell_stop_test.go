package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func startBlockedOpenShellStop(t *testing.T, w *OpenShellWorker, stopErr error, reader OpenShellSandboxReader) (func(), <-chan error) {
	t.Helper()
	entered, released, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	result := make(chan error, 1)
	go func() {
		defer close(returned)
		result <- w.Stop(t.Context(), openShellStopperFunc(func(_ context.Context, ref OpenShellSandboxRef) error {
			if ref != openShellTestIdentity().Sandbox {
				t.Errorf("stop changed original native identity: %+v", ref)
			}
			close(entered)
			<-released
			return stopErr
		}), reader)
	}()
	t.Cleanup(func() { release(); <-returned })
	<-entered
	return release, result
}

func TestOpenShellPendingStopRetainsCustodyAndInvalidatesConcurrentReads(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"acknowledged", nil},
		{"failed", errors.New("native stop failed")},
		{"response lost", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newOpenShellTestWorker(t)
			var postStopReads atomic.Int32
			releaseStop, stopResult := startBlockedOpenShellStop(t, w, tc.err, openShellReaderFunc(func(ctx context.Context, ref OpenShellSandboxRef) (OpenShellSandboxObservation, error) {
				postStopReads.Add(1)
				return openShellPhase("Ready")(ctx, ref)
			}))

			if err := w.Readback(t.Context(), openShellPhase("Stopped")); err != nil {
				t.Fatal(err)
			}
			assertOpenShellBoundary(t, w, OpenShellBoundaryUnknown)
			if !w.Record().StopPending {
				t.Fatal("a Stopped read transferred custody while native stop was outstanding")
			}
			// A duplicate submitted during the unresolved request cannot replay it.
			duplicateStopper := openShellStopperFunc(func(context.Context, OpenShellSandboxRef) error {
				t.Error("duplicate Stop issued another native control effect")
				return nil
			})
			if err := w.Stop(t.Context(), duplicateStopper, openShellPhase("Stopped")); err == nil || !strings.Contains(err.Error(), "unresolved") {
				t.Fatalf("duplicate stop lost pending custody: %v", err)
			}

			// Also hold a response sampled while stop is pending until after the
			// control response returns. It must not become fresh retroactively.
			readEntered, releaseRead := make(chan struct{}), make(chan struct{})
			staleResult := make(chan error, 1)
			go func() {
				staleResult <- w.Readback(t.Context(), openShellReaderFunc(func(_ context.Context, ref OpenShellSandboxRef) (OpenShellSandboxObservation, error) {
					close(readEntered)
					<-releaseRead
					return OpenShellSandboxObservation{Sandbox: ref, Phase: "Stopped"}, nil
				}))
			}()
			<-readEntered
			releaseStop()
			err := <-stopResult
			close(releaseRead)
			staleErr := <-staleResult
			if !errors.Is(err, tc.err) {
				t.Fatalf("stop result = %v, want %v", err, tc.err)
			}
			if staleErr == nil || !strings.Contains(staleErr.Error(), "superseded") {
				t.Fatalf("read sampled before the control response was accepted: %v", staleErr)
			}
			want, wantReads := OpenShellBoundaryReady, int32(1)
			if tc.err != nil {
				want, wantReads = OpenShellBoundaryUnknown, 0
			}
			assertOpenShellBoundary(t, w, want)
			if w.Record().StopPending != (tc.err != nil) || postStopReads.Load() != wantReads {
				t.Fatal("stop response lost unknown state or reused evidence from an outstanding call")
			}
			if err := w.Readback(t.Context(), openShellPhase("Stopped")); err != nil {
				t.Fatal(err)
			}
			if tc.err == nil {
				assertOpenShellBoundary(t, w, OpenShellBoundaryStopped)
			} else {
				assertOpenShellBoundary(t, w, OpenShellBoundaryUnknown)
				if w.Record().StopError != tc.err.Error() || !w.Record().StopPending {
					t.Fatal("a VM read discarded an unacknowledged stop and its original diagnostic")
				}
			}
		})
	}
}

func TestOpenShellUnknownStopSurvivesReadbackAndRetainedRecovery(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled, errors.New("gateway unavailable"), errors.New("stop response lost")} {
		t.Run(cause.Error(), func(t *testing.T) {
			w := newOpenShellTestWorker(t)
			w.RecordModelOutcome(SessionOutcome{Status: StatusCompleted})
			stops := 0
			stopper := openShellStopperFunc(func(context.Context, OpenShellSandboxRef) error {
				stops++
				return cause
			})
			if err := w.Stop(t.Context(), stopper, openShellPhase("Stopped")); !errors.Is(err, cause) {
				t.Fatalf("original stop error lost: %v", err)
			}
			for _, phase := range []string{"Stopped", "Ready", "Stopped"} {
				if err := w.Readback(t.Context(), openShellPhase(phase)); err != nil {
					t.Fatal(err)
				}
				assertOpenShellBoundary(t, w, OpenShellBoundaryUnknown)
			}
			data, err := json.Marshal(w.Record())
			if err != nil {
				t.Fatal(err)
			}
			var retained OpenShellWorkerRecord
			if err := json.Unmarshal(data, &retained); err != nil {
				t.Fatal(err)
			}
			recovered, err := RecoverOpenShellWorker(t.Context(), openShellTestIdentity(), retained, openShellPhase("Stopped"))
			if err != nil {
				t.Fatal(err)
			}
			for _, owner := range []*OpenShellWorker{w, recovered} {
				assertOpenShellBoundary(t, owner, OpenShellBoundaryUnknown)
				got := owner.Record()
				if !got.StopPending || got.StopError != cause.Error() || got.Identity != openShellTestIdentity() || got.ModelOutcome.Status != StatusCompleted {
					t.Fatalf("original stop custody/diagnostic lost: %+v", got)
				}
				if err := owner.Stop(t.Context(), stopper, openShellPhase("Stopped")); err == nil || !strings.Contains(err.Error(), "unresolved") {
					t.Fatalf("unacknowledged control effect was replayed: %v", err)
				}
			}
			if stops != 1 {
				t.Fatalf("unknown native stop was replayed %d times", stops-1)
			}
		})
	}
}

func TestOpenShellStopRequiresStoppedReadAfterAcknowledgement(t *testing.T) {
	w := newOpenShellTestWorker(t)
	var reads atomic.Int32
	release, result := startBlockedOpenShellStop(t, w, nil, openShellReaderFunc(func(ctx context.Context, ref OpenShellSandboxRef) (OpenShellSandboxObservation, error) {
		if w.Record().StopPending {
			t.Error("post-stop read started before the native stop response")
		}
		reads.Add(1)
		return openShellPhase("Stopped")(ctx, ref)
	}))
	if err := w.Readback(t.Context(), openShellPhase("Stopped")); err != nil {
		t.Fatal(err)
	}
	assertOpenShellBoundary(t, w, OpenShellBoundaryUnknown)
	release()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	assertOpenShellBoundary(t, w, OpenShellBoundaryStopped)
	if reads.Load() != 1 {
		t.Fatal("native stop acknowledgement reused an observation from the outstanding call")
	}
}

func TestOpenShellAcknowledgedStopSurvivesCancelledVMRead(t *testing.T) {
	w := newOpenShellTestWorker(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stopper := openShellStopperFunc(func(context.Context, OpenShellSandboxRef) error {
		cancel()
		return nil // native acknowledgement arrived before the VM read was cancelled
	})
	if err := w.Stop(ctx, stopper, openShellPhase("Stopped")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled VM read diagnostic lost: %v", err)
	}
	if w.Record().StopPending {
		t.Fatal("VM read cancellation discarded the conclusive native stop acknowledgement")
	}
	assertOpenShellBoundary(t, w, OpenShellBoundaryUnknown)
	if err := w.Readback(t.Context(), openShellPhase("Stopped")); err != nil {
		t.Fatal(err)
	}
	assertOpenShellBoundary(t, w, OpenShellBoundaryStopped)
}

func TestOpenShellTerminalOwnerCannotStopSuccessorOnSameVM(t *testing.T) {
	old := newOpenShellTestWorker(t)
	if err := old.Readback(t.Context(), openShellPhase("Stopped")); err != nil {
		t.Fatal(err)
	}
	identity := openShellTestIdentity()
	identity.AttemptID, identity.SessionID = "manual-next-attempt", "next-session"
	next, err := NewOpenShellWorker(identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Readback(t.Context(), openShellPhase("Ready")); err != nil {
		t.Fatal(err)
	}
	var stops, reads atomic.Int32
	var nativeReady atomic.Bool
	nativeReady.Store(true)
	stopper := openShellStopperFunc(func(context.Context, OpenShellSandboxRef) error {
		stops.Add(1)
		nativeReady.Store(false)
		return nil
	})
	reader := openShellReaderFunc(func(ctx context.Context, ref OpenShellSandboxRef) (OpenShellSandboxObservation, error) {
		reads.Add(1)
		return openShellPhase("Ready")(ctx, ref)
	})
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if err := old.Stop(t.Context(), stopper, reader); err != nil {
				t.Error(err)
			}
			if err := old.Readback(t.Context(), reader); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if stops.Load() != 0 || reads.Load() != 0 || !nativeReady.Load() {
		t.Fatalf("completed owner affected successor: native stops=%d reads=%d ready=%v", stops.Load(), reads.Load(), nativeReady.Load())
	}
	assertOpenShellBoundary(t, old, OpenShellBoundaryStopped)
	assertOpenShellBoundary(t, next, OpenShellBoundaryReady)
}

func TestOpenShellRecoveryRetainsOutstandingStopCustody(t *testing.T) {
	w := newOpenShellTestWorker(t)
	release, result := startBlockedOpenShellStop(t, w, nil, openShellPhase("Ready"))
	recovered, err := RecoverOpenShellWorker(t.Context(), openShellTestIdentity(), w.Record(), openShellPhase("Stopped"))
	if err != nil {
		t.Fatal(err)
	}
	assertOpenShellBoundary(t, recovered, OpenShellBoundaryUnknown)
	if !recovered.Record().StopPending {
		t.Fatal("recovery discarded original control custody")
	}
	stopper := openShellStopperFunc(func(context.Context, OpenShellSandboxRef) error {
		t.Error("recovery replayed an unresolved native stop")
		return nil
	})
	if err := recovered.Stop(t.Context(), stopper, openShellPhase("Stopped")); err == nil {
		t.Fatal("recovery adopted original control authority")
	}
	release()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	// Only a new record from the original owner can establish that its call
	// returned; the recovered placeholder cannot clear that pending state.
	settled, err := RecoverOpenShellWorker(t.Context(), openShellTestIdentity(), w.Record(), openShellPhase("Stopped"))
	if err != nil {
		t.Fatal(err)
	}
	assertOpenShellBoundary(t, settled, OpenShellBoundaryStopped)
	assertOpenShellBoundary(t, recovered, OpenShellBoundaryUnknown)
}
