package dispatcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

type NativeCodexRecovered struct {
	Session Session
	OrderID string
	Reason  string
}

// Recover reads the original native attempt. It never dispatches a new turn,
// starts a daemon, or converts a missing process.json into an exit.
func (d *CodexNativeDispatcher) Recover(ctx context.Context) ([]NativeCodexRecovered, error) {
	root := filepath.Join(d.prepare.runtimeDir, "sessions")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recovered []NativeCodexRecovered
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		var record NativeCodexRecord
		if err := readResumeJSON(filepath.Join(dir, "native.json"), &record); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		if (record.Schema != 1 && record.Schema != 2) || record.SessionID != entry.Name() || record.OrderID == "" || !filepath.IsAbs(record.Worktree) || (record.Schema == 1 && !filepath.IsAbs(record.Socket)) || (record.Schema == 2 && !filepath.IsAbs(record.NativeHome)) {
			return nil, fmt.Errorf("native attempt custody conflicts: %s", entry.Name())
		}
		if record.State == "released" {
			continue
		}
		s := &nativeCodexSession{sessionBase: newSessionBase(sessionBaseConfig{id: record.SessionID,
			stampedPath: filepath.Join(dir, "raw.ndjson"), canonicalPath: filepath.Join(dir, "canonical.ndjson"), sink: d.prepare.sink}),
			dir: dir, record: record, terminal: make(chan struct{}, 1), release: make(chan struct{}, 1)}
		reason := "original native effect requires readback"
		if record.Schema == 2 {
			recovered = append(recovered, NativeCodexRecovered{Session: s, OrderID: record.OrderID, Reason: "foreground transport cannot be reattached; original exit and history require readback"})
			continue
		}

		if !codexThreadUUID.MatchString(record.ThreadID) || record.TurnID == "" {
			recovered = append(recovered, NativeCodexRecovered{Session: s, OrderID: record.OrderID, Reason: reason})
			continue
		}
		client, err := dialCodexRPC(ctx, record.Socket, s.observe)
		if err != nil {
			s.unknown(err)
			recovered = append(recovered, NativeCodexRecovered{Session: s, OrderID: record.OrderID, Reason: reason})
			continue
		}
		s.client = client
		if record.SocketID == "" || record.SocketID != client.socketID {
			client.close()
			s.client = nil
			s.unknown(fmt.Errorf("native backend endpoint changed; original owner readback required"))
			recovered = append(recovered, NativeCodexRecovered{Session: s, OrderID: record.OrderID, Reason: reason})
			continue
		}
		var read struct {
			Thread struct {
				ID     string
				Status struct{ Type string }
			}
		}
		err = client.call(ctx, "thread/read", map[string]string{"threadId": record.ThreadID}, &read)
		var turns struct{ Data []struct{ ID, Status string } }
		if err == nil {
			err = client.call(ctx, "thread/turns/list", map[string]any{"threadId": record.ThreadID,
				"limit": 1, "sortDirection": "desc", "itemsView": "notLoaded"}, &turns)
		}
		if err == nil && (read.Thread.ID != record.ThreadID || len(turns.Data) != 1 || turns.Data[0].ID != record.TurnID) {
			err = fmt.Errorf("native recovery cannot adopt a later or different turn")
		}
		if err == nil && nativeTerminal(turns.Data[0].Status) {
			s.nativeMu.Lock()
			s.record.Terminal = turns.Data[0].Status
			s.nativeMu.Unlock()
			if read.Thread.Status.Type == "notLoaded" {
				s.rootUnloaded = true
				s.terminal <- struct{}{}
				s.startCompletion()
				recovered = append(recovered, NativeCodexRecovered{Session: s, OrderID: record.OrderID, Reason: "original unloaded root; descendants still require readback"})
				continue
			} else if record.ArchiveIntent || record.State == "archive_intent" || len(record.ArchiveACK) != 0 {
				err = fmt.Errorf("original archive effect still unknown; recovery does not replay it")
			}
		}
		if err == nil && (read.Thread.Status.Type == "active" || read.Thread.Status.Type == "idle") {
			// Attach only to an already-loaded original turn, with no new input or config.
			var resumed struct{ Thread struct{ ID string } }
			err = client.call(ctx, "thread/resume", map[string]string{"threadId": record.ThreadID}, &resumed)
			if err == nil && resumed.Thread.ID != record.ThreadID {
				err = fmt.Errorf("native recovery thread changed")
			}
			if err == nil {
				s.nativeMu.Lock()
				terminal := nativeTerminal(s.record.Terminal)
				s.nativeMu.Unlock()
				if terminal {
					select {
					case s.terminal <- struct{}{}:
					default:
					}
				}
				s.startCompletion()
				reason = "original native turn attached without new dispatch"
			}
		} else if err == nil {
			err = fmt.Errorf("native recovery status is not an owned loaded turn")
		}
		if err != nil {
			client.close()
			s.client = nil
			s.unknown(err)
		}
		recovered = append(recovered, NativeCodexRecovered{Session: s, OrderID: record.OrderID, Reason: reason})
	}
	return recovered, nil
}

func (s *nativeCodexSession) finishReleased() error {
	s.nativeMu.Lock()
	previous := s.record.State
	s.record.State = "released"
	err := s.persist()
	if err != nil {
		s.record.State = previous
	}
	terminal := s.record.Terminal
	s.nativeMu.Unlock()
	if err != nil {
		return err
	}
	if s.client != nil {
		s.client.close()
	}
	status := StatusCompleted
	if terminal == "interrupted" {
		status = StatusCancelled
	}
	if terminal == "failed" {
		status = StatusFailed
	}
	s.doneOnce.Do(func() {
		s.mu.Lock()
		s.status = string(status)
		s.outcome = SessionOutcome{Status: status, HasDeliverable: terminal == "completed"}
		s.mu.Unlock()
		close(s.done)
	})
	return nil
}
