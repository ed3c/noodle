package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/workspaceartifacts"
)

// ReclaimCleanupSession is called only by the quiescent original task owner
// after accepted completion or explicit failed-task closure. It preserves all
// original receipt bytes; its new closure binds the decision and evidence.
func (r *OpenShellRuntime) ReclaimCleanupSession(ctx context.Context, b CompletedSessionBinding, repository string, expected SessionStatus, reason string, authority SessionReadbackFileRef) (CleanupSessionReadback, error) {
	if strings.TrimSpace(reason) == "" {
		return CleanupSessionReadback{}, fmt.Errorf("execution closure reason missing")
	}
	if b.SessionID == "" || b.SessionID == "." || b.SessionID == ".." || filepath.Base(b.SessionID) != b.SessionID {
		return CleanupSessionReadback{}, fmt.Errorf("execution reclamation session identity missing")
	}
	if err := VerifySessionReadbackFiles(map[string]SessionReadbackFileRef{"task_closure": authority}); err != nil {
		return CleanupSessionReadback{}, err
	}
	dir := filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID)
	var receipt openShellDispatchReceipt
	if _, err := ReadSessionReadbackFile(filepath.Join(dir, "openshell-dispatch.json"), &receipt); err != nil {
		return CleanupSessionReadback{}, err
	}
	// A compacted publication remains verifiable without the original full copies.
	observed, err := r.ReadCleanupSession(ctx, b, repository, expected)
	if err != nil {
		return CleanupSessionReadback{}, err
	}
	if observed.Diagnostic != nil {
		return r.reclaimDiagnosticFailure(ctx, b, repository, reason, authority, receipt, observed)
	}
	delivery := filepath.Join(dir, "delivery")
	deletionRoots := []string{b.WorktreePath, receipt.Baseline.Directory, receipt.Delivery.ReturnedDirectory, filepath.Join(delivery, "baseline"), filepath.Join(delivery, "returned"), filepath.Join(delivery, "patch-repository")}
	evidence, err := prepareReclamationEvidence(authority, observed.Execution.Sources, deletionRoots, b.OrderID)
	if err != nil {
		return CleanupSessionReadback{}, err
	}
	if err := workspaceartifacts.CompactRetainedPublication(ctx, repository, receipt.Baseline, receipt.Delivery, observed.PublishedHead); err != nil {
		return CleanupSessionReadback{}, err
	}
	compactRef, err := ReadSessionReadbackFile(filepath.Join(dir, "delivery", "compaction.json"), nil)
	if err != nil {
		return CleanupSessionReadback{}, err
	}
	closurePath := filepath.Join(dir, "execution-closure.json")
	// Native custody is append-only mutable owner state, so retain the immutable
	// native observation in its tombstone instead of freezing the whole live ledger.
	evidence["publication_compaction"] = compactRef
	closure := OpenShellClosure{Identity: receipt.Worker.Identity, Outcome: expected, Reason: reason, Evidence: evidence}
	raw, err := json.Marshal(closure)
	if err != nil {
		return CleanupSessionReadback{}, err
	}
	if _, err := os.Lstat(closurePath); os.IsNotExist(err) {
		if err := filex.WriteFileAtomicDurable(closurePath, raw); err != nil {
			return CleanupSessionReadback{}, err
		}
	} else if err != nil {
		return CleanupSessionReadback{}, err
	}
	closureRef, err := ReadSessionReadbackFile(closurePath, nil)
	if err != nil {
		return CleanupSessionReadback{}, err
	}
	original, err := readOpenShellClosure(closureRef, closure.Identity)
	if err != nil {
		return CleanupSessionReadback{}, err
	}
	if original.Outcome != expected || original.Reason != reason || !maps.Equal(original.Evidence, evidence) {
		return CleanupSessionReadback{}, fmt.Errorf("execution closure authority differs")
	}
	retired := false
	for _, record := range r.config.Gateway.custody.Snapshot().Records {
		if record.Worker.Identity == closure.Identity && record.Retirement != nil {
			retired = true
		}
	}
	if !retired {
		if err := r.config.Gateway.Retire(ctx, r.config.ContextID, closure.Identity, closureRef); err != nil {
			return CleanupSessionReadback{}, err
		}
	}
	return r.ReadCleanupSession(ctx, b, repository, expected)
}
