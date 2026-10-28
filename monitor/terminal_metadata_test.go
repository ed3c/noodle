package monitor

import (
	"bytes"
	"os"
	"testing"
	"time"
)

func TestTerminalMetadataRemainsStableUntilEvidenceChanges(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "failed"}[failed], func(t *testing.T) {
			root := t.TempDir()
			ensureFixtureSessionDirs(t, root, monitorRunOnceFixtureInput{
				Observations: map[string]Observation{"worker": {}},
			})
			now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
			observations := map[string]Observation{"worker": {}}
			claims := map[string]SessionClaims{"worker": {
				Completed: !failed, Failed: failed, HasEvents: true,
				FirstEventAt: now.Add(-time.Minute), LastEventAt: now,
			}}
			m := NewMonitor(root)
			m.now = func() time.Time { return now }
			m.observer = stubObserver{observations: observations}
			m.claims = stubClaimsReader{claims: claims}
			m.tickets = &stubTicketMaterializer{}
			run := func() SessionMeta {
				t.Helper()
				rows, err := m.RunOnce(t.Context())
				if err != nil || len(rows) != 1 {
					t.Fatalf("monitor result: %+v, %v", rows, err)
				}
				return rows[0]
			}
			first := run()
			path := sessionMetaPath(root, "worker")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Hour)
			if got := run(); got != first {
				t.Fatalf("clock alone changed terminal evidence: %+v -> %+v", first, got)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("terminal source bytes changed: %v", err)
			}
			current, err := os.Stat(path)
			if err != nil || !os.SameFile(original, current) {
				t.Fatalf("terminal source was rewritten: %v", err)
			}
			late := claims["worker"]
			late.TotalCostUSD = 0.2
			claims["worker"] = late
			if got := run(); got.TotalCostUSD != 0.2 || !got.UpdatedAt.Equal(now) {
				t.Fatalf("late evidence hidden: %+v", got)
			}
			observations["worker"] = Observation{Alive: true}
			if got := run(); !got.Alive || !got.ExitPending || got.Status != SessionStatusRunning {
				t.Fatalf("new liveness hidden by terminal projection: %+v", got)
			}
		})
	}
}
