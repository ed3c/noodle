package monitor

import (
	"context"
	"testing"
)

func TestMonitorTerminalEventDoesNotCertifyProcessExit(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "failed"}[failed], func(t *testing.T) {
			runtimeDir := t.TempDir()
			m := NewMonitor(runtimeDir)
			ensureFixtureSessionDirs(t, runtimeDir, monitorRunOnceFixtureInput{
				Observations: map[string]Observation{"worker": {Alive: true}},
			})
			m.observer = stubObserver{observations: map[string]Observation{"worker": {Alive: true}}}
			m.claims = stubClaimsReader{claims: map[string]SessionClaims{"worker": {
				Completed: !failed, Failed: failed, HasEvents: true,
			}}}
			m.tickets = &stubTicketMaterializer{}
			metas, err := m.RunOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(metas) != 1 || !metas[0].Alive || !metas[0].ExitPending || metas[0].Status != SessionStatusRunning {
				t.Fatalf("terminal event hid live process: %+v", metas)
			}
			m.observer = stubObserver{observations: map[string]Observation{"worker": {Alive: false}}}
			metas, err = m.RunOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := SessionStatusExited
			if failed {
				want = SessionStatusFailed
			}
			if len(metas) != 1 || metas[0].Alive || metas[0].ExitPending || metas[0].Status != want {
				t.Fatalf("actual exit not projected: %+v", metas)
			}
		})
	}
}
