package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/workspaceartifacts"
)

func (r *OpenShellRuntime) reclaimDiagnosticFailure(ctx context.Context, b CompletedSessionBinding, repository, reason string, authority SessionReadbackFileRef, receipt openShellDispatchReceipt, observed CleanupSessionReadback) (CleanupSessionReadback, error) {
	if observed.Diagnostic.ExecutionBasis != openShellNotStarted || observed.OutcomeStatus != StatusFailed || receipt.Baseline == nil || receipt.Worker == nil {
		return CleanupSessionReadback{}, fmt.Errorf("diagnostic reclamation binding differs")
	}
	var current openShellDispatchReceipt
	dispatch, err := ReadSessionReadbackFile(observed.Execution.Sources["diagnostic_session/openshell-dispatch.json"].Path, &current)
	if err != nil || dispatch != observed.Execution.Sources["diagnostic_session/openshell-dispatch.json"] || !reflect.DeepEqual(current, receipt) {
		return CleanupSessionReadback{}, fmt.Errorf("diagnostic original dispatch changed before reclamation")
	}
	evidence, err := prepareReclamationEvidence(authority, observed.Execution.Sources, []string{b.WorktreePath, receipt.Baseline.Directory}, b.OrderID)
	if err != nil {
		return CleanupSessionReadback{}, err
	}
	// Freeze only original session sources in the baseline manifest. Closure
	// authority has its own durable, expanded evidence chain and can be issued
	// after a read-only baseline observation.
	if err = workspaceartifacts.CompactDiagnosticBaseline(ctx, repository, receipt.Baseline, b.WorktreePath, diagnosticBaselineSources(observed.Execution.Sources)); err != nil {
		return CleanupSessionReadback{}, err
	}
	dir := filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID)
	compactRef, err := ReadSessionReadbackFile(filepath.Join(dir, workspaceartifacts.DiagnosticBaselineCompactionName), nil)
	if err != nil {
		return CleanupSessionReadback{}, err
	}
	evidence["diagnostic_baseline_compaction"] = compactRef
	closure := OpenShellClosure{ExecutionBasis: openShellNotStarted, Identity: receipt.Worker.Identity, Outcome: StatusFailed, Reason: reason, Evidence: evidence}
	path := filepath.Join(dir, "execution-closure.json")
	raw, err := json.Marshal(closure)
	if err != nil {
		return CleanupSessionReadback{}, err
	}
	if _, err = os.Lstat(path); os.IsNotExist(err) {
		if err = filex.WriteFileAtomicDurable(path, raw); err != nil {
			return CleanupSessionReadback{}, err
		}
	} else if err != nil {
		return CleanupSessionReadback{}, err
	}
	ref, err := ReadSessionReadbackFile(path, nil)
	if err != nil {
		return CleanupSessionReadback{}, err
	}
	original, err := readOpenShellClosure(ref, closure.Identity)
	if err != nil {
		return CleanupSessionReadback{}, err
	}
	if original.ExecutionBasis != openShellNotStarted || original.Outcome != StatusFailed || original.Reason != reason || !maps.Equal(original.Evidence, evidence) {
		return CleanupSessionReadback{}, fmt.Errorf("diagnostic original closure authority differs")
	}
	retired := false
	for _, record := range r.config.Gateway.custody.Snapshot().Records {
		if record.Worker.Identity == closure.Identity && record.Retirement != nil {
			retired = true
		}
	}
	if !retired {
		if err = r.config.Gateway.Retire(ctx, r.config.ContextID, closure.Identity, ref); err != nil {
			return CleanupSessionReadback{}, err
		}
	}
	return r.ReadCleanupSession(ctx, b, repository, StatusFailed)
}
