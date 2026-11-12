package runtime

import (
	"context"
	"errors"
	"time"
)

// Waiting stays in the original pre-Create session. Each observation is bounded;
// an uncertain effect still uses its persisted source readback/native request ID.
func (s *openShellSession) waitCredentials(ctx context.Context, providers []string) (func(), error) {
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.stopRequested:
			cancel()
		case <-waitCtx.Done():
		}
	}()
	backoff := 500 * time.Millisecond // same initial observation interval as provider readiness
	for {
		if err := waitCtx.Err(); err != nil {
			return nil, err
		}
		op, stop := context.WithTimeout(waitCtx, 10*time.Second)
		release, err := s.owner.config.Gateway.EnsureCredentials(op, providers)
		stop()
		if err == nil {
			if waitCtx.Err() != nil {
				release()
				return nil, waitCtx.Err()
			}
			s.mu.Lock()
			if s.receipt.Phase == "credential_waiting" {
				s.receipt.Error = ""
			}
			persistErr := s.persistLocked()
			s.mu.Unlock()
			if persistErr != nil {
				release()
				return nil, persistErr
			}
			return release, nil
		}
		if waitCtx.Err() != nil {
			return nil, waitCtx.Err()
		}
		var waiting *openShellCredentialWaiting
		if !errors.As(err, &waiting) {
			return nil, err
		}
		s.mu.Lock()
		s.receipt.Phase, s.receipt.Error, s.receipt.CredentialWaitError = "credential_waiting", err.Error(), err.Error()
		persistErr := s.persistLocked()
		s.mu.Unlock()
		if persistErr != nil {
			return nil, persistErr
		}
		timer := time.NewTimer(backoff)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return nil, waitCtx.Err()
		case <-timer.C:
		}
		if backoff < 5*time.Second {
			backoff = min(backoff*2, 5*time.Second)
		}
	}
}
