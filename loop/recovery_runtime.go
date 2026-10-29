package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/poteto/noodle/dispatcher"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

func recoveryFiles(runtimeName string) []string {
	if runtimeName == dispatcher.CodexNativeRuntime {
		return []string{"spawn.json", "prompt.txt", "events.ndjson", "native.json"}
	}
	if runtimeName == "openshell" {
		return []string{"spawn.json", "prompt.txt", "events.ndjson", "meta.json", "openshell-dispatch.json", "delivery/result.json", "delivery/publish.json", "delivery/changes.patch"}
	}
	return recoverySessionFiles
}

// Recovery consumes the original runtime's evidence. An OpenShell worker has
// no host PID; a missing gateway or changed candidate cannot be substituted
// with process absence, a new runtime owner, or a fabricated process.json.
func (l *Loop) requireRecoveryStopped(order state.OrderNode, stage state.StageNode, b requestChangesBinding, processBytes []byte) error {
	if stage.Runtime == "openshell" {
		if !l.isAdmittedLocalTask(order, stage) {
			return fmt.Errorf("OpenShell review requires an admitted local task")
		}
		reader, ok := l.deps.Runtimes[stage.Runtime].(completedSessionReader)
		if !ok {
			return fmt.Errorf("original OpenShell completion reader unavailable")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		observed, err := reader.ReadCompletedSession(ctx, loopruntime.CompletedSessionBinding{OrderID: order.OrderID, SessionID: b.SessionID, WorktreePath: b.WorktreePath, Provider: stage.Provider, Model: stage.Model, StageIndex: stage.StageIndex, RetryCount: b.Attempt})
		if err != nil {
			return fmt.Errorf("original OpenShell completion unavailable: %w", err)
		}
		if observed.NativeAttemptID == "" || observed.CandidateHead != b.Head || observed.NativeExecExitCode != 0 || observed.NativePhase != "Stopped" || !observed.StopAcknowledged || observed.MainProcessInstanceID == "" || len(observed.Sources) == 0 {
			return fmt.Errorf("original OpenShell completion differs from reviewed candidate")
		}
		return loopruntime.VerifySessionReadbackFiles(observed.Sources)
	}
	if nonEmpty(stage.Runtime, "process") != "process" {
		return fmt.Errorf("review runtime %q has no stopped execution proof", stage.Runtime)
	}
	var process struct {
		SessionID string `json:"session_id"`
		PID       int    `json:"pid"`
	}
	if json.Unmarshal(processBytes, &process) != nil || process.SessionID != b.SessionID || process.PID <= 0 {
		return fmt.Errorf("recovery process identity mismatch")
	}
	absent, err := procx.ProcessGroupAbsent(process.PID)
	if err != nil {
		return err
	}
	if !absent {
		return fmt.Errorf("recovery prior process/group is live")
	}
	return nil
}
