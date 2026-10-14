package admission

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

func TestUnsupportedSampler(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("Darwin has a native sampler")
	}
	sample, err := NewSampler().Read(context.Background())
	if err != nil || !sample.Unsupported || sample.Pressure != Unknown || sample.CPUs < 1 || sample.At.IsZero() {
		t.Fatalf("unsupported platform reported a healthy or invalid sample: %+v, %v", sample, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewSampler().Read(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled unsupported read: %v", err)
	}
}
