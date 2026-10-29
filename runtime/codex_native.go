package runtime

import (
	"context"

	"github.com/poteto/noodle/dispatcher"
)

type codexNativeRuntime struct {
	*DispatcherRuntime
	native *dispatcher.CodexNativeDispatcher
}

func NewCodexNativeRuntime(cfg dispatcher.ProcessDispatcherConfig) Runtime {
	native := dispatcher.NewCodexNativeDispatcher(cfg)
	return &codexNativeRuntime{DispatcherRuntime: NewDispatcherRuntime(dispatcher.CodexNativeRuntime, native, cfg.RuntimeDir), native: native}
}

func (r *codexNativeRuntime) Recover(ctx context.Context) ([]RecoveredSession, error) {
	rows, err := r.native.Recover(ctx)
	if err != nil {
		return nil, err
	}
	var recovered []RecoveredSession
	for _, row := range rows {
		recovered = append(recovered, RecoveredSession{OrderID: row.OrderID, RuntimeName: dispatcher.CodexNativeRuntime,
			SessionHandle: dispatcherSessionHandle{session: row.Session, runtimeDir: r.runtimeDir}, Reason: row.Reason})
	}
	return recovered, nil
}
