package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func (s *nativeCodexSession) releaseForeground() error {
	if s.process == nil {
		return fmt.Errorf("native release has no original foreground process")
	}
	for {
		err := s.client.beginDrain()
		if err == nil {
			break
		}
		if !errors.Is(err, errNativeRequestPending) {
			return err
		}
		select {
		case <-time.After(250 * time.Millisecond):
		case <-s.client.done:
			return fmt.Errorf("native request resolution readback lost")
		}
	}
	s.nativeMu.Lock()
	id, turn, terminal := s.record.ThreadID, s.record.TurnID, s.record.Terminal
	s.nativeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), codexRPCOperationTimeout)
	defer cancel()
	for {
		err := s.sameTerminal(ctx, id, turn, terminal)
		if err == nil {
			break
		}
		if !errors.Is(err, errNativeTerminalPending) {
			return err
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return err
		}
	}
	cancel()
	if err := s.drainNativeChildren(); err != nil {
		return err
	}
	if err := s.waitNativeBackground(id); err != nil {
		return err
	}
	ctx, cancel = context.WithTimeout(context.Background(), codexRPCOperationTimeout)
	defer cancel()
	var before struct {
		Data       []string
		NextCursor *string
	}
	if err := s.client.call(ctx, "thread/loaded/list", map[string]any{}, &before); err != nil {
		return err
	}
	if before.Data == nil || before.NextCursor != nil || len(before.Data) != 1 || before.Data[0] != id {
		return fmt.Errorf("native root release has unresolved loaded threads")
	}
	if err := s.captureNativeProcesses(ctx); err != nil {
		return err
	}
	s.nativeMu.Lock()
	children := append([]nativeProcessIdentity(nil), s.record.ObservedChildren...)
	s.record.State = "unsubscribe_intent"
	s.record.UnsubscribeIntent = true
	err := s.persist()
	s.nativeMu.Unlock()
	if err != nil {
		return err
	}
	var ack json.RawMessage
	if err := s.client.call(ctx, "thread/unsubscribe", map[string]string{"threadId": id}, &ack); err != nil {
		return err
	}
	s.nativeMu.Lock()
	s.record.UnsubscribeACK = ack
	s.record.State = "unsubscribe_ack_received"
	err = s.persist()
	s.nativeMu.Unlock()
	if err != nil {
		return err
	}
	if err := validateNativeUnsubscribeACK(ack); err != nil {
		return err
	}
	s.nativeMu.Lock()
	s.record.State = "waiting_native_unload"
	err = s.persist()
	s.nativeMu.Unlock()
	if err != nil {
		return err
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		readCtx, readCancel := context.WithTimeout(context.Background(), codexRPCOperationTimeout)
		var loaded struct {
			Data       []string
			NextCursor *string
		}
		var raw json.RawMessage
		err = s.client.call(readCtx, "thread/loaded/list", map[string]any{}, &raw)
		readCancel()
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &loaded); err != nil {
			return fmt.Errorf("native loaded thread readback invalid: %w", err)
		}
		if loaded.Data == nil || loaded.NextCursor != nil {
			return fmt.Errorf("native loaded thread readback incomplete")
		}
		if len(loaded.Data) == 0 {
			s.nativeMu.Lock()
			s.record.UnloadReadback = append(json.RawMessage(nil), raw...)
			err = s.persist()
			s.nativeMu.Unlock()
			if err != nil {
				return err
			}
			break
		}
		if len(loaded.Data) != 1 || loaded.Data[0] != id {
			return fmt.Errorf("foreground server contains another loaded thread")
		}
		select {
		case <-ticker.C:
		case <-s.client.done:
			return fmt.Errorf("native unload readback lost")
		}
	}
	readCtx, readCancel := context.WithTimeout(context.Background(), codexRPCOperationTimeout)
	err = s.originalTerminal(readCtx)
	readCancel()
	if err != nil {
		return err
	}
	if err := s.client.conn.Close(); err != nil {
		return err
	}
	exitCtx, exitCancel := context.WithTimeout(context.Background(), codexRPCOperationTimeout)
	defer exitCancel()
	select {
	case <-s.process.Done():
	case <-exitCtx.Done():
		return fmt.Errorf("foreground exit remains unverified")
	}
	select {
	case <-s.client.done:
	case <-exitCtx.Done():
		return fmt.Errorf("foreground output remains undrained")
	}
	select {
	case err := <-s.stderrDone:
		if err != nil {
			return err
		}
	case <-exitCtx.Done():
		return fmt.Errorf("foreground stderr remains undrained")
	}
	code, exited := s.process.ExitCode()
	if !exited || code != 0 {
		return fmt.Errorf("foreground exit unsuccessful: code=%d observed=%v", code, exited)
	}
	for {
		rows, err := nativeProcessSnapshot(exitCtx)
		if err != nil {
			return err
		}
		if err = nativeObservedProcessesExited(rows, children); err == nil {
			break
		}
		select {
		case <-ticker.C:
		case <-exitCtx.Done():
			return err
		}
	}
	s.nativeMu.Lock()
	s.record.ServerExit = &code
	err = s.persist()
	s.nativeMu.Unlock()
	if err != nil {
		return err
	}
	return s.finishReleased()
}

func (s *nativeCodexSession) waitNativeBackground(id string) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), codexRPCOperationTimeout)
		var background struct {
			Data       []json.RawMessage
			NextCursor *string
		}
		err := s.client.call(ctx, "thread/backgroundTerminals/list", map[string]string{"threadId": id}, &background)
		cancel()
		if err != nil {
			return err
		}
		if background.Data == nil {
			return fmt.Errorf("native background work readback incomplete")
		}
		if len(background.Data) == 0 && background.NextCursor == nil {
			return nil
		}
		s.nativeMu.Lock()
		s.record.State = "draining_background"
		err = s.persist()
		s.nativeMu.Unlock()
		if err != nil {
			return err
		}
		select {
		case <-ticker.C:
		case <-s.client.done:
			return fmt.Errorf("native background drain readback lost")
		}
	}
}

func (s *nativeCodexSession) captureNativeProcesses(ctx context.Context) error {
	rows, err := nativeProcessSnapshot(ctx)
	if err != nil {
		return err
	}
	children, err := nativeProcessDescendants(rows, s.record.Foreground)
	if err != nil {
		return err
	}
	s.nativeMu.Lock()
	defer s.nativeMu.Unlock()
	for _, child := range children {
		found := false
		for _, old := range s.record.ObservedChildren {
			if old.PID == child.PID && old.Started == child.Started {
				found = true
				break
			}
		}
		if !found {
			s.record.ObservedChildren = append(s.record.ObservedChildren, child)
		}
	}
	return s.persist()
}

func validateNativeUnsubscribeACK(raw json.RawMessage) error {
	var ack struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &ack); err != nil {
		return fmt.Errorf("native unsubscribe response invalid: %w", err)
	}
	switch ack.Status {
	case "unsubscribed", "notSubscribed", "notLoaded":
		return nil
	default:
		return fmt.Errorf("native unsubscribe status unknown: %q", ack.Status)
	}
}
