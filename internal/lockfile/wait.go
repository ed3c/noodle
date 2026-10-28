package lockfile

import (
	"context"
	"errors"
	"time"
)

// Acquire waits only for known lock contention; other failures return unchanged.
func Acquire(ctx context.Context, path string) (*Lock, error) {
	for {
		lock, err := TryLock(path)
		if err == nil {
			return lock, nil
		}
		var busy *AlreadyLockedError
		if !errors.As(err, &busy) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}
