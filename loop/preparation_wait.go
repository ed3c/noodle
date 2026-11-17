package loop

import (
	"errors"
	"io"
)

// Run alone calls this after the first complete Cycle and watcher setup. An
// already-dispatched or adopted context keeps its original full worker claim.
func (l *Loop) beginPreparationWait() (io.Closer, error) {
	if l.deps.PreparationReady == nil {
		return nil, nil
	}
	if l.resourceUnknown != nil {
		return nil, l.resourceUnknown
	}
	if !l.canonicalLoaded {
		return nil, errors.New("original preparation canonical state unavailable")
	}
	if l.registryErr != nil {
		return nil, l.registryErr
	}
	for _, order := range l.canonical.Orders {
		for _, stage := range order.Stages {
			if len(stage.Attempts) != 0 {
				return nil, nil
			}
		}
	}
	if err := l.checkQuiescent(); err != nil {
		return nil, err
	}
	return l.deps.PreparationReady()
}
