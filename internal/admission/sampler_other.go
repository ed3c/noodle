//go:build !darwin

package admission

import (
	"context"
	"runtime"
	"time"
)

type unsupportedSampler struct{}

func NewSampler() Sampler { return unsupportedSampler{} }

func (unsupportedSampler) Read(ctx context.Context) (Sample, error) {
	if err := ctx.Err(); err != nil {
		return Sample{}, err
	}
	return Sample{At: time.Now(), Pressure: Unknown, CPUs: runtime.NumCPU(), Unsupported: true}, nil
}
