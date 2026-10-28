package loop

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestHydratedHoldUsesFirstACKWithoutReplayingEffects(t *testing.T) {
	for _, tc := range []struct {
		name string
		acks []ControlAck
		want State
	}{
		{"pause-then-duplicate", []ControlAck{{ID: "pause", Action: "pause", Status: "ok", DispatchState: StatePaused}, {ID: "pause", Action: "pause", Status: "duplicate", DispatchState: StateRunning}}, StatePaused},
		{"legacy-pause", []ControlAck{{ID: "pause", Action: "pause", Status: "ok"}}, StatePaused},
		{"failed-pause", []ControlAck{{ID: "pause", Action: "pause", Status: "error"}}, StateRunning},
		{"stop-all-is-not-pause", []ControlAck{{ID: "stop", Action: "stop-all", Status: "ok"}}, StateRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			var raw bytes.Buffer
			for _, ack := range tc.acks {
				if err := json.NewEncoder(&raw).Encode(ack); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(root, "control-ack.ndjson")
			if err := os.WriteFile(path, raw.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			l := &Loop{runtimeDir: root, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			l.state = StateRunning
			l.cmds.processedIDs = map[string]struct{}{}
			if err := l.hydrateProcessedCommands(); err != nil {
				t.Fatal(err)
			}
			if l.state != tc.want {
				t.Fatalf("state=%s want %s", l.state, tc.want)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(after, raw.Bytes()) {
				t.Fatal("ACK history rewritten")
			}
			if _, err := os.Stat(filepath.Join(root, "control.ndjson")); !os.IsNotExist(err) {
				t.Fatal("readback created a control", err)
			}
		})
	}
}
