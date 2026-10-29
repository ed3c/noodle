package monitor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeOwnerReleaseOverridesFreshHeartbeatAndLegacyClaims(t *testing.T) {
	runtime := t.TempDir()
	session := "native-fixture"
	dir := filepath.Join(runtime, "sessions", session)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]any{
		"spawn.json":     map[string]any{"runtime": "codex-app-server"},
		"meta.json":      map[string]any{"session_id": session, "status": "running", "runtime": "codex-app-server", "alive": true},
		"heartbeat.json": map[string]any{"timestamp": time.Now(), "ttl_seconds": 100000},
		"native.json":    map[string]any{"schema": 2, "session_id": session, "native_home": t.TempDir(), "worktree": t.TempDir(), "foreground": map[string]any{"pid": 99999999, "started": "Mon Oct 26 00:00:00 2026"}, "state": "released", "terminal": "failed", "thread_id": "11111111-1111-4111-8111-111111111111", "server_exit": 0, "unsubscribe_ack": map[string]any{"status": "unsubscribed"}, "unload_readback": map[string]any{"data": []string{}, "nextCursor": nil}},
	} {
		raw, _ := json.Marshal(data)
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	m := NewMonitor(runtime)
	m.claims = stubClaimsReader{claims: map[string]SessionClaims{session: {HasEvents: true, Runtime: "codex-app-server"}}}
	m.tickets = &stubTicketMaterializer{}
	for range 2 {
		rows, err := m.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].Status != SessionStatusFailed || rows[0].Alive || rows[0].NativeState != "released" {
			t.Fatalf("legacy projection replaced original native exit: %+v", rows)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "native.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	json.Unmarshal(raw, &record)
	record["state"] = "needs_owner_readback"
	record["problem"] = "original transport lost"
	raw, _ = json.Marshal(record)
	os.WriteFile(filepath.Join(dir, "native.json"), raw, 0600)
	rows, err := m.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Status != SessionStatusUnknown || !rows[0].ExitPending {
		t.Fatalf("unknown native effect became exit: %+v", rows[0])
	}
}
