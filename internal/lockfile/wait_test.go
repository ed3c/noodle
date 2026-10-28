package lockfile

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireWaitsForKnownContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	first, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		first.Close()
		close(released)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	second, err := Acquire(ctx, path)
	<-released
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
}
