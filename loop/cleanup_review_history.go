package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

func cleanupReviewRejected(attempt state.AttemptNode) bool {
	return attempt.Status == state.AttemptFailed && strings.HasPrefix(attempt.Error, "changes requested")
}

// A rejected review retains a failed canonical attempt. The original runtime
// still owes completed-candidate evidence, rather than interrupted progress.
func (l *Loop) cleanupReviewHistory(order state.OrderNode, stage state.StageNode, retry int) (loopruntime.SessionReadbackFileRef, error) {
	refuse := fmt.Errorf("cleanup rejected review has no bound requeue history")
	if retry < 0 || retry+1 >= len(stage.Attempts) {
		return loopruntime.SessionReadbackFileRef{}, refuse
	}
	path := filepath.Join(l.runtimeDir, "loop-events.ndjson")
	ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
	if err != nil {
		return ref, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ref, err
	}
	packet := requestChangesPacket{Order: order,
		Review:  state.PendingReviewNode{OrderID: order.OrderID, StageIndex: stage.StageIndex},
		Binding: requestChangesBinding{Attempt: retry, SessionID: stage.Attempts[retry].SessionID}}
	offset := 0
	for _, line := range bytes.SplitAfter(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) != 0 {
			var e event.LoopEvent
			if err := json.Unmarshal(line, &e); err != nil {
				return ref, fmt.Errorf("cleanup review history: %w", err)
			}
			if e.Type == event.LoopEventOrderRequeued {
				var payload struct {
					OrderID string `json:"order_id"`
				}
				if err := json.Unmarshal(e.Payload, &payload); err != nil {
					return ref, err
				}
				if payload.OrderID == order.OrderID && validateRequestChangesFailure(data[:offset], packet) == nil {
					return ref, loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"review_history": ref})
				}
			}
		}
		offset += len(line)
	}
	return ref, refuse
}
