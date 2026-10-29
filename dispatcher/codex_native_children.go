package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Child identities come from the original server's ancestor filter, not PIDs,
// names, or another session's process inventory.
type NativeCodexChild struct {
	UnsubscribeIntent bool            `json:"unsubscribe_intent"`
	UnsubscribeACK    json.RawMessage `json:"unsubscribe_ack,omitempty"`
	Source            json.RawMessage `json:"source"`
	TurnID            string          `json:"turn_id,omitempty"`
	Terminal          string          `json:"terminal,omitempty"`
	ArchivedBefore    bool            `json:"archived_before"`
	ArchiveIntent     bool            `json:"archive_intent"`
	ArchiveACK        json.RawMessage `json:"archive_ack,omitempty"`
	StopIntent        bool            `json:"stop_intent"`
	StopACK           json.RawMessage `json:"stop_ack,omitempty"`
	Released          bool            `json:"released"`
	Subscribed        bool            `json:"-"`
}

func (s *nativeCodexSession) drainNativeChildren() error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), codexRPCOperationTimeout)
		ready, err := s.readAndDrainNativeChildren(ctx)
		cancel()
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		select {
		case <-ticker.C:
		case <-s.client.done:
			return fmt.Errorf("native child drain readback lost")
		}
	}
}

func (s *nativeCodexSession) readAndDrainNativeChildren(ctx context.Context) (bool, error) {
	s.nativeMu.Lock()
	root, interrupt := s.record.ThreadID, s.record.StopRequested
	s.nativeMu.Unlock()
	for _, archived := range []bool{false, true} {
		children, err := s.descendants(ctx, root, archived)
		if err != nil {
			return false, err
		}
		s.nativeMu.Lock()
		if s.record.Children == nil {
			s.record.Children = make(map[string]*NativeCodexChild)
		}
		for id, raw := range children {
			if s.record.Children[id] == nil {
				s.record.Children[id] = &NativeCodexChild{Source: raw, ArchivedBefore: archived}
			}
		}
		err = s.persist()
		s.nativeMu.Unlock()
		if err != nil {
			return false, err
		}
	}
	s.nativeMu.Lock()
	ids := make([]string, 0, len(s.record.Children))
	for id := range s.record.Children {
		ids = append(ids, id)
	}
	s.nativeMu.Unlock()
	ready := true
	for _, id := range ids {
		var read struct {
			Thread struct {
				ID     string
				Cwd    string
				Source json.RawMessage
				Status struct{ Type string }
			}
		}
		if err := s.client.call(ctx, "thread/read", map[string]string{"threadId": id}, &read); err != nil {
			return false, err
		}
		var origin struct {
			SubAgent struct {
				Spawn struct {
					Parent string `json:"parent_thread_id"`
				} `json:"thread_spawn"`
			} `json:"subAgent"`
		}
		if err := json.Unmarshal(read.Thread.Source, &origin); err != nil {
			return false, fmt.Errorf("native child source unavailable: %w", err)
		}
		s.nativeMu.Lock()
		parentKnown := origin.SubAgent.Spawn.Parent == root || s.record.Children[origin.SubAgent.Spawn.Parent] != nil
		sameWorktree := read.Thread.Cwd == s.record.Worktree
		s.nativeMu.Unlock()
		if !parentKnown || !sameWorktree {
			return false, fmt.Errorf("native child source or worktree conflicts")
		}
		var turns struct{ Data []struct{ ID, Status string } }
		if err := s.client.call(ctx, "thread/turns/list", map[string]any{"threadId": id, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded"}, &turns); err != nil {
			return false, err
		}
		if read.Thread.ID == id && len(turns.Data) == 0 && read.Thread.Status.Type != "notLoaded" {
			ready = false
			continue
		}
		if read.Thread.ID != id || len(turns.Data) != 1 || turns.Data[0].ID == "" {
			return false, fmt.Errorf("native child identity or turn unavailable")
		}
		s.nativeMu.Lock()
		child := s.record.Children[id]
		if child.TurnID != "" && child.TurnID != turns.Data[0].ID {
			s.nativeMu.Unlock()
			return false, fmt.Errorf("native child advanced beyond recorded turn")
		}
		child.TurnID, child.Terminal = turns.Data[0].ID, turns.Data[0].Status
		snapshot := *child
		err := s.persist()
		s.nativeMu.Unlock()
		if err != nil {
			return false, err
		}
		if read.Thread.Status.Type == "notLoaded" {
			if !nativeTerminal(snapshot.Terminal) {
				return false, fmt.Errorf("unloaded child has no terminal readback")
			}
			s.nativeMu.Lock()
			child.Released = true
			err = s.persist()
			s.nativeMu.Unlock()
			if err != nil {
				return false, err
			}
			continue
		}
		ready = false
		if snapshot.ArchivedBefore {
			return false, fmt.Errorf("historically archived child has another loaded consumer")
		}
		if !nativeTerminal(snapshot.Terminal) {
			if interrupt && !snapshot.StopIntent {
				s.nativeMu.Lock()
				child.StopIntent = true
				err = s.persist()
				s.nativeMu.Unlock()
				if err != nil {
					return false, err
				}
				var ack json.RawMessage
				if err := s.client.call(ctx, "turn/interrupt", map[string]string{"threadId": id, "turnId": snapshot.TurnID}, &ack); err != nil {
					return false, err
				}
				s.nativeMu.Lock()
				child.StopACK = ack
				err = s.persist()
				s.nativeMu.Unlock()
				if err != nil {
					return false, err
				}
			}
			continue
		}
		if snapshot.UnsubscribeIntent {
			continue
		}
		var background struct {
			Data       []json.RawMessage
			NextCursor *string
		}
		if err := s.client.call(ctx, "thread/backgroundTerminals/list", map[string]string{"threadId": id}, &background); err != nil {
			return false, err
		}
		if background.Data == nil {
			return false, fmt.Errorf("native child background readback incomplete")
		}
		if len(background.Data) != 0 || background.NextCursor != nil {
			continue
		}
		leaves, err := s.descendants(ctx, id, false)
		if err != nil {
			return false, err
		}
		blocked := false
		s.nativeMu.Lock()
		for leaf := range leaves {
			if known := s.record.Children[leaf]; known == nil || !known.Released {
				blocked = true
			}
		}
		s.nativeMu.Unlock()
		if blocked {
			continue
		}
		if err := s.captureNativeProcesses(ctx); err != nil {
			return false, err
		}
		s.nativeMu.Lock()
		child.UnsubscribeIntent = true
		err = s.persist()
		s.nativeMu.Unlock()
		if err != nil {
			return false, err
		}
		var ack json.RawMessage
		if err := s.client.call(ctx, "thread/unsubscribe", map[string]string{"threadId": id}, &ack); err != nil {
			return false, err
		}
		s.nativeMu.Lock()
		child.UnsubscribeACK = ack
		err = s.persist()
		s.nativeMu.Unlock()
		if err != nil {
			return false, err
		}
		if err := validateNativeUnsubscribeACK(ack); err != nil {
			return false, err
		}
	}
	return ready, nil
}

func (s *nativeCodexSession) descendants(ctx context.Context, parent string, archived bool) (map[string]json.RawMessage, error) {
	result := map[string]json.RawMessage{}
	var cursor *string
	for page := 0; page < 16; page++ {
		params := map[string]any{"ancestorThreadId": parent, "archived": archived, "limit": 100,
			"sourceKinds": []string{"subAgent", "subAgentReview", "subAgentCompact", "subAgentThreadSpawn", "subAgentOther"}}
		if cursor != nil {
			params["cursor"] = *cursor
		}
		var rows struct {
			Data       []json.RawMessage
			NextCursor *string
		}
		if err := s.client.call(ctx, "thread/list", params, &rows); err != nil {
			return nil, err
		}
		if rows.Data == nil {
			return nil, fmt.Errorf("native descendant list did not return an array")
		}
		for _, raw := range rows.Data {
			var thread struct{ ID, Cwd string }
			if json.Unmarshal(raw, &thread) != nil || !codexThreadUUID.MatchString(thread.ID) || thread.Cwd != s.record.Worktree || thread.ID == parent {
				return nil, fmt.Errorf("native descendant source or worktree conflicts")
			}
			result[thread.ID] = raw
		}
		if rows.NextCursor == nil {
			return result, nil
		}
		if *rows.NextCursor == "" || (cursor != nil && *cursor == *rows.NextCursor) {
			return nil, fmt.Errorf("native descendant cursor did not advance")
		}
		cursor = rows.NextCursor
	}
	return nil, fmt.Errorf("original native descendant readback incomplete")
}
