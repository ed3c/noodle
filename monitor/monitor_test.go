package monitor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMonitorTerminalClockUpdatesPreserveReadbackBytes(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "exited"
		if failed {
			name = "failed"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "sessions", "original"), 0700); err != nil {
				t.Fatal(err)
			}
			activity := time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)
			claims := map[string]SessionClaims{"original": {SessionID: "original", HasEvents: true, Completed: !failed, Failed: failed, Runtime: "openshell", Provider: "codex", Model: "selected", LastEventAt: activity}}
			m := NewMonitor(root)
			m.observer = stubObserver{observations: map[string]Observation{"original": {SessionID: "original"}}}
			m.claims = stubClaimsReader{claims: claims}
			tickets := &stubTicketMaterializer{}
			m.tickets = tickets
			now := activity.Add(time.Minute)
			m.now = func() time.Time { return now }
			if _, err := m.RunOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			path := sessionMetaPath(root, "original")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Minute)
			tickets.calledWith = nil
			metas, err := m.RunOnce(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) || len(metas) != 1 || !metas[0].UpdatedAt.Equal(activity.Add(time.Minute)) {
				t.Fatalf("clock-only terminal projection changed readback bytes: %s %v", after, err)
			}
			if len(tickets.calledWith) != 1 || tickets.calledWith[0] != "original" {
				t.Fatal("terminal NOOP skipped ticket materialization")
			}
			updated := claims["original"]
			updated.TotalCostUSD = 0.25
			claims["original"] = updated
			metas, err = m.RunOnce(t.Context())
			if err != nil || metas[0].TotalCostUSD != 0.25 || !metas[0].UpdatedAt.Equal(now) {
				t.Fatalf("new terminal facts were frozen: %+v %v", metas, err)
			}
		})
	}
}

func TestMonitorUnsettledSessionsContinueUpdating(t *testing.T) {
	for _, kind := range []string{"running", "unknown", "exit-pending"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "sessions", "original"), 0700); err != nil {
				t.Fatal(err)
			}
			activity := time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)
			observation := Observation{SessionID: "original", Alive: true}
			claims := SessionClaims{SessionID: "original", HasEvents: true, LastEventAt: activity}
			if kind == "unknown" {
				observation.Alive = false
				observation.NativeState = "needs_owner_readback"
			}
			claims.Completed = kind == "exit-pending"
			m := NewMonitor(root)
			m.observer = stubObserver{observations: map[string]Observation{"original": observation}}
			m.claims = stubClaimsReader{claims: map[string]SessionClaims{"original": claims}}
			m.tickets = &stubTicketMaterializer{}
			now := activity.Add(time.Minute)
			m.now = func() time.Time { return now }
			if _, err := m.RunOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Minute)
			metas, err := m.RunOnce(t.Context())
			if err != nil || len(metas) != 1 || !metas[0].UpdatedAt.Equal(now) {
				t.Fatalf("unsettled session stopped updating: %+v %v", metas, err)
			}
		})
	}
}

type stubObserver struct {
	observations map[string]Observation
}

func (s stubObserver) Observe(sessionID string) (Observation, error) {
	obs, ok := s.observations[sessionID]
	if !ok {
		return Observation{SessionID: sessionID}, nil
	}
	return obs, nil
}

type stubClaimsReader struct {
	claims map[string]SessionClaims
}

func (s stubClaimsReader) ReadSession(sessionID string) (SessionClaims, error) {
	claim, ok := s.claims[sessionID]
	if !ok {
		return SessionClaims{SessionID: sessionID}, nil
	}
	return claim, nil
}

type stubTicketMaterializer struct {
	calledWith []string
}

func (s *stubTicketMaterializer) Materialize(_ context.Context, sessionIDs []string) error {
	s.calledWith = append([]string(nil), sessionIDs...)
	return nil
}

func TestMonitorRunOncePassesSessionIDsToTickets(t *testing.T) {
	runtimeDir := t.TempDir()
	for _, sessionID := range []string{"cook-a", "cook-b"} {
		if err := os.MkdirAll(filepath.Join(runtimeDir, "sessions", sessionID), 0o755); err != nil {
			t.Fatalf("mkdir session directory: %v", err)
		}
	}

	monitor := NewMonitor(runtimeDir)
	monitor.observer = stubObserver{}
	monitor.claims = stubClaimsReader{}
	stubTickets := &stubTicketMaterializer{}
	monitor.tickets = stubTickets

	if _, err := monitor.RunOnce(context.Background()); err != nil {
		t.Fatalf("run once: %v", err)
	}
	if len(stubTickets.calledWith) != 2 {
		t.Fatalf("ticket materializer called with %d sessions", len(stubTickets.calledWith))
	}
}
