package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/poteto/noodle/internal/filex"
)

type ExecutionReclaimedSession struct {
	OrderID   string                 `json:"order_id"`
	SessionID string                 `json:"session_id"`
	Readback  CleanupSessionReadback `json:"readback"`
}

type ExecutionReclamationReceipt struct {
	Schema    int                         `json:"schema"`
	OrderID   string                      `json:"order_id"`
	Authority SessionReadbackFileRef      `json:"authority"`
	Sessions  []ExecutionReclaimedSession `json:"sessions"`
	Worktrees map[string]string           `json:"worktrees"`
}

// Freeze the original mutable custody source at the owner's observed retirement
// boundary. Future orders may append custody history without invalidating this
// historical receipt. Never re-label it as a fresh observation after detach.
func RetainReclamationCustody(value CleanupSessionReadback, destination string) (CleanupSessionReadback, error) {
	if value.Retirement == nil || !value.Retirement.DeleteAcknowledged || !value.Retirement.AbsenceObserved {
		return value, fmt.Errorf("native retirement has not been observed")
	}
	ref, ok := value.Execution.Sources["native_custody"]
	if !ok {
		return value, fmt.Errorf("original retirement custody source missing")
	}
	if err := VerifySessionReadbackFiles(value.Execution.Sources); err != nil {
		return value, err
	}
	raw, err := os.ReadFile(ref.Path)
	if err != nil {
		return value, err
	}
	if retainedRecoveryHash(raw) != ref.SHA256 {
		return value, fmt.Errorf("native custody changed before retention")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return value, err
	}
	if _, err := os.Lstat(destination); os.IsNotExist(err) {
		if err := filex.WriteFileAtomicDurable(destination, raw); err != nil {
			return value, err
		}
	} else if err != nil {
		return value, err
	}
	saved, err := ReadSessionReadbackFile(destination, nil)
	if err != nil {
		return value, err
	}
	// A retry retains its first observation; current custody can have more history.
	if saved.SHA256 != ref.SHA256 {
		var original, current OpenShellCustodyState
		if _, err := ReadSessionReadbackFile(saved.Path, &original); err != nil {
			return value, err
		}
		if _, err := ReadSessionReadbackFile(ref.Path, &current); err != nil {
			return value, err
		}
		if original.Version != current.Version || original.Binding != current.Binding {
			return value, fmt.Errorf("original immutable retirement custody binding differs")
		}
		var oldRecord, newRecord *OpenShellCustodyRecord
		for i := range original.Records {
			if original.Records[i].Worker.Identity.AttemptID == value.Execution.NativeAttemptID {
				if oldRecord != nil {
					return value, fmt.Errorf("original retirement custody is ambiguous")
				}
				oldRecord = &original.Records[i]
			}
		}
		for i := range current.Records {
			if current.Records[i].Worker.Identity.AttemptID == value.Execution.NativeAttemptID {
				if newRecord != nil {
					return value, fmt.Errorf("current retirement custody is ambiguous")
				}
				newRecord = &current.Records[i]
			}
		}
		if oldRecord == nil || newRecord == nil || !reflect.DeepEqual(oldRecord, newRecord) {
			return value, fmt.Errorf("original immutable retired worker differs")
		}
	}
	sources := map[string]SessionReadbackFileRef{}
	for name, source := range value.Execution.Sources {
		sources[name] = source
	}
	sources["native_custody"] = saved
	value.Execution.Sources = sources
	return value, nil
}

// ReadExecutionReclamationReceipt is read-only original-owner evidence. Its
// native absence was observed at cleanup time; it does not acquire a new lease.
func ReadExecutionReclamationReceipt(path, orderID string, authority SessionReadbackFileRef) (ExecutionReclamationReceipt, SessionReadbackFileRef, error) {
	var value ExecutionReclamationReceipt
	ref, err := ReadSessionReadbackFile(path, &value)
	if err != nil {
		return value, ref, err
	}
	if value.Schema != 1 || value.OrderID != orderID || value.Authority != authority || len(value.Sessions) == 0 || len(value.Worktrees) == 0 {
		return value, ref, fmt.Errorf("original execution reclamation receipt binding differs")
	}
	if err := VerifySessionReadbackFiles(map[string]SessionReadbackFileRef{"authority": authority}); err != nil {
		return value, ref, err
	}
	seen := map[string]bool{}
	for _, session := range value.Sessions {
		if session.OrderID != orderID || session.SessionID == "" || seen[session.SessionID] {
			return value, ref, fmt.Errorf("original reclaimed session identity differs")
		}
		seen[session.SessionID] = true
		v := session.Readback.Retirement
		basis := "historical_stopped_with_fresh_native_absence"
		if session.Readback.Diagnostic != nil {
			if session.Readback.Diagnostic.ExecutionBasis != openShellNotStarted || session.Readback.OutcomeStatus != StatusFailed || session.Readback.Execution.NativeExecExitCode != -1 {
				return value, ref, fmt.Errorf("diagnostic execution was not proven unsubmitted")
			}
			basis = "historical_not_started_stopped_with_fresh_native_absence"
		}
		if v == nil || v.ObservationBasis != basis || !v.DeleteAcknowledged || !v.AbsenceObserved || v.DeleteRequestID == "" {
			return value, ref, fmt.Errorf("original native retirement evidence incomplete")
		}
		if err := VerifySessionReadbackFiles(session.Readback.Execution.Sources); err != nil {
			return value, ref, err
		}
		var custody OpenShellCustodyState
		source, ok := session.Readback.Execution.Sources["native_custody"]
		if !ok {
			return value, ref, fmt.Errorf("original retirement custody source missing")
		}
		if _, err := ReadSessionReadbackFile(source.Path, &custody); err != nil {
			return value, ref, err
		}
		if custody.Version != openShellCustodyVersion || custody.Blocked != "" {
			return value, ref, fmt.Errorf("retained original custody schema or owner state differs")
		}
		matched := 0
		for _, record := range custody.Records {
			if record.Worker.Identity.OrderID != orderID || record.Worker.Identity.SessionID != session.SessionID {
				continue
			}
			matched++
			if record.Worker.Identity.AttemptID != session.Readback.Execution.NativeAttemptID || record.MainProcessInstanceID != session.Readback.Execution.MainProcessInstanceID || session.Readback.Execution.NativePhase != "Stopped" || !session.Readback.Execution.StopAcknowledged {
				return value, ref, fmt.Errorf("retained original session observation differs")
			}
			if record.Retirement == nil || record.Retirement.Closure != v.Closure || record.Retirement.RequestID != v.DeleteRequestID || !record.Retirement.Acknowledged || !record.Retirement.Completed {
				return value, ref, fmt.Errorf("original native tombstone differs")
			}
			if err := validateRetirement(record); err != nil {
				return value, ref, err
			}
			closure, err := readOpenShellClosure(v.Closure, record.Worker.Identity)
			if err != nil {
				return value, ref, err
			}
			if closure.Evidence["task_closure"] != authority || closure.Outcome != session.Readback.OutcomeStatus {
				return value, ref, fmt.Errorf("original native closure authority or outcome differs")
			}
		}
		if matched != 1 {
			return value, ref, fmt.Errorf("original retired worker missing or ambiguous")
		}
	}
	return value, ref, VerifySessionReadbackFiles(map[string]SessionReadbackFileRef{"reclamation_receipt": ref})
}
